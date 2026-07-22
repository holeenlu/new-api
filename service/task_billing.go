package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// CreateTaskSubmissionMarker persists the billing owner before the first
// upstream write. PENDING_SUBMIT rows are excluded from ordinary polling but
// remain visible to the timeout sweeper, so a crash cannot leave a reservation
// without a durable reconciliation marker.
func CreateTaskSubmissionMarker(info *relaycommon.RelayInfo, platform constant.TaskPlatform) (*model.Task, error) {
	if info == nil || info.TaskRelayInfo == nil {
		return nil, fmt.Errorf("task relay info is unavailable")
	}
	if info.PersistedTaskID > 0 {
		return model.RenewPendingTaskSubmission(info.PersistedTaskID)
	}

	fundingQuota := 0
	tokenQuota := 0
	tokenTracked := false
	if info.Billing != nil {
		session, ok := info.Billing.(*BillingSession)
		if !ok {
			return nil, fmt.Errorf("unsupported task billing session %T", info.Billing)
		}
		session.mu.Lock()
		if session.settled || session.refunded {
			session.mu.Unlock()
			return nil, fmt.Errorf("task billing session is already closed")
		}
		fundingQuota = session.preConsumedQuota
		tokenQuota = session.tokenConsumed
		tokenTracked = session.relayInfo != nil && !session.relayInfo.IsPlayground
		session.mu.Unlock()
	}
	if fundingQuota < 0 || fundingQuota > common.MaxQuota || tokenQuota < 0 || tokenQuota > common.MaxQuota {
		return nil, fmt.Errorf("invalid task billing reservation funding=%d token=%d", fundingQuota, tokenQuota)
	}
	if tokenQuota > 0 && info.TokenId <= 0 {
		return nil, fmt.Errorf("task token reservation %d has no token id", tokenQuota)
	}

	task := model.InitTask(platform, info)
	task.Status = model.TaskStatusPendingSubmit
	task.Action = info.Action
	task.Quota = fundingQuota
	task.PrivateData.BillingSource = info.BillingSource
	task.PrivateData.SubscriptionId = info.SubscriptionId
	task.PrivateData.TokenId = info.TokenId
	task.PrivateData.NodeName = common.NodeName
	task.PrivateData.BillingReservation = &model.TaskBillingReservation{
		FundingQuota: fundingQuota,
		TokenQuota:   tokenQuota,
		TokenTracked: tokenTracked,
	}
	task.PrivateData.BillingContext = &model.TaskBillingContext{
		ModelPrice:      info.PriceData.ModelPrice,
		GroupRatio:      info.PriceData.GroupRatioInfo.GroupRatio,
		ModelRatio:      info.PriceData.ModelRatio,
		OtherRatios:     info.PriceData.OtherRatios(),
		OriginModelName: info.OriginModelName,
		PerCallBilling:  common.StringsContains(constant.TaskPricePatches, info.OriginModelName) || info.PriceData.UsePrice,
	}
	if err := task.Insert(); err != nil {
		return nil, err
	}
	info.PersistedTaskID = task.ID
	return task, nil
}

// CommitTaskSubmission first makes the accepted upstream task pollable, then
// atomically advances funding, token usage, and the task refund ledger. If the
// settlement transaction fails, the accepted task remains persisted with its
// original exact reservations and can still be polled or refunded later.
func CommitTaskSubmission(c *gin.Context, info *relaycommon.RelayInfo, task *model.Task, actualQuota int) error {
	if info == nil || task == nil || task.ID <= 0 {
		return fmt.Errorf("invalid task submission settlement")
	}
	if actualQuota < 0 {
		return fmt.Errorf("actual task quota cannot be negative: %d", actualQuota)
	}

	if task.Status == model.TaskStatusPendingSubmit {
		renewed, err := model.RenewPendingTaskSubmission(task.ID)
		if err != nil {
			return fmt.Errorf("renew accepted task marker: %w", err)
		}
		// Preserve the accepted metadata assembled from the upstream response;
		// only the lease generation comes from the renewed persisted row.
		task.SubmitTime = renewed.SubmitTime
		fromStatus := task.Status
		task.Status = model.TaskStatusSubmitted
		won, err := task.UpdateWithStatus(fromStatus)
		if err != nil {
			return fmt.Errorf("persist accepted task: %w", err)
		}
		if !won {
			persisted, err := model.GetTaskByID(task.ID)
			if err != nil {
				return fmt.Errorf("reload accepted task: %w", err)
			}
			if persisted.Status != model.TaskStatusSubmitted {
				return fmt.Errorf("task %d changed to status %s before acceptance commit", task.ID, persisted.Status)
			}
			*task = *persisted
		}
	}

	if info.Billing == nil {
		if actualQuota != 0 {
			return fmt.Errorf("task %d has quota %d without a billing session", task.ID, actualQuota)
		}
		return nil
	}
	session, ok := info.Billing.(*BillingSession)
	if !ok {
		return fmt.Errorf("unsupported task billing session %T", info.Billing)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.refunded {
		return fmt.Errorf("task billing session already refunded")
	}
	if session.settled {
		persisted, err := model.GetTaskByID(task.ID)
		if err != nil {
			return err
		}
		*task = *persisted
		return nil
	}
	settledTask, _, _, err := model.SettleTaskQuotaAtomically(task.ID, actualQuota)
	if err != nil {
		return err
	}

	fundingDelta := actualQuota - session.preConsumedQuota
	session.fundingSettled = true
	session.settled = true
	if session.funding.Source() == BillingSourceSubscription {
		info.SubscriptionPostDelta += int64(fundingDelta)
	}
	*task = *settledTask

	if actualQuota != 0 {
		if info.BillingSource == BillingSourceSubscription {
			checkAndSendSubscriptionQuotaNotify(info)
		} else {
			checkAndSendQuotaNotify(info, fundingDelta, session.preConsumedQuota)
		}
	}
	return nil
}

// FailTaskSubmission transfers a terminal pre-acceptance failure to the
// persisted marker and its atomic refund path. Callers must not also invoke
// BillingSession.Refund after a marker has been created.
func FailTaskSubmission(ctx context.Context, taskID int64, reason string) bool {
	task, err := model.GetTaskByID(taskID)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("load pending task %d for failure: %s", taskID, err.Error()))
		return false
	}
	if task.Status == model.TaskStatusPendingSubmit {
		fromStatus := task.Status
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.FinishTime = time.Now().Unix()
		task.FailReason = reason
		won, err := task.UpdateWithStatus(fromStatus)
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("persist pending task %s failure: %s", task.TaskID, err.Error()))
			return false
		}
		if !won {
			task, err = model.GetTaskByID(taskID)
			if err != nil {
				logger.LogWarn(ctx, fmt.Sprintf("reload pending task %d after CAS: %s", taskID, err.Error()))
				return false
			}
		}
	}
	if task.Status != model.TaskStatusFailure {
		logger.LogWarn(ctx, fmt.Sprintf("pending task %s cannot fail from status %s", task.TaskID, task.Status))
		return false
	}
	return RefundTaskQuota(ctx, task, reason)
}

// LogTaskConsumption 记录任务消费日志和统计信息（仅记录，不涉及实际扣费）。
// 实际扣费已由 BillingSession（PreConsumeBilling + SettleBilling）完成。
func LogTaskConsumption(c *gin.Context, info *relaycommon.RelayInfo) {
	tokenName := c.GetString("token_name")
	logContent := fmt.Sprintf("操作 %s", info.Action)
	// 支持任务仅按次计费
	if common.StringsContains(constant.TaskPricePatches, info.OriginModelName) {
		logContent = fmt.Sprintf("%s，按次计费", logContent)
	} else {
		if otherRatios := info.PriceData.OtherRatios(); len(otherRatios) > 0 {
			var contents []string
			for key, ra := range otherRatios {
				if 1.0 != ra {
					contents = append(contents, fmt.Sprintf("%s: %.2f", key, ra))
				}
			}
			if len(contents) > 0 {
				logContent = fmt.Sprintf("%s, 计算参数：%s", logContent, strings.Join(contents, ", "))
			}
		}
	}
	other := make(map[string]interface{})
	other["is_task"] = true
	other["request_path"] = c.Request.URL.Path
	other["model_price"] = info.PriceData.ModelPrice
	if info.PriceData.ModelRatio > 0 {
		other["model_ratio"] = info.PriceData.ModelRatio
	}
	other["group_ratio"] = info.PriceData.GroupRatioInfo.GroupRatio
	if info.PriceData.GroupRatioInfo.HasSpecialRatio {
		other["user_group_ratio"] = info.PriceData.GroupRatioInfo.GroupSpecialRatio
	}
	if info.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = info.UpstreamModelName
	}
	attachQuotaSaturation(c, info, other)
	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId: info.ChannelId,
		ModelName: info.OriginModelName,
		TokenName: tokenName,
		Quota:     info.PriceData.Quota,
		Content:   logContent,
		TokenId:   info.TokenId,
		Group:     info.UsingGroup,
		Other:     other,
	})
	model.UpdateUserUsedQuotaAndRequestCount(info.UserId, info.PriceData.Quota)
	model.UpdateChannelUsedQuota(info.ChannelId, info.PriceData.Quota)
}

// ---------------------------------------------------------------------------
// 异步任务计费辅助函数
// ---------------------------------------------------------------------------

// taskBillingOther 从 task 的 BillingContext 构建日志 Other 字段。
func taskBillingOther(task *model.Task) map[string]interface{} {
	other := make(map[string]interface{})
	if bc := task.PrivateData.BillingContext; bc != nil {
		other["model_price"] = bc.ModelPrice
		if bc.ModelRatio > 0 {
			other["model_ratio"] = bc.ModelRatio
		}
		other["group_ratio"] = bc.GroupRatio
		if priceData := taskBillingContextPriceData(bc); priceData != nil {
			for k, v := range priceData.OtherRatios() {
				other[k] = v
			}
		}
	}
	props := task.Properties
	if props.UpstreamModelName != "" && props.UpstreamModelName != props.OriginModelName {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = props.UpstreamModelName
	}
	return other
}

func taskBillingContextPriceData(bc *model.TaskBillingContext) *types.PriceData {
	if bc == nil || len(bc.OtherRatios) == 0 {
		return nil
	}
	priceData := &types.PriceData{}
	if !priceData.ReplaceOtherRatios(bc.OtherRatios) {
		return nil
	}
	return priceData
}

// taskModelName 从 BillingContext 或 Properties 中获取模型名称。
func taskModelName(task *model.Task) string {
	if bc := task.PrivateData.BillingContext; bc != nil && bc.OriginModelName != "" {
		return bc.OriginModelName
	}
	return task.Properties.OriginModelName
}

// RefundTaskQuota 统一的任务失败退款逻辑。
// quota marker、资金来源和 token 账本在同一主库事务中提交；失败时全部
// 回滚供后续对账重试，重复调用不会再次退款。
func RefundTaskQuota(ctx context.Context, task *model.Task, reason string) bool {
	quota := task.Quota
	if quota == 0 {
		return true
	}

	refunded, err := model.RefundTaskQuotaAtomically(task.ID, quota)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("原子退还任务额度失败 task %s: %s", task.TaskID, err.Error()))
		return false
	}
	task.Quota = 0
	if !refunded {
		// Another caller already committed the same refund. Do not duplicate its
		// accounting log.
		return true
	}

	// The primary-database ledgers and marker are committed exactly once; the
	// log database remains best-effort observability.
	other := taskBillingOther(task)
	other["task_id"] = task.TaskID
	other["reason"] = reason
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeRefund,
		Content:   "",
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     quota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
	})

	return true
}

// RecalculateTaskQuota 通用的异步差额结算。
// actualQuota 是任务完成后的实际应扣额度，与预扣额度 (task.Quota) 做差额结算。
// reason 用于日志记录（例如 "token重算" 或 "adaptor调整"）。
// clamps 可选：若计算 actualQuota 时发生额度饱和，将其记入日志 admin_info（仅管理员可见）。
func RecalculateTaskQuota(ctx context.Context, task *model.Task, actualQuota int, reason string, clamps ...*common.QuotaClamp) {
	if actualQuota <= 0 {
		return
	}
	if task.ID <= 0 {
		logger.LogError(ctx, fmt.Sprintf("任务差额原子结算失败 task %s: task is not persisted", task.TaskID))
		return
	}

	// Always reconcile against the locked persisted reservation. A caller may
	// hold an old Task.Quota after another poller has already settled this row;
	// deriving the delta from that snapshot would duplicate or overstate stats.
	settledTask, quotaDelta, settlementChanged, err := model.SettleTaskQuotaAtomically(task.ID, actualQuota)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务差额原子结算失败 task %s: %s", task.TaskID, err.Error()))
		return
	}
	*task = *settledTask
	if !settlementChanged {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 预扣费准确（%s，%s）",
			task.TaskID, logger.LogQuota(actualQuota), reason))
		return
	}
	preConsumedQuota := actualQuota - quotaDelta

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 差额结算：delta=%s（实际：%s，预扣：%s，%s）",
		task.TaskID,
		logger.LogQuota(quotaDelta),
		logger.LogQuota(actualQuota),
		logger.LogQuota(preConsumedQuota),
		reason,
	))

	var logType int
	var logQuota int
	if quotaDelta > 0 {
		logType = model.LogTypeConsume
		logQuota = quotaDelta
		// The accepted submission already owns this task's request count.
		// Completion reconciliation adjusts usage only.
		model.UpdateUserUsedQuota(task.UserId, quotaDelta)
		model.UpdateChannelUsedQuota(task.ChannelId, quotaDelta)
	} else {
		logType = model.LogTypeRefund
		logQuota = -quotaDelta
	}
	other := taskBillingOther(task)
	other["task_id"] = task.TaskID
	other["pre_consumed_quota"] = preConsumedQuota
	other["actual_quota"] = actualQuota
	for _, clamp := range clamps {
		attachQuotaSaturationToOther(other, clamp)
	}
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   logType,
		Content:   reason,
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     logQuota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
		NodeName:  task.PrivateData.NodeName,
	})
}

// RecalculateTaskQuotaByTokens 根据实际 token 消耗重新计费（异步差额结算）。
// 当任务成功且返回了 totalTokens 时，根据模型倍率和分组倍率重新计算实际扣费额度，
// 与预扣费的差额进行补扣或退还。支持钱包和订阅计费来源。
func RecalculateTaskQuotaByTokens(ctx context.Context, task *model.Task, totalTokens int) {
	if totalTokens <= 0 {
		return
	}

	modelName := taskModelName(task)

	// 获取模型价格和倍率
	modelRatio, hasRatioSetting, _ := ratio_setting.GetModelRatio(modelName)
	// 只有配置了倍率(非固定价格)时才按 token 重新计费
	if !hasRatioSetting || modelRatio <= 0 {
		return
	}

	// 获取用户和组的倍率信息
	group := task.Group
	if group == "" {
		user, err := model.GetUserById(task.UserId, false)
		if err == nil {
			group = user.Group
		}
	}
	if group == "" {
		return
	}

	groupRatio := ratio_setting.GetGroupRatio(group)
	userGroupRatio, hasUserGroupRatio := ratio_setting.GetGroupGroupRatio(group, group)

	var finalGroupRatio float64
	if hasUserGroupRatio {
		finalGroupRatio = userGroupRatio
	} else {
		finalGroupRatio = groupRatio
	}

	// 计算 OtherRatios 乘积（视频折扣、时长等）
	otherMultiplier := 1.0
	if priceData := taskBillingContextPriceData(task.PrivateData.BillingContext); priceData != nil {
		otherMultiplier = priceData.OtherRatioMultiplier()
	}

	// 计算实际应扣费额度: totalTokens * modelRatio * groupRatio * otherMultiplier（饱和转换，防止溢出成负数）
	actualQuota, clamp := common.QuotaFromFloatChecked(float64(totalTokens) * modelRatio * finalGroupRatio * otherMultiplier)

	reason := fmt.Sprintf("token重算：tokens=%d, modelRatio=%.2f, groupRatio=%.2f, otherMultiplier=%.4f", totalTokens, modelRatio, finalGroupRatio, otherMultiplier)
	RecalculateTaskQuota(ctx, task, actualQuota, reason, clamp)
}

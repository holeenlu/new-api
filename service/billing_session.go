package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
type BillingSession struct {
	relayInfo        *relaycommon.RelayInfo
	funding          FundingSource
	preConsumedQuota int   // 资金来源实际预扣额度（信任用户可能为 0）
	tokenConsumed    int   // token 数据库实际预扣额度
	extraReserved    int   // 发送前补充的资金预扣（订阅退款时单独回滚）
	fundingSettled   bool  // funding.Settle 已成功，资金来源已提交
	settled          bool  // Settle 全部完成（资金 + 令牌）
	settlementErr    error // 非幂等资金结算尝试或后续 token 对账的粘性错误
	refunded         bool  // Refund 已调用
	mu               sync.Mutex
}

// Settle 根据实际消耗额度进行结算。
// 资金来源和令牌额度分两步提交：若资金来源已提交但令牌调整失败，
// 会标记 fundingSettled 防止 Refund 对已提交的资金来源执行退款。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if actualQuota < 0 {
		return fmt.Errorf("actual quota cannot be negative: %d", actualQuota)
	}
	if s.refunded {
		// 预扣已退回，delta 结算的基准不复存在；此时结算会按 actual-preConsumed
		// 少扣。会话生命周期要求"先结算、失败才退款"，违反即为调用方时序错误。
		return fmt.Errorf("billing session already refunded; cannot settle %d", actualQuota)
	}
	if s.settled {
		return s.settlementErr
	}
	if s.settlementErr != nil {
		// Funding settlement is not idempotent and a database/transport error may
		// be commit-unknown. Never issue the same delta a second time from this
		// in-memory session; callers can still refund a definitely uncommitted
		// reservation according to the ordinary failure path.
		return s.settlementErr
	}
	fundingDelta := actualQuota - s.preConsumedQuota
	tokenDelta := 0
	if s.relayInfo != nil && !s.relayInfo.IsPlayground {
		tokenDelta = actualQuota - s.tokenConsumed
	}
	if fundingDelta == 0 && tokenDelta == 0 {
		s.fundingSettled = true
		s.settled = true
		return nil
	}
	// 1) 调整资金来源（仅在尚未提交时执行，防止重复调用）
	if !s.fundingSettled {
		if err := s.funding.Settle(fundingDelta); err != nil {
			s.settlementErr = err
			return err
		}
		s.fundingSettled = true
	}
	// 2) 调整令牌额度
	var tokenErr error
	if !s.relayInfo.IsPlayground {
		if tokenDelta > 0 {
			tokenErr = model.ConsumeTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, tokenDelta)
		} else if tokenDelta < 0 {
			tokenErr = model.RestoreTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -tokenDelta)
		}
		if tokenErr != nil {
			// 资金来源已提交，令牌调整失败只能记录日志；标记 settled 防止 Refund 误退资金
			common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
				s.relayInfo.UserId, s.relayInfo.TokenId, tokenDelta, tokenErr.Error()))
		}
	}
	// 3) 更新 relayInfo 上的订阅 PostDelta（用于日志）
	if s.funding.Source() == BillingSourceSubscription {
		s.relayInfo.SubscriptionPostDelta += int64(fundingDelta)
	}
	s.settled = true
	s.settlementErr = tokenErr
	return tokenErr
}

// Refund 退还所有预扣费，幂等安全，异步执行。
func (s *BillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	if s.settled || s.refunded || !s.needsRefundLocked() {
		s.mu.Unlock()
		return
	}
	s.refunded = true
	s.mu.Unlock()

	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费（token_quota=%s, funding=%s）",
		s.relayInfo.UserId,
		logger.FormatQuota(s.tokenConsumed),
		s.funding.Source(),
	))

	// 复制需要的值到闭包中
	tokenId := s.relayInfo.TokenId
	tokenKey := s.relayInfo.TokenKey
	isPlayground := s.relayInfo.IsPlayground
	tokenConsumed := s.tokenConsumed
	extraReserved := s.extraReserved
	subscriptionId := s.relayInfo.SubscriptionId
	funding := s.funding

	gopool.Go(func() {
		// 1) 退还资金来源
		if err := funding.Refund(); err != nil {
			common.SysLog("error refunding billing source: " + err.Error())
		}
		if extraReserved > 0 && funding.Source() == BillingSourceSubscription && subscriptionId > 0 {
			if err := model.PostConsumeUserSubscriptionDelta(subscriptionId, -int64(extraReserved)); err != nil {
				common.SysLog("error refunding subscription extra reserved quota: " + err.Error())
			}
		}
		// 2) 退还令牌额度
		if tokenConsumed > 0 && !isPlayground {
			if err := model.RestoreTokenQuota(tokenId, tokenKey, tokenConsumed); err != nil {
				common.SysLog("error refunding token quota: " + err.Error())
			}
		}
	})
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

// FundingCommitted 返回资金来源是否已成功提交结算差额。
// 例如原预留高于最终费用时，差额退款成功但令牌调整失败会返回 true；
// 资金差额本身失败时保持 false，原预留仍可整体退款。
func (s *BillingSession) FundingCommitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fundingSettled
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.refunded || s.fundingSettled {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	if s.tokenConsumed > 0 {
		return true
	}
	if wallet, ok := s.funding.(*WalletFunding); ok && wallet.consumed > 0 {
		return true
	}
	// 订阅可能在 tokenConsumed=0 时仍预扣了额度
	if sub, ok := s.funding.(*SubscriptionFunding); ok && sub.preConsumed > 0 {
		return true
	}
	return false
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preConsumedQuota
}

func (s *BillingSession) Reserve(targetQuota int) error {
	return s.reserve(targetQuota)
}

// ReserveStrict reserves the full target even when the original request used
// the trusted-user pre-consume bypass. Violation fees use this before settle so
// a positive settlement delta can never overdraw a wallet.
func (s *BillingSession) ReserveStrict(targetQuota int) error {
	return s.reserve(targetQuota)
}

func (s *BillingSession) reserve(targetQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if targetQuota < 0 {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("reserve target quota cannot be negative: %d", targetQuota),
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if s.settled {
		return fmt.Errorf("billing session already settled; cannot reserve %d", targetQuota)
	}
	if s.refunded {
		return fmt.Errorf("billing session already refunded; cannot reserve %d", targetQuota)
	}
	fundingDelta := targetQuota - s.preConsumedQuota
	if fundingDelta < 0 {
		fundingDelta = 0
	}
	tokenDelta := 0
	if !s.relayInfo.IsPlayground {
		tokenDelta = targetQuota - s.tokenConsumed
		if tokenDelta < 0 {
			tokenDelta = 0
		}
	}
	if fundingDelta == 0 && tokenDelta == 0 {
		return nil
	}

	// Match initial pre-consume ordering: reserve token quota first, then commit
	// the funding source. If funding rejects the full amount, token quota can be
	// restored without ever creating a partial financial charge.
	if err := s.reserveToken(tokenDelta); err != nil {
		return err
	}
	if fundingDelta > 0 {
		if err := s.reserveFunding(fundingDelta); err != nil {
			if err := s.rollbackTokenReserve(tokenDelta); err != nil {
				s.tokenConsumed += tokenDelta
				s.syncRelayInfo()
			}
			return err
		}
	}

	s.preConsumedQuota += fundingDelta
	s.tokenConsumed += tokenDelta
	s.extraReserved += fundingDelta
	s.syncRelayInfo()
	return nil
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口（含信任额度旁路）
// ---------------------------------------------------------------------------

// preConsume 执行预扣费：信任检查 -> 令牌预扣 -> 资金来源预扣。
// 任一步骤失败时原子回滚已完成的步骤。
func (s *BillingSession) preConsume(c *gin.Context, quota int) *types.NewAPIError {
	return s.preConsumeMode(c, quota, false)
}

func (s *BillingSession) preConsumeStrict(c *gin.Context, quota int) *types.NewAPIError {
	return s.preConsumeMode(c, quota, true)
}

func (s *BillingSession) preConsumeMode(c *gin.Context, quota int, strict bool) *types.NewAPIError {
	fundingQuota := quota

	// ---- 资金来源信任旁路 ----
	if !strict && s.shouldTrust() {
		fundingQuota = 0
		logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足, 信任且不需要资金预扣 (funding=%s)", s.relayInfo.UserId, s.funding.Source()))
	} else if fundingQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s (funding=%s)", s.relayInfo.UserId, logger.FormatQuota(fundingQuota), s.funding.Source()))
	}

	// ---- 1) token 始终独立预扣；信任优化只影响资金来源 ----
	if quota > 0 {
		tokenErr := PreConsumeTokenQuota(s.relayInfo, quota)
		if tokenErr != nil {
			return types.NewErrorWithStatusCode(tokenErr, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if !s.relayInfo.IsPlayground {
			s.tokenConsumed = quota
		}
	}

	// ---- 2) 预扣资金来源 ----
	var fundingErr error
	if strict {
		fundingErr = s.funding.PreConsumeStrict(fundingQuota)
	} else {
		fundingErr = s.funding.PreConsume(fundingQuota)
	}
	if fundingErr != nil {
		// 预扣费失败，回滚令牌额度
		if s.tokenConsumed > 0 && !s.relayInfo.IsPlayground {
			rollbackErr := model.RestoreTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed)
			if rollbackErr != nil {
				common.SysLog(fmt.Sprintf("error rolling back token quota (userId=%d, tokenId=%d, amount=%d, fundingErr=%s): %s",
					s.relayInfo.UserId, s.relayInfo.TokenId, s.tokenConsumed, fundingErr.Error(), rollbackErr.Error()))
			} else {
				s.tokenConsumed = 0
			}
		}
		if errors.Is(fundingErr, model.ErrInsufficientUserQuota) {
			return types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足: %w", fundingErr),
				types.ErrorCodeInsufficientUserQuota,
				http.StatusForbidden,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithNoRecordErrorLog(),
			)
		}
		// TODO: model 层应定义哨兵错误（如 ErrNoActiveSubscription），用 errors.Is 替代字符串匹配
		errMsg := fundingErr.Error()
		if strings.Contains(errMsg, "no active subscription") || strings.Contains(errMsg, "subscription quota insufficient") {
			return types.NewErrorWithStatusCode(fmt.Errorf("订阅额度不足或未配置订阅: %s", errMsg), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		return types.NewError(fundingErr, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}

	s.preConsumedQuota = fundingQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return nil
}

func (s *BillingSession) reserveFunding(delta int) error {
	switch funding := s.funding.(type) {
	case *WalletFunding:
		err := model.DecreaseUserQuotaIfEnough(funding.userId, delta)
		if err != nil {
			if errors.Is(err, model.ErrInsufficientUserQuota) {
				return types.NewErrorWithStatusCode(
					fmt.Errorf("用户额度不足: %w", err),
					types.ErrorCodeInsufficientUserQuota,
					http.StatusForbidden,
					types.ErrOptionWithSkipRetry(),
					types.ErrOptionWithNoRecordErrorLog(),
				)
			}
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		funding.consumed += delta
		return nil
	case *SubscriptionFunding:
		if err := model.PostConsumeUserSubscriptionDelta(funding.subscriptionId, int64(delta)); err != nil {
			return types.NewErrorWithStatusCode(
				fmt.Errorf("订阅额度不足或未配置订阅: %s", err.Error()),
				types.ErrorCodeInsufficientUserQuota,
				http.StatusForbidden,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithNoRecordErrorLog(),
			)
		}
		return nil
	default:
		return types.NewError(fmt.Errorf("unsupported funding source: %s", s.funding.Source()), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}
}

func (s *BillingSession) reserveToken(delta int) error {
	if delta <= 0 || s.relayInfo.IsPlayground {
		return nil
	}
	err := PreConsumeTokenQuota(s.relayInfo, delta)
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return nil
}

func (s *BillingSession) rollbackTokenReserve(delta int) error {
	if delta <= 0 || s.relayInfo.IsPlayground {
		return nil
	}
	err := model.RestoreTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
	if err != nil {
		common.SysLog("error rolling back token quota after funding reservation failed: " + err.Error())
		return err
	}
	return nil
}

// shouldTrust 统一信任额度检查，适用于钱包和订阅。
func (s *BillingSession) shouldTrust() bool {
	// 异步任务（ForcePreConsume=true）必须预扣全额，不允许信任旁路
	if s.relayInfo.ForcePreConsume {
		return false
	}

	trustQuota := common.GetTrustQuota()
	if trustQuota <= 0 {
		return false
	}

	switch s.funding.Source() {
	case BillingSourceWallet:
		return s.relayInfo.UserQuota > trustQuota
	case BillingSourceSubscription:
		// 订阅不能启用信任旁路。原因：
		// 1. PreConsumeUserSubscription 要求 amount>0 来创建预扣记录并锁定订阅
		// 2. SubscriptionFunding.PreConsume 忽略参数，始终用 s.amount 预扣
		// 3. 若信任旁路将 funding quota 设为 0，会导致预扣记录与会话状态不一致
		return false
	default:
		return false
	}
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的兼容字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()

	if sub, ok := s.funding.(*SubscriptionFunding); ok {
		info.SubscriptionId = sub.subscriptionId
		info.SubscriptionPreConsumed = sub.preConsumed + int64(s.extraReserved)
		info.SubscriptionPostDelta = 0
		info.SubscriptionAmountTotal = sub.AmountTotal
		info.SubscriptionAmountUsedAfterPreConsume = sub.AmountUsedAfter + int64(s.extraReserved)
		info.SubscriptionPlanId = sub.PlanId
		info.SubscriptionPlanTitle = sub.PlanTitle
	} else {
		info.SubscriptionId = 0
		info.SubscriptionPreConsumed = 0
	}
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 根据计费偏好创建会话并处理回退
// ---------------------------------------------------------------------------

// NewBillingSession 根据用户计费偏好创建 BillingSession，处理 subscription_first / wallet_first 的回退。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	return newBillingSession(c, relayInfo, preConsumedQuota, false)
}

func newBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int, strict bool) (*BillingSession, *types.NewAPIError) {
	if relayInfo == nil {
		return nil, types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	pref := common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference)

	// 钱包路径需要先检查用户额度
	tryWallet := func() (*BillingSession, *types.NewAPIError) {
		if !strict {
			// Trust and friendly preflight errors must use the same authoritative
			// ledger as the conditional reservation below. Redis remains a display
			// cache and cannot authorize a wallet debit.
			userQuota, err := model.GetUserQuota(relayInfo.UserId, true)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
			}
			if userQuota <= 0 {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
					types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
					types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
			}
			if userQuota-preConsumedQuota < 0 {
				return nil, types.NewErrorWithStatusCode(
					fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)),
					types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
					types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
			}
			relayInfo.UserQuota = userQuota
		}

		session := &BillingSession{
			relayInfo: relayInfo,
			funding:   &WalletFunding{userId: relayInfo.UserId},
		}
		var apiErr *types.NewAPIError
		if strict {
			apiErr = session.preConsumeStrict(c, preConsumedQuota)
		} else {
			apiErr = session.preConsume(c, preConsumedQuota)
		}
		if apiErr != nil {
			if session.NeedsRefund() {
				return session, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}

	trySubscription := func() (*BillingSession, *types.NewAPIError) {
		subConsume := preConsumedQuota
		if subConsume <= 0 {
			subConsume = 1
		}
		session := &BillingSession{
			relayInfo: relayInfo,
			funding: &SubscriptionFunding{
				requestId: relayInfo.RequestId,
				userId:    relayInfo.UserId,
				modelName: relayInfo.OriginModelName,
				amount:    int64(subConsume),
			},
		}
		// 必须传 subConsume 而非 preConsumedQuota，保证 SubscriptionFunding.amount、
		// preConsume 参数和 FinalPreConsumedQuota 三者一致，避免订阅多扣费。
		var apiErr *types.NewAPIError
		if strict {
			apiErr = session.preConsumeStrict(c, subConsume)
		} else {
			apiErr = session.preConsume(c, subConsume)
		}
		if apiErr != nil {
			if session.NeedsRefund() {
				return session, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}

	switch pref {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		session, err := tryWallet()
		if err != nil {
			if session != nil {
				return session, err
			}
			if err.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				return trySubscription()
			}
			return nil, err
		}
		return session, nil
	case "subscription_first":
		fallthrough
	default:
		hasSub, subCheckErr := model.HasActiveUserSubscription(relayInfo.UserId)
		if subCheckErr != nil {
			return nil, types.NewError(subCheckErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !hasSub {
			return tryWallet()
		}
		session, apiErr := trySubscription()
		if apiErr != nil {
			if session != nil {
				return session, apiErr
			}
			if apiErr.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				// 仅当用户的活跃订阅允许钱包回退时才回退到钱包，否则返回订阅额度不足错误
				allowOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
				if overflowErr != nil {
					return nil, types.NewError(overflowErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
				}
				if allowOverflow {
					return tryWallet()
				}
				return nil, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}
}

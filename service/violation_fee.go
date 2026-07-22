package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

const (
	ViolationFeeCodePrefix     = "violation_fee."
	CSAMViolationMarker        = "Failed check: SAFETY_CHECK_TYPE"
	ContentViolatesUsageMarker = "Content violates usage guidelines"
)

func IsViolationFeeCode(code types.ErrorCode) bool {
	return strings.HasPrefix(string(code), ViolationFeeCodePrefix)
}

func HasCSAMViolationMarker(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), CSAMViolationMarker) || strings.Contains(err.Error(), ContentViolatesUsageMarker) {
		return true
	}
	msg := err.ToOpenAIError().Message
	return strings.Contains(msg, CSAMViolationMarker) || strings.Contains(msg, ContentViolatesUsageMarker)
}

func WrapAsViolationFeeGrokCSAM(err *types.NewAPIError) *types.NewAPIError {
	if err == nil {
		return nil
	}
	oai := err.ToOpenAIError()
	oai.Type = string(types.ErrorCodeViolationFeeGrokCSAM)
	oai.Code = string(types.ErrorCodeViolationFeeGrokCSAM)
	return types.WithOpenAIError(oai, err.StatusCode, types.ErrOptionWithSkipRetry())
}

// NormalizeViolationFeeError ensures:
// - if the CSAM marker is present, error.code is set to a stable violation-fee code and skip-retry is enabled.
// - if error.code already has the violation-fee prefix, skip-retry is enabled.
//
// It must be called before retry decision logic.
func NormalizeViolationFeeError(err *types.NewAPIError) *types.NewAPIError {
	if err == nil {
		return nil
	}

	if HasCSAMViolationMarker(err) {
		return WrapAsViolationFeeGrokCSAM(err)
	}

	if IsViolationFeeCode(err.GetErrorCode()) {
		oai := err.ToOpenAIError()
		return types.WithOpenAIError(oai, err.StatusCode, types.ErrOptionWithSkipRetry())
	}

	return err
}

func shouldChargeViolationFee(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if err.GetErrorCode() == types.ErrorCodeViolationFeeGrokCSAM {
		return true
	}
	// In case some callers didn't normalize, keep a safety net.
	return HasCSAMViolationMarker(err)
}

func calcViolationFeeQuota(amount, groupRatio float64) (int, *common.QuotaClamp) {
	if amount <= 0 {
		return 0, nil
	}
	if groupRatio <= 0 {
		return 0, nil
	}
	quota, clamp := common.QuotaRoundChecked(amount * common.QuotaPerUnit * groupRatio)
	if quota <= 0 {
		return 0, clamp
	}
	return quota, clamp
}

// ChargeViolationFeeIfNeeded settles a failed request to the configured violation fee.
// It returns true only when the fee was committed. Callers must refund the ordinary
// reservation when it returns false.
func ChargeViolationFeeIfNeeded(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, apiErr *types.NewAPIError) bool {
	if ctx == nil || relayInfo == nil || apiErr == nil {
		return false
	}
	//if relayInfo.IsPlayground {
	//	return false
	//}
	if !shouldChargeViolationFee(apiErr) {
		return false
	}

	settings := model_setting.GetGrokSettings()
	if !settings.ViolationDeductionEnabled {
		return false
	}

	groupRatio := relayInfo.PriceData.GroupRatioInfo.GroupRatio
	feeQuota, clamp := calcViolationFeeQuota(settings.ViolationDeductionAmount, groupRatio)
	noteQuotaClamp(relayInfo, clamp)
	if clamp != nil {
		logger.LogWarn(ctx, fmt.Sprintf("violation fee rejected after quota saturation: op=%s kind=%s original=%g clamped=%d user=%d model=%s",
			clamp.Op, clamp.Kind, clamp.Original, clamp.Clamped, relayInfo.UserId, relayInfo.OriginModelName))
		return false
	}
	if feeQuota <= 0 {
		return false
	}

	if relayInfo.Billing == nil {
		// Free models normally skip pre-consume, but a policy fee still has to honor
		// the user's wallet/subscription preference and quota bounds. Create the same
		// billing session a paid request would have used instead of falling through to
		// the legacy wallet-only delta path. Force a real reservation so the fee cannot
		// enter the trusted-user settle path without funds already held.
		preConsumeErr := PreConsumeBillingStrict(ctx, feeQuota, relayInfo)
		if preConsumeErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("violation fee not charged because strict pre-consume failed: %s", preConsumeErr.Error()))
			return false
		}
	}
	if err := relayInfo.Billing.ReserveStrict(feeQuota); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("violation fee not charged because full reservation failed: %s", err.Error()))
		return false
	}

	// Settle only after the complete fee is held. This makes settlement a zero or
	// negative delta and prevents wallet funding from being overdrawn by a fee.
	if err := SettleBilling(ctx, relayInfo, feeQuota); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("failed to settle fully reserved violation fee: %s", err.Error()))
		if relayInfo.Billing == nil || !relayInfo.Billing.FundingCommitted() {
			// Returning excess reservation failed at the funding source, so the
			// original reservation remains refundable and no fee was committed.
			return false
		}
		// The funding source returned reservation excess before token quota was
		// adjusted. If that token step fails, the fee is still financially committed
		// and Refund is intentionally unavailable. Preserve the consume ledger/log;
		// BillingSession has already emitted the reconciliation error.
		logger.LogWarn(ctx, "violation fee funding committed with a token quota adjustment error")
	}

	model.UpdateUserUsedQuotaAndRequestCount(relayInfo.UserId, feeQuota)
	model.UpdateChannelUsedQuota(relayInfo.ChannelId, feeQuota)

	useTimeSeconds := time.Now().Unix() - relayInfo.StartTime.Unix()
	tokenName := ctx.GetString("token_name")
	oai := apiErr.ToOpenAIError()

	other := map[string]any{
		"violation_fee":        true,
		"violation_fee_code":   string(types.ErrorCodeViolationFeeGrokCSAM),
		"fee_quota":            feeQuota,
		"base_amount":          settings.ViolationDeductionAmount,
		"group_ratio":          groupRatio,
		"status_code":          apiErr.StatusCode,
		"upstream_error_type":  oai.Type,
		"upstream_error_code":  fmt.Sprintf("%v", oai.Code),
		"violation_fee_marker": CSAMViolationMarker,
	}
	attachQuotaSaturation(ctx, relayInfo, other)

	model.RecordConsumeLog(ctx, relayInfo.UserId, model.RecordConsumeLogParams{
		ChannelId:      relayInfo.ChannelId,
		ModelName:      relayInfo.OriginModelName,
		TokenName:      tokenName,
		Quota:          feeQuota,
		Content:        "Violation fee charged",
		TokenId:        relayInfo.TokenId,
		UseTimeSeconds: int(useTimeSeconds),
		IsStream:       relayInfo.IsStream,
		Group:          relayInfo.UsingGroup,
		Other:          other,
	})

	return true
}

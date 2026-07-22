package billingexpr

import (
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
)

// quotaConversion converts raw expression output to quota based on the
// expression version. This is the central dispatch point for future versions
// that may use a different conversion formula.
func quotaConversion(exprOutput float64, snap *BillingSnapshot) float64 {
	switch snap.ExprVersion {
	default: // v1: coefficients are $/1M tokens prices
		return exprOutput / 1_000_000 * snap.QuotaPerUnit
	}
}

// ComputeTieredQuota runs the Expr from a frozen BillingSnapshot against
// actual token counts and returns the settlement result.
func ComputeTieredQuota(snap *BillingSnapshot, params TokenParams) (TieredResult, error) {
	return ComputeTieredQuotaWithRequest(snap, params, RequestInput{})
}

func ComputeTieredQuotaWithRequest(snap *BillingSnapshot, params TokenParams, request RequestInput) (TieredResult, error) {
	cost, trace, err := RunExprByHashWithRequest(snap.ExprString, snap.ExprHash, params, request)
	if err != nil {
		return TieredResult{}, err
	}
	if math.IsNaN(cost) {
		return TieredResult{}, fmt.Errorf("billing expression produced NaN cost")
	}
	if cost < 0 {
		return TieredResult{}, fmt.Errorf("billing expression produced negative cost: %g", cost)
	}

	quotaBeforeGroup := quotaConversion(cost, snap)
	if math.IsNaN(quotaBeforeGroup) {
		return TieredResult{}, fmt.Errorf("billing expression produced NaN quota before group ratio")
	}
	if quotaBeforeGroup < 0 {
		return TieredResult{}, fmt.Errorf("billing expression produced negative quota before group ratio: %g", quotaBeforeGroup)
	}
	quotaAfterGroup := quotaBeforeGroup * snap.GroupRatio
	if math.IsNaN(quotaAfterGroup) {
		return TieredResult{}, fmt.Errorf("billing expression produced NaN quota after group ratio")
	}
	if quotaAfterGroup < 0 {
		return TieredResult{}, fmt.Errorf("billing expression produced negative quota after group ratio: %g", quotaAfterGroup)
	}
	afterGroup, clamp := common.QuotaRoundChecked(quotaAfterGroup)
	if afterGroup < 0 {
		return TieredResult{}, fmt.Errorf("billing expression produced negative rounded quota: %d", afterGroup)
	}
	crossed := trace.MatchedTier != snap.EstimatedTier

	return TieredResult{
		ActualQuotaBeforeGroup: quotaBeforeGroup,
		ActualQuotaAfterGroup:  afterGroup,
		MatchedTier:            trace.MatchedTier,
		CrossedTier:            crossed,
		Clamp:                  clamp,
	}, nil
}

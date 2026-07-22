package service

import (
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// TieredResultWrapper wraps billingexpr.TieredResult for use at the service layer.
type TieredResultWrapper = billingexpr.TieredResult

// clampTieredTokenParams keeps every billing multiplier non-negative at the
// service boundary. Upstream usage is untrusted, and direct settlement callers
// can bypass BuildTieredTokenParams, so both construction and settlement apply
// the same normalization.
func clampTieredTokenParams(params billingexpr.TokenParams) billingexpr.TokenParams {
	if params.P < 0 || math.IsNaN(params.P) {
		params.P = 0
	}
	if params.C < 0 || math.IsNaN(params.C) {
		params.C = 0
	}
	if params.Len < 0 || math.IsNaN(params.Len) {
		params.Len = 0
	}
	if params.CR < 0 || math.IsNaN(params.CR) {
		params.CR = 0
	}
	if params.CC < 0 || math.IsNaN(params.CC) {
		params.CC = 0
	}
	if params.CC1h < 0 || math.IsNaN(params.CC1h) {
		params.CC1h = 0
	}
	if params.Img < 0 || math.IsNaN(params.Img) {
		params.Img = 0
	}
	if params.ImgO < 0 || math.IsNaN(params.ImgO) {
		params.ImgO = 0
	}
	if params.AI < 0 || math.IsNaN(params.AI) {
		params.AI = 0
	}
	if params.AO < 0 || math.IsNaN(params.AO) {
		params.AO = 0
	}
	return params
}

// BuildTieredTokenParams constructs billingexpr.TokenParams from a dto.Usage,
// normalizing P and C so they mean "tokens not separately priced by the
// expression". Sub-categories (cache, image, audio) are only subtracted
// when the expression references them via their own variable.
//
// GPT-format APIs report prompt_tokens / completion_tokens as totals that
// include all sub-categories (cache, image, audio). Claude-format APIs
// report them as text-only. This function normalizes to text-only when
// sub-categories are separately priced.
func BuildTieredTokenParams(usage *dto.Usage, isClaudeUsageSemantic bool, usedVars map[string]bool) billingexpr.TokenParams {
	cc5m := float64(usage.PromptTokensDetails.CacheCreationTokensTotal())
	cc1h := float64(0)

	if usage.UsageSemantic == "anthropic" {
		cc1h = float64(usage.ClaudeCacheCreation1hTokens)
		cc5m = float64(usage.ClaudeCacheCreation5mTokens)
	}

	params := clampTieredTokenParams(billingexpr.TokenParams{
		P:    float64(usage.PromptTokens),
		C:    float64(usage.CompletionTokens),
		CR:   float64(usage.PromptTokensDetails.CachedTokens),
		CC:   cc5m,
		CC1h: cc1h,
		Img:  float64(usage.PromptTokensDetails.ImageTokens),
		ImgO: float64(usage.CompletionTokenDetails.ImageTokens),
		AI:   float64(usage.PromptTokensDetails.AudioTokens),
		AO:   float64(usage.CompletionTokenDetails.AudioTokens),
	})

	// len = total input context length for tier condition evaluation.
	// Non-Claude: prompt_tokens already includes everything.
	// Claude: input_tokens is text-only, so add cache read + cache creation.
	params.Len = params.P
	if isClaudeUsageSemantic {
		params.Len = params.P + params.CR + params.CC + params.CC1h
	}

	if !isClaudeUsageSemantic {
		if usedVars["cr"] {
			params.P -= params.CR
		}
		if usedVars["cc"] {
			params.P -= params.CC
		}
		if usedVars["cc1h"] {
			params.P -= params.CC1h
		}
		if usedVars["img"] {
			params.P -= params.Img
		}
		if usedVars["ai"] {
			params.P -= params.AI
		}
		if usedVars["img_o"] {
			params.C -= params.ImgO
		}
		if usedVars["ao"] {
			params.C -= params.AO
		}
	}

	// OpenAI cache-write usage reports unadjusted prefix counts, so cr + cc can
	// exceed the prompt and drive the remainder negative. Clamp at zero.
	return clampTieredTokenParams(params)
}

// TryTieredSettle checks if the request uses tiered_expr billing and, if so,
// computes the actual quota using the frozen BillingSnapshot. Returns:
//   - ok=true, quota, result  when tiered billing applies
//   - ok=false, 0, nil        when it doesn't (caller should fall through to existing logic)
func TryTieredSettle(relayInfo *relaycommon.RelayInfo, params billingexpr.TokenParams) (ok bool, quota int, result *billingexpr.TieredResult) {
	snap := relayInfo.TieredBillingSnapshot
	if snap == nil || snap.BillingMode != "tiered_expr" {
		return false, 0, nil
	}

	requestInput := billingexpr.RequestInput{}
	if relayInfo.BillingRequestInput != nil {
		requestInput = *relayInfo.BillingRequestInput
	}

	params = clampTieredTokenParams(params)
	tr, err := billingexpr.ComputeTieredQuotaWithRequest(snap, params, requestInput)
	if err != nil {
		quota = snap.EstimatedQuotaAfterGroup
		if quota < 0 {
			quota = 0
		}
		common.SysError(fmt.Sprintf(
			"tiered billing settlement failed for model %q; using estimated quota %d: %v",
			snap.ModelName,
			quota,
			err,
		))
		return true, quota, nil
	}

	// Surface any int32 saturation from settlement onto RelayInfo so the
	// consume log records it under admin_info, regardless of which caller
	// (text, audio, WSS) consumes the returned quota. First non-nil wins.
	noteQuotaClamp(relayInfo, tr.Clamp)

	return true, tr.ActualQuotaAfterGroup, &tr
}

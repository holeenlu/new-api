package billingexpr_test

import (
	"math"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeTieredQuota_ClampOnOverflow guards the billing-safety invariant
// that an oversized tiered settlement clamps to the int32 max instead of
// wrapping into a credit, and that the saturation event is surfaced on the
// result so callers can record it for admin auditing.
func TestComputeTieredQuota_ClampOnOverflow(t *testing.T) {
	// exprOutput = p * 1e9 = 1e18; quotaBeforeGroup = 1e18 / 1e6 * 5e5 = 5e17,
	// which far exceeds MaxInt32 and must saturate.
	exprStr := `tier("base", p * 1000000000)`
	snap := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprStr,
		ExprHash:     billingexpr.ExprHashString(exprStr),
		GroupRatio:   1.0,
		QuotaPerUnit: 500_000,
	}

	result, err := billingexpr.ComputeTieredQuota(snap, billingexpr.TokenParams{P: 1_000_000_000})
	require.NoError(t, err)

	assert.Equal(t, math.MaxInt32, result.ActualQuotaAfterGroup, "oversized quota must clamp to int32 max, never wrap negative")
	require.NotNil(t, result.Clamp, "clamp event must be surfaced so it can be audited")
	assert.Equal(t, common.QuotaClampOverflow, result.Clamp.Kind)
	assert.Equal(t, math.MaxInt32, result.Clamp.Clamped)
}

// TestComputeTieredQuota_NoClampInRange confirms an in-range settlement leaves
// Clamp nil, so the audit path is a no-op in the common case.
func TestComputeTieredQuota_NoClampInRange(t *testing.T) {
	exprStr := `tier("base", p * 2 + c * 10)`
	snap := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprStr,
		ExprHash:     billingexpr.ExprHashString(exprStr),
		GroupRatio:   1.0,
		QuotaPerUnit: 500_000,
	}

	result, err := billingexpr.ComputeTieredQuota(snap, billingexpr.TokenParams{P: 1000, C: 500})
	require.NoError(t, err)
	assert.Nil(t, result.Clamp, "in-range settlement must not report a clamp")
}

func TestComputeTieredQuotaRejectsNegativeCharge(t *testing.T) {
	tests := []struct {
		name         string
		expr         string
		params       billingexpr.TokenParams
		quotaPerUnit float64
		groupRatio   float64
		wantError    string
	}{
		{
			name:         "negative expression cost",
			expr:         `tier("base", -p)`,
			params:       billingexpr.TokenParams{P: 100},
			quotaPerUnit: 500_000,
			groupRatio:   1,
			wantError:    "negative cost",
		},
		{
			name:         "negative quota unit",
			expr:         `tier("base", p)`,
			params:       billingexpr.TokenParams{P: 100},
			quotaPerUnit: -500_000,
			groupRatio:   1,
			wantError:    "negative quota before group ratio",
		},
		{
			name:         "negative group ratio",
			expr:         `tier("base", p)`,
			params:       billingexpr.TokenParams{P: 100},
			quotaPerUnit: 500_000,
			groupRatio:   -1,
			wantError:    "negative quota after group ratio",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snap := &billingexpr.BillingSnapshot{
				BillingMode:  "tiered_expr",
				ExprString:   test.expr,
				ExprHash:     billingexpr.ExprHashString(test.expr),
				GroupRatio:   test.groupRatio,
				QuotaPerUnit: test.quotaPerUnit,
			}

			result, err := billingexpr.ComputeTieredQuota(snap, test.params)

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantError)
			assert.Equal(t, billingexpr.TieredResult{}, result)
		})
	}
}

func TestComputeTieredQuotaRejectsNaNAtEveryChargeStage(t *testing.T) {
	tests := []struct {
		name         string
		params       billingexpr.TokenParams
		quotaPerUnit float64
		groupRatio   float64
		wantError    string
	}{
		{
			name:         "expression cost",
			params:       billingexpr.TokenParams{P: math.NaN()},
			quotaPerUnit: 500_000,
			groupRatio:   1,
			wantError:    "NaN cost",
		},
		{
			name:         "quota before group ratio",
			params:       billingexpr.TokenParams{P: 1},
			quotaPerUnit: math.NaN(),
			groupRatio:   1,
			wantError:    "NaN quota before group ratio",
		},
		{
			name:         "quota after group ratio",
			params:       billingexpr.TokenParams{P: 1},
			quotaPerUnit: 1_000_000,
			groupRatio:   math.NaN(),
			wantError:    "NaN quota after group ratio",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exprStr := `tier("base", p)`
			snap := &billingexpr.BillingSnapshot{
				BillingMode:  "tiered_expr",
				ExprString:   exprStr,
				ExprHash:     billingexpr.ExprHashString(exprStr),
				GroupRatio:   test.groupRatio,
				QuotaPerUnit: test.quotaPerUnit,
			}

			result, err := billingexpr.ComputeTieredQuota(snap, test.params)

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantError)
			assert.Equal(t, billingexpr.TieredResult{}, result)
		})
	}
}

func TestComputeTieredQuotaAuditsPositiveInfinityAtEveryChargeStage(t *testing.T) {
	tests := []struct {
		name         string
		params       billingexpr.TokenParams
		quotaPerUnit float64
		groupRatio   float64
	}{
		{
			name:         "expression cost",
			params:       billingexpr.TokenParams{P: math.Inf(1)},
			quotaPerUnit: 500_000,
			groupRatio:   1,
		},
		{
			name:         "quota before group ratio",
			params:       billingexpr.TokenParams{P: 1},
			quotaPerUnit: math.Inf(1),
			groupRatio:   1,
		},
		{
			name:         "quota after group ratio",
			params:       billingexpr.TokenParams{P: 1},
			quotaPerUnit: 1_000_000,
			groupRatio:   math.Inf(1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exprStr := `tier("base", p)`
			snap := &billingexpr.BillingSnapshot{
				BillingMode:  "tiered_expr",
				ExprString:   exprStr,
				ExprHash:     billingexpr.ExprHashString(exprStr),
				GroupRatio:   test.groupRatio,
				QuotaPerUnit: test.quotaPerUnit,
			}

			result, err := billingexpr.ComputeTieredQuota(snap, test.params)

			require.NoError(t, err)
			assert.Equal(t, math.MaxInt32, result.ActualQuotaAfterGroup)
			require.NotNil(t, result.Clamp)
			assert.Equal(t, common.QuotaClampOverflow, result.Clamp.Kind)
		})
	}
}

package model_setting

import (
	"math"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrokSettingsHotUpdatePublishesCompleteSnapshot(t *testing.T) {
	original := GetGrokSettings()
	registered := config.GlobalConfig.Get("grok")
	require.NotNil(t, registered)
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
			"violation_deduction_enabled": strconv.FormatBool(original.ViolationDeductionEnabled),
			"violation_deduction_amount":  strconv.FormatFloat(original.ViolationDeductionAmount, 'g', -1, 64),
		}))
	})

	require.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
		"violation_deduction_enabled": "false",
		"violation_deduction_amount":  "0.125",
	}))

	assert.Equal(t, GrokSettings{
		ViolationDeductionEnabled: false,
		ViolationDeductionAmount:  0.125,
	}, GetGrokSettings())
}

func TestGrokSettingsRejectsInvalidDeductionAmount(t *testing.T) {
	tests := []struct {
		name   string
		amount float64
	}{
		{name: "negative", amount: -0.001},
		{name: "nan", amount: math.NaN()},
		{name: "positive infinity", amount: math.Inf(1)},
		{name: "negative infinity", amount: math.Inf(-1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, (GrokSettings{ViolationDeductionAmount: test.amount}).Validate())
		})
	}
}

func TestGrokSettingsRejectsNegativeHotUpdateWithoutPublishing(t *testing.T) {
	original := GetGrokSettings()
	registered := config.GlobalConfig.Get("grok")
	require.NotNil(t, registered)

	err := config.UpdateConfigFromMap(registered, map[string]string{
		"violation_deduction_enabled": strconv.FormatBool(!original.ViolationDeductionEnabled),
		"violation_deduction_amount":  "-0.001",
	})

	require.Error(t, err)
	assert.Equal(t, original, GetGrokSettings())
}

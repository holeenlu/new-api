package model_setting

import (
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/setting/config"
)

// GrokSettings defines Grok model configuration.
type GrokSettings struct {
	ViolationDeductionEnabled bool    `json:"violation_deduction_enabled"`
	ViolationDeductionAmount  float64 `json:"violation_deduction_amount"`
}

var defaultGrokSettings = GrokSettings{
	ViolationDeductionEnabled: true,
	ViolationDeductionAmount:  0.05,
}

var grokSettings = config.NewValidatedAtomicConfig(defaultGrokSettings, GrokSettings.Validate)

func init() {
	config.GlobalConfig.Register("grok", grokSettings)
}

func (settings GrokSettings) Validate() error {
	if math.IsNaN(settings.ViolationDeductionAmount) || math.IsInf(settings.ViolationDeductionAmount, 0) {
		return fmt.Errorf("violation_deduction_amount must be finite")
	}
	if settings.ViolationDeductionAmount < 0 {
		return fmt.Errorf("violation_deduction_amount must not be negative")
	}
	return nil
}

func GetGrokSettings() GrokSettings {
	return grokSettings.Load()
}

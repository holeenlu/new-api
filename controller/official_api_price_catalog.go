package controller

import (
	"math"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// officialAPIPricingEntry is a reviewed public API token price in USD per one
// million tokens. It deliberately excludes subscription-plan fees because they
// cannot be converted to a meaningful per-token price.
type officialAPIPricingEntry struct {
	Model       string
	Input       float64
	Output      float64
	CacheRead   *float64
	CacheCreate *float64
}

type officialAPIPricingSourceDefinition struct {
	ID      int
	Name    string
	BaseURL string
	Entries []officialAPIPricingEntry
}

func officialPricePtr(value float64) *float64 {
	return &value
}

var officialAPIPricingSourceCatalog = []officialAPIPricingSourceDefinition{
	{
		ID:      officialOpenAIPresetID,
		Name:    officialOpenAIPresetName,
		BaseURL: officialOpenAIPresetBaseURL,
		Entries: []officialAPIPricingEntry{
			{Model: "gpt-5", Input: 1.25, Output: 10, CacheRead: officialPricePtr(0.125)},
			{Model: "gpt-5-mini", Input: 0.25, Output: 2, CacheRead: officialPricePtr(0.025)},
			{Model: "gpt-5-nano", Input: 0.05, Output: 0.4, CacheRead: officialPricePtr(0.005)},
			{Model: "gpt-4.1", Input: 2, Output: 8, CacheRead: officialPricePtr(0.5)},
			{Model: "gpt-4.1-mini", Input: 0.4, Output: 1.6, CacheRead: officialPricePtr(0.1)},
			{Model: "gpt-4.1-nano", Input: 0.1, Output: 0.4, CacheRead: officialPricePtr(0.025)},
			{Model: "gpt-4o", Input: 2.5, Output: 10, CacheRead: officialPricePtr(1.25)},
			{Model: "gpt-4o-mini", Input: 0.15, Output: 0.6, CacheRead: officialPricePtr(0.075)},
			{Model: "o3", Input: 2, Output: 8, CacheRead: officialPricePtr(0.5)},
			{Model: "o4-mini", Input: 1.1, Output: 4.4, CacheRead: officialPricePtr(0.275)},
		},
	},
	{
		ID:      officialAnthropicPresetID,
		Name:    officialAnthropicPresetName,
		BaseURL: officialAnthropicPresetBaseURL,
		Entries: []officialAPIPricingEntry{
			{Model: "claude-opus-4-1", Input: 15, Output: 75, CacheRead: officialPricePtr(1.5), CacheCreate: officialPricePtr(18.75)},
			{Model: "claude-opus-4", Input: 15, Output: 75, CacheRead: officialPricePtr(1.5), CacheCreate: officialPricePtr(18.75)},
			{Model: "claude-sonnet-4", Input: 3, Output: 15, CacheRead: officialPricePtr(0.3), CacheCreate: officialPricePtr(3.75)},
			{Model: "claude-3-7-sonnet-latest", Input: 3, Output: 15, CacheRead: officialPricePtr(0.3), CacheCreate: officialPricePtr(3.75)},
			{Model: "claude-3-5-haiku-latest", Input: 0.8, Output: 4, CacheRead: officialPricePtr(0.08), CacheCreate: officialPricePtr(1)},
			{Model: "claude-3-haiku-20240307", Input: 0.25, Output: 1.25, CacheRead: officialPricePtr(0.025), CacheCreate: officialPricePtr(0.3125)},
		},
	},
}

func officialAPIPricingSources() []officialAPIPricingSourceDefinition {
	return officialAPIPricingSourceCatalog
}

func isOfficialAPIPricingSource(id int) bool {
	_, ok := officialAPIPricingSourceByID(id)
	return ok
}

func officialAPIPricingSourceByID(id int) (officialAPIPricingSourceDefinition, bool) {
	for _, source := range officialAPIPricingSourceCatalog {
		if source.ID == id {
			return source, true
		}
	}
	return officialAPIPricingSourceDefinition{}, false
}

func officialAPIPricingUpstream(upstream dto.UpstreamDTO) dto.UpstreamDTO {
	source, ok := officialAPIPricingSourceByID(upstream.ID)
	if !ok {
		return upstream
	}
	return dto.UpstreamDTO{ID: source.ID, Name: source.Name, BaseURL: source.BaseURL}
}

func officialAPIPricingRatioData(sourceID int) (map[string]any, bool) {
	source, ok := officialAPIPricingSourceByID(sourceID)
	if !ok {
		return nil, false
	}

	modelRatios := make(map[string]any, len(source.Entries))
	completionRatios := make(map[string]any, len(source.Entries))
	cacheRatios := make(map[string]any)
	createCacheRatios := make(map[string]any)
	for _, entry := range source.Entries {
		if entry.Model == "" || entry.Input <= 0 || entry.Output < 0 || math.IsNaN(entry.Input) || math.IsInf(entry.Input, 0) || math.IsNaN(entry.Output) || math.IsInf(entry.Output, 0) {
			continue
		}
		modelRatios[entry.Model] = roundRatioValue(entry.Input * float64(ratio_setting.USD) / modelsDevInputCostRatioBase)
		completionRatios[entry.Model] = roundRatioValue(entry.Output / entry.Input)
		if entry.CacheRead != nil && *entry.CacheRead >= 0 && !math.IsNaN(*entry.CacheRead) && !math.IsInf(*entry.CacheRead, 0) {
			cacheRatios[entry.Model] = roundRatioValue(*entry.CacheRead / entry.Input)
		}
		if entry.CacheCreate != nil && *entry.CacheCreate >= 0 && !math.IsNaN(*entry.CacheCreate) && !math.IsInf(*entry.CacheCreate, 0) {
			createCacheRatios[entry.Model] = roundRatioValue(*entry.CacheCreate / entry.Input)
		}
	}

	data := map[string]any{"model_ratio": modelRatios, "completion_ratio": completionRatios}
	if len(cacheRatios) > 0 {
		data["cache_ratio"] = cacheRatios
	}
	if len(createCacheRatios) > 0 {
		data["create_cache_ratio"] = createCacheRatios
	}
	return data, true
}

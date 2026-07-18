package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

// GetEnabledModelUpstreamMetadata returns the conservative live capability
// shared by every enabled channel that can route each requested model. Passing
// channel types limits the query for callers that need a provider-specific
// view; no channel types means every eligible route must confirm the metadata.
func GetEnabledModelUpstreamMetadata(
	modelNames []string,
	groups []string,
	channelTypes ...int,
) (map[string]dto.UpstreamModelMetadata, error) {
	result := make(map[string]dto.UpstreamModelMetadata)
	modelNames = normalizeLookupValues(modelNames)
	groups = normalizeLookupValues(groups)
	if len(modelNames) == 0 || len(groups) == 0 {
		return result, nil
	}

	type row struct {
		Model        string
		Settings     *string
		ModelMapping *string
	}
	var rows []row
	query := DB.Table("abilities").
		Select("abilities.model as model, channels.settings as settings, channels.model_mapping as model_mapping").
		Joins("JOIN channels ON abilities.channel_id = channels.id").
		Where("abilities.model IN ? AND abilities.enabled = ? AND channels.status = ?", modelNames, true, common.ChannelStatusEnabled).
		Where("abilities."+commonGroupCol+" IN ?", groups)
	if len(channelTypes) > 0 {
		query = query.Where("channels.type IN ?", channelTypes)
	}
	err := query.Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	for _, item := range rows {
		upstreamModel := strings.TrimSpace(item.Model)
		if item.ModelMapping != nil && strings.TrimSpace(*item.ModelMapping) != "" {
			mapping := make(map[string]string)
			if common.UnmarshalJsonStr(*item.ModelMapping, &mapping) == nil {
				if mapped := strings.TrimSpace(mapping[upstreamModel]); mapped != "" {
					upstreamModel = mapped
				}
			}
		}

		settings := dto.ChannelOtherSettings{}
		if item.Settings != nil && *item.Settings != "" {
			_ = common.UnmarshalJsonStr(*item.Settings, &settings)
		}
		profile, valid := settings.UpstreamModelMetadata[upstreamModel]
		valid = valid && profile.Valid()

		aggregate, exists := result[item.Model]
		if !exists {
			if valid {
				result[item.Model] = profile
			} else {
				result[item.Model] = dto.UpstreamModelMetadata{}
			}
			continue
		}
		if !aggregate.Valid() || !valid {
			aggregate.Complete = false
			result[item.Model] = aggregate
			continue
		}
		aggregate.ContextWindow = min(aggregate.ContextWindow, profile.ContextWindow)
		aggregate.MaxContextWindow = min(aggregate.MaxContextWindow, profile.MaxContextWindow)
		result[item.Model] = aggregate
	}
	return result, nil
}

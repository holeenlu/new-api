package common

import (
	"fmt"
	"strings"
)

func getParamOverrideMap(info *RelayInfo) map[string]interface{} {
	if info == nil || info.ChannelMeta == nil {
		return nil
	}
	return info.ChannelMeta.ParamOverride
}

func getHeaderOverrideMap(info *RelayInfo) map[string]interface{} {
	if info == nil || info.ChannelMeta == nil {
		return nil
	}
	return info.ChannelMeta.HeadersOverride
}

func sanitizeHeaderOverrideMap(source map[string]interface{}) map[string]interface{} {
	if len(source) == 0 {
		return map[string]interface{}{}
	}
	target := make(map[string]interface{}, len(source))
	for key, value := range source {
		normalizedKey := normalizeHeaderContextKey(key)
		if normalizedKey == "" {
			continue
		}
		normalizedValue := strings.TrimSpace(fmt.Sprintf("%v", value))
		if normalizedValue == "" {
			if isHeaderPassthroughRuleKeyForOverride(normalizedKey) {
				target[normalizedKey] = ""
			}
			continue
		}
		target[normalizedKey] = normalizedValue
	}
	return target
}

func isHeaderPassthroughRuleKeyForOverride(key string) bool {
	key = strings.TrimSpace(strings.ToLower(key))
	if key == "" {
		return false
	}
	if key == "*" {
		return true
	}
	return strings.HasPrefix(key, "re:") || strings.HasPrefix(key, "regex:")
}

func GetEffectiveHeaderOverride(info *RelayInfo) map[string]interface{} {
	if info == nil {
		return map[string]interface{}{}
	}
	if info.UseRuntimeHeadersOverride {
		return sanitizeHeaderOverrideMap(info.RuntimeHeadersOverride)
	}
	return sanitizeHeaderOverrideMap(getHeaderOverrideMap(info))
}

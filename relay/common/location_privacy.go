package common

import (
	"strings"

	rootcommon "github.com/QuantumNous/new-api/common"
)

var sensitiveNetworkMetadataKeys = map[string]struct{}{
	"cf_connecting_ip":         {},
	"client_ip":                {},
	"connecting_ip":            {},
	"forwarded_for":            {},
	"ip":                       {},
	"ip_address":               {},
	"remote_addr":              {},
	"remote_ip":                {},
	"source_ip":                {},
	"true_client_ip":           {},
	"x_cluster_client_ip":      {},
	"x_envoy_external_address": {},
	"x_forwarded_for":          {},
	"x_original_forwarded_for": {},
	"x_real_ip":                {},
}

var sensitiveLocationMetadataKeys = map[string]string{
	"city":      "city",
	"country":   "country",
	"lat":       "latitude",
	"latitude":  "latitude",
	"lng":       "longitude",
	"lon":       "longitude",
	"longitude": "longitude",
	"region":    "region",
	"time_zone": "timezone",
	"timezone":  "timezone",
}

var directLocationPrivacyKeys = map[string]struct{}{
	"inference_geo": {},
	"lat_lng":       {},
	"latlng":        {},
	"user_location": {},
}

func isLocationPrivacyCandidateKey(key string) bool {
	normalized := normalizeSensitiveMetadataKey(key)
	if _, sensitive := sensitiveNetworkMetadataKeys[normalized]; sensitive {
		return true
	}
	if _, sensitive := sensitiveLocationMetadataKeys[normalized]; sensitive {
		return true
	}
	_, direct := directLocationPrivacyKeys[normalized]
	return direct
}

// FilterUpstreamLocationData applies the outbound privacy policy only to
// protocol-defined location containers and metadata. Prompt text and tool
// arguments are deliberately left untouched.
func FilterUpstreamLocationData(data []byte, channelProxy ...string) ([]byte, bool, error) {
	mode := rootcommon.GetUpstreamLocationMode()
	var request map[string]interface{}
	if err := rootcommon.Unmarshal(data, &request); err != nil {
		return nil, false, err
	}
	if mode == rootcommon.UpstreamLocationModeClient {
		if !sanitizeRequestNetworkData(request) {
			return data, false, nil
		}
		filtered, err := rootcommon.Marshal(request)
		if err != nil {
			return nil, false, err
		}
		return filtered, true, nil
	}
	proxyURL := ""
	if len(channelProxy) > 0 {
		proxyURL = strings.TrimSpace(channelProxy[0])
	}
	profile, replaceLocation := selectUpstreamLocationProfile(mode, proxyURL)

	changed := sanitizeRequestLocation(request, replaceLocation, profile)
	if !changed {
		return data, false, nil
	}
	filtered, err := rootcommon.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	return filtered, true, nil
}

func selectUpstreamLocationProfile(mode string, channelProxy string) (rootcommon.UpstreamLocationProfile, bool) {
	hostProfile, egressProfile := rootcommon.GetUpstreamLocationProfiles()
	switch mode {
	case rootcommon.UpstreamLocationModeHost:
		return hostProfile, true
	case rootcommon.UpstreamLocationModeEgress:
		if channelProxy != "" {
			profile, _ := rootcommon.GetChannelProxyLocationProfile(channelProxy)
			return profile, true
		}
		return egressProfile, true
	case rootcommon.UpstreamLocationModeAuto:
		if channelProxy != "" {
			profile, _ := rootcommon.GetChannelProxyLocationProfile(channelProxy)
			return profile, true
		}
		if rootcommon.UpstreamSystemProxyEnabled {
			return egressProfile, true
		}
		return hostProfile, true
	default:
		return rootcommon.UpstreamLocationProfile{}, false
	}
}

func sanitizeRequestNetworkData(request map[string]interface{}) bool {
	changed := false
	for key := range request {
		if _, sensitive := sensitiveNetworkMetadataKeys[normalizeSensitiveMetadataKey(key)]; sensitive {
			delete(request, key)
			changed = true
		}
	}
	if sanitizeNetworkDataInLocationField(request, "user_location") {
		changed = true
	}
	if options, ok := request["web_search_options"].(map[string]interface{}); ok {
		if sanitizeNetworkDataInLocationField(options, "user_location") {
			changed = true
		}
	}
	if tools, ok := request["tools"].([]interface{}); ok {
		for _, rawTool := range tools {
			if tool, valid := rawTool.(map[string]interface{}); valid && sanitizeNetworkDataInLocationField(tool, "user_location") {
				changed = true
			}
		}
	}
	for _, key := range []string{"client_metadata", "metadata"} {
		if metadata, ok := request[key].(map[string]interface{}); ok && sanitizeMetadataNetworkData(metadata) {
			changed = true
		}
	}
	if requests, ok := request["requests"].([]interface{}); ok {
		for _, rawRequest := range requests {
			if nested, valid := rawRequest.(map[string]interface{}); valid && sanitizeRequestNetworkData(nested) {
				changed = true
			}
		}
	}
	return changed
}

func sanitizeNetworkDataInLocationField(parent map[string]interface{}, key string) bool {
	location, ok := parent[key].(map[string]interface{})
	if !ok {
		return false
	}
	return sanitizeMetadataNetworkData(location)
}

func sanitizeMetadataNetworkData(metadata map[string]interface{}) bool {
	changed := false
	for key, value := range metadata {
		if _, sensitive := sensitiveNetworkMetadataKeys[normalizeSensitiveMetadataKey(key)]; sensitive {
			delete(metadata, key)
			changed = true
			continue
		}
		switch nested := value.(type) {
		case map[string]interface{}:
			if sanitizeMetadataNetworkData(nested) {
				changed = true
			}
		case []interface{}:
			for _, item := range nested {
				if child, ok := item.(map[string]interface{}); ok && sanitizeMetadataNetworkData(child) {
					changed = true
				}
			}
		}
	}
	return changed
}

func sanitizeRequestLocation(request map[string]interface{}, replaceLocation bool, profile rootcommon.UpstreamLocationProfile) bool {
	changed := false
	if sanitizeDirectLocationValues(request, replaceLocation, profile) {
		changed = true
	}
	if sanitizeLocationField(request, "user_location", replaceLocation, profile) {
		changed = true
	}
	if options, ok := request["web_search_options"].(map[string]interface{}); ok {
		if sanitizeLocationField(options, "user_location", replaceLocation, profile) {
			changed = true
		}
	}
	if tools, ok := request["tools"].([]interface{}); ok {
		for _, rawTool := range tools {
			if tool, valid := rawTool.(map[string]interface{}); valid {
				if sanitizeLocationField(tool, "user_location", replaceLocation, profile) {
					changed = true
				}
			}
		}
	}
	for _, key := range []string{"toolConfig", "tool_config"} {
		if config, ok := request[key].(map[string]interface{}); ok {
			if sanitizeGeminiLocation(config, replaceLocation, profile) {
				changed = true
			}
		}
	}
	for _, key := range []string{"client_metadata", "metadata"} {
		if metadata, ok := request[key].(map[string]interface{}); ok {
			if sanitizeMetadataLocation(metadata, replaceLocation, profile) {
				changed = true
			}
		}
	}
	if requests, ok := request["requests"].([]interface{}); ok {
		for _, rawRequest := range requests {
			if nested, valid := rawRequest.(map[string]interface{}); valid && sanitizeRequestLocation(nested, replaceLocation, profile) {
				changed = true
			}
		}
	}
	return changed
}

func sanitizeDirectLocationValues(values map[string]interface{}, replaceLocation bool, profile rootcommon.UpstreamLocationProfile) bool {
	changed := false
	for key := range values {
		normalized := normalizeSensitiveMetadataKey(key)
		if _, sensitive := sensitiveNetworkMetadataKeys[normalized]; sensitive {
			delete(values, key)
			changed = true
			continue
		}
		if field, sensitive := sensitiveLocationMetadataKeys[normalized]; sensitive {
			replacement, include := profileLocationMetadataValue(field, replaceLocation, profile)
			if include {
				values[key] = replacement
			} else {
				delete(values, key)
			}
			changed = true
			continue
		}
		switch normalized {
		case "inference_geo":
			delete(values, key)
			changed = true
		case "lat_lng", "latlng":
			if replaceLocation && profile.Latitude != nil && profile.Longitude != nil {
				values[key] = map[string]interface{}{
					"latitude":  *profile.Latitude,
					"longitude": *profile.Longitude,
				}
			} else {
				delete(values, key)
			}
			changed = true
		}
	}
	return changed
}

func sanitizeLocationField(parent map[string]interface{}, key string, replaceLocation bool, profile rootcommon.UpstreamLocationProfile) bool {
	raw, exists := parent[key]
	if !exists {
		return false
	}
	if !replaceLocation {
		delete(parent, key)
		return true
	}

	location := profileTextLocation(profile)
	if len(location) == 0 {
		delete(parent, key)
		return true
	}
	original, _ := raw.(map[string]interface{})
	if _, nestedApproximate := original["approximate"]; nestedApproximate {
		parent[key] = map[string]interface{}{
			"type":        "approximate",
			"approximate": location,
		}
	} else {
		location["type"] = "approximate"
		parent[key] = location
	}
	return true
}

func sanitizeGeminiLocation(config map[string]interface{}, replaceLocation bool, profile rootcommon.UpstreamLocationProfile) bool {
	changed := false
	for _, retrievalKey := range []string{"retrievalConfig", "retrieval_config"} {
		retrieval, ok := config[retrievalKey].(map[string]interface{})
		if !ok {
			continue
		}
		for _, locationKey := range []string{"latLng", "lat_lng"} {
			if _, exists := retrieval[locationKey]; !exists {
				continue
			}
			if replaceLocation && profile.Latitude != nil && profile.Longitude != nil {
				retrieval[locationKey] = map[string]interface{}{
					"latitude":  *profile.Latitude,
					"longitude": *profile.Longitude,
				}
			} else {
				delete(retrieval, locationKey)
			}
			changed = true
		}
	}
	return changed
}

func sanitizeMetadataLocation(metadata map[string]interface{}, replaceLocation bool, profile rootcommon.UpstreamLocationProfile) bool {
	changed := false
	for key, value := range metadata {
		normalized := normalizeSensitiveMetadataKey(key)
		if _, sensitive := sensitiveNetworkMetadataKeys[normalized]; sensitive {
			delete(metadata, key)
			changed = true
			continue
		}
		if field, sensitive := sensitiveLocationMetadataKeys[normalized]; sensitive {
			replacement, include := profileLocationMetadataValue(field, replaceLocation, profile)
			if include {
				metadata[key] = replacement
			} else {
				delete(metadata, key)
			}
			changed = true
			continue
		}
		switch normalized {
		case "lat_lng", "latlng":
			if replaceLocation && profile.Latitude != nil && profile.Longitude != nil {
				metadata[key] = map[string]interface{}{
					"latitude":  *profile.Latitude,
					"longitude": *profile.Longitude,
				}
			} else {
				delete(metadata, key)
			}
			changed = true
			continue
		case "geo", "geolocation", "location", "user_location":
			if replaceLocation {
				replacement := profileTextLocation(profile)
				if len(replacement) > 0 {
					replacement["type"] = "approximate"
					metadata[key] = replacement
				} else {
					delete(metadata, key)
				}
			} else {
				delete(metadata, key)
			}
			changed = true
			continue
		}

		switch nested := value.(type) {
		case map[string]interface{}:
			if sanitizeMetadataLocation(nested, replaceLocation, profile) {
				changed = true
			}
		case []interface{}:
			for _, item := range nested {
				if child, ok := item.(map[string]interface{}); ok && sanitizeMetadataLocation(child, replaceLocation, profile) {
					changed = true
				}
			}
		}
	}
	return changed
}

func normalizeSensitiveMetadataKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, ".", "_")
	return key
}

func profileTextLocation(location rootcommon.UpstreamLocationProfile) map[string]interface{} {
	result := make(map[string]interface{})
	if location.Country != "" {
		result["country"] = location.Country
	}
	if location.Region != "" {
		result["region"] = location.Region
	}
	if location.City != "" {
		result["city"] = location.City
	}
	if location.Timezone != "" {
		result["timezone"] = location.Timezone
	}
	return result
}

func profileLocationMetadataValue(field string, replaceLocation bool, location rootcommon.UpstreamLocationProfile) (interface{}, bool) {
	if !replaceLocation {
		return nil, false
	}
	switch field {
	case "country":
		return location.Country, location.Country != ""
	case "region":
		return location.Region, location.Region != ""
	case "city":
		return location.City, location.City != ""
	case "timezone":
		return location.Timezone, location.Timezone != ""
	case "latitude":
		if location.Latitude != nil {
			return *location.Latitude, true
		}
	case "longitude":
		if location.Longitude != nil {
			return *location.Longitude, true
		}
	}
	return nil, false
}

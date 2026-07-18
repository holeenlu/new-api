package common

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ensureContextMap(conditionContext map[string]interface{}) map[string]interface{} {
	if conditionContext != nil {
		return conditionContext
	}
	return make(map[string]interface{})
}

func marshalContextJSON(context map[string]interface{}) (string, error) {
	if context == nil || len(context) == 0 {
		return "", nil
	}
	ctxBytes, err := common.Marshal(context)
	if err != nil {
		return "", err
	}
	return string(ctxBytes), nil
}

func setHeaderOverrideInContext(context map[string]interface{}, headerName string, value interface{}, keepOrigin bool) error {
	headerName = normalizeHeaderContextKey(headerName)
	if headerName == "" {
		return fmt.Errorf("header name is required")
	}

	rawHeaders := ensureMapKeyInContext(context, paramOverrideContextHeaderOverride)
	if keepOrigin {
		if existing, ok := rawHeaders[headerName]; ok {
			existingValue := strings.TrimSpace(fmt.Sprintf("%v", existing))
			if existingValue != "" {
				return nil
			}
		}
	}

	headerValue, hasValue, err := resolveHeaderOverrideValue(context, headerName, value)
	if err != nil {
		return err
	}
	if !hasValue {
		delete(rawHeaders, headerName)
		return nil
	}

	rawHeaders[headerName] = headerValue
	return nil
}

func resolveHeaderOverrideValue(context map[string]interface{}, headerName string, value interface{}) (string, bool, error) {
	if value == nil {
		return "", false, fmt.Errorf("header value is required")
	}

	if mapping, ok := value.(map[string]interface{}); ok {
		return resolveHeaderOverrideValueByMapping(context, headerName, mapping)
	}
	if mapping, ok := value.(map[string]string); ok {
		converted := make(map[string]interface{}, len(mapping))
		for key, item := range mapping {
			converted[key] = item
		}
		return resolveHeaderOverrideValueByMapping(context, headerName, converted)
	}

	headerValue := strings.TrimSpace(fmt.Sprintf("%v", value))
	if headerValue == "" {
		return "", false, nil
	}
	return headerValue, true, nil
}

func resolveHeaderOverrideValueByMapping(context map[string]interface{}, headerName string, mapping map[string]interface{}) (string, bool, error) {
	if len(mapping) == 0 {
		return "", false, fmt.Errorf("header value mapping cannot be empty")
	}

	appendTokens, err := parseHeaderAppendTokens(mapping)
	if err != nil {
		return "", false, err
	}
	keepOnlyDeclared := parseHeaderKeepOnlyDeclared(mapping)

	sourceValue, exists := getHeaderValueFromContext(context, headerName)
	sourceTokens := make([]string, 0)
	if exists {
		sourceTokens = splitHeaderListValue(sourceValue)
	}

	wildcardValue, hasWildcard := mapping["*"]
	resultTokens := make([]string, 0, len(sourceTokens)+len(appendTokens))
	for _, token := range sourceTokens {
		replacementRaw, hasReplacement := mapping[token]
		if !hasReplacement && hasWildcard && !keepOnlyDeclared {
			replacementRaw = wildcardValue
			hasReplacement = true
		}
		if !hasReplacement {
			if keepOnlyDeclared {
				continue
			}
			resultTokens = append(resultTokens, token)
			continue
		}
		replacementTokens, err := parseHeaderReplacementTokens(replacementRaw)
		if err != nil {
			return "", false, err
		}
		resultTokens = append(resultTokens, replacementTokens...)
	}

	resultTokens = append(resultTokens, appendTokens...)
	resultTokens = lo.Uniq(resultTokens)
	if len(resultTokens) == 0 {
		return "", false, nil
	}
	return strings.Join(resultTokens, ","), true, nil
}

func parseHeaderAppendTokens(mapping map[string]interface{}) ([]string, error) {
	appendRaw, ok := mapping["$append"]
	if !ok {
		return nil, nil
	}
	return parseHeaderReplacementTokens(appendRaw)
}

func parseHeaderKeepOnlyDeclared(mapping map[string]interface{}) bool {
	keepOnlyDeclaredRaw, ok := mapping["$keep_only_declared"]
	if !ok {
		return false
	}
	keepOnlyDeclared, ok := keepOnlyDeclaredRaw.(bool)
	if !ok {
		return false
	}
	return keepOnlyDeclared
}

func parseHeaderReplacementTokens(value interface{}) ([]string, error) {
	switch raw := value.(type) {
	case nil:
		return nil, nil
	case string:
		return splitHeaderListValue(raw), nil
	case []string:
		tokens := make([]string, 0, len(raw))
		for _, item := range raw {
			tokens = append(tokens, splitHeaderListValue(item)...)
		}
		return lo.Uniq(tokens), nil
	case []interface{}:
		tokens := make([]string, 0, len(raw))
		for _, item := range raw {
			itemTokens, err := parseHeaderReplacementTokens(item)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, itemTokens...)
		}
		return lo.Uniq(tokens), nil
	case map[string]interface{}, map[string]string:
		return nil, fmt.Errorf("header replacement value must be string, array or null")
	default:
		token := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if token == "" {
			return nil, nil
		}
		return []string{token}, nil
	}
}

func splitHeaderListValue(raw string) []string {
	items := strings.Split(raw, ",")
	return lo.FilterMap(items, func(item string, _ int) (string, bool) {
		token := strings.TrimSpace(item)
		if token == "" {
			return "", false
		}
		return token, true
	})
}

func copyHeaderInContext(context map[string]interface{}, fromHeader, toHeader string, keepOrigin bool) error {
	fromHeader = normalizeHeaderContextKey(fromHeader)
	toHeader = normalizeHeaderContextKey(toHeader)
	if fromHeader == "" || toHeader == "" {
		return fmt.Errorf("copy_header from/to is required")
	}
	value, exists := getHeaderValueFromContext(context, fromHeader)
	if !exists {
		return fmt.Errorf("%w: %s", errSourceHeaderNotFound, fromHeader)
	}
	return setHeaderOverrideInContext(context, toHeader, value, keepOrigin)
}

func moveHeaderInContext(context map[string]interface{}, fromHeader, toHeader string, keepOrigin bool) error {
	fromHeader = normalizeHeaderContextKey(fromHeader)
	toHeader = normalizeHeaderContextKey(toHeader)
	if fromHeader == "" || toHeader == "" {
		return fmt.Errorf("move_header from/to is required")
	}
	if err := copyHeaderInContext(context, fromHeader, toHeader, keepOrigin); err != nil {
		return err
	}
	if strings.EqualFold(fromHeader, toHeader) {
		return nil
	}
	return deleteHeaderOverrideInContext(context, fromHeader)
}

func deleteHeaderOverrideInContext(context map[string]interface{}, headerName string) error {
	headerName = normalizeHeaderContextKey(headerName)
	if headerName == "" {
		return fmt.Errorf("header name is required")
	}
	rawHeaders := ensureMapKeyInContext(context, paramOverrideContextHeaderOverride)
	delete(rawHeaders, headerName)
	return nil
}

func parseHeaderPassThroughNames(value interface{}) ([]string, error) {
	normalizeNames := func(values []string) []string {
		names := lo.FilterMap(values, func(item string, _ int) (string, bool) {
			headerName := normalizeHeaderContextKey(item)
			if headerName == "" {
				return "", false
			}
			return headerName, true
		})
		return lo.Uniq(names)
	}

	switch raw := value.(type) {
	case nil:
		return nil, fmt.Errorf("pass_headers value is required")
	case string:
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, fmt.Errorf("pass_headers value is required")
		}
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
			var parsed interface{}
			if err := common.UnmarshalJsonStr(trimmed, &parsed); err == nil {
				return parseHeaderPassThroughNames(parsed)
			}
		}
		names := normalizeNames(strings.Split(trimmed, ","))
		if len(names) == 0 {
			return nil, fmt.Errorf("pass_headers value is invalid")
		}
		return names, nil
	case []interface{}:
		names := lo.FilterMap(raw, func(item interface{}, _ int) (string, bool) {
			headerName := normalizeHeaderContextKey(fmt.Sprintf("%v", item))
			if headerName == "" {
				return "", false
			}
			return headerName, true
		})
		names = lo.Uniq(names)
		if len(names) == 0 {
			return nil, fmt.Errorf("pass_headers value is invalid")
		}
		return names, nil
	case []string:
		names := lo.FilterMap(raw, func(item string, _ int) (string, bool) {
			headerName := normalizeHeaderContextKey(item)
			if headerName == "" {
				return "", false
			}
			return headerName, true
		})
		names = lo.Uniq(names)
		if len(names) == 0 {
			return nil, fmt.Errorf("pass_headers value is invalid")
		}
		return names, nil
	case map[string]interface{}:
		candidates := make([]string, 0, 8)
		if headersRaw, ok := raw["headers"]; ok {
			names, err := parseHeaderPassThroughNames(headersRaw)
			if err == nil {
				candidates = append(candidates, names...)
			}
		}
		if namesRaw, ok := raw["names"]; ok {
			names, err := parseHeaderPassThroughNames(namesRaw)
			if err == nil {
				candidates = append(candidates, names...)
			}
		}
		if headerRaw, ok := raw["header"]; ok {
			names, err := parseHeaderPassThroughNames(headerRaw)
			if err == nil {
				candidates = append(candidates, names...)
			}
		}
		names := normalizeNames(candidates)
		if len(names) == 0 {
			return nil, fmt.Errorf("pass_headers value is invalid")
		}
		return names, nil
	default:
		return nil, fmt.Errorf("pass_headers value must be string, array or object")
	}
}

type syncTarget struct {
	kind string
	key  string
}

func parseSyncTarget(spec string) (syncTarget, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return syncTarget{}, fmt.Errorf("sync_fields target is required")
	}

	idx := strings.Index(raw, ":")
	if idx < 0 {
		// Backward compatibility: treat bare value as JSON path.
		return syncTarget{
			kind: "json",
			key:  raw,
		}, nil
	}

	kind := strings.ToLower(strings.TrimSpace(raw[:idx]))
	key := strings.TrimSpace(raw[idx+1:])
	if key == "" {
		return syncTarget{}, fmt.Errorf("sync_fields target key is required: %s", raw)
	}

	switch kind {
	case "json", "body":
		return syncTarget{
			kind: "json",
			key:  key,
		}, nil
	case "header":
		return syncTarget{
			kind: "header",
			key:  key,
		}, nil
	default:
		return syncTarget{}, fmt.Errorf("sync_fields target prefix is invalid: %s", raw)
	}
}

func readSyncTargetValue(data []byte, context map[string]interface{}, target syncTarget) (interface{}, bool, error) {
	switch target.kind {
	case "json":
		path := processNegativeIndex(data, target.key)
		value := gjson.GetBytes(data, path)
		if !value.Exists() || value.Type == gjson.Null {
			return nil, false, nil
		}
		if value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			return nil, false, nil
		}
		return value.Value(), true, nil
	case "header":
		value, ok := getHeaderValueFromContext(context, target.key)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, false, nil
		}
		return value, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported sync_fields target kind: %s", target.kind)
	}
}

func writeSyncTargetValue(data []byte, context map[string]interface{}, target syncTarget, value interface{}) ([]byte, error) {
	switch target.kind {
	case "json":
		path := processNegativeIndex(data, target.key)
		nextJSON, err := sjson.SetBytes(data, path, value)
		if err != nil {
			return nil, err
		}
		return nextJSON, nil
	case "header":
		if err := setHeaderOverrideInContext(context, target.key, value, false); err != nil {
			return nil, err
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported sync_fields target kind: %s", target.kind)
	}
}

func syncFieldsBetweenTargets(data []byte, context map[string]interface{}, fromSpec string, toSpec string) ([]byte, error) {
	fromTarget, err := parseSyncTarget(fromSpec)
	if err != nil {
		return nil, err
	}
	toTarget, err := parseSyncTarget(toSpec)
	if err != nil {
		return nil, err
	}

	fromValue, fromExists, err := readSyncTargetValue(data, context, fromTarget)
	if err != nil {
		return nil, err
	}
	toValue, toExists, err := readSyncTargetValue(data, context, toTarget)
	if err != nil {
		return nil, err
	}

	// If one side exists and the other side is missing, sync the missing side.
	if fromExists && !toExists {
		return writeSyncTargetValue(data, context, toTarget, fromValue)
	}
	if toExists && !fromExists {
		return writeSyncTargetValue(data, context, fromTarget, toValue)
	}
	return data, nil
}

func ensureMapKeyInContext(context map[string]interface{}, key string) map[string]interface{} {
	if context == nil {
		return map[string]interface{}{}
	}
	if existing, ok := context[key]; ok {
		if mapVal, ok := existing.(map[string]interface{}); ok {
			return mapVal
		}
	}
	result := make(map[string]interface{})
	context[key] = result
	return result
}

func getHeaderValueFromContext(context map[string]interface{}, headerName string) (string, bool) {
	headerName = normalizeHeaderContextKey(headerName)
	if headerName == "" {
		return "", false
	}
	for _, key := range []string{paramOverrideContextHeaderOverride, paramOverrideContextRequestHeaders} {
		source := ensureMapKeyInContext(context, key)
		raw, ok := source[headerName]
		if !ok {
			continue
		}
		value := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func normalizeHeaderContextKey(key string) string {
	return strings.TrimSpace(strings.ToLower(key))
}

func buildRequestHeadersContext(headers map[string]string) map[string]interface{} {
	if len(headers) == 0 {
		return map[string]interface{}{}
	}
	entries := lo.Entries(headers)
	normalizedEntries := lo.FilterMap(entries, func(item lo.Entry[string, string], _ int) (lo.Entry[string, string], bool) {
		normalized := normalizeHeaderContextKey(item.Key)
		value := strings.TrimSpace(item.Value)
		if normalized == "" || value == "" {
			return lo.Entry[string, string]{}, false
		}
		return lo.Entry[string, string]{Key: normalized, Value: value}, true
	})
	return lo.SliceToMap(normalizedEntries, func(item lo.Entry[string, string]) (string, interface{}) {
		return item.Key, item.Value
	})
}

func syncRuntimeHeaderOverrideFromContext(info *RelayInfo, context map[string]interface{}) {
	if info == nil || context == nil {
		return
	}
	raw, exists := context[paramOverrideContextHeaderOverride]
	if !exists {
		return
	}
	rawMap, ok := raw.(map[string]interface{})
	if !ok {
		return
	}
	info.RuntimeHeadersOverride = sanitizeHeaderOverrideMap(rawMap)
	info.UseRuntimeHeadersOverride = true
}

package common

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func moveValue(data []byte, fromPath, toPath string) ([]byte, error) {
	sourceValue := gjson.GetBytes(data, fromPath)
	if !sourceValue.Exists() {
		return data, fmt.Errorf("source path does not exist: %s", fromPath)
	}
	result, err := sjson.SetBytes(data, toPath, sourceValue.Value())
	if err != nil {
		return nil, err
	}
	return sjson.DeleteBytes(result, fromPath)
}

func copyValue(data []byte, fromPath, toPath string) ([]byte, error) {
	sourceValue := gjson.GetBytes(data, fromPath)
	if !sourceValue.Exists() {
		return data, fmt.Errorf("source path does not exist: %s", fromPath)
	}
	return sjson.SetBytes(data, toPath, sourceValue.Value())
}

func isPathBasedOperation(mode string) bool {
	switch mode {
	case "delete", "set", "prepend", "append", "trim_prefix", "trim_suffix", "ensure_prefix", "ensure_suffix", "trim_space", "to_lower", "to_upper", "replace", "regex_replace", "prune_objects":
		return true
	default:
		return false
	}
}

func resolveOperationPaths(data []byte, path string) ([]string, error) {
	if !strings.Contains(path, "*") {
		return []string{path}, nil
	}
	return expandWildcardPaths(data, path)
}

func expandWildcardPaths(data []byte, path string) ([]string, error) {
	var root interface{}
	if err := common.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	segments := strings.Split(path, ".")
	paths := collectWildcardPaths(root, segments, nil)
	return lo.Uniq(paths), nil
}

func collectWildcardPaths(node interface{}, segments []string, prefix []string) []string {
	if len(segments) == 0 {
		return []string{strings.Join(prefix, ".")}
	}

	segment := strings.TrimSpace(segments[0])
	if segment == "" {
		return nil
	}
	isLast := len(segments) == 1

	if segment == "*" {
		switch typed := node.(type) {
		case map[string]interface{}:
			keys := lo.Keys(typed)
			sort.Strings(keys)
			return lo.FlatMap(keys, func(key string, _ int) []string {
				return collectWildcardPaths(typed[key], segments[1:], append(prefix, key))
			})
		case []interface{}:
			return lo.FlatMap(lo.Range(len(typed)), func(index int, _ int) []string {
				return collectWildcardPaths(typed[index], segments[1:], append(prefix, strconv.Itoa(index)))
			})
		default:
			return nil
		}
	}

	switch typed := node.(type) {
	case map[string]interface{}:
		if isLast {
			return []string{strings.Join(append(prefix, segment), ".")}
		}
		next, exists := typed[segment]
		if !exists {
			return nil
		}
		return collectWildcardPaths(next, segments[1:], append(prefix, segment))
	case []interface{}:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(typed) {
			return nil
		}
		if isLast {
			return []string{strings.Join(append(prefix, segment), ".")}
		}
		return collectWildcardPaths(typed[index], segments[1:], append(prefix, segment))
	default:
		return nil
	}
}

func deleteValue(data []byte, path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return data, nil
	}
	return sjson.DeleteBytes(data, path)
}

func modifyValue(data []byte, path string, value interface{}, keepOrigin, isPrepend bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	switch {
	case current.IsArray():
		return modifyArray(data, path, value, isPrepend)
	case current.Type == gjson.String:
		return modifyString(data, path, value, isPrepend)
	case current.Type == gjson.JSON:
		return mergeObjects(data, path, value, keepOrigin)
	}
	return data, fmt.Errorf("operation not supported for type: %v", current.Type)
}

func modifyArray(data []byte, path string, value interface{}, isPrepend bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	var newArray []interface{}
	// 添加新值
	addValue := func() {
		if arr, ok := value.([]interface{}); ok {
			newArray = append(newArray, arr...)
		} else {
			newArray = append(newArray, value)
		}
	}
	// 添加原值
	addOriginal := func() {
		current.ForEach(func(_, val gjson.Result) bool {
			newArray = append(newArray, val.Value())
			return true
		})
	}
	if isPrepend {
		addValue()
		addOriginal()
	} else {
		addOriginal()
		addValue()
	}
	return sjson.SetBytes(data, path, newArray)
}

func modifyString(data []byte, path string, value interface{}, isPrepend bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	valueStr := fmt.Sprintf("%v", value)
	var newStr string
	if isPrepend {
		newStr = valueStr + current.String()
	} else {
		newStr = current.String() + valueStr
	}
	return sjson.SetBytes(data, path, newStr)
}

func trimStringValue(data []byte, path string, value interface{}, isPrefix bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	if current.Type != gjson.String {
		return data, fmt.Errorf("operation not supported for type: %v", current.Type)
	}

	if value == nil {
		return data, fmt.Errorf("trim value is required")
	}
	valueStr := fmt.Sprintf("%v", value)

	var newStr string
	if isPrefix {
		newStr = strings.TrimPrefix(current.String(), valueStr)
	} else {
		newStr = strings.TrimSuffix(current.String(), valueStr)
	}
	return sjson.SetBytes(data, path, newStr)
}

func ensureStringAffix(data []byte, path string, value interface{}, isPrefix bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	if current.Type != gjson.String {
		return data, fmt.Errorf("operation not supported for type: %v", current.Type)
	}

	if value == nil {
		return data, fmt.Errorf("ensure value is required")
	}
	valueStr := fmt.Sprintf("%v", value)
	if valueStr == "" {
		return data, fmt.Errorf("ensure value is required")
	}

	currentStr := current.String()
	if isPrefix {
		if strings.HasPrefix(currentStr, valueStr) {
			return data, nil
		}
		return sjson.SetBytes(data, path, valueStr+currentStr)
	}

	if strings.HasSuffix(currentStr, valueStr) {
		return data, nil
	}
	return sjson.SetBytes(data, path, currentStr+valueStr)
}

func transformStringValue(data []byte, path string, transform func(string) string) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	if current.Type != gjson.String {
		return data, fmt.Errorf("operation not supported for type: %v", current.Type)
	}
	return sjson.SetBytes(data, path, transform(current.String()))
}

func replaceStringValue(data []byte, path, from, to string) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	if current.Type != gjson.String {
		return data, fmt.Errorf("operation not supported for type: %v", current.Type)
	}
	if from == "" {
		return data, fmt.Errorf("replace from is required")
	}
	return sjson.SetBytes(data, path, strings.ReplaceAll(current.String(), from, to))
}

func regexReplaceStringValue(data []byte, path, pattern, replacement string) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	if current.Type != gjson.String {
		return data, fmt.Errorf("operation not supported for type: %v", current.Type)
	}
	if pattern == "" {
		return data, fmt.Errorf("regex pattern is required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return data, err
	}
	return sjson.SetBytes(data, path, re.ReplaceAllString(current.String(), replacement))
}

type pruneObjectsOptions struct {
	conditions []ConditionOperation
	logic      string
	recursive  bool
}

func pruneObjects(data []byte, path, contextJSON string, value interface{}) ([]byte, error) {
	options, err := parsePruneObjectsOptions(value)
	if err != nil {
		return nil, err
	}

	if path == "" {
		var root interface{}
		if err := common.Unmarshal(data, &root); err != nil {
			return nil, err
		}
		cleaned, _, err := pruneObjectsNode(root, options, contextJSON, true)
		if err != nil {
			return nil, err
		}
		return common.Marshal(cleaned)
	}

	target := gjson.GetBytes(data, path)
	if !target.Exists() {
		return data, nil
	}

	var targetNode interface{}
	if target.Type == gjson.JSON {
		if err := common.UnmarshalJsonStr(target.Raw, &targetNode); err != nil {
			return nil, err
		}
	} else {
		targetNode = target.Value()
	}

	cleaned, _, err := pruneObjectsNode(targetNode, options, contextJSON, true)
	if err != nil {
		return nil, err
	}
	cleanedBytes, err := common.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(data, path, cleanedBytes)
}

func parsePruneObjectsOptions(value interface{}) (pruneObjectsOptions, error) {
	opts := pruneObjectsOptions{
		logic:     "AND",
		recursive: true,
	}

	switch raw := value.(type) {
	case nil:
		return opts, fmt.Errorf("prune_objects value is required")
	case string:
		v := strings.TrimSpace(raw)
		if v == "" {
			return opts, fmt.Errorf("prune_objects value is required")
		}
		opts.conditions = []ConditionOperation{
			{
				Path:  "type",
				Mode:  "full",
				Value: v,
			},
		}
	case map[string]interface{}:
		if logic, ok := raw["logic"].(string); ok && strings.TrimSpace(logic) != "" {
			opts.logic = logic
		}
		if recursive, ok := raw["recursive"].(bool); ok {
			opts.recursive = recursive
		}

		if condRaw, exists := raw["conditions"]; exists {
			conditions, err := parseConditionOperations(condRaw)
			if err != nil {
				return opts, err
			}
			opts.conditions = append(opts.conditions, conditions...)
		}

		if whereRaw, exists := raw["where"]; exists {
			whereMap, ok := whereRaw.(map[string]interface{})
			if !ok {
				return opts, fmt.Errorf("prune_objects where must be object")
			}
			for key, val := range whereMap {
				key = strings.TrimSpace(key)
				if key == "" {
					continue
				}
				opts.conditions = append(opts.conditions, ConditionOperation{
					Path:  key,
					Mode:  "full",
					Value: val,
				})
			}
		}

		if matchType, exists := raw["type"]; exists {
			opts.conditions = append(opts.conditions, ConditionOperation{
				Path:  "type",
				Mode:  "full",
				Value: matchType,
			})
		}
	default:
		return opts, fmt.Errorf("prune_objects value must be string or object")
	}

	if len(opts.conditions) == 0 {
		return opts, fmt.Errorf("prune_objects conditions are required")
	}
	return opts, nil
}

func parseConditionOperations(raw interface{}) ([]ConditionOperation, error) {
	switch typed := raw.(type) {
	case map[string]interface{}:
		entries := lo.Entries(typed)
		conditions := lo.FilterMap(entries, func(item lo.Entry[string, interface{}], _ int) (ConditionOperation, bool) {
			path := strings.TrimSpace(item.Key)
			if path == "" {
				return ConditionOperation{}, false
			}
			return ConditionOperation{
				Path:  path,
				Mode:  "full",
				Value: item.Value,
			}, true
		})
		if len(conditions) == 0 {
			return nil, fmt.Errorf("conditions object must contain at least one key")
		}
		return conditions, nil
	case []interface{}:
		items := typed
		result := make([]ConditionOperation, 0, len(items))
		for _, item := range items {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("condition must be object")
			}
			path, _ := itemMap["path"].(string)
			mode, _ := itemMap["mode"].(string)
			if strings.TrimSpace(path) == "" || strings.TrimSpace(mode) == "" {
				return nil, fmt.Errorf("condition path/mode is required")
			}
			condition := ConditionOperation{
				Path: path,
				Mode: mode,
			}
			if value, exists := itemMap["value"]; exists {
				condition.Value = value
			}
			if invert, ok := itemMap["invert"].(bool); ok {
				condition.Invert = invert
			}
			if passMissingKey, ok := itemMap["pass_missing_key"].(bool); ok {
				condition.PassMissingKey = passMissingKey
			}
			result = append(result, condition)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("conditions must be an array or object")
	}
}

func pruneObjectsNode(node interface{}, options pruneObjectsOptions, contextJSON string, isRoot bool) (interface{}, bool, error) {
	switch value := node.(type) {
	case []interface{}:
		result := make([]interface{}, 0, len(value))
		for _, item := range value {
			next, drop, err := pruneObjectsNode(item, options, contextJSON, false)
			if err != nil {
				return nil, false, err
			}
			if drop {
				continue
			}
			result = append(result, next)
		}
		return result, false, nil
	case map[string]interface{}:
		shouldDrop, err := shouldPruneObject(value, options, contextJSON)
		if err != nil {
			return nil, false, err
		}
		if shouldDrop && !isRoot {
			return nil, true, nil
		}
		if !options.recursive {
			return value, false, nil
		}
		for key, child := range value {
			next, drop, err := pruneObjectsNode(child, options, contextJSON, false)
			if err != nil {
				return nil, false, err
			}
			if drop {
				delete(value, key)
				continue
			}
			value[key] = next
		}
		return value, false, nil
	default:
		return node, false, nil
	}
}

func shouldPruneObject(node map[string]interface{}, options pruneObjectsOptions, contextJSON string) (bool, error) {
	nodeBytes, err := common.Marshal(node)
	if err != nil {
		return false, err
	}
	return checkConditions(nodeBytes, contextJSON, options.conditions, options.logic)
}

func mergeObjects(data []byte, path string, value interface{}, keepOrigin bool) ([]byte, error) {
	current := gjson.GetBytes(data, path)
	var currentMap, newMap map[string]interface{}

	// 解析当前值（current.Raw 是 data 的子串，避免再分配一份）
	if err := common.UnmarshalJsonStr(current.Raw, &currentMap); err != nil {
		return nil, err
	}
	// 解析新值
	switch v := value.(type) {
	case map[string]interface{}:
		newMap = v
	default:
		jsonBytes, _ := common.Marshal(v)
		if err := common.Unmarshal(jsonBytes, &newMap); err != nil {
			return nil, err
		}
	}
	// 合并
	result := make(map[string]interface{})
	for k, v := range currentMap {
		result[k] = v
	}
	for k, v := range newMap {
		if !keepOrigin || result[k] == nil {
			result[k] = v
		}
	}
	return sjson.SetBytes(data, path, result)
}

// BuildParamOverrideContext 提供 ApplyParamOverride 可用的上下文信息。
// 目前内置以下字段：
//   - upstream_model/model：始终为通道映射后的上游模型名。
//   - original_model：请求最初指定的模型名。
//   - request_path：请求路径
//   - is_channel_test：是否为渠道测试请求（同 is_test）。

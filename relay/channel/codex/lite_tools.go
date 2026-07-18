package codex

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func filterCodexResponsesLiteRequest(request *dto.OpenAIResponsesRequest) error {
	data, err := common.Marshal(request)
	if err != nil {
		return err
	}
	filtered, err := FilterResponsesLitePayload(data)
	if err != nil {
		return err
	}
	var filteredRequest dto.OpenAIResponsesRequest
	if err := common.Unmarshal(filtered, &filteredRequest); err != nil {
		return err
	}
	*request = filteredRequest
	return nil
}

// FilterResponsesLitePayload removes hosted tool declarations unsupported by
// Responses Lite while preserving client-executed tools. Codex collaboration
// tools are declared as namespaces and must remain intact for derived agent
// sessions. Tool declarations may also appear in additional_tools input items
// or nested historical response metadata.
func FilterResponsesLitePayload(payload []byte) ([]byte, error) {
	var root map[string]any
	if err := common.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	filterCodexResponsesLiteObject(root)
	return common.Marshal(root)
}

func filterCodexResponsesLiteObject(object map[string]any) {
	if tools, exists := object["tools"]; exists {
		if filtered, ok := filterCodexResponsesLiteTools(tools); ok {
			object["tools"] = filtered
		}
	}
	if choice, exists := object["tool_choice"]; exists && !codexResponsesLiteToolChoiceAllowed(choice) {
		delete(object, "tool_choice")
	}

	if input, ok := object["input"].([]any); ok {
		filteredInput := make([]any, 0, len(input))
		for _, item := range input {
			itemObject, isObject := item.(map[string]any)
			if !isObject || !strings.EqualFold(strings.TrimSpace(stringValue(itemObject["type"])), "additional_tools") {
				filteredInput = append(filteredInput, item)
				continue
			}
			filterCodexResponsesLiteObject(itemObject)
			if tools, exists := itemObject["tools"].([]any); exists && len(tools) == 0 {
				continue
			}
			filteredInput = append(filteredInput, itemObject)
		}
		object["input"] = filteredInput
	}
	if response, ok := object["response"].(map[string]any); ok {
		filterCodexResponsesLiteObject(response)
	}
}

func filterCodexResponsesLiteTools(value any) ([]any, bool) {
	tools, ok := value.([]any)
	if !ok {
		return nil, false
	}
	filtered := make([]any, 0, len(tools))
	for _, tool := range tools {
		if codexResponsesLiteToolAllowed(tool) {
			filtered = append(filtered, tool)
		}
	}
	return filtered, true
}

func codexResponsesLiteToolAllowed(value any) bool {
	tool, ok := value.(map[string]any)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) {
	case "function", "custom", "namespace":
		return true
	case "tool_search":
		return strings.EqualFold(strings.TrimSpace(stringValue(tool["execution"])), "client")
	default:
		return false
	}
}

func codexResponsesLiteToolChoiceAllowed(value any) bool {
	if choice, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "auto", "none", "required":
			return true
		default:
			return false
		}
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(stringValue(choice["type"])), "allowed_tools") {
		hasAllowedTools := false
		if tools, exists := choice["tools"]; exists {
			if filtered, valid := filterCodexResponsesLiteTools(tools); valid {
				choice["tools"] = filtered
				hasAllowedTools = hasAllowedTools || len(filtered) > 0
			}
		}
		if allowedTools, exists := choice["allowed_tools"]; exists {
			switch nested := allowedTools.(type) {
			case []any:
				filtered, _ := filterCodexResponsesLiteTools(nested)
				choice["allowed_tools"] = filtered
				hasAllowedTools = hasAllowedTools || len(filtered) > 0
			case map[string]any:
				if filtered, valid := filterCodexResponsesLiteTools(nested["tools"]); valid {
					nested["tools"] = filtered
					hasAllowedTools = hasAllowedTools || len(filtered) > 0
				}
			}
		}
		return hasAllowedTools
	}
	return codexResponsesLiteToolAllowed(choice)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	if number, ok := value.(json.Number); ok {
		return number.String()
	}
	return ""
}

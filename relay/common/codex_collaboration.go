package common

import (
	"strings"

	rootcommon "github.com/QuantumNous/new-api/common"
)

// NormalizeCodexCollaborationSpawnAgentModel expands the short model aliases
// emitted by Codex collaboration tools. The client uses these tool-call
// arguments to create derived sessions, so returning the upstream model ID is
// required even though incoming Responses requests are normalized separately.
func NormalizeCodexCollaborationSpawnAgentModel(data string) string {
	var event map[string]any
	if err := rootcommon.UnmarshalJsonStr(data, &event); err != nil {
		return data
	}

	changed := false
	switch eventType := stringValue(event["type"]); eventType {
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		changed = normalizeCodexSpawnAgentItemModel(item)
	case "response.completed", "response.done":
		response, _ := event["response"].(map[string]any)
		if output, ok := response["output"].([]any); ok {
			for _, value := range output {
				item, _ := value.(map[string]any)
				if normalizeCodexSpawnAgentItemModel(item) {
					changed = true
				}
			}
		}
	}
	if !changed {
		return data
	}

	normalized, err := rootcommon.Marshal(event)
	if err != nil {
		return data
	}
	return string(normalized)
}

func normalizeCodexSpawnAgentItemModel(item map[string]any) bool {
	if !isCodexCollaborationSpawnAgentCall(item) {
		return false
	}
	arguments, exists := item["arguments"]
	if !exists {
		return false
	}

	switch value := arguments.(type) {
	case string:
		var object map[string]any
		if err := rootcommon.UnmarshalJsonStr(value, &object); err != nil || !normalizeCodexSpawnAgentArguments(object) {
			return false
		}
		normalized, err := rootcommon.Marshal(object)
		if err != nil {
			return false
		}
		item["arguments"] = string(normalized)
		return true
	case map[string]any:
		return normalizeCodexSpawnAgentArguments(value)
	default:
		return false
	}
}

func normalizeCodexSpawnAgentArguments(arguments map[string]any) bool {
	model, _ := arguments["model"].(string)
	modelID := codexSpawnAgentModelID(model)
	if modelID == "" || modelID == model {
		return false
	}
	arguments["model"] = modelID
	return true
}

func isCodexCollaborationSpawnAgentCall(item map[string]any) bool {
	if stringValue(item["type"]) != "function_call" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(stringValue(item["name"])))
	if name == "spawn_agent" {
		return true
	}
	return (strings.HasSuffix(name, "__spawn_agent") || strings.HasSuffix(name, ".spawn_agent")) &&
		(strings.Contains(name, "collaboration") || strings.Contains(name, "multi_agent"))
}

func codexSpawnAgentModelID(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "sol":
		return "gpt-5.6-sol"
	case "terra":
		return "gpt-5.6-terra"
	case "luna":
		return "gpt-5.6-luna"
	default:
		return model
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

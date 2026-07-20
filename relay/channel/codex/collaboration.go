package codex

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeCollaborationSpawnAgentModel expands model aliases in client-run
// collaboration calls without re-encoding the surrounding Responses event.
func normalizeCollaborationSpawnAgentModel(data string) string {
	var itemPaths []string
	switch gjson.Get(data, "type").String() {
	case "response.output_item.done":
		itemPaths = []string{"item"}
	case "response.completed", "response.done":
		output := gjson.Get(data, "response.output")
		if !output.IsArray() {
			return data
		}
		itemPaths = make([]string, len(output.Array()))
		for index := range itemPaths {
			itemPaths[index] = fmt.Sprintf("response.output.%d", index)
		}
	default:
		return data
	}

	for _, itemPath := range itemPaths {
		item := gjson.Get(data, itemPath)
		if !isCollaborationSpawnAgentCall(item) {
			continue
		}
		arguments := item.Get("arguments")
		var argumentsJSON string
		switch arguments.Type {
		case gjson.String:
			argumentsJSON = arguments.String()
		case gjson.JSON:
			argumentsJSON = arguments.Raw
		default:
			continue
		}
		if !gjson.Valid(argumentsJSON) {
			continue
		}
		model := gjson.Get(argumentsJSON, "model")
		modelID := ratio_setting.CanonicalCodexModelAlias(model.String())
		if model.Type != gjson.String || modelID == "" || modelID == model.String() {
			continue
		}
		normalizedArguments, err := sjson.Set(argumentsJSON, "model", modelID)
		if err != nil {
			continue
		}
		argumentsPath := itemPath + ".arguments"
		var updated string
		if arguments.Type == gjson.JSON {
			updated, err = sjson.SetRaw(data, argumentsPath, normalizedArguments)
		} else {
			updated, err = sjson.Set(data, argumentsPath, normalizedArguments)
		}
		if err == nil {
			data = updated
		}
	}
	return data
}

func isCollaborationSpawnAgentCall(item gjson.Result) bool {
	if item.Get("type").String() != "function_call" {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(item.Get("name").String()))
	if name == "spawn_agent" {
		return true
	}
	return (strings.HasSuffix(name, "__spawn_agent") || strings.HasSuffix(name, ".spawn_agent")) &&
		(strings.Contains(name, "collaboration") || strings.Contains(name, "multi_agent"))
}

package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeCollaborationSpawnAgentModel(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		argumentsPath string
		wantModel     string
	}{
		{
			name:          "output item",
			input:         `{"type":"response.output_item.done","item":{"type":"function_call","name":"spawn_agent","arguments":"{\"model\":\"luna\"}"}}`,
			argumentsPath: "item.arguments",
			wantModel:     "gpt-5.6-luna",
		},
		{
			name:          "completed response",
			input:         `{"type":"response.completed","response":{"output":[{"type":"function_call","name":"collaboration__spawn_agent","arguments":{"model":"terra"}}]}}`,
			argumentsPath: "response.output.0.arguments",
			wantModel:     "gpt-5.6-terra",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := normalizeCollaborationSpawnAgentModel(test.input)
			arguments := gjson.Get(output, test.argumentsPath)
			if arguments.Type == gjson.String {
				arguments = gjson.Parse(arguments.String())
			}
			require.Equal(t, test.wantModel, arguments.Get("model").String())
		})
	}
}

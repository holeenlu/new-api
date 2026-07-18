package common

import (
	"testing"
)

func TestApplyParamOverridePruneObjectsByTypeString(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[
				{"type":"output_text","text":"a"},
				{"type":"redacted_thinking","text":"secret"},
				{"type":"tool_call","name":"tool_a"}
			]},
			{"role":"assistant","content":[
				{"type":"output_text","text":"b"},
				{"type":"wrapper","parts":[
					{"type":"redacted_thinking","text":"secret2"},
					{"type":"output_text","text":"c"}
				]}
			]}
		]
	}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode":  "prune_objects",
				"value": "redacted_thinking",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{
		"messages":[
			{"role":"assistant","content":[
				{"type":"output_text","text":"a"},
				{"type":"tool_call","name":"tool_a"}
			]},
			{"role":"assistant","content":[
				{"type":"output_text","text":"b"},
				{"type":"wrapper","parts":[
					{"type":"output_text","text":"c"}
				]}
			]}
		]
	}`, string(out))
}

func TestApplyParamOverridePruneObjectsWhereAndPath(t *testing.T) {
	input := []byte(`{
		"a":{"items":[{"type":"redacted_thinking","id":1},{"type":"output_text","id":2}]},
		"b":{"items":[{"type":"redacted_thinking","id":3},{"type":"output_text","id":4}]}
	}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "a",
				"mode": "prune_objects",
				"value": map[string]interface{}{
					"where": map[string]interface{}{
						"type": "redacted_thinking",
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{
		"a":{"items":[{"type":"output_text","id":2}]},
		"b":{"items":[{"type":"redacted_thinking","id":3},{"type":"output_text","id":4}]}
	}`, string(out))
}

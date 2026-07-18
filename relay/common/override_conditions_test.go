package common

import (
	"testing"
)

func TestApplyParamOverrideConditionORDefault(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "model",
						"mode":  "prefix",
						"value": "gpt",
					},
					map[string]interface{}{
						"path":  "model",
						"mode":  "prefix",
						"value": "claude",
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.1}`, string(out))
}

func TestApplyParamOverrideConditionAND(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"logic": "AND",
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "model",
						"mode":  "prefix",
						"value": "gpt",
					},
					map[string]interface{}{
						"path":  "temperature",
						"mode":  "gt",
						"value": 0.5,
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.1}`, string(out))
}

func TestApplyParamOverrideConditionInvert(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":   "model",
						"mode":   "prefix",
						"value":  "gpt",
						"invert": true,
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideConditionPassMissingKey(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":             "model",
						"mode":             "prefix",
						"value":            "gpt",
						"pass_missing_key": true,
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

func TestApplyParamOverrideConditionFromContext(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "model",
						"mode":  "prefix",
						"value": "gpt",
					},
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"model": "gpt-4",
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

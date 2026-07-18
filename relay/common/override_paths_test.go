package common

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/samber/lo"
)

func TestApplyParamOverrideDelete(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "temperature",
				"mode": "delete",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("failed to unmarshal output JSON: %v", err)
	}
	if _, exists := got["temperature"]; exists {
		t.Fatalf("expected temperature to be deleted")
	}
}

func TestApplyParamOverrideDeleteWildcardPath(t *testing.T) {
	input := []byte(`{"tools":[{"type":"bash","custom":{"input_examples":["a"],"other":1}},{"type":"code","custom":{"input_examples":["b"]}},{"type":"noop","custom":{"other":2}}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "tools.*.custom.input_examples",
				"mode": "delete",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"tools":[{"type":"bash","custom":{"other":1}},{"type":"code","custom":{}},{"type":"noop","custom":{"other":2}}]}`, string(out))
}

func TestApplyParamOverrideSetWildcardPath(t *testing.T) {
	input := []byte(`{"tools":[{"custom":{"tag":"A"}},{"custom":{"tag":"B"}},{"custom":{"tag":"C"}}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "tools.*.custom.enabled",
				"mode":  "set",
				"value": true,
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	var got struct {
		Tools []struct {
			Custom struct {
				Enabled bool `json:"enabled"`
			} `json:"custom"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("failed to unmarshal output JSON: %v", err)
	}

	if !lo.EveryBy(got.Tools, func(item struct {
		Custom struct {
			Enabled bool `json:"enabled"`
		} `json:"custom"`
	}) bool {
		return item.Custom.Enabled
	}) {
		t.Fatalf("expected wildcard set to enable all tools, got: %s", string(out))
	}
}

func TestApplyParamOverrideTrimSpaceWildcardPath(t *testing.T) {
	input := []byte(`{"tools":[{"custom":{"name":" alpha "}},{"custom":{"name":" beta"}},{"custom":{"name":"gamma "}}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "tools.*.custom.name",
				"mode": "trim_space",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	var got struct {
		Tools []struct {
			Custom struct {
				Name string `json:"name"`
			} `json:"custom"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("failed to unmarshal output JSON: %v", err)
	}

	names := lo.Map(got.Tools, func(item struct {
		Custom struct {
			Name string `json:"name"`
		} `json:"custom"`
	}, _ int) string {
		return item.Custom.Name
	})
	if !reflect.DeepEqual(names, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("unexpected names after wildcard trim_space: %v", names)
	}
}

func TestApplyParamOverrideDeleteWildcardEqualsIndexedPaths(t *testing.T) {
	input := []byte(`{"tools":[{"custom":{"input_examples":["a"],"other":1}},{"custom":{"input_examples":["b"],"other":2}},{"custom":{"input_examples":["c"],"other":3}}]}`)

	wildcardOverride := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "tools.*.custom.input_examples",
				"mode": "delete",
			},
		},
	}

	indexedOverride := map[string]interface{}{
		"operations": lo.Map(lo.Range(3), func(index int, _ int) interface{} {
			return map[string]interface{}{
				"path": fmt.Sprintf("tools.%d.custom.input_examples", index),
				"mode": "delete",
			}
		}),
	}

	wildcardOut, err := ApplyParamOverride(input, wildcardOverride, nil)
	if err != nil {
		t.Fatalf("wildcard ApplyParamOverride returned error: %v", err)
	}

	indexedOut, err := ApplyParamOverride(input, indexedOverride, nil)
	if err != nil {
		t.Fatalf("indexed ApplyParamOverride returned error: %v", err)
	}

	assertJSONEqual(t, string(indexedOut), string(wildcardOut))
}

func TestApplyParamOverrideSetWildcardKeepOrigin(t *testing.T) {
	input := []byte(`{"tools":[{"custom":{"tag":"A"}},{"custom":{"tag":"B","enabled":false}},{"custom":{"tag":"C"}}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":        "tools.*.custom.enabled",
				"mode":        "set",
				"value":       true,
				"keep_origin": true,
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	var got struct {
		Tools []struct {
			Custom struct {
				Enabled bool `json:"enabled"`
			} `json:"custom"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("failed to unmarshal output JSON: %v", err)
	}

	enabledValues := lo.Map(got.Tools, func(item struct {
		Custom struct {
			Enabled bool `json:"enabled"`
		} `json:"custom"`
	}, _ int) bool {
		return item.Custom.Enabled
	})
	if !reflect.DeepEqual(enabledValues, []bool{true, false, true}) {
		t.Fatalf("unexpected enabled values after wildcard keep_origin set: %v", enabledValues)
	}
}

func TestApplyParamOverrideTrimSpaceMultiWildcardPath(t *testing.T) {
	input := []byte(`{"tools":[{"custom":{"items":[{"name":" alpha "},{"name":" beta "}]}},{"custom":{"items":[{"name":" gamma"}]}}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "tools.*.custom.items.*.name",
				"mode": "trim_space",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	var got struct {
		Tools []struct {
			Custom struct {
				Items []struct {
					Name string `json:"name"`
				} `json:"items"`
			} `json:"custom"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("failed to unmarshal output JSON: %v", err)
	}

	names := lo.FlatMap(got.Tools, func(tool struct {
		Custom struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		} `json:"custom"`
	}, _ int) []string {
		return lo.Map(tool.Custom.Items, func(item struct {
			Name string `json:"name"`
		}, _ int) string {
			return item.Name
		})
	})
	if !reflect.DeepEqual(names, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("unexpected names after multi wildcard trim_space: %v", names)
	}
}

func TestApplyParamOverrideNegativeIndexPath(t *testing.T) {
	input := []byte(`{"arr":[{"model":"a"},{"model":"b"}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "arr.-1.model",
				"mode":  "set",
				"value": "c",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"arr":[{"model":"a"},{"model":"c"}]}`, string(out))
}

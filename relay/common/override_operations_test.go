package common

import (
	"testing"
)

func TestApplyParamOverrideTrimPrefix(t *testing.T) {
	// trim_prefix example:
	// {"operations":[{"path":"model","mode":"trim_prefix","value":"openai/"}]}
	input := []byte(`{"model":"openai/gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "trim_prefix",
				"value": "openai/",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideTrimSuffix(t *testing.T) {
	// trim_suffix example:
	// {"operations":[{"path":"model","mode":"trim_suffix","value":"-latest"}]}
	input := []byte(`{"model":"gpt-4-latest","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "trim_suffix",
				"value": "-latest",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideTrimNoop(t *testing.T) {
	// trim_prefix no-op example:
	// {"operations":[{"path":"model","mode":"trim_prefix","value":"openai/"}]}
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "trim_prefix",
				"value": "openai/",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideMixedLegacyAndOperations(t *testing.T) {
	input := []byte(`{"model":"openai/gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"temperature": 0.2,
		"top_p":       0.95,
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "trim_prefix",
				"value": "openai/",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.2,"top_p":0.95}`, string(out))
}

func TestApplyParamOverrideMixedLegacyAndOperationsConflictPrefersOperations(t *testing.T) {
	input := []byte(`{"model":"openai/gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"model":       "legacy-model",
		"temperature": 0.2,
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "set",
				"value": "op-model",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"op-model","temperature":0.2}`, string(out))
}

func TestApplyParamOverrideTrimRequiresValue(t *testing.T) {
	// trim_prefix requires value example:
	// {"operations":[{"path":"model","mode":"trim_prefix"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "trim_prefix",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideReplace(t *testing.T) {
	// replace example:
	// {"operations":[{"path":"model","mode":"replace","from":"openai/","to":""}]}
	input := []byte(`{"model":"openai/gpt-4o-mini","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "replace",
				"from": "openai/",
				"to":   "",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4o-mini","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideRegexReplace(t *testing.T) {
	// regex_replace example:
	// {"operations":[{"path":"model","mode":"regex_replace","from":"^gpt-","to":"openai/gpt-"}]}
	input := []byte(`{"model":"gpt-4o-mini","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "regex_replace",
				"from": "^gpt-",
				"to":   "openai/gpt-",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"openai/gpt-4o-mini","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideReplaceRequiresFrom(t *testing.T) {
	// replace requires from example:
	// {"operations":[{"path":"model","mode":"replace"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "replace",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideRegexReplaceRequiresPattern(t *testing.T) {
	// regex_replace requires from(pattern) example:
	// {"operations":[{"path":"model","mode":"regex_replace"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "regex_replace",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideSet(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.1}`, string(out))
}

func TestApplyParamOverrideSetWithDescriptionKeepsCompatibility(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	overrideWithoutDesc := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
			},
		},
	}
	overrideWithDesc := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"description": "set temperature for deterministic output",
				"path":        "temperature",
				"mode":        "set",
				"value":       0.1,
			},
		},
	}

	outWithoutDesc, err := ApplyParamOverride(input, overrideWithoutDesc, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride without description returned error: %v", err)
	}

	outWithDesc, err := ApplyParamOverride(input, overrideWithDesc, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride with description returned error: %v", err)
	}

	assertJSONEqual(t, string(outWithoutDesc), string(outWithDesc))
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.1}`, string(outWithDesc))
}

func TestApplyParamOverrideSetKeepOrigin(t *testing.T) {
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":        "temperature",
				"mode":        "set",
				"value":       0.1,
				"keep_origin": true,
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideMove(t *testing.T) {
	input := []byte(`{"model":"gpt-4","meta":{"x":1}}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "move",
				"from": "model",
				"to":   "meta.model",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"meta":{"x":1,"model":"gpt-4"}}`, string(out))
}

func TestApplyParamOverrideMoveMissingSource(t *testing.T) {
	input := []byte(`{"meta":{"x":1}}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "move",
				"from": "model",
				"to":   "meta.model",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverridePrependAppendString(t *testing.T) {
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "prepend",
				"value": "openai/",
			},
			map[string]interface{}{
				"path":  "model",
				"mode":  "append",
				"value": "-latest",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"openai/gpt-4-latest"}`, string(out))
}

func TestApplyParamOverridePrependAppendArray(t *testing.T) {
	input := []byte(`{"arr":[1,2]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "arr",
				"mode":  "prepend",
				"value": 0,
			},
			map[string]interface{}{
				"path":  "arr",
				"mode":  "append",
				"value": []interface{}{3, 4},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"arr":[0,1,2,3,4]}`, string(out))
}

func TestApplyParamOverrideAppendObjectMergeKeepOrigin(t *testing.T) {
	input := []byte(`{"obj":{"a":1}}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":        "obj",
				"mode":        "append",
				"keep_origin": true,
				"value": map[string]interface{}{
					"a": 2,
					"b": 3,
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"obj":{"a":1,"b":3}}`, string(out))
}

func TestApplyParamOverrideAppendObjectMergeOverride(t *testing.T) {
	input := []byte(`{"obj":{"a":1}}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "obj",
				"mode": "append",
				"value": map[string]interface{}{
					"a": 2,
					"b": 3,
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"obj":{"a":2,"b":3}}`, string(out))
}

func TestApplyParamOverrideRegexReplaceInvalidPattern(t *testing.T) {
	// regex_replace invalid pattern example:
	// {"operations":[{"path":"model","mode":"regex_replace","from":"(","to":"x"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "regex_replace",
				"from": "(",
				"to":   "x",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideCopy(t *testing.T) {
	// copy example:
	// {"operations":[{"mode":"copy","from":"model","to":"original_model"}]}
	input := []byte(`{"model":"gpt-4","temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "copy",
				"from": "model",
				"to":   "original_model",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","original_model":"gpt-4","temperature":0.7}`, string(out))
}

func TestApplyParamOverrideCopyMissingSource(t *testing.T) {
	// copy missing source example:
	// {"operations":[{"mode":"copy","from":"model","to":"original_model"}]}
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "copy",
				"from": "model",
				"to":   "original_model",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideCopyRequiresFromTo(t *testing.T) {
	// copy requires from/to example:
	// {"operations":[{"mode":"copy"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "copy",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideEnsurePrefix(t *testing.T) {
	// ensure_prefix example:
	// {"operations":[{"path":"model","mode":"ensure_prefix","value":"openai/"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "ensure_prefix",
				"value": "openai/",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"openai/gpt-4"}`, string(out))
}

func TestApplyParamOverrideEnsurePrefixNoop(t *testing.T) {
	// ensure_prefix no-op example:
	// {"operations":[{"path":"model","mode":"ensure_prefix","value":"openai/"}]}
	input := []byte(`{"model":"openai/gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "ensure_prefix",
				"value": "openai/",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"openai/gpt-4"}`, string(out))
}

func TestApplyParamOverrideEnsureSuffix(t *testing.T) {
	// ensure_suffix example:
	// {"operations":[{"path":"model","mode":"ensure_suffix","value":"-latest"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "ensure_suffix",
				"value": "-latest",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4-latest"}`, string(out))
}

func TestApplyParamOverrideEnsureSuffixNoop(t *testing.T) {
	// ensure_suffix no-op example:
	// {"operations":[{"path":"model","mode":"ensure_suffix","value":"-latest"}]}
	input := []byte(`{"model":"gpt-4-latest"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "model",
				"mode":  "ensure_suffix",
				"value": "-latest",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4-latest"}`, string(out))
}

func TestApplyParamOverrideEnsureRequiresValue(t *testing.T) {
	// ensure_prefix requires value example:
	// {"operations":[{"path":"model","mode":"ensure_prefix"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "ensure_prefix",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideTrimSpace(t *testing.T) {
	// trim_space example:
	// {"operations":[{"path":"model","mode":"trim_space"}]}
	input := []byte("{\"model\":\"  gpt-4 \\n\"}")
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "trim_space",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4"}`, string(out))
}

func TestApplyParamOverrideToLower(t *testing.T) {
	// to_lower example:
	// {"operations":[{"path":"model","mode":"to_lower"}]}
	input := []byte(`{"model":"GPT-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "to_lower",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4"}`, string(out))
}

func TestApplyParamOverrideToUpper(t *testing.T) {
	// to_upper example:
	// {"operations":[{"path":"model","mode":"to_upper"}]}
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path": "model",
				"mode": "to_upper",
			},
		},
	}

	out, err := ApplyParamOverride(input, override, nil)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"GPT-4"}`, string(out))
}

func TestApplyParamOverrideReturnError(t *testing.T) {
	input := []byte(`{"model":"gemini-2.5-pro"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "return_error",
				"value": map[string]interface{}{
					"message":     "forced bad request by param override",
					"status_code": 422,
					"code":        "forced_bad_request",
					"type":        "invalid_request_error",
					"skip_retry":  true,
				},
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "retry.is_retry",
						"mode":  "full",
						"value": true,
					},
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"retry": map[string]interface{}{
			"index":    1,
			"is_retry": true,
		},
	}

	_, err := ApplyParamOverride(input, override, ctx)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	returnErr, ok := AsParamOverrideReturnError(err)
	if !ok {
		t.Fatalf("expected ParamOverrideReturnError, got %T: %v", err, err)
	}
	if returnErr.StatusCode != 422 {
		t.Fatalf("expected status 422, got %d", returnErr.StatusCode)
	}
	if returnErr.Code != "forced_bad_request" {
		t.Fatalf("expected code forced_bad_request, got %s", returnErr.Code)
	}
	if !returnErr.SkipRetry {
		t.Fatalf("expected skip_retry true")
	}
}

func TestApplyParamOverrideNormalizeThinkingSignatureUnsupported(t *testing.T) {
	input := []byte(`{"items":[{"type":"redacted_thinking"}]}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "normalize_thinking_signature",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

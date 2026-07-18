package common

import (
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestApplyParamOverrideConditionFromRetryAndLastErrorContext(t *testing.T) {
	info := &RelayInfo{
		RetryIndex: 1,
		LastError: types.WithOpenAIError(types.OpenAIError{
			Message: "invalid thinking signature",
			Type:    "invalid_request_error",
			Code:    "bad_thought_signature",
		}, 400),
	}
	ctx := BuildParamOverrideContext(info)

	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"logic": "AND",
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "is_retry",
						"mode":  "full",
						"value": true,
					},
					map[string]interface{}{
						"path":  "last_error.code",
						"mode":  "contains",
						"value": "thought_signature",
					},
				},
			},
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

func TestApplyParamOverrideConditionFromRequestHeaders(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "request_headers.authorization",
						"mode":  "contains",
						"value": "Bearer ",
					},
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"authorization": "Bearer token-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

func TestApplyParamOverrideSetHeaderAndUseInLaterCondition(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode":  "set_header",
				"path":  "X-Debug-Mode",
				"value": "enabled",
			},
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "header_override.x-debug-mode",
						"mode":  "full",
						"value": "enabled",
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

func TestApplyParamOverrideCopyHeaderFromRequestHeaders(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "copy_header",
				"from": "Authorization",
				"to":   "X-Upstream-Auth",
			},
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"conditions": []interface{}{
					map[string]interface{}{
						"path":  "header_override.x-upstream-auth",
						"mode":  "contains",
						"value": "Bearer ",
					},
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"authorization": "Bearer token-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

func TestApplyParamOverridePassHeadersSkipsMissingHeaders(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode":  "pass_headers",
				"value": []interface{}{"X-Codex-Beta-Features", "Session_id"},
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"session_id": "sess-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["session_id"] != "sess-123" {
		t.Fatalf("expected session_id to be passed, got: %v", headers["session_id"])
	}
	if _, exists := headers["x-codex-beta-features"]; exists {
		t.Fatalf("expected missing header to be skipped")
	}
}

func TestApplyParamOverrideCopyHeaderSkipsMissingSource(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "copy_header",
				"from": "X-Missing-Header",
				"to":   "X-Upstream-Auth",
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"authorization": "Bearer token-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		return
	}
	if _, exists := headers["x-upstream-auth"]; exists {
		t.Fatalf("expected X-Upstream-Auth to be skipped when source header is missing")
	}
}

func TestApplyParamOverrideMoveHeaderSkipsMissingSource(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "move_header",
				"from": "X-Missing-Header",
				"to":   "X-Upstream-Auth",
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"authorization": "Bearer token-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		return
	}
	if _, exists := headers["x-upstream-auth"]; exists {
		t.Fatalf("expected X-Upstream-Auth to be skipped when source header is missing")
	}
}

func TestApplyParamOverrideSyncFieldsHeaderToJSON(t *testing.T) {
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "sync_fields",
				"from": "header:session_id",
				"to":   "json:prompt_cache_key",
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"session_id": "sess-123",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","prompt_cache_key":"sess-123"}`, string(out))
}

func TestApplyParamOverrideSyncFieldsJSONToHeader(t *testing.T) {
	input := []byte(`{"model":"gpt-4","prompt_cache_key":"cache-abc"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "sync_fields",
				"from": "header:session_id",
				"to":   "json:prompt_cache_key",
			},
		},
	}
	ctx := map[string]interface{}{}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","prompt_cache_key":"cache-abc"}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["session_id"] != "cache-abc" {
		t.Fatalf("expected session_id to be synced from prompt_cache_key, got: %v", headers["session_id"])
	}
}

func TestApplyParamOverrideSyncFieldsNoChangeWhenBothExist(t *testing.T) {
	input := []byte(`{"model":"gpt-4","prompt_cache_key":"cache-body"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "sync_fields",
				"from": "header:session_id",
				"to":   "json:prompt_cache_key",
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"session_id": "cache-header",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-4","prompt_cache_key":"cache-body"}`, string(out))

	headers, _ := ctx["header_override"].(map[string]interface{})
	if headers != nil {
		if _, exists := headers["session_id"]; exists {
			t.Fatalf("expected no override when both sides already have value")
		}
	}
}

func TestApplyParamOverrideSyncFieldsInvalidTarget(t *testing.T) {
	input := []byte(`{"model":"gpt-4"}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "sync_fields",
				"from": "foo:session_id",
				"to":   "json:prompt_cache_key",
			},
		},
	}

	_, err := ApplyParamOverride(input, override, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestApplyParamOverrideSetHeaderKeepOrigin(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode":        "set_header",
				"path":        "X-Feature-Flag",
				"value":       "new-value",
				"keep_origin": true,
			},
		},
	}
	ctx := map[string]interface{}{
		"header_override": map[string]interface{}{
			"x-feature-flag": "legacy-value",
		},
	}

	_, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["x-feature-flag"] != "legacy-value" {
		t.Fatalf("expected keep_origin to preserve old value, got: %v", headers["x-feature-flag"])
	}
}

func TestApplyParamOverrideSetHeaderMapRewritesCommaSeparatedHeader(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"advanced-tool-use-2025-11-20": nil,
					"computer-use-2025-01-24":      "computer-use-2025-01-24",
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"request_headers": map[string]interface{}{
			"anthropic-beta": "advanced-tool-use-2025-11-20, computer-use-2025-01-24",
		},
	}

	_, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["anthropic-beta"] != "computer-use-2025-01-24" {
		t.Fatalf("expected anthropic-beta to keep only mapped value, got: %v", headers["anthropic-beta"])
	}
}

func TestApplyParamOverrideSetHeaderMapDeleteWholeHeaderWhenAllTokensCleared(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"advanced-tool-use-2025-11-20": nil,
					"computer-use-2025-01-24":      nil,
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"header_override": map[string]interface{}{
			"anthropic-beta": "advanced-tool-use-2025-11-20,computer-use-2025-01-24",
		},
	}

	_, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if _, exists := headers["anthropic-beta"]; exists {
		t.Fatalf("expected anthropic-beta to be deleted when all mapped values are null")
	}
}

func TestApplyParamOverrideSetHeaderMapAppendsTokens(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"$append": []interface{}{"context-1m-2025-08-07", "computer-use-2025-01-24"},
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"header_override": map[string]interface{}{
			"anthropic-beta": "computer-use-2025-01-24",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["anthropic-beta"] != "computer-use-2025-01-24,context-1m-2025-08-07" {
		t.Fatalf("expected anthropic-beta to append new token without duplicates, got: %v", headers["anthropic-beta"])
	}
}

func TestApplyParamOverrideSetHeaderMapAppendsTokensWhenHeaderMissing(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"$append": []interface{}{"context-1m-2025-08-07", "computer-use-2025-01-24"},
				},
			},
		},
	}

	ctx := map[string]interface{}{}
	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["anthropic-beta"] != "context-1m-2025-08-07,computer-use-2025-01-24" {
		t.Fatalf("expected anthropic-beta to be created from appended tokens, got: %v", headers["anthropic-beta"])
	}
}

func TestApplyParamOverrideSetHeaderMapKeepOnlyDeclaredDropsUndeclaredTokens(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"computer-use-2025-01-24": "computer-use-2025-01-24",
					"$append":                 []interface{}{"context-1m-2025-08-07"},
					"$keep_only_declared":     true,
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"header_override": map[string]interface{}{
			"anthropic-beta": "advanced-tool-use-2025-11-20,computer-use-2025-01-24",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if headers["anthropic-beta"] != "computer-use-2025-01-24,context-1m-2025-08-07" {
		t.Fatalf("expected anthropic-beta to keep only declared tokens, got: %v", headers["anthropic-beta"])
	}
}

func TestApplyParamOverrideSetHeaderMapKeepOnlyDeclaredDeletesHeaderWhenNothingDeclaredMatches(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode": "set_header",
				"path": "anthropic-beta",
				"value": map[string]interface{}{
					"computer-use-2025-01-24": "computer-use-2025-01-24",
					"$keep_only_declared":     true,
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"header_override": map[string]interface{}{
			"anthropic-beta": "advanced-tool-use-2025-11-20",
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	headers, ok := ctx["header_override"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected header_override context map")
	}
	if _, exists := headers["anthropic-beta"]; exists {
		t.Fatalf("expected anthropic-beta to be deleted when no declared tokens remain, got: %v", headers["anthropic-beta"])
	}
}

func TestApplyParamOverrideConditionsObjectShorthand(t *testing.T) {
	input := []byte(`{"temperature":0.7}`)
	override := map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"path":  "temperature",
				"mode":  "set",
				"value": 0.1,
				"logic": "AND",
				"conditions": map[string]interface{}{
					"is_retry":               true,
					"last_error.status_code": 400.0,
				},
			},
		},
	}
	ctx := map[string]interface{}{
		"is_retry": true,
		"last_error": map[string]interface{}{
			"status_code": 400.0,
		},
	}

	out, err := ApplyParamOverride(input, override, ctx)
	if err != nil {
		t.Fatalf("ApplyParamOverride returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.1}`, string(out))
}

func TestApplyParamOverrideWithRelayInfoSyncRuntimeHeaders(t *testing.T) {
	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode":  "set_header",
						"path":  "X-Injected-By-Param-Override",
						"value": "enabled",
					},
					map[string]interface{}{
						"mode": "delete_header",
						"path": "X-Delete-Me",
					},
				},
			},
			HeadersOverride: map[string]interface{}{
				"X-Delete-Me": "legacy",
				"X-Keep-Me":   "keep",
			},
		},
	}

	input := []byte(`{"temperature":0.7}`)
	out, err := ApplyParamOverrideWithRelayInfo(input, info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}
	assertJSONEqual(t, `{"temperature":0.7}`, string(out))

	if !info.UseRuntimeHeadersOverride {
		t.Fatalf("expected runtime header override to be enabled")
	}
	if info.RuntimeHeadersOverride["x-keep-me"] != "keep" {
		t.Fatalf("expected x-keep-me header to be preserved, got: %v", info.RuntimeHeadersOverride["x-keep-me"])
	}
	if info.RuntimeHeadersOverride["x-injected-by-param-override"] != "enabled" {
		t.Fatalf("expected x-injected-by-param-override header to be set, got: %v", info.RuntimeHeadersOverride["x-injected-by-param-override"])
	}
	if _, exists := info.RuntimeHeadersOverride["x-delete-me"]; exists {
		t.Fatalf("expected x-delete-me header to be deleted")
	}
}

func TestApplyParamOverrideWithRelayInfoMixedLegacyAndOperations(t *testing.T) {
	info := &RelayInfo{
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
		},
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"temperature": 0.2,
				"operations": []interface{}{
					map[string]interface{}{
						"mode":  "pass_headers",
						"value": []interface{}{"Originator"},
					},
				},
			},
			HeadersOverride: map[string]interface{}{
				"X-Static": "legacy-static",
			},
		},
	}

	out, err := ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-5","temperature":0.7}`), info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}
	assertJSONEqual(t, `{"model":"gpt-5","temperature":0.2}`, string(out))

	if !info.UseRuntimeHeadersOverride {
		t.Fatalf("expected runtime header override to be enabled")
	}
	if info.RuntimeHeadersOverride["x-static"] != "legacy-static" {
		t.Fatalf("expected x-static to be preserved, got: %v", info.RuntimeHeadersOverride["x-static"])
	}
	if info.RuntimeHeadersOverride["originator"] != "Codex CLI" {
		t.Fatalf("expected originator header to be passed, got: %v", info.RuntimeHeadersOverride["originator"])
	}
}

func TestApplySubscriptionOAuthHeaderPassthroughRejectsBodyMutation(t *testing.T) {
	info := &RelayInfo{ChannelMeta: &ChannelMeta{ParamOverride: map[string]interface{}{
		"operations": []interface{}{map[string]interface{}{
			"mode":  "set",
			"path":  "model",
			"value": "replacement",
		}},
	}}}

	_, err := ApplySubscriptionOAuthHeaderPassthrough([]byte(`{"model":"original"}`), info)

	require.Error(t, err)
	require.Contains(t, err.Error(), "do not allow parameter override mode")
}

func TestApplySubscriptionOAuthHeaderPassthroughPreservesBody(t *testing.T) {
	input := []byte(`{"model":"gpt-5","input":"hello"}`)
	info := &RelayInfo{
		RequestHeaders: map[string]string{"Session_id": "session-123"},
		ChannelMeta: &ChannelMeta{ParamOverride: map[string]interface{}{
			"operations": []interface{}{map[string]interface{}{
				"mode":  "pass_headers",
				"value": []interface{}{"Session_id"},
			}},
		}},
	}

	out, err := ApplySubscriptionOAuthHeaderPassthrough(input, info)

	require.NoError(t, err)
	require.JSONEq(t, string(input), string(out))
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "session-123", info.RuntimeHeadersOverride["session_id"])
}

func TestApplyParamOverrideWithRelayInfoMoveAndCopyHeaders(t *testing.T) {
	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode": "move_header",
						"from": "X-Legacy-Trace",
						"to":   "X-Trace",
					},
					map[string]interface{}{
						"mode": "copy_header",
						"from": "X-Trace",
						"to":   "X-Trace-Backup",
					},
				},
			},
			HeadersOverride: map[string]interface{}{
				"X-Legacy-Trace": "trace-123",
			},
		},
	}

	input := []byte(`{"temperature":0.7}`)
	_, err := ApplyParamOverrideWithRelayInfo(input, info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}
	if _, exists := info.RuntimeHeadersOverride["x-legacy-trace"]; exists {
		t.Fatalf("expected source header to be removed after move")
	}
	if info.RuntimeHeadersOverride["x-trace"] != "trace-123" {
		t.Fatalf("expected x-trace to be set, got: %v", info.RuntimeHeadersOverride["x-trace"])
	}
	if info.RuntimeHeadersOverride["x-trace-backup"] != "trace-123" {
		t.Fatalf("expected x-trace-backup to be copied, got: %v", info.RuntimeHeadersOverride["x-trace-backup"])
	}
}

func TestApplyParamOverrideWithRelayInfoSetHeaderMapRewritesAnthropicBeta(t *testing.T) {
	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode": "set_header",
						"path": "anthropic-beta",
						"value": map[string]interface{}{
							"advanced-tool-use-2025-11-20": nil,
							"computer-use-2025-01-24":      "computer-use-2025-01-24",
						},
					},
				},
			},
			HeadersOverride: map[string]interface{}{
				"anthropic-beta": "advanced-tool-use-2025-11-20, computer-use-2025-01-24",
			},
		},
	}

	_, err := ApplyParamOverrideWithRelayInfo([]byte(`{"temperature":0.7}`), info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}

	if !info.UseRuntimeHeadersOverride {
		t.Fatalf("expected runtime header override to be enabled")
	}
	if info.RuntimeHeadersOverride["anthropic-beta"] != "computer-use-2025-01-24" {
		t.Fatalf("expected anthropic-beta to be rewritten, got: %v", info.RuntimeHeadersOverride["anthropic-beta"])
	}
}

func TestGetEffectiveHeaderOverrideUsesRuntimeOverrideAsFinalResult(t *testing.T) {
	info := &RelayInfo{
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]interface{}{
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &ChannelMeta{
			HeadersOverride: map[string]interface{}{
				"X-Static":  "static-value",
				"X-Deleted": "should-not-exist",
			},
		},
	}

	effective := GetEffectiveHeaderOverride(info)
	if effective["x-runtime"] != "runtime-only" {
		t.Fatalf("expected x-runtime from runtime override, got: %v", effective["x-runtime"])
	}
	if _, exists := effective["x-static"]; exists {
		t.Fatalf("expected runtime override to be final and not merge channel headers")
	}
}

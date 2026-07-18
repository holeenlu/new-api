package common

import (
	"reflect"
	"testing"

	common2 "github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestApplyParamOverrideWithRelayInfoRecordsOperationAuditInDebugMode(t *testing.T) {
	originalDebugEnabled := common2.DebugEnabled
	common2.DebugEnabled = true
	t.Cleanup(func() {
		common2.DebugEnabled = originalDebugEnabled
	})

	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode": "copy",
						"from": "metadata.target_model",
						"to":   "model",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "service_tier",
						"value": "flex",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "temperature",
						"value": 0.1,
					},
				},
			},
		},
	}

	out, err := ApplyParamOverrideWithRelayInfo([]byte(`{
		"model":"gpt-4.1",
		"temperature":0.7,
		"metadata":{"target_model":"gpt-4.1-mini"}
	}`), info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}
	assertJSONEqual(t, `{
		"model":"gpt-4.1-mini",
		"temperature":0.1,
		"service_tier":"flex",
		"metadata":{"target_model":"gpt-4.1-mini"}
	}`, string(out))

	expected := []string{
		"copy metadata.target_model -> model",
		"set service_tier = flex",
		"set temperature = 0.1",
	}
	if !reflect.DeepEqual(info.ParamOverrideAudit, expected) {
		t.Fatalf("unexpected param override audit, got %#v", info.ParamOverrideAudit)
	}
}

func TestApplyParamOverrideWithRelayInfoRecordsOnlyKeyOperationsWhenDebugDisabled(t *testing.T) {
	originalDebugEnabled := common2.DebugEnabled
	common2.DebugEnabled = false
	t.Cleanup(func() {
		common2.DebugEnabled = originalDebugEnabled
	})

	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode": "copy",
						"from": "metadata.target_model",
						"to":   "model",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "temperature",
						"value": 0.1,
					},
				},
			},
		},
	}

	_, err := ApplyParamOverrideWithRelayInfo([]byte(`{
		"model":"gpt-4.1",
		"temperature":0.7,
		"metadata":{"target_model":"gpt-4.1-mini"}
	}`), info)
	if err != nil {
		t.Fatalf("ApplyParamOverrideWithRelayInfo returned error: %v", err)
	}

	expected := []string{
		"copy metadata.target_model -> model",
	}
	if !reflect.DeepEqual(info.ParamOverrideAudit, expected) {
		t.Fatalf("unexpected param override audit, got %#v", info.ParamOverrideAudit)
	}
}

func TestApplyParamOverrideWithRelayInfoRecordsConversationBodyOperationsWhenDebugDisabled(t *testing.T) {
	originalDebugEnabled := common2.DebugEnabled
	common2.DebugEnabled = false
	t.Cleanup(func() {
		common2.DebugEnabled = originalDebugEnabled
	})

	info := &RelayInfo{
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{
					map[string]interface{}{
						"mode": "replace",
						"path": "messages.0.content",
						"from": "hello",
						"to":   "hi",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "input.0.content.0.text",
						"value": "rewritten response input",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "instructions",
						"value": "new instruction",
					},
					map[string]interface{}{
						"mode":  "append",
						"path":  "contents.0.parts",
						"value": map[string]interface{}{"text": "new gemini part"},
					},
					map[string]interface{}{
						"mode": "copy",
						"from": "system",
						"to":   "metadata.system_copy",
					},
					map[string]interface{}{
						"mode":  "set",
						"path":  "temperature",
						"value": 0.1,
					},
				},
			},
		},
	}

	out, err := ApplyParamOverrideWithRelayInfo([]byte(`{
		"messages":[{"role":"user","content":"hello world"}],
		"input":[{"role":"user","content":[{"type":"input_text","text":"original response input"}]}],
		"instructions":"old instruction",
		"system":"old system",
		"contents":[{"role":"user","parts":[{"text":"hello gemini"}]}],
		"temperature":0.7
	}`), info)
	require.NoError(t, err)
	assertJSONEqual(t, `{
		"messages":[{"role":"user","content":"hi world"}],
		"input":[{"role":"user","content":[{"type":"input_text","text":"rewritten response input"}]}],
		"instructions":"new instruction",
		"system":"old system",
		"contents":[{"role":"user","parts":[{"text":"hello gemini"},{"text":"new gemini part"}]}],
		"temperature":0.1,
		"metadata":{"system_copy":"old system"}
	}`, string(out))

	require.Equal(t, []string{
		"replace messages.0.content from hello to hi",
		"set input.0.content.0.text = rewritten response input",
		"set instructions = new instruction",
		"append contents.0.parts with {\"text\":\"new gemini part\"}",
		"copy system -> metadata.system_copy",
	}, info.ParamOverrideAudit)
}

func TestShouldAuditParamPathUsesFieldBoundaryPrefixMatching(t *testing.T) {
	originalDebugEnabled := common2.DebugEnabled
	common2.DebugEnabled = false
	t.Cleanup(func() {
		common2.DebugEnabled = originalDebugEnabled
	})

	require.True(t, shouldAuditParamPath("messages"))
	require.True(t, shouldAuditParamPath("messages.0.content"))
	require.True(t, shouldAuditParamPath("systemInstruction.parts.0.text"))
	require.False(t, shouldAuditParamPath("model_name"))
	require.False(t, shouldAuditParamPath("message"))
}

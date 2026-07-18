package common

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/require"
)

func TestRemoveDisabledFieldsSkipWhenChannelPassThroughEnabled(t *testing.T) {
	input := `{
		"service_tier":"flex",
		"safety_identifier":"user-123",
		"store":true,
		"stream_options":{"include_obfuscation":false}
	}`
	settings := dto.ChannelOtherSettings{}

	out, err := RemoveDisabledFields([]byte(input), settings, true)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	assertJSONEqual(t, input, string(out))
}

func TestRemoveDisabledFieldsSkipWhenGlobalPassThroughEnabled(t *testing.T) {
	original := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	model_setting.GetGlobalSettings().PassThroughRequestEnabled = true
	t.Cleanup(func() {
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = original
	})

	input := `{
		"service_tier":"flex",
		"safety_identifier":"user-123",
		"stream_options":{"include_obfuscation":false}
	}`
	settings := dto.ChannelOtherSettings{}

	out, err := RemoveDisabledFields([]byte(input), settings, false)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	assertJSONEqual(t, input, string(out))
}

func TestRemoveDisabledFieldsForSubscriptionOAuthIgnoresGlobalPassThrough(t *testing.T) {
	original := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	model_setting.GetGlobalSettings().PassThroughRequestEnabled = true
	t.Cleanup(func() {
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = original
	})

	out, err := RemoveDisabledFieldsForSubscriptionOAuth(
		[]byte(`{"model":"gpt-5","service_tier":"flex","safety_identifier":"user-123"}`),
		dto.ChannelOtherSettings{},
	)

	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-5"}`, string(out))
}

func TestRemoveDisabledFieldsDefaultFiltering(t *testing.T) {
	input := `{
		"service_tier":"flex",
		"inference_geo":"eu",
		"speed":"fast",
		"cache_control":{"type":"ephemeral"},
		"safety_identifier":"user-123",
		"store":true,
		"stream_options":{"include_obfuscation":false}
	}`
	settings := dto.ChannelOtherSettings{}

	out, err := RemoveDisabledFields([]byte(input), settings, false)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	assertJSONEqual(t, `{"cache_control":{"type":"ephemeral"},"store":true}`, string(out))
}

func TestRemoveDisabledFieldsNoControlledFieldsKeepsBody(t *testing.T) {
	input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	settings := dto.ChannelOtherSettings{}

	out, err := RemoveDisabledFields([]byte(input), settings, false)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	require.Equal(t, input, string(out))
}

func TestRemoveDisabledFieldsAllowInferenceGeo(t *testing.T) {
	input := `{
		"inference_geo":"eu",
		"store":true
	}`
	settings := dto.ChannelOtherSettings{
		AllowInferenceGeo: true,
	}

	out, err := RemoveDisabledFields([]byte(input), settings, false)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	assertJSONEqual(t, `{"inference_geo":"eu","store":true}`, string(out))
}

func TestRemoveDisabledFieldsAllowSpeed(t *testing.T) {
	input := `{
		"speed":"fast",
		"store":true
	}`
	settings := dto.ChannelOtherSettings{
		AllowSpeed: true,
	}

	out, err := RemoveDisabledFields([]byte(input), settings, false)
	if err != nil {
		t.Fatalf("RemoveDisabledFields returned error: %v", err)
	}
	assertJSONEqual(t, `{"speed":"fast","store":true}`, string(out))
}

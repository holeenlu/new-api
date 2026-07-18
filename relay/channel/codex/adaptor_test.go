package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexChannelTestMetadataIsStablePerRequestAndUniqueAcrossRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	firstContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	firstContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	secondContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	secondContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		IsStream:      true,
		RelayMode:     relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-sol",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(firstContext, info, dto.OpenAIResponsesRequest{})
	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	var clientMetadata map[string]any
	require.NoError(t, common.Unmarshal(request.ClientMetadata, &clientMetadata))

	headers := make(http.Header)
	require.NoError(t, (&Adaptor{}).SetupRequestHeader(firstContext, &headers, info))
	require.Equal(t, clientMetadata["session_id"], headers.Get("session-id"))
	require.Equal(t, clientMetadata["thread_id"], headers.Get("thread-id"))
	require.Equal(t, clientMetadata["x-codex-window-id"], headers.Get("x-codex-window-id"))

	firstMetadata, err := getOrCreateCodexResponsesMetadata(firstContext)
	require.NoError(t, err)
	secondMetadata, err := getOrCreateCodexResponsesMetadata(secondContext)
	require.NoError(t, err)
	require.NotEqual(t, firstMetadata.SessionID, secondMetadata.SessionID)
	require.NotEqual(t, firstMetadata.TurnID, secondMetadata.TurnID)
	require.NotEqual(t, firstMetadata.InstallationID, secondMetadata.InstallationID)
}

func TestCodexResponsesLiteProductionRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "session-123")
	c.Request.Header.Set("Thread-Id", "thread-456")
	c.Request.Header.Set("X-Codex-Window-Id", "window-789")

	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-terra",
		},
	}
	request := dto.OpenAIResponsesRequest{
		Reasoning:      &dto.Reasoning{Effort: "high", Summary: "auto"},
		Include:        json.RawMessage(`["message.output_text.logprobs"]`),
		Tools:          json.RawMessage(`[{"type":"function","name":"lookup"},{"type":"custom","name":"shell"},{"type":"tool_search","execution":"client"}]`),
		ClientMetadata: json.RawMessage(`{"device_uuid":"client-device","country":"CN"}`),
	}

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, info, request)
	require.NoError(t, err)
	got := converted.(dto.OpenAIResponsesRequest)
	require.Equal(t, "high", got.Reasoning.Effort)
	require.Equal(t, "auto", got.Reasoning.Summary)
	require.JSONEq(t, `"all_turns"`, string(got.Reasoning.Context))
	require.JSONEq(t, `false`, string(got.ParallelToolCalls))
	require.JSONEq(t, `false`, string(got.Store))
	require.NotNil(t, got.Stream)
	require.True(t, *got.Stream)
	require.Contains(t, string(got.Include), "reasoning.encrypted_content")
	require.Contains(t, string(got.Include), "message.output_text.logprobs")

	var clientMetadata map[string]any
	require.NoError(t, common.Unmarshal(got.ClientMetadata, &clientMetadata))
	require.NotEqual(t, "session-123", clientMetadata["session_id"])
	require.NotEqual(t, "thread-456", clientMetadata["thread_id"])
	require.NotEqual(t, "window-789", clientMetadata["x-codex-window-id"])
	require.NotContains(t, clientMetadata, "device_uuid")
	require.NotContains(t, clientMetadata, "country")

	headers := make(http.Header)
	require.NoError(t, (&Adaptor{}).SetupRequestHeader(c, &headers, info))
	require.Equal(t, "true", headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Equal(t, "remote_compaction_v2", headers.Get("X-Codex-Beta-Features"))
	require.Equal(t, clientMetadata["session_id"], headers.Get("Session-Id"))
	require.Equal(t, clientMetadata["thread_id"], headers.Get("Thread-Id"))
}

func TestCodexResponsesLiteRejectsNonStreamRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.6-luna",
		},
	}, dto.OpenAIResponsesRequest{})
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	require.Contains(t, apiErr.Error(), "stream=true")
}

func TestCodexResponsesLiteFiltersHostedToolsAndKeepsClientTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-terra",
		},
	}
	tools := json.RawMessage(`[{"type":"function","name":"lookup"},{"type":"custom","name":"shell"},{"type":"tool_search","execution":"client"},{"type":"namespace","name":"collaboration"},{"type":"web_search"},{"type":"image_generation"}]`)
	input := json.RawMessage(`[{"type":"additional_tools","tools":[{"type":"function","name":"keep"},{"type":"web_search"}]},{"role":"user","content":"hello"}]`)

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, info, dto.OpenAIResponsesRequest{
		Tools:      tools,
		Input:      input,
		ToolChoice: json.RawMessage(`{"type":"image_generation"}`),
	})
	require.NoError(t, err)
	got := converted.(dto.OpenAIResponsesRequest)
	require.JSONEq(t, `[{"type":"function","name":"lookup"},{"type":"custom","name":"shell"},{"type":"tool_search","execution":"client"}]`, string(got.Tools))
	require.JSONEq(t, `[{"type":"additional_tools","tools":[{"type":"function","name":"keep"}]},{"role":"user","content":"hello"}]`, string(got.Input))
	require.JSONEq(t, `"auto"`, string(got.ToolChoice))
	require.NotNil(t, got.Reasoning)
	require.NotEmpty(t, got.ClientMetadata)

	headers := make(http.Header)
	require.NoError(t, (&Adaptor{}).SetupRequestHeader(c, &headers, info))
	require.Equal(t, "true", headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Equal(t, "Codex Desktop", headers.Get("Originator"))
}

func TestCodexResponsesLiteIsNotEnabledForOtherModelsOrCompact(t *testing.T) {
	for _, info := range []*relaycommon.RelayInfo{
		{
			IsStream:  true,
			RelayMode: relayconstant.RelayModeResponses,
			ChannelMeta: &relaycommon.ChannelMeta{
				UpstreamModelName: "gpt-5.5",
			},
		},
		{
			IsStream:  true,
			RelayMode: relayconstant.RelayModeResponsesCompact,
			ChannelMeta: &relaycommon.ChannelMeta{
				UpstreamModelName: "gpt-5.6-sol",
			},
		},
	} {
		require.False(t, shouldUseCodexResponsesLite(info))
	}
}

func TestSetupRequestHeaderDoesNotForwardProtectedCodexClientHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	c.Request.Header.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	c.Request.Header.Set("Session-Id", "session-123")
	c.Request.Header.Set("User-Agent", "untrusted-client")
	c.Request.Header.Set("Originator", "untrusted-originator")

	headers := make(http.Header)
	err := (&Adaptor{}).SetupRequestHeader(c, &headers, &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey: `{"access_token":"access-token","account_id":"account-id"}`,
		},
	})

	require.NoError(t, err)
	require.Empty(t, headers.Get("X-Codex-Beta-Features"))
	require.Empty(t, headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Empty(t, headers.Get("Session-Id"))
	require.Equal(t, "Bearer access-token", headers.Get("Authorization"))
	require.Equal(t, "account-id", headers.Get("Chatgpt-Account-Id"))
	require.Equal(t, CodexOAuthUserAgent, headers.Get("User-Agent"))
	require.Equal(t, CodexOAuthOriginator, headers.Get("Originator"))
}

func TestCodexStandardRequestDropsClientMetadata(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	stream := true
	request := dto.OpenAIResponsesRequest{
		Model:          "gpt-5.4",
		Stream:         &stream,
		ClientMetadata: json.RawMessage(`{"device_uuid":"client-device","session_id":"client-session"}`),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeCodex}}

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, info, request)
	require.NoError(t, err)
	got := converted.(dto.OpenAIResponsesRequest)
	require.Empty(t, got.ClientMetadata)
}

func TestCodexOAuthPacingHonorsContextCancellation(t *testing.T) {
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 910003, 0, `{"account_id":"pacing-account"}`)
	lease, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, time.Hour)
	require.NoError(t, err)
	lease.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.AcquireSubscriptionOAuthCapacity(ctx, fingerprint, 10, time.Hour)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCodexOAuthConcurrencySlots(t *testing.T) {
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 910001, 0, `{"account_id":"concurrency-account"}`)
	first, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	second, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	_, err = service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.Error(t, err)
	first.Release()
	third, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	second.Release()
	third.Release()
}

func TestCodexOAuthRuntimeKeyUsesAccountIdentity(t *testing.T) {
	first := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"access_token":"token-a","account_id":"shared-account"}`)
	second := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 2, 0, `{"access_token":"token-b","account_id":"shared-account"}`)
	different := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 3, 0, `{"access_token":"token-c","account_id":"other-account"}`)

	require.Equal(t, first, second)
	require.NotEqual(t, first, different)
}

func TestInitOAuthRuntimeSettingsReadsLoadedEnvironment(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	originalInterval := CodexOAuthMinRequestInterval
	t.Cleanup(func() {
		CodexOAuthMaxConcurrency = originalMaxConcurrency
		CodexOAuthMinRequestInterval = originalInterval
	})
	t.Setenv("CODEX_OAUTH_MAX_CONCURRENCY", "7")
	t.Setenv("CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS", "125")

	InitOAuthRuntimeSettings()

	require.Equal(t, 7, CodexOAuthMaxConcurrency)
	require.Equal(t, 125*time.Millisecond, CodexOAuthMinRequestInterval)
}

func TestCodexLocalConcurrencyLimitRemainsRetryable(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 1
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 910004
	key := `{"access_token":"token","account_id":"limited-account"}`
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, channelID, 0, key)
	lease, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 1, 0)
	require.NoError(t, err)
	defer lease.Release()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err = (&Adaptor{}).DoRequest(c, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelID, ChannelType: constant.ChannelTypeCodex, ApiKey: key},
	}, http.NoBody)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeOAuthChannelConcurrencyLimit, apiErr.GetErrorCode())
	require.False(t, types.IsSkipRetryError(apiErr))
	require.False(t, types.IsRecordErrorLog(apiErr))
	require.Equal(t, "1", recorder.Header().Get("Retry-After"))
}

func TestCodexOAuthResponseBodyReleasesSlotOnce(t *testing.T) {
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 910002, 0, `{"account_id":"response-body-account"}`)
	lease, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	body := &codexOAuthResponseBody{
		ReadCloser: io.NopCloser(strings.NewReader("ok")),
		lease:      lease,
	}

	require.NoError(t, body.Close())
	require.NoError(t, body.Close())

	first, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	second, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.NoError(t, err)
	_, err = service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 2, 0)
	require.Error(t, err)
	first.Release()
	second.Release()
}

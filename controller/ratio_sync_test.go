package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportsUpstreamPricingSyncExcludesSubscriptionOAuthChannels(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		supported   bool
	}{
		{name: "standard channel", channelType: constant.ChannelTypeOpenAI, supported: true},
		{name: "codex", channelType: constant.ChannelTypeCodex, supported: false},
		{name: "claude code", channelType: constant.ChannelTypeClaudeCode, supported: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.supported, supportsUpstreamPricingSync(test.channelType))
		})
	}
}

func TestGetSyncableChannelsExcludesSubscriptionOAuthChannels(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeOpenAI,
		Key:     "standard-key",
		Name:    "standard",
		BaseURL: common.GetPointer("https://standard.example.com"),
		Status:  common.ChannelStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&model.Channel{
		Id:      2,
		Type:    constant.ChannelTypeCodex,
		Key:     `{"access_token":"token","account_id":"account"}`,
		Name:    "codex",
		BaseURL: common.GetPointer("https://codex.example.com"),
		Status:  common.ChannelStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&model.Channel{
		Id:      3,
		Type:    constant.ChannelTypeClaudeCode,
		Key:     "CLAUDE_CODE_OAUTH_TOKEN=token",
		Name:    "claude-code",
		BaseURL: common.GetPointer("https://claude.example.com"),
		Status:  common.ChannelStatusEnabled,
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/ratio_sync/channels", nil)

	GetSyncableChannels(ctx)

	var response struct {
		Success bool                  `json:"success"`
		Data    []dto.SyncableChannel `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Contains(t, response.Data, dto.SyncableChannel{
		ID:      1,
		Name:    "standard",
		BaseURL: "https://standard.example.com",
		Status:  common.ChannelStatusEnabled,
		Type:    constant.ChannelTypeOpenAI,
	})
	for _, channel := range response.Data {
		assert.NotEqual(t, constant.ChannelTypeCodex, channel.Type)
		assert.NotEqual(t, constant.ChannelTypeClaudeCode, channel.Type)
	}
}

func TestGetSyncableChannelsIncludesOfficialAPIPricingSources(t *testing.T) {
	setupModelListControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/ratio_sync/channels", nil)

	GetSyncableChannels(ctx)

	var response struct {
		Success bool                  `json:"success"`
		Data    []dto.SyncableChannel `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Contains(t, response.Data, dto.SyncableChannel{
		ID:      officialOpenAIPresetID,
		Name:    officialOpenAIPresetName,
		BaseURL: officialOpenAIPresetBaseURL,
		Status:  common.ChannelStatusEnabled,
	})
	assert.Contains(t, response.Data, dto.SyncableChannel{
		ID:      officialAnthropicPresetID,
		Name:    officialAnthropicPresetName,
		BaseURL: officialAnthropicPresetBaseURL,
		Status:  common.ChannelStatusEnabled,
	})
}

func TestOfficialAPIPricingCatalogConvertsPublicTokenPrices(t *testing.T) {
	openAIData, ok := officialAPIPricingRatioData(officialOpenAIPresetID)
	require.True(t, ok)
	assert.Equal(t, 0.625, openAIData["model_ratio"].(map[string]any)["gpt-5"])
	assert.Equal(t, 8.0, openAIData["completion_ratio"].(map[string]any)["gpt-5"])
	assert.Equal(t, 0.1, openAIData["cache_ratio"].(map[string]any)["gpt-5"])

	anthropicData, ok := officialAPIPricingRatioData(officialAnthropicPresetID)
	require.True(t, ok)
	assert.Equal(t, 1.5, anthropicData["model_ratio"].(map[string]any)["claude-sonnet-4"])
	assert.Equal(t, 5.0, anthropicData["completion_ratio"].(map[string]any)["claude-sonnet-4"])
	assert.Equal(t, 1.25, anthropicData["create_cache_ratio"].(map[string]any)["claude-sonnet-4"])

	_, ok = officialAPIPricingRatioData(-999)
	assert.False(t, ok)
}

func TestFetchUpstreamRatiosUsesOfficialAPIPricingCatalogWithoutHTTP(t *testing.T) {
	setupModelListControllerTestDB(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/ratio_sync/fetch", http.NoBody)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Body = io.NopCloser(strings.NewReader(`{"upstreams":[{"id":-102,"name":"untrusted client name","base_url":"` + server.URL + `","endpoint":"custom"}]}`))

	FetchUpstreamRatios(ctx)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			TestResults []dto.TestResult `json:"test_results"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Len(t, response.Data.TestResults, 1)
	assert.Equal(t, "success", response.Data.TestResults[0].Status)
	assert.Equal(t, officialOpenAIPresetName+"(-102)", response.Data.TestResults[0].Name)
	assert.Zero(t, requests.Load())
}

func TestFetchUpstreamRatiosDoesNotProbeSubscriptionOAuthChannel(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	require.NoError(t, db.Create(&model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCodex,
		Key:     `{"access_token":"token","account_id":"account"}`,
		Name:    "codex",
		BaseURL: common.GetPointer(server.URL),
		Status:  common.ChannelStatusEnabled,
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/ratio_sync/fetch", http.NoBody)
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Body = io.NopCloser(strings.NewReader(`{"upstreams":[{"id":1,"name":"codex","base_url":"` + server.URL + `","endpoint":"/api/pricing"}]}`))

	FetchUpstreamRatios(ctx)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			TestResults []dto.TestResult `json:"test_results"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Len(t, response.Data.TestResults, 1)
	assert.Equal(t, "error", response.Data.TestResults[0].Status)
	assert.Contains(t, response.Data.TestResults[0].Error, "不支持上游价格同步")
	assert.Zero(t, requests.Load())
}

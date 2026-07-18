package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFetchModelsUsesSharedChannelFetchBehavior(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "first-key" {
			t.Errorf("unexpected x-api-key header: %s", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":" claude-sonnet "},{"id":"claude-sonnet"}]}`))
	}))
	t.Cleanup(server.Close)

	body, err := common.Marshal(map[string]any{
		"base_url": server.URL,
		"type":     constant.ChannelTypeAnthropic,
		"key":      "first-key\nsecond-key",
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", strings.NewReader(string(body)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	FetchModels(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"success":true,"message":"","data":["claude-sonnet"]}`, recorder.Body.String())
}

func TestNormalizeModelNames(t *testing.T) {
	result := normalizeModelNames([]string{
		" gpt-4o ",
		"",
		"gpt-4o",
		"gpt-4.1",
		"   ",
	})

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestGetResponseBodyWithContextHonorsCancellation(t *testing.T) {
	service.InitHttpClient()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := GetResponseBodyWithContext(ctx, http.MethodGet, server.URL, &model.Channel{}, http.Header{}, 0)

	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, requests.Load())
}

func TestGetResponseBodyWithContextPreservesUpstreamError(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"organization disabled"}}`))
	}))
	t.Cleanup(server.Close)

	_, err := GetResponseBodyWithContext(context.Background(), http.MethodGet, server.URL, &model.Channel{}, http.Header{}, 0)

	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	require.Equal(t, http.StatusForbidden, apiError.GetUpstreamStatusCode())
	require.Equal(t, 7*time.Second, apiError.RetryAfter)
}

func TestGetResponseBodyWithContextRejectsOversizedSuccessBody(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxChannelManagementResponseBytes+1)))
	}))
	t.Cleanup(server.Close)

	_, err := GetResponseBodyWithContext(
		context.Background(),
		http.MethodGet,
		server.URL,
		&model.Channel{},
		http.Header{},
		0,
	)

	require.ErrorContains(t, err, "management response exceeds")
}

func TestOpenAIModelUpstreamMetadataReadsAnthropicCapabilities(t *testing.T) {
	metadata := OpenAIModel{
		ID: "claude-sonnet-5",
	}
	metadata.Capabilities.MaxInputTokens = 1_000_000

	profile := metadata.UpstreamMetadata()

	require.True(t, profile.Valid())
	require.Equal(t, 1_000_000, profile.ContextWindow)
	require.Equal(t, 1_000_000, profile.MaxContextWindow)
}

func TestSubscriptionOAuthModelFetchErrorQuarantinesCredential(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	key := `{"access_token":"token","account_id":"model-update-disabled"}`
	channel := &model.Channel{
		Type: constant.ChannelTypeCodex, Name: "codex", Key: key,
		Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test",
	}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: "gpt-test", ChannelId: channel.Id, Enabled: true,
	}).Error)
	upstreamError := types.NewErrorWithStatusCode(
		errors.New("OAuth credential is invalid or expired"),
		types.ErrorCodeOAuthUnauthorized,
		http.StatusUnauthorized,
	)
	upstreamError.UpstreamStatusCode = http.StatusUnauthorized

	got := applySubscriptionOAuthModelFetchError(channel, key, upstreamError)

	var gotAPIError *types.NewAPIError
	require.ErrorAs(t, got, &gotAPIError)
	require.Equal(t, types.ErrorCodeOAuthUnauthorized, gotAPIError.GetErrorCode())
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	require.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, 0, key)
	_, capacityErr := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.True(t, service.IsSubscriptionOAuthCapacityError(capacityErr))
}

func TestCodexModelCatalogRejectsPartialCredentialScan(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good-token" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.4","context_window":1000000}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure"}}`))
	}))
	t.Cleanup(server.Close)

	channel := &model.Channel{
		Id:      980010,
		Type:    constant.ChannelTypeCodex,
		BaseURL: &server.URL,
		Keys: []string{
			`{"access_token":"good-token","account_id":"good-account"}`,
			`{"access_token":"bad-token","account_id":"bad-account"}`,
		},
		ChannelInfo: model.ChannelInfo{IsMultiKey: true},
	}

	catalog, err := fetchCodexChannelUpstreamModelCatalog(context.Background(), channel)

	require.Error(t, err)
	require.Empty(t, catalog.IDs)
	require.Nil(t, catalog.Metadata)
}

func TestBuildFetchModelsHeadersRejectsInvalidClaudeCodeToken(t *testing.T) {
	headers, err := buildFetchModelsHeaders(&model.Channel{Type: constant.ChannelTypeClaudeCode}, "invalid-token")

	require.Error(t, err)
	require.Nil(t, headers)
}

func TestMergeModelNames(t *testing.T) {
	result := mergeModelNames(
		[]string{"gpt-4o", "gpt-4.1"},
		[]string{"gpt-4.1", " gpt-4.1-mini ", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
}

func TestSubtractModelNames(t *testing.T) {
	result := subtractModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"},
		[]string{"gpt-4.1", "not-exists"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1-mini"}, result)
}

func TestIntersectModelNames(t *testing.T) {
	result := intersectModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1", "not-exists"},
		[]string{"gpt-4.1", "gpt-4o-mini", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestApplySelectedModelChanges(t *testing.T) {
	t.Run("add and remove together", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o", "gpt-4.1", "claude-3"},
			[]string{"gpt-4.1-mini"},
			[]string{"claude-3"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
	})

	t.Run("add wins when conflict with remove", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o"},
			[]string{"gpt-4.1"},
			[]string{"gpt-4.1"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
	})
}

func TestCollectPendingApplyUpstreamModelChanges(t *testing.T) {
	settings := dto.ChannelOtherSettings{
		UpstreamModelUpdateLastDetectedModels: []string{" gpt-4o ", "gpt-4o", "gpt-4.1"},
		UpstreamModelUpdateLastRemovedModels:  []string{" old-model ", "", "old-model"},
	}

	pendingAddModels, pendingRemoveModels := collectPendingApplyUpstreamModelChanges(settings)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, pendingAddModels)
	require.Equal(t, []string{"old-model"}, pendingRemoveModels)
}

func TestModelUpdateScheduleFollowsEnabledChannelSettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)

	disabledChannel := &model.Channel{
		Name:   "disabled-channel",
		Key:    "key",
		Status: common.ChannelStatusManuallyDisabled,
	}
	disabledChannel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: true,
	})
	require.NoError(t, db.Create(disabledChannel).Error)

	enabledChannel := &model.Channel{
		Name:   "enabled-channel",
		Key:    "key",
		Status: common.ChannelStatusEnabled,
	}
	enabledChannel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: false,
	})
	require.NoError(t, db.Create(enabledChannel).Error)
	require.False(t, modelUpdateHandler{}.Enabled())

	enabledChannel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: true,
	})
	require.NoError(t, db.Model(enabledChannel).Update("settings", enabledChannel.OtherSettings).Error)
	require.True(t, modelUpdateHandler{}.Enabled())

	enabledChannel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: false,
	})
	require.NoError(t, db.Model(enabledChannel).Update("settings", enabledChannel.OtherSettings).Error)
	require.False(t, modelUpdateHandler{}.Enabled())
}

func TestModelUpdateScheduleRequiresEnvironmentAndChannelSwitches(t *testing.T) {
	t.Setenv("CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED", "false")
	db := setupModelListControllerTestDB(t)
	channel := &model.Channel{
		Name:   "enabled-channel",
		Key:    "key",
		Status: common.ChannelStatusEnabled,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: true,
	})
	require.NoError(t, db.Create(channel).Error)
	require.False(t, modelUpdateHandler{}.Enabled())
}

func TestNormalizeChannelModelMapping(t *testing.T) {
	modelMapping := `{
		" alias-model ": " upstream-model ",
		"": "invalid",
		"invalid-target": ""
	}`
	channel := &model.Channel{
		ModelMapping: &modelMapping,
	}

	result := normalizeChannelModelMapping(channel)
	require.Equal(t, map[string]string{
		"alias-model": "upstream-model",
	}, result)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithModelMapping(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"alias-model", "gpt-4o", "stale-model"},
		[]string{"gpt-4o", "gpt-4.1", "mapped-target"},
		[]string{"gpt-4.1"},
		map[string]string{
			"alias-model": "mapped-target",
		},
	)

	require.Equal(t, []string{}, pendingAddModels)
	require.Equal(t, []string{"stale-model"}, pendingRemoveModels)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithIgnoredRegexPatterns(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "claude-3-5-sonnet", "sora-video", "gpt-4.1"},
		[]string{"regex:^sora-.*$", "gpt-4.1"},
		nil,
	)

	require.Equal(t, []string{"claude-3-5-sonnet"}, pendingAddModels)
	require.Equal(t, []string{}, pendingRemoveModels)
}

func TestBuildUpstreamModelUpdateTaskNotificationContent_OmitOverflowDetails(t *testing.T) {
	channelSummaries := make([]upstreamModelUpdateChannelSummary, 0, 12)
	for i := 0; i < 12; i++ {
		channelSummaries = append(channelSummaries, upstreamModelUpdateChannelSummary{
			ChannelName: "channel-" + string(rune('A'+i)),
			AddCount:    i + 1,
			RemoveCount: i,
		})
	}

	content := buildUpstreamModelUpdateTaskNotificationContent(
		24,
		12,
		56,
		21,
		9,
		[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		channelSummaries,
		[]string{
			"gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini", "gemini-2.5-pro", "claude-3.7-sonnet",
			"qwen-max", "deepseek-r1", "llama-3.3-70b", "mistral-large", "command-r-plus", "doubao-pro-32k",
			"hunyuan-large",
		},
		[]string{
			"gpt-3.5-turbo", "claude-2.1", "gemini-1.5-pro", "mixtral-8x7b", "qwen-plus", "glm-4",
			"yi-large", "moonshot-v1", "doubao-lite",
		},
	)

	require.Contains(t, content, "其余 4 个渠道已省略")
	require.Contains(t, content, "其余 1 个已省略")
	require.Contains(t, content, "失败渠道 ID（展示 10/12）")
	require.Contains(t, content, "其余 2 个已省略")
}

func TestShouldSendUpstreamModelUpdateNotification(t *testing.T) {
	channelUpstreamModelUpdateNotifyState.Lock()
	channelUpstreamModelUpdateNotifyState.lastNotifiedAt = 0
	channelUpstreamModelUpdateNotifyState.lastChangedChannels = 0
	channelUpstreamModelUpdateNotifyState.lastFailedChannels = 0
	channelUpstreamModelUpdateNotifyState.Unlock()

	baseTime := int64(2000000)

	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime, 6, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 6, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 7, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+7200, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+8000, 0, 3))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+9000, 0, 3))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+10000, 0, 4))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90000, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90001, 0, 0))
}

func TestFetchCodexChannelUpstreamModelCatalogUsesMinimumAcrossCredentials(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Chatgpt-Account-Id") {
		case "account-a":
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","context_window":1000000,"max_context_window":1000000}]}`))
		case "account-b":
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","context_window":800000,"max_context_window":900000}]}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)

	channel := &model.Channel{
		Id:      100,
		Type:    constant.ChannelTypeCodex,
		BaseURL: common.GetPointer(server.URL),
		Key: strings.Join([]string{
			`{"access_token":"token-a","account_id":"account-a"}`,
			`{"access_token":"token-b","account_id":"account-b"}`,
		}, "\n"),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusEnabled,
				1: common.ChannelStatusEnabled,
			},
		},
	}

	catalog, err := fetchCodexChannelUpstreamModelCatalog(context.Background(), channel)

	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6-sol"}, catalog.IDs)
	require.Equal(t, dto.UpstreamModelMetadata{
		ContextWindow:    800_000,
		MaxContextWindow: 900_000,
		Complete:         true,
	}, catalog.Metadata["gpt-5.6-sol"])
}

func TestFetchCodexChannelUpstreamModelCatalogMarksMissingMetadataIncomplete(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Chatgpt-Account-Id") == "account-c" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","context_window":1000000,"max_context_window":1000000}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"}]}`))
	}))
	t.Cleanup(server.Close)

	channel := &model.Channel{
		Id:      101,
		Type:    constant.ChannelTypeCodex,
		BaseURL: common.GetPointer(server.URL),
		Key: strings.Join([]string{
			`{"access_token":"token-c","account_id":"account-c"}`,
			`{"access_token":"token-d","account_id":"account-d"}`,
		}, "\n"),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusEnabled,
				1: common.ChannelStatusEnabled,
			},
		},
	}

	catalog, err := fetchCodexChannelUpstreamModelCatalog(context.Background(), channel)

	require.NoError(t, err)
	require.False(t, catalog.Metadata["gpt-5.6-sol"].Complete)
}

func TestReconcileChannelUpstreamModelMetadataPreservesCacheForUnrelatedSettings(t *testing.T) {
	origin := &model.Channel{Id: 102, Type: constant.ChannelTypeCodex, Key: "original-key"}
	origin.SetOtherSettings(dto.ChannelOtherSettings{
		AllowSpeed: true,
		UpstreamModelMetadata: map[string]dto.UpstreamModelMetadata{
			"gpt-5.6-sol": {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
		},
		UpstreamModelMetadataUpdatedTime: 123,
	})
	incoming := &model.Channel{Id: 102, Type: constant.ChannelTypeCodex}
	incoming.SetOtherSettings(dto.ChannelOtherSettings{AllowSpeed: false})

	reconcileChannelUpstreamModelMetadata(incoming, origin, map[string]any{"settings": incoming.OtherSettings})

	settings := incoming.GetOtherSettings()
	require.False(t, settings.AllowSpeed)
	require.Equal(t, int64(123), settings.UpstreamModelMetadataUpdatedTime)
	require.True(t, settings.UpstreamModelMetadata["gpt-5.6-sol"].Valid())
}

func TestReconcileChannelUpstreamModelMetadataClearsCacheForRoutingIdentityChange(t *testing.T) {
	originMapping := `{"codex":"gpt-5.6-sol"}`
	newMapping := `{"codex":"gpt-5.6-terra"}`
	origin := &model.Channel{Id: 103, Type: constant.ChannelTypeCodex, Key: "original-key", ModelMapping: &originMapping}
	origin.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelMetadata: map[string]dto.UpstreamModelMetadata{
			"gpt-5.6-sol": {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
		},
		UpstreamModelMetadataUpdatedTime: 123,
	})
	incoming := &model.Channel{Id: 103, Type: constant.ChannelTypeCodex, ModelMapping: &newMapping}

	reconcileChannelUpstreamModelMetadata(incoming, origin, map[string]any{"model_mapping": newMapping})

	settings := incoming.GetOtherSettings()
	require.Nil(t, settings.UpstreamModelMetadata)
	require.Zero(t, settings.UpstreamModelMetadataUpdatedTime)
}

func TestOpenAIModelUpstreamMetadataAcceptsContextLength(t *testing.T) {
	metadata := OpenAIModel{ID: "provider-model", ContextLength: 400_000}.UpstreamMetadata()

	require.True(t, metadata.Valid())
	require.Equal(t, 400_000, metadata.ContextWindow)
	require.Equal(t, 400_000, metadata.MaxContextWindow)
}

func TestFailedCodexCatalogRefreshClearsCachedMetadata(t *testing.T) {
	service.InitHttpClient()
	db := setupModelListControllerTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure"}}`))
	}))
	t.Cleanup(server.Close)

	channel := &model.Channel{
		Type:    constant.ChannelTypeCodex,
		Name:    "codex-metadata-refresh",
		Key:     `{"access_token":"token-a","account_id":"account-a"}`,
		Status:  common.ChannelStatusEnabled,
		BaseURL: common.GetPointer(server.URL),
		Models:  "gpt-5.6-sol",
		Group:   "default",
	}
	settings := dto.ChannelOtherSettings{
		UpstreamModelMetadata: map[string]dto.UpstreamModelMetadata{
			"gpt-5.6-sol": {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
		},
		UpstreamModelMetadataUpdatedTime: 123,
	}
	channel.SetOtherSettings(settings)
	require.NoError(t, db.Create(channel).Error)

	_, _, err := checkAndPersistChannelUpstreamModelUpdates(context.Background(), channel, &settings, true, false)

	require.Error(t, err)
	var persisted model.Channel
	require.NoError(t, db.First(&persisted, channel.Id).Error)
	persistedSettings := persisted.GetOtherSettings()
	require.Empty(t, persistedSettings.UpstreamModelMetadata)
	require.Zero(t, persistedSettings.UpstreamModelMetadataUpdatedTime)
}

func TestDetectAllChannelUpstreamModelUpdatesRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeModelUpdate, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/upstream-models/detect-all", nil)

	DetectAllChannelUpstreamModelUpdates(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有模型更新任务正在运行或等待中")
}

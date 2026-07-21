package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/advancedcustom"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/ollama"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/samber/lo"
)

const (
	channelUpstreamModelUpdateTaskDefaultIntervalMinutes  = 30
	channelUpstreamModelUpdateTaskBatchSize               = 100
	channelUpstreamModelUpdateMinCheckIntervalSeconds     = 300
	channelUpstreamModelUpdateNotifySuppressWindowSeconds = 86400
	channelUpstreamModelUpdateNotifyMaxChannelDetails     = 8
	channelUpstreamModelUpdateNotifyMaxModelDetails       = 12
	channelUpstreamModelUpdateNotifyMaxFailedChannelIDs   = 10
)

var channelUpstreamModelUpdateSelectFields = []string{
	"id",
	"name",
	"type",
	"key",
	"status",
	"base_url",
	"models",
	"model_mapping",
	"settings",
	"setting",
	"other",
	"group",
	"priority",
	"weight",
	"tag",
	"channel_info",
	"header_override",
}

var channelUpstreamModelUpdateNotifyState = struct {
	sync.Mutex
	lastNotifiedAt      int64
	lastChangedChannels int
	lastFailedChannels  int
}{}

func hasEnabledChannelUpstreamModelUpdateCheck() bool {
	var channels []model.Channel
	if err := model.DB.
		Select("id", "settings").
		Where("status = ?", common.ChannelStatusEnabled).
		Find(&channels).Error; err != nil {
		common.SysLog(fmt.Sprintf("failed to query upstream model update settings: %v", err))
		return false
	}

	for i := range channels {
		settings := dto.ChannelOtherSettings{}
		if channels[i].OtherSettings == "" {
			continue
		}
		if err := common.UnmarshalJsonStr(channels[i].OtherSettings, &settings); err != nil {
			common.SysLog(fmt.Sprintf("failed to parse upstream model update settings: channel_id=%d error=%v", channels[i].Id, err))
			continue
		}
		if settings.UpstreamModelUpdateCheckEnabled {
			return true
		}
	}
	return false
}

type applyChannelUpstreamModelUpdatesRequest struct {
	ID           int      `json:"id"`
	AddModels    []string `json:"add_models"`
	RemoveModels []string `json:"remove_models"`
	IgnoreModels []string `json:"ignore_models"`
}

type applyAllChannelUpstreamModelUpdatesResult struct {
	ChannelID             int      `json:"channel_id"`
	ChannelName           string   `json:"channel_name"`
	AddedModels           []string `json:"added_models"`
	RemovedModels         []string `json:"removed_models"`
	RemainingModels       []string `json:"remaining_models"`
	RemainingRemoveModels []string `json:"remaining_remove_models"`
}

type detectChannelUpstreamModelUpdatesResult struct {
	ChannelID       int      `json:"channel_id"`
	ChannelName     string   `json:"channel_name"`
	AddModels       []string `json:"add_models"`
	RemoveModels    []string `json:"remove_models"`
	LastCheckTime   int64    `json:"last_check_time"`
	AutoAddedModels int      `json:"auto_added_models"`
}

type upstreamModelUpdateChannelSummary struct {
	ChannelName string
	AddCount    int
	RemoveCount int
}

func normalizeModelNames(models []string) []string {
	return lo.Uniq(lo.FilterMap(models, func(model string, _ int) (string, bool) {
		trimmed := strings.TrimSpace(model)
		return trimmed, trimmed != ""
	}))
}

func mergeModelNames(base []string, appended []string) []string {
	merged := normalizeModelNames(base)
	seen := make(map[string]struct{}, len(merged))
	for _, model := range merged {
		seen[model] = struct{}{}
	}
	for _, model := range normalizeModelNames(appended) {
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		merged = append(merged, model)
	}
	return merged
}

func subtractModelNames(base []string, removed []string) []string {
	removeSet := make(map[string]struct{}, len(removed))
	for _, model := range normalizeModelNames(removed) {
		removeSet[model] = struct{}{}
	}
	return lo.Filter(normalizeModelNames(base), func(model string, _ int) bool {
		_, ok := removeSet[model]
		return !ok
	})
}

func intersectModelNames(base []string, allowed []string) []string {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, model := range normalizeModelNames(allowed) {
		allowedSet[model] = struct{}{}
	}
	return lo.Filter(normalizeModelNames(base), func(model string, _ int) bool {
		_, ok := allowedSet[model]
		return ok
	})
}

func applySelectedModelChanges(originModels []string, addModels []string, removeModels []string) []string {
	// Add wins when the same model appears in both selected lists.
	normalizedAdd := normalizeModelNames(addModels)
	normalizedRemove := subtractModelNames(normalizeModelNames(removeModels), normalizedAdd)
	return subtractModelNames(mergeModelNames(originModels, normalizedAdd), normalizedRemove)
}

func normalizeChannelModelMapping(channel *model.Channel) map[string]string {
	if channel == nil || channel.ModelMapping == nil {
		return nil
	}
	rawMapping := strings.TrimSpace(*channel.ModelMapping)
	if rawMapping == "" || rawMapping == "{}" {
		return nil
	}
	parsed := make(map[string]string)
	if err := common.UnmarshalJsonStr(rawMapping, &parsed); err != nil {
		return nil
	}
	normalized := make(map[string]string, len(parsed))
	for source, target := range parsed {
		normalizedSource := strings.TrimSpace(source)
		normalizedTarget := strings.TrimSpace(target)
		if normalizedSource == "" || normalizedTarget == "" {
			continue
		}
		normalized[normalizedSource] = normalizedTarget
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func collectPendingUpstreamModelChangesFromModels(
	localModels []string,
	upstreamModels []string,
	ignoredModels []string,
	modelMapping map[string]string,
) (pendingAddModels []string, pendingRemoveModels []string) {
	localSet := make(map[string]struct{})
	localModels = normalizeModelNames(localModels)
	upstreamModels = normalizeModelNames(upstreamModels)
	for _, modelName := range localModels {
		localSet[modelName] = struct{}{}
	}
	upstreamSet := make(map[string]struct{}, len(upstreamModels))
	for _, modelName := range upstreamModels {
		upstreamSet[modelName] = struct{}{}
	}

	normalizedIgnoredModels := normalizeModelNames(ignoredModels)

	redirectSourceSet := make(map[string]struct{}, len(modelMapping))
	redirectTargetSet := make(map[string]struct{}, len(modelMapping))
	for source, target := range modelMapping {
		redirectSourceSet[source] = struct{}{}
		redirectTargetSet[target] = struct{}{}
	}

	coveredUpstreamSet := make(map[string]struct{}, len(localSet)+len(redirectTargetSet))
	for modelName := range localSet {
		coveredUpstreamSet[modelName] = struct{}{}
	}
	for modelName := range redirectTargetSet {
		coveredUpstreamSet[modelName] = struct{}{}
	}

	pendingAdd := lo.Filter(upstreamModels, func(modelName string, _ int) bool {
		if _, ok := coveredUpstreamSet[modelName]; ok {
			return false
		}
		if lo.ContainsBy(normalizedIgnoredModels, func(ignoredModel string) bool {
			if regexBody, ok := strings.CutPrefix(ignoredModel, "regex:"); ok {
				matched, err := regexp.MatchString(strings.TrimSpace(regexBody), modelName)
				return err == nil && matched
			}
			return ignoredModel == modelName
		}) {
			return false
		}
		return true
	})
	pendingRemove := lo.Filter(localModels, func(modelName string, _ int) bool {
		// Redirect source models are virtual aliases and should not be removed
		// only because they are absent from upstream model list.
		if _, ok := redirectSourceSet[modelName]; ok {
			return false
		}
		_, ok := upstreamSet[modelName]
		return !ok
	})
	return normalizeModelNames(pendingAdd), normalizeModelNames(pendingRemove)
}

func collectPendingUpstreamModelChanges(ctx context.Context, channel *model.Channel, settings *dto.ChannelOtherSettings) (pendingAddModels []string, pendingRemoveModels []string, err error) {
	catalog, err := fetchChannelUpstreamModelCatalog(ctx, channel)
	if err != nil {
		return nil, nil, err
	}
	if catalog.Metadata != nil {
		settings.UpstreamModelMetadata = catalog.Metadata
		settings.UpstreamModelMetadataUpdatedTime = common.GetTimestamp()
	}
	pendingAddModels, pendingRemoveModels = collectPendingUpstreamModelChangesFromModels(
		channel.GetModels(),
		catalog.IDs,
		settings.UpstreamModelUpdateIgnoredModels,
		normalizeChannelModelMapping(channel),
	)
	return pendingAddModels, pendingRemoveModels, nil
}

func getUpstreamModelUpdateMinCheckIntervalSeconds() int64 {
	interval := int64(common.GetEnvOrDefault(
		"CHANNEL_UPSTREAM_MODEL_UPDATE_MIN_CHECK_INTERVAL_SECONDS",
		channelUpstreamModelUpdateMinCheckIntervalSeconds,
	))
	if interval < 0 {
		return channelUpstreamModelUpdateMinCheckIntervalSeconds
	}
	return interval
}

func fetchCodexUpstreamModelCatalog(
	ctx context.Context,
	baseURL string,
	key string,
	proxyURL string,
) ([]codex.UpstreamModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(common.SubscriptionOAuthResponseHeaderTimeout) * time.Second
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	client, err := service.GetHttpClientWithResponseHeaderTimeout(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	return codex.FetchUpstreamModelCatalog(ctx, client, baseURL, key)
}

type channelUpstreamModelCatalog struct {
	IDs      []string
	Metadata map[string]dto.UpstreamModelMetadata
}

func fetchCodexChannelUpstreamModelCatalog(ctx context.Context, channel *model.Channel) (channelUpstreamModelCatalog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout := time.Duration(common.ChannelManagementRequestTimeout) * time.Second; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	keys := channel.GetKeys()
	if len(keys) == 0 {
		return channelUpstreamModelCatalog{}, errors.New("渠道没有可用密钥")
	}

	credentialCatalogs := make([]map[string]dto.UpstreamModelMetadata, 0, len(keys))
	allModels := make([]string, 0)
	seenModels := make(map[string]struct{})
	seenCredentials := make(map[string]struct{})
	var firstErr error

	for keyIndex := range keys {
		key, apiErr := channel.GetEnabledKeyAt(keyIndex)
		if apiErr != nil {
			continue
		}
		fingerprint := service.SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, keyIndex, key)
		if _, exists := seenCredentials[fingerprint]; exists {
			continue
		}
		seenCredentials[fingerprint] = struct{}{}

		upstreamCatalog, err := fetchCodexUpstreamModelCatalog(
			ctx,
			channel.GetBaseURL(),
			key,
			channel.GetSetting().Proxy,
		)
		if err != nil {
			err = applySubscriptionOAuthModelFetchError(channel, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		metadataByModel := make(map[string]dto.UpstreamModelMetadata, len(upstreamCatalog))
		for _, item := range upstreamCatalog {
			name := item.Name()
			if name == "" {
				continue
			}
			metadataByModel[name] = item.Metadata()
			if _, exists := seenModels[name]; !exists {
				seenModels[name] = struct{}{}
				allModels = append(allModels, name)
			}
		}
		credentialCatalogs = append(credentialCatalogs, metadataByModel)
	}

	// A partial credential scan is not a trustworthy model catalog. Returning
	// it could make the UI or patrol task mistake an unreachable account for an
	// upstream model deletion.
	if firstErr != nil {
		return channelUpstreamModelCatalog{}, firstErr
	}
	if len(credentialCatalogs) == 0 {
		return channelUpstreamModelCatalog{}, errors.New("渠道没有已启用的 OAuth 凭证")
	}

	metadata := make(map[string]dto.UpstreamModelMetadata, len(allModels))
	for _, name := range allModels {
		profile := dto.UpstreamModelMetadata{Complete: true}
		for _, credentialCatalog := range credentialCatalogs {
			credentialProfile, exists := credentialCatalog[name]
			if !exists || !credentialProfile.Complete {
				profile.Complete = false
				continue
			}
			if profile.ContextWindow == 0 || credentialProfile.ContextWindow < profile.ContextWindow {
				profile.ContextWindow = credentialProfile.ContextWindow
			}
			if profile.MaxContextWindow == 0 || credentialProfile.MaxContextWindow < profile.MaxContextWindow {
				profile.MaxContextWindow = credentialProfile.MaxContextWindow
			}
		}
		if profile.ContextWindow == 0 || profile.MaxContextWindow < profile.ContextWindow {
			profile.Complete = false
		}
		metadata[name] = profile
	}

	return channelUpstreamModelCatalog{
		IDs:      normalizeModelNames(allModels),
		Metadata: metadata,
	}, nil
}

func fetchChannelUpstreamModelCatalog(ctx context.Context, channel *model.Channel) (channelUpstreamModelCatalog, error) {
	if channel == nil {
		return channelUpstreamModelCatalog{}, errors.New("channel is nil")
	}
	if channel.Type == constant.ChannelTypeCodex {
		return fetchCodexChannelUpstreamModelCatalog(ctx, channel)
	}
	return fetchNonCodexUpstreamModelCatalog(ctx, channel)
}

func fetchNonCodexUpstreamModelCatalog(ctx context.Context, channel *model.Channel) (channelUpstreamModelCatalog, error) {
	baseURL := constant.ChannelBaseURLs[channel.Type]
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}

	if channel.Type == constant.ChannelTypeOllama {
		key := strings.TrimSpace(strings.Split(channel.Key, "\n")[0])
		models, err := ollama.FetchOllamaModels(baseURL, key)
		if err != nil {
			return channelUpstreamModelCatalog{}, err
		}
		return channelUpstreamModelCatalog{
			IDs: normalizeModelNames(lo.Map(models, func(item ollama.OllamaModel, _ int) string {
				return item.Name
			})),
			Metadata: map[string]dto.UpstreamModelMetadata{},
		}, nil
	}

	if channel.Type == constant.ChannelTypeGemini {
		key, _, apiErr := channel.GetNextEnabledKey()
		if apiErr != nil {
			return channelUpstreamModelCatalog{}, fmt.Errorf("获取渠道密钥失败: %w", apiErr)
		}
		key = strings.TrimSpace(key)
		models, err := gemini.FetchGeminiModelCatalog(ctx, baseURL, key, channel.GetSetting().Proxy)
		if err != nil {
			return channelUpstreamModelCatalog{}, err
		}
		ids := make([]string, 0, len(models))
		metadata := make(map[string]dto.UpstreamModelMetadata)
		for _, upstreamModel := range models {
			name := strings.TrimSpace(upstreamModel.ID)
			if name == "" {
				continue
			}
			ids = append(ids, name)
			if upstreamModel.Metadata.Valid() {
				metadata[name] = upstreamModel.Metadata
			}
		}
		return channelUpstreamModelCatalog{IDs: normalizeModelNames(ids), Metadata: metadata}, nil
	}

	if channel.Type == constant.ChannelTypeAdvancedCustom {
		key, _, apiErr := channel.GetNextEnabledKey()
		if apiErr != nil {
			return channelUpstreamModelCatalog{}, fmt.Errorf("获取渠道密钥失败: %w", apiErr)
		}
		key = strings.TrimSpace(key)
		info := &relaycommon.RelayInfo{
			RelayFormat:    types.RelayFormatOpenAI,
			RelayMode:      relayconstant.RelayModeUnknown,
			RequestURLPath: dto.AdvancedCustomModelListPath,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType:          constant.ChannelTypeAdvancedCustom,
				ChannelBaseUrl:       baseURL,
				ApiKey:               key,
				ChannelOtherSettings: channel.GetOtherSettings(),
			},
		}
		adaptor := &advancedcustom.Adaptor{}
		url, headers, err := adaptor.BuildModelListRequest(info)
		if err != nil {
			return channelUpstreamModelCatalog{}, err
		}
		if err := applyFetchModelsHeaderOverrides(channel, key, headers); err != nil {
			return channelUpstreamModelCatalog{}, err
		}
		body, err := GetResponseBodyWithContext(ctx, http.MethodGet, url, channel, headers, time.Duration(common.ChannelManagementRequestTimeout)*time.Second)
		if err != nil {
			return channelUpstreamModelCatalog{}, err
		}
		var result OpenAIModelsResponse
		if err := common.Unmarshal(body, &result); err != nil {
			return channelUpstreamModelCatalog{}, fmt.Errorf("invalid OpenAI Models response: %w", err)
		}
		ids := normalizeModelNames(lo.Map(result.Data, func(item OpenAIModel, _ int) string { return item.ID }))
		if len(ids) == 0 {
			return channelUpstreamModelCatalog{}, errors.New("OpenAI Models response contains no valid model IDs")
		}
		return channelUpstreamModelCatalog{IDs: ids}, nil
	}

	var url string
	switch channel.Type {
	case constant.ChannelTypeAli:
		url = fmt.Sprintf("%s/compatible-mode/v1/models", baseURL)
	case constant.ChannelTypeZhipu_v4:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/api/paas/v4/models", baseURL)
		}
	case constant.ChannelTypeVolcEngine:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/v1/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/v1/models", baseURL)
		}
	case constant.ChannelTypeMoonshot:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/v1/models", baseURL)
		}
	default:
		url = fmt.Sprintf("%s/v1/models", baseURL)
	}

	key, _, apiErr := channel.GetNextEnabledKey()
	if apiErr != nil {
		return channelUpstreamModelCatalog{}, fmt.Errorf("获取渠道密钥失败: %w", apiErr)
	}
	key = strings.TrimSpace(key)
	headers, err := buildFetchModelsHeaders(channel, key)
	if err != nil {
		return channelUpstreamModelCatalog{}, err
	}

	timeout := time.Duration(common.ChannelManagementRequestTimeout) * time.Second
	body, err := GetResponseBodyWithContext(ctx, http.MethodGet, url, channel, headers, timeout)
	if err != nil {
		return channelUpstreamModelCatalog{}, applySubscriptionOAuthModelFetchError(channel, err)
	}

	var result OpenAIModelsResponse
	if err := common.Unmarshal(body, &result); err != nil {
		return channelUpstreamModelCatalog{}, err
	}

	ids := make([]string, 0, len(result.Data))
	metadata := make(map[string]dto.UpstreamModelMetadata)
	for _, item := range result.Data {
		name := strings.TrimSpace(item.ID)
		if name == "" {
			continue
		}
		ids = append(ids, name)
		if profile := item.UpstreamMetadata(); profile.Valid() {
			metadata[name] = profile
		}
	}

	return channelUpstreamModelCatalog{IDs: normalizeModelNames(ids), Metadata: metadata}, nil
}

func applySubscriptionOAuthModelFetchError(channel *model.Channel, err error) error {
	if channel == nil || err == nil || !constant.IsSubscriptionOAuthChannel(channel.Type) {
		return err
	}
	var apiError *types.NewAPIError
	if !errors.As(err, &apiError) {
		return err
	}
	apiError = service.ApplyChannelErrorPolicy(channel.Type, apiError)
	return apiError
}

func updateChannelUpstreamModelSettings(channel *model.Channel, settings dto.ChannelOtherSettings, updateModels bool) error {
	channel.SetOtherSettings(settings)
	updates := map[string]interface{}{
		"settings": channel.OtherSettings,
	}
	if updateModels {
		updates["models"] = channel.Models
	}
	return model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(updates).Error
}

func checkAndPersistChannelUpstreamModelUpdates(
	ctx context.Context,
	channel *model.Channel,
	settings *dto.ChannelOtherSettings,
	force bool,
	allowAutoApply bool,
) (modelsChanged bool, autoAdded int, err error) {
	now := common.GetTimestamp()
	if !force {
		minInterval := getUpstreamModelUpdateMinCheckIntervalSeconds()
		if settings.UpstreamModelUpdateLastCheckTime > 0 &&
			now-settings.UpstreamModelUpdateLastCheckTime < minInterval {
			return false, 0, nil
		}
	}

	pendingAddModels, pendingRemoveModels, fetchErr := collectPendingUpstreamModelChanges(ctx, channel, settings)
	settings.UpstreamModelUpdateLastCheckTime = now
	if fetchErr != nil {
		settings.ClearUpstreamModelMetadata()
		if err = updateChannelUpstreamModelSettings(channel, *settings, false); err != nil {
			return false, 0, err
		}
		return false, 0, fetchErr
	}

	if allowAutoApply && settings.UpstreamModelUpdateAutoSyncEnabled && len(pendingAddModels) > 0 {
		originModels := normalizeModelNames(channel.GetModels())
		mergedModels := mergeModelNames(originModels, pendingAddModels)
		if len(mergedModels) > len(originModels) {
			channel.Models = strings.Join(mergedModels, ",")
			autoAdded = len(mergedModels) - len(originModels)
			modelsChanged = true
		}
		settings.UpstreamModelUpdateLastDetectedModels = []string{}
	} else {
		settings.UpstreamModelUpdateLastDetectedModels = pendingAddModels
	}
	settings.UpstreamModelUpdateLastRemovedModels = pendingRemoveModels

	if err = updateChannelUpstreamModelSettings(channel, *settings, modelsChanged); err != nil {
		return false, autoAdded, err
	}
	if modelsChanged {
		if err = channel.UpdateAbilities(nil); err != nil {
			return true, autoAdded, err
		}
	}
	return modelsChanged, autoAdded, nil
}

func refreshChannelRuntimeCache() {
	if common.MemoryCacheEnabled {
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v", r))
				}
			}()
			model.InitChannelCache()
		}()
	}
}

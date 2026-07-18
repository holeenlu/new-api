package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

type OpenAIModel struct {
	ID               string `json:"id"`
	Object           string `json:"object"`
	Created          int64  `json:"created"`
	OwnedBy          string `json:"owned_by"`
	ContextWindow    int    `json:"context_window"`
	MaxContextWindow int    `json:"max_context_window"`
	ContextLength    int    `json:"context_length"`
	MaxInputTokens   int    `json:"max_input_tokens"`
	InputTokenLimit  int    `json:"inputTokenLimit"`
	Capabilities     struct {
		MaxInputTokens int `json:"max_input_tokens"`
	} `json:"capabilities"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Permission []struct {
		ID                 string `json:"id"`
		Object             string `json:"object"`
		Created            int64  `json:"created"`
		AllowCreateEngine  bool   `json:"allow_create_engine"`
		AllowSampling      bool   `json:"allow_sampling"`
		AllowLogprobs      bool   `json:"allow_logprobs"`
		AllowSearchIndices bool   `json:"allow_search_indices"`
		AllowView          bool   `json:"allow_view"`
		AllowFineTuning    bool   `json:"allow_fine_tuning"`
		Organization       string `json:"organization"`
		Group              string `json:"group"`
		IsBlocking         bool   `json:"is_blocking"`
	} `json:"permission"`
	Root   string `json:"root"`
	Parent string `json:"parent"`
}

type OpenAIModelsResponse struct {
	Data    []OpenAIModel `json:"data"`
	Success bool          `json:"success"`
}

func (m OpenAIModel) UpstreamMetadata() dto.UpstreamModelMetadata {
	contextWindow := m.ContextWindow
	if contextWindow == 0 {
		contextWindow = m.ContextLength
	}
	if contextWindow == 0 {
		contextWindow = m.MaxInputTokens
	}
	if contextWindow == 0 {
		contextWindow = m.InputTokenLimit
	}
	if contextWindow == 0 {
		contextWindow = m.Capabilities.MaxInputTokens
	}
	maxContextWindow := m.MaxContextWindow
	if maxContextWindow == 0 {
		maxContextWindow = contextWindow
	}
	metadata := dto.UpstreamModelMetadata{
		ContextWindow:    contextWindow,
		MaxContextWindow: maxContextWindow,
		Complete:         true,
	}
	if !metadata.Valid() {
		return dto.UpstreamModelMetadata{}
	}
	return metadata
}

func buildFetchModelsHeaders(channel *model.Channel, key string) (http.Header, error) {
	var headers http.Header
	switch channel.Type {
	case constant.ChannelTypeAnthropic:
		headers = GetClaudeAuthHeader(key)
	case constant.ChannelTypeClaudeCode:
		var err error
		headers, err = claude.BuildClaudeCodeOAuthHeaders(key)
		if err != nil {
			return nil, err
		}
	default:
		headers = GetAuthHeader(key)
	}

	headerOverride := channel.GetHeaderOverride()
	for k, v := range headerOverride {
		if relaychannel.IsHeaderPassthroughRuleKey(k) {
			continue
		}
		if relaychannel.IsSubscriptionOAuthProtectedHeader(channel.Type, k) {
			return nil, fmt.Errorf("critical header %q cannot be overridden for subscription OAuth channels", k)
		}
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid header override for key %s", k)
		}
		if strings.Contains(str, "{api_key}") {
			str = strings.ReplaceAll(str, "{api_key}", key)
		}
		headers.Set(k, str)
	}

	return headers, nil
}

func applyFetchModelsHeaderOverrides(channel *model.Channel, key string, headers http.Header) error {
	info := &relaycommon.RelayInfo{IsChannelTest: true, ChannelMeta: &relaycommon.ChannelMeta{
		ApiKey: key, HeadersOverride: channel.GetHeaderOverride(),
	}}
	overrides, err := relaychannel.ResolveHeaderOverride(info, nil)
	if err != nil {
		return err
	}
	for name, value := range overrides {
		headers.Set(name, value)
	}
	return nil
}

func FetchUpstreamModels(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}

	channel, err := model.GetChannelById(id, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	catalog, err := fetchChannelUpstreamModelCatalog(c.Request.Context(), channel)
	if err != nil {
		settings := channel.GetOtherSettings()
		settings.ClearUpstreamModelMetadata()
		if persistErr := updateChannelUpstreamModelSettings(channel, settings, false); persistErr != nil {
			common.ApiError(c, persistErr)
			return
		}
		response := gin.H{
			"success": false,
			"message": fmt.Sprintf("获取模型列表失败: %s", err.Error()),
		}
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) {
			response["error_code"] = apiErr.GetErrorCode()
		}
		c.JSON(http.StatusOK, response)
		return
	}
	if catalog.Metadata != nil {
		settings := channel.GetOtherSettings()
		settings.UpstreamModelMetadata = catalog.Metadata
		settings.UpstreamModelMetadataUpdatedTime = common.GetTimestamp()
		if err := updateChannelUpstreamModelSettings(channel, settings, false); err != nil {
			common.ApiError(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    catalog.IDs,
	})
}

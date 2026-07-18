package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/ollama"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func FetchModels(c *gin.Context) {
	var req struct {
		BaseURL        string `json:"base_url"`
		Type           int    `json:"type"`
		Key            string `json:"key"`
		AdvancedCustom string `json:"advanced_custom"`
		HeaderOverride string `json:"header_override"`
		Proxy          string `json:"proxy"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request",
		})
		return
	}

	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = constant.ChannelBaseURLs[req.Type]
	}

	// Preserve formatted Codex OAuth JSON; other providers use the first key line.
	key := strings.TrimSpace(req.Key)
	if req.Type != constant.ChannelTypeCodex {
		key = strings.Split(key, "\n")[0]
	}

	if req.Type == constant.ChannelTypeOllama {
		models, err := ollama.FetchOllamaModels(baseURL, key)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": fmt.Sprintf("获取Ollama模型失败: %s", err.Error()),
			})
			return
		}

		names := make([]string, 0, len(models))
		for _, modelInfo := range models {
			names = append(names, modelInfo.Name)
		}

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    names,
		})
		return
	}

	if req.Type == constant.ChannelTypeGemini {
		models, err := gemini.FetchGeminiModels(baseURL, key, "")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": fmt.Sprintf("获取Gemini模型失败: %s", err.Error()),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    models,
		})
		return
	}

	if req.Type == constant.ChannelTypeAdvancedCustom {
		var config dto.AdvancedCustomConfig
		if err := common.UnmarshalJsonStr(strings.TrimSpace(req.AdvancedCustom), &config); err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "advanced_custom is required and must be valid JSON"})
			return
		}
		channel := &model.Channel{Type: req.Type, Key: key, BaseURL: &baseURL}
		settings := channel.GetOtherSettings()
		settings.AdvancedCustom = &config
		channel.SetOtherSettings(settings)
		if strings.TrimSpace(req.HeaderOverride) != "" {
			var headerOverride map[string]any
			if err := common.UnmarshalJsonStr(req.HeaderOverride, &headerOverride); err != nil {
				c.JSON(http.StatusOK, gin.H{"success": false, "message": "header_override must be a JSON object"})
				return
			}
			channel.HeaderOverride = &req.HeaderOverride
		}
		channelSettings := channel.GetSetting()
		channelSettings.Proxy = strings.TrimSpace(req.Proxy)
		channel.SetSetting(channelSettings)
		catalog, err := fetchNonCodexUpstreamModelCatalog(c.Request.Context(), channel)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": fmt.Sprintf("获取模型列表失败: %s", err.Error())})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": catalog.IDs})
		return
	}

	channel := &model.Channel{
		Type:    req.Type,
		Key:     key,
		BaseURL: &baseURL,
	}
	if req.Type == constant.ChannelTypeCodex {
		models, err := service.FetchCodexChannelModels(channel)
		if err != nil {
			response := gin.H{
				"success": false,
				"message": fmt.Sprintf("获取 Codex 上游模型失败: %s", err.Error()),
			}
			c.JSON(http.StatusOK, response)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    models,
		})
		return
	}

	timeout := time.Duration(0)
	if req.Type == constant.ChannelTypeClaudeCode {
		timeout = time.Duration(common.SubscriptionOAuthResponseHeaderTimeout) * time.Second
	}
	requestCtx := c.Request.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, timeout)
		defer cancel()
	}
	client, err := service.GetHttpClientWithResponseHeaderTimeout("", timeout)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	url := fmt.Sprintf("%s/v1/models", baseURL)

	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": fmt.Sprintf("获取模型列表失败: %s", err.Error()),
		})
		return
	}
	lease, err := acquireSubscriptionOAuthManagementCapacity(requestCtx, req.Type, 0, 0, key)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if lease != nil {
		defer lease.Release()
	}

	switch req.Type {
	case constant.ChannelTypeAnthropic:
		request.Header = GetClaudeAuthHeader(key)
	case constant.ChannelTypeClaudeCode:
		request.Header, err = claude.BuildClaudeCodeOAuthHeaders(key)
		if err != nil {
			common.ApiError(c, err)
			return
		}
	default:
		request.Header.Set("Authorization", "Bearer "+key)
	}

	response, err := client.Do(request)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	defer response.Body.Close()
	//check status code
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to fetch models",
		})
		return
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := common.DecodeJson(response.Body, &result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	models := make([]string, 0, len(result.Data))
	for _, model := range result.Data {
		models = append(models, model.ID)
	}
	models = normalizeModelNames(models)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    models,
	})
}

func BatchSetChannelTag(c *gin.Context) {
	channelBatch := ChannelBatch{}
	err := c.ShouldBindJSON(&channelBatch)
	if err != nil || len(channelBatch.Ids) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "参数错误",
		})
		return
	}
	err = model.BatchSetChannelTag(channelBatch.Ids, channelBatch.Tag)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	model.InitChannelCache()
	recordManageAudit(c, "channel.tag_batch_set", map[string]interface{}{
		"count": len(channelBatch.Ids),
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    len(channelBatch.Ids),
	})
	return
}

func GetTagModels(c *gin.Context) {
	tag := c.Query("tag")
	if tag == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "tag不能为空",
		})
		return
	}

	channels, err := model.GetChannelsByTag(tag, false, false) // idSort=false, selectAll=false
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	var longestModels string
	maxLength := 0

	// Find the longest models string among all channels with the given tag
	for _, channel := range channels {
		if channel.Models != "" {
			currentModels := strings.Split(channel.Models, ",")
			if len(currentModels) > maxLength {
				maxLength = len(currentModels)
				longestModels = channel.Models
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    longestModels,
	})
	return
}

package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/ollama"

	"github.com/gin-gonic/gin"
)

// bindOllamaModelRequest parses the {channel_id, model_name} body shared by the
// Ollama model operations, writing the request error response and returning
// ok=false when the payload is missing or incomplete.
func bindOllamaModelRequest(c *gin.Context) (channelID int, modelName string, ok bool) {
	var req struct {
		ChannelID int    `json:"channel_id"`
		ModelName string `json:"model_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request parameters",
		})
		return 0, "", false
	}
	if req.ChannelID == 0 || req.ModelName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Channel ID and model name are required",
		})
		return 0, "", false
	}
	return req.ChannelID, req.ModelName, true
}

// resolveOllamaChannel loads an Ollama channel, verifies its type, and returns
// the effective base URL and first API key. It writes the appropriate error
// response and returns ok=false when the channel is missing or not an Ollama
// channel.
func resolveOllamaChannel(c *gin.Context, channelID int) (baseURL, key string, ok bool) {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "Channel not found",
		})
		return "", "", false
	}
	if channel.Type != constant.ChannelTypeOllama {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "This operation is only supported for Ollama channels",
		})
		return "", "", false
	}
	baseURL = constant.ChannelBaseURLs[channel.Type]
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}
	key = strings.Split(channel.Key, "\n")[0]
	return baseURL, key, true
}

// OllamaPullModel 拉取 Ollama 模型
func OllamaPullModel(c *gin.Context) {
	channelID, modelName, ok := bindOllamaModelRequest(c)
	if !ok {
		return
	}
	baseURL, key, ok := resolveOllamaChannel(c, channelID)
	if !ok {
		return
	}

	if err := ollama.PullOllamaModel(baseURL, key, modelName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("Failed to pull model: %s", err.Error()),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Model %s pulled successfully", modelName),
	})
}

// OllamaPullModelStream 流式拉取 Ollama 模型
func OllamaPullModelStream(c *gin.Context) {
	channelID, modelName, ok := bindOllamaModelRequest(c)
	if !ok {
		return
	}
	baseURL, key, ok := resolveOllamaChannel(c, channelID)
	if !ok {
		return
	}

	// 设置 SSE 头部
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")

	// 创建进度回调函数
	progressCallback := func(progress ollama.OllamaPullResponse) {
		data, _ := common.Marshal(progress)
		fmt.Fprintf(c.Writer, "data: %s\n\n", string(data))
		c.Writer.Flush()
	}

	// 执行拉取
	err := ollama.PullOllamaModelStream(baseURL, key, modelName, progressCallback)

	if err != nil {
		errorData, _ := common.Marshal(gin.H{
			"error": err.Error(),
		})
		fmt.Fprintf(c.Writer, "data: %s\n\n", string(errorData))
	} else {
		successData, _ := common.Marshal(gin.H{
			"message": fmt.Sprintf("Model %s pulled successfully", modelName),
		})
		fmt.Fprintf(c.Writer, "data: %s\n\n", string(successData))
	}

	// 发送结束标志
	fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()
}

// OllamaDeleteModel 删除 Ollama 模型
func OllamaDeleteModel(c *gin.Context) {
	channelID, modelName, ok := bindOllamaModelRequest(c)
	if !ok {
		return
	}
	baseURL, key, ok := resolveOllamaChannel(c, channelID)
	if !ok {
		return
	}

	if err := ollama.DeleteOllamaModel(baseURL, key, modelName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("Failed to delete model: %s", err.Error()),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Model %s deleted successfully", modelName),
	})
}

// OllamaVersion 获取 Ollama 服务版本信息
func OllamaVersion(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid channel id",
		})
		return
	}

	baseURL, key, ok := resolveOllamaChannel(c, id)
	if !ok {
		return
	}

	version, err := ollama.FetchOllamaVersion(baseURL, key)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": fmt.Sprintf("获取Ollama版本失败: %s", err.Error()),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"version": version,
		},
	})
}

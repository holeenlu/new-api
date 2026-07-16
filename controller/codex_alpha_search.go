package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func CodexAlphaSearch(c *gin.Context) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	body, err := storage.Bytes()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	body, err = codex.SanitizeAlphaSearchBody(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}

	var routing struct {
		Model string `json:"model"`
		ID    string `json:"id"`
	}
	if err := common.Unmarshal(body, &routing); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	if strings.TrimSpace(routing.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(errors.New("model is required"), types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}

	request := &dto.OpenAIResponsesRequest{Model: strings.TrimSpace(routing.Model)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	info.InitChannelMeta(c)
	if info.ChannelType != constant.ChannelTypeCodex {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(errors.New("Codex alpha search requires a ChatGPT Subscription (Codex) channel"), types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeChannelModelMappedError).ToOpenAIError()})
		return
	}
	var upstreamPayload map[string]any
	if err := common.Unmarshal(body, &upstreamPayload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	upstreamPayload["model"] = request.Model
	body, err = common.Marshal(upstreamPayload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": types.NewError(err, types.ErrorCodeJsonMarshalFailed).ToOpenAIError()})
		return
	}
	if routing.ID != "" && c.GetHeader("X-Session-ID") == "" {
		c.Request.Header.Set("X-Session-ID", routing.ID)
	}

	resp, err := codex.DoAlphaSearch(c, info, body)
	if err != nil {
		status := http.StatusBadGateway
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode > 0 {
			status = apiErr.StatusCode
		}
		c.JSON(status, gin.H{"error": types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, status).ToOpenAIError()})
		return
	}
	defer resp.Body.Close()
	payload, err := codex.ReadAlphaSearchResponse(resp)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": types.NewError(err, types.ErrorCodeReadResponseBodyFailed).ToOpenAIError()})
		return
	}
	for _, name := range []string{"Retry-After", "X-Request-ID", "OpenAI-Request-ID"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, payload)
}

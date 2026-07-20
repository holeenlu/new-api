package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const codexOAuthFlowTTL = 10 * time.Minute

type codexOAuthFlowPayload struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
}

type codexOAuthCompleteRequest struct {
	Input     string `json:"input"`
	FlowToken string `json:"flow_token"`
}

func respondCodexOAuthError(c *gin.Context, err error, fallbackMessage string) {
	response := gin.H{"success": false, "message": fallbackMessage}
	var upstreamErr *service.CodexOAuthUpstreamError
	if errors.As(err, &upstreamErr) {
		response["message"] = upstreamErr.Message
		response["error_code"] = upstreamErr.Code
	} else if err != nil {
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) {
			response["error_code"] = apiErr.GetErrorCode()
		}
	}
	c.JSON(http.StatusOK, response)
}

func StartCodexOAuth(c *gin.Context) {
	flow, err := service.CreateCodexOAuthAuthorizationFlow()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	payload, err := common.Marshal(codexOAuthFlowPayload{
		State:    flow.State,
		Verifier: flow.Verifier,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	flowToken, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeOAuth,
		Provider:  "codex",
		Intent:    model.AuthFlowIntentLogin,
		Payload:   string(payload),
		ExpiresAt: time.Now().Add(codexOAuthFlowTTL),
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"authorize_url": flow.AuthorizeURL,
		"flow_token":    flowToken,
	}})
}

func CompleteCodexOAuth(c *gin.Context) {
	var req codexOAuthCompleteRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiError(c, err)
		return
	}
	req.FlowToken = strings.TrimSpace(req.FlowToken)
	if req.FlowToken == "" {
		common.ApiError(c, errors.New("flow token is required"))
		return
	}
	callbackURL, err := url.Parse(strings.TrimSpace(req.Input))
	if err != nil {
		common.ApiError(c, errors.New("invalid callback URL"))
		return
	}
	code := strings.TrimSpace(callbackURL.Query().Get("code"))
	state := strings.TrimSpace(callbackURL.Query().Get("state"))
	if code == "" || state == "" {
		common.ApiError(c, errors.New("callback URL must include code and state"))
		return
	}
	authFlow, err := model.GetAuthFlow(req.FlowToken, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "codex",
		Intent:   model.AuthFlowIntentLogin,
	})
	if err != nil {
		common.ApiError(c, errors.New("OAuth session has expired or state does not match"))
		return
	}
	var payload codexOAuthFlowPayload
	if err := common.UnmarshalJsonStr(authFlow.Payload, &payload); err != nil {
		common.ApiError(c, err)
		return
	}
	if strings.TrimSpace(payload.State) == "" || strings.TrimSpace(payload.Verifier) == "" || state != payload.State {
		common.ApiError(c, errors.New("OAuth session has expired or state does not match"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, err := service.ExchangeCodexAuthorizationCode(ctx, code, payload.Verifier)
	if err != nil {
		logMessage := err.Error()
		var upstreamErr *service.CodexOAuthUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.Cause != nil {
			logMessage += ": " + upstreamErr.Cause.Error()
		}
		common.SysError("Codex OAuth authorization failed: " + common.RedactSensitiveCredentials(logMessage))
		respondCodexOAuthError(c, err, "OAuth authorization failed")
		return
	}
	accountID, ok := service.ExtractCodexAccountIDFromJWT(result.AccessToken)
	if !ok {
		common.ApiError(c, errors.New("could not determine the ChatGPT account ID"))
		return
	}
	email, _ := service.ExtractEmailFromJWT(result.AccessToken)
	key, err := common.Marshal(codex.OAuthKey{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, AccountID: accountID, Email: email, Type: "codex", LastRefresh: time.Now().Format(time.RFC3339), Expired: result.ExpiresAt.Format(time.RFC3339)})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := model.ConsumeAuthFlow(req.FlowToken, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "codex",
		Intent:   model.AuthFlowIntentLogin,
	}); err != nil {
		common.ApiError(c, errors.New("OAuth session has expired or state does not match"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"key": string(key)}})
}

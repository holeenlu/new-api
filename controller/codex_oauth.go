package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

const codexOAuthSessionStateKey = "codex_oauth_state"
const codexOAuthSessionVerifierKey = "codex_oauth_verifier"

type codexOAuthCompleteRequest struct {
	Input string `json:"input"`
}

func StartCodexOAuth(c *gin.Context) {
	flow, err := service.CreateCodexOAuthAuthorizationFlow()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	session := sessions.Default(c)
	session.Set(codexOAuthSessionStateKey, flow.State)
	session.Set(codexOAuthSessionVerifierKey, flow.Verifier)
	if err := session.Save(); err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"authorize_url": flow.AuthorizeURL}})
}

func CompleteCodexOAuth(c *gin.Context) {
	var req codexOAuthCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
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
	session := sessions.Default(c)
	expectedState, _ := session.Get(codexOAuthSessionStateKey).(string)
	verifier, _ := session.Get(codexOAuthSessionVerifierKey).(string)
	if expectedState == "" || verifier == "" || state != expectedState {
		common.ApiError(c, errors.New("OAuth session has expired or state does not match"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, err := service.ExchangeCodexAuthorizationCode(ctx, code, verifier)
	if err != nil {
		common.ApiError(c, err)
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
	session.Delete(codexOAuthSessionStateKey)
	session.Delete(codexOAuthSessionVerifierKey)
	_ = session.Save()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"key": string(key)}})
}

package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCodexOAuthControllerTest(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
	})
}

func TestStartCodexOAuthCreatesAuthFlow(t *testing.T) {
	setupCodexOAuthControllerTest(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/codex/oauth/start", strings.NewReader(`{}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	StartCodexOAuth(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			AuthorizeURL string `json:"authorize_url"`
			FlowToken    string `json:"flow_token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.NotEmpty(t, response.Data.AuthorizeURL)
	require.NotEmpty(t, response.Data.FlowToken)

	flow, err := model.GetAuthFlow(response.Data.FlowToken, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "codex",
		Intent:   model.AuthFlowIntentLogin,
	})
	require.NoError(t, err)
	var payload codexOAuthFlowPayload
	require.NoError(t, common.UnmarshalJsonStr(flow.Payload, &payload))
	assert.NotEmpty(t, payload.State)
	assert.NotEmpty(t, payload.Verifier)
}

func TestCompleteCodexOAuthRequiresFlowToken(t *testing.T) {
	setupCodexOAuthControllerTest(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/codex/oauth/complete", strings.NewReader(`{"input":"http://localhost:1455/auth/callback?code=test&state=state"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	CompleteCodexOAuth(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "flow token is required")
}

func TestCompleteCodexOAuthRejectsStateMismatchWithoutConsumingFlow(t *testing.T) {
	setupCodexOAuthControllerTest(t)

	payload, err := common.Marshal(codexOAuthFlowPayload{
		State:    "expected-state",
		Verifier: "expected-verifier",
	})
	require.NoError(t, err)
	flowToken, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeOAuth,
		Provider:  "codex",
		Intent:    model.AuthFlowIntentLogin,
		Payload:   string(payload),
		ExpiresAt: time.Now().Add(time.Minute),
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/codex/oauth/complete", strings.NewReader(`{"input":"http://localhost:1455/auth/callback?code=test&state=wrong-state","flow_token":"`+flowToken+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	CompleteCodexOAuth(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "OAuth session has expired or state does not match")
	flow, err := model.GetAuthFlow(flowToken, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "codex",
		Intent:   model.AuthFlowIntentLogin,
	})
	require.NoError(t, err)
	assert.Nil(t, flow.ConsumedAt)
}

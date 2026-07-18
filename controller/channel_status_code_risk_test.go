package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type channelMutationResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func TestAddChannelRequiresExplicitStatusCodeRiskConfirmation(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))

	newRequest := func(mapping string, confirmed bool) map[string]any {
		return map[string]any{
			"mode":                       "single",
			"status_code_risk_confirmed": confirmed,
			"channel": map[string]any{
				"type":                constant.ChannelTypeOpenAI,
				"name":                "risk-confirmation-test",
				"key":                 "test-key",
				"models":              "gpt-4o-mini",
				"group":               "default",
				"status_code_mapping": mapping,
			},
		}
	}

	t.Run("rejects risky mapping without confirmation", func(t *testing.T) {
		ctx, recorder := newAuthenticatedContext(
			t,
			http.MethodPost,
			"/api/channel/",
			newRequest(`{"504":500}`, false),
			7,
		)

		AddChannel(ctx)

		var response channelMutationResponse
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.False(t, response.Success)
		assert.Contains(t, response.Message, "504 -> 500")

		var count int64
		require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
		assert.Zero(t, count)
	})

	t.Run("rejects non-canonical source status code", func(t *testing.T) {
		ctx, recorder := newAuthenticatedContext(
			t,
			http.MethodPost,
			"/api/channel/",
			newRequest(`{"0504":500}`, true),
			7,
		)

		AddChannel(ctx)

		var response channelMutationResponse
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.False(t, response.Success)
		assert.Contains(t, response.Message, "invalid source status code")
	})

	t.Run("persists confirmed risk in the management audit", func(t *testing.T) {
		ctx, recorder := newAuthenticatedContext(
			t,
			http.MethodPost,
			"/api/channel/",
			newRequest(`{"504":500,"524":503}`, true),
			7,
		)
		ctx.Set("username", "risk-admin")
		ctx.Set("role", common.RoleRootUser)

		AddChannel(ctx)

		var response channelMutationResponse
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, response.Message)

		var auditLog model.Log
		require.NoError(t, db.Where("type = ?", model.LogTypeManage).Last(&auditLog).Error)
		var other struct {
			Op struct {
				Action string `json:"action"`
				Params struct {
					Confirmed bool     `json:"status_code_risk_confirmed"`
					Mappings  []string `json:"status_code_risk_mappings"`
				} `json:"params"`
			} `json:"op"`
		}
		require.NoError(t, common.Unmarshal([]byte(auditLog.Other), &other))
		assert.Equal(t, "channel.create", other.Op.Action)
		assert.True(t, other.Op.Params.Confirmed)
		assert.Equal(t, []string{"504 -> 500", "524 -> 503"}, other.Op.Params.Mappings)
	})
}

func TestValidateStatusCodeMappingRiskDoesNotReconfirmExistingMapping(t *testing.T) {
	risks, err := validateStatusCodeMappingRisk(
		`{"504":500}`,
		`{"504":500}`,
		false,
	)
	require.NoError(t, err)
	assert.Empty(t, risks)
}

func TestUpdateChannelRequiresAndAuditsStatusCodeRiskConfirmation(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	channel := model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "update-risk-confirmation-test",
		Key:     "test-key",
		Status:  common.ChannelStatusEnabled,
		Models:  "gpt-4o-mini",
		Group:   "default",
		AutoBan: common.GetPointer(1),
	}
	require.NoError(t, db.Create(&channel).Error)

	newRequest := func(confirmed bool) map[string]any {
		return map[string]any{
			"id":                         channel.Id,
			"name":                       channel.Name,
			"models":                     channel.Models,
			"group":                      channel.Group,
			"status_code_mapping":        `{"524":503}`,
			"status_code_risk_confirmed": confirmed,
		}
	}

	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/channel/", newRequest(false), 7)
	UpdateChannel(ctx)

	var response channelMutationResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Contains(t, response.Message, "524 -> 503")

	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Empty(t, stored.GetStatusCodeMapping())

	ctx, recorder = newAuthenticatedContext(t, http.MethodPut, "/api/channel/", newRequest(true), 7)
	ctx.Set("username", "risk-admin")
	ctx.Set("role", common.RoleRootUser)
	UpdateChannel(ctx)

	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, `{"524":503}`, stored.GetStatusCodeMapping())

	var auditLog model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeManage).Last(&auditLog).Error)
	var other struct {
		Op struct {
			Action string `json:"action"`
			Params struct {
				Confirmed bool     `json:"status_code_risk_confirmed"`
				Mappings  []string `json:"status_code_risk_mappings"`
			} `json:"params"`
		} `json:"op"`
	}
	require.NoError(t, common.Unmarshal([]byte(auditLog.Other), &other))
	assert.Equal(t, "channel.update", other.Op.Action)
	assert.True(t, other.Op.Params.Confirmed)
	assert.Equal(t, []string{"524 -> 503"}, other.Op.Params.Mappings)
}

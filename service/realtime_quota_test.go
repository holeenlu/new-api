package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalculateWssQuotaUsesModelPriceForPerCallBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{PriceData: types.PriceData{
		UsePrice:   true,
		ModelPrice: 2.5,
		GroupRatioInfo: types.GroupRatioInfo{
			GroupRatio: 0.4,
		},
	}}
	usage := &dto.RealtimeUsage{TotalTokens: 1, InputTokens: 1}
	usage.InputTokenDetails.TextTokens = 1

	quota, tieredResult := calculateWssQuota(info, "per-call-realtime", usage)

	want := common.QuotaFromDecimal(
		decimal.NewFromFloat(2.5).
			Mul(decimal.NewFromFloat(common.QuotaPerUnit)).
			Mul(decimal.NewFromFloat(0.4)),
	)
	assert.Equal(t, want, quota)
	assert.Positive(t, quota, "per-call realtime billing must not settle the reservation to zero")
	assert.Nil(t, tieredResult)
}

func TestCalculateWssQuotaUsesFrozenTieredExpressionAndRealtimeAudioDimensions(t *testing.T) {
	expr := `tier("audio", p * 1 + ai * 3 + c * 2 + ao * 4)`
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{ModelRatio: 999},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:  "tiered_expr",
			ModelName:    "frozen-realtime-model",
			ExprString:   expr,
			ExprHash:     billingexpr.ExprHashString(expr),
			GroupRatio:   1,
			QuotaPerUnit: 1_000_000,
		},
	}
	usage := &dto.RealtimeUsage{TotalTokens: 15, InputTokens: 10, OutputTokens: 5}
	usage.InputTokenDetails.TextTokens = 6
	usage.InputTokenDetails.AudioTokens = 4
	usage.OutputTokenDetails.TextTokens = 3
	usage.OutputTokenDetails.AudioTokens = 2

	quota, tieredResult := calculateWssQuota(info, "runtime-model-name", usage)

	require.NotNil(t, tieredResult)
	assert.Equal(t, 32, quota)
	assert.Equal(t, "audio", tieredResult.MatchedTier)
}

func TestPostAudioConsumeQuotaUsesConfiguredPerCallModelPrice(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	billing := &recordingBillingSettler{}
	info := &relaycommon.RelayInfo{
		StartTime:       time.Now(),
		OriginModelName: "per-call-audio",
		UserQuota:       common.GetTrustQuota(),
		Billing:         billing,
		PriceData: types.PriceData{
			UsePrice:   true,
			ModelPrice: 2.5,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 0.4,
			},
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	usage := &dto.Usage{TotalTokens: 1, PromptTokens: 1}
	usage.PromptTokensDetails.TextTokens = 1

	PostAudioConsumeQuota(ctx, info, usage, "")

	want := common.QuotaFromDecimal(
		decimal.NewFromFloat(2.5).
			Mul(decimal.NewFromFloat(common.QuotaPerUnit)).
			Mul(decimal.NewFromFloat(0.4)),
	)
	assert.Equal(t, 1, billing.settleCalls)
	assert.Equal(t, want, billing.actualQuota)
	assert.Positive(t, billing.actualQuota)
}

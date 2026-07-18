package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

func LogAlphaSearchConsumption(c *gin.Context, info *relaycommon.RelayInfo, quota int) {
	if c == nil || info == nil {
		return
	}
	other := map[string]interface{}{
		"request_path": c.Request.URL.Path,
		"alpha_search": true,
		"model_price": info.PriceData.ModelPrice,
		"group_ratio": info.PriceData.GroupRatioInfo.GroupRatio,
	}
	if info.PriceData.ModelRatio > 0 {
		other["model_ratio"] = info.PriceData.ModelRatio
	}
	if info.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = info.UpstreamModelName
	}
	attachQuotaSaturation(c, info, other)
	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	useTimeSeconds := 0
	if !startTime.IsZero() {
		useTimeSeconds = int(time.Since(startTime).Seconds())
	}
	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId:      info.ChannelId,
		ModelName:      info.OriginModelName,
		TokenName:      c.GetString("token_name"),
		Quota:          quota,
		Content:        "Codex Alpha Search",
		TokenId:        info.TokenId,
		UseTimeSeconds: useTimeSeconds,
		Group:          info.UsingGroup,
		Other:          other,
	})
	model.UpdateUserUsedQuotaAndRequestCount(info.UserId, quota)
	model.UpdateChannelUsedQuota(info.ChannelId, quota)
}

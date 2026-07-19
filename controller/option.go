package controller

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/console_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

var completionRatioMetaOptionKeys = []string{
	"ModelPrice",
	"ModelRatio",
	"CompletionRatio",
	"CacheRatio",
	"CreateCacheRatio",
	"ImageRatio",
	"AudioRatio",
	"AudioCompletionRatio",
}

func isPaymentComplianceOptionKey(key string) bool {
	return strings.HasPrefix(key, "payment_setting.compliance_")
}

func isPositiveOptionValue(value string) bool {
	intValue, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil {
		return intValue > 0
	}
	floatValue, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return err == nil && floatValue > 0
}

func collectModelNamesFromOptionValue(raw string, modelNames map[string]struct{}) {
	if strings.TrimSpace(raw) == "" {
		return
	}

	var parsed map[string]any
	if err := common.UnmarshalJsonStr(raw, &parsed); err != nil {
		return
	}

	for modelName := range parsed {
		modelNames[modelName] = struct{}{}
	}
}

func buildCompletionRatioMetaValue(optionValues map[string]string) string {
	modelNames := make(map[string]struct{})
	for _, key := range completionRatioMetaOptionKeys {
		collectModelNamesFromOptionValue(optionValues[key], modelNames)
	}

	meta := make(map[string]ratio_setting.CompletionRatioInfo, len(modelNames))
	for modelName := range modelNames {
		meta[modelName] = ratio_setting.GetCompletionRatioInfo(modelName)
	}

	jsonBytes, err := common.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(jsonBytes)
}

func GetOptions(c *gin.Context) {
	var options []*model.Option
	optionValues := make(map[string]string)
	common.OptionMapRWMutex.Lock()
	for k, v := range common.OptionMap {
		value := common.Interface2String(v)
		isSensitiveKey := strings.HasSuffix(k, "Token") ||
			strings.HasSuffix(k, "Secret") ||
			strings.HasSuffix(k, "Key") ||
			strings.HasSuffix(k, "secret") ||
			strings.HasSuffix(k, "api_key")
		if isSensitiveKey {
			continue
		}
		options = append(options, &model.Option{
			Key:   k,
			Value: value,
		})
		for _, optionKey := range completionRatioMetaOptionKeys {
			if optionKey == k {
				optionValues[k] = value
				break
			}
		}
	}
	common.OptionMapRWMutex.Unlock()
	options = append(options, &model.Option{
		Key:   "CompletionRatioMeta",
		Value: buildCompletionRatioMetaValue(optionValues),
	})
	options = append(options, upstreamLocationRuntimeOptions()...)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
}

func upstreamLocationRuntimeOptions() []*model.Option {
	host, egress := common.GetUpstreamLocationProfiles()
	return []*model.Option{
		{Key: "UpstreamSystemProxyEnabled", Value: strconv.FormatBool(common.UpstreamSystemProxyEnabled)},
		{Key: "UpstreamHostPublicIP", Value: host.PublicIP},
		{Key: "UpstreamHostLocationCountry", Value: host.Country},
		{Key: "UpstreamHostLocationRegion", Value: host.Region},
		{Key: "UpstreamHostLocationCity", Value: host.City},
		{Key: "UpstreamHostLocationTimezone", Value: host.Timezone},
		{Key: "UpstreamHostLocationLatitude", Value: formatLocationCoordinate(host.Latitude)},
		{Key: "UpstreamHostLocationLongitude", Value: formatLocationCoordinate(host.Longitude)},
		{Key: "UpstreamEgressPublicIP", Value: egress.PublicIP},
		{Key: "UpstreamEgressLocationCountry", Value: egress.Country},
		{Key: "UpstreamEgressLocationRegion", Value: egress.Region},
		{Key: "UpstreamEgressLocationCity", Value: egress.City},
		{Key: "UpstreamEgressLocationTimezone", Value: egress.Timezone},
		{Key: "UpstreamEgressLocationLatitude", Value: formatLocationCoordinate(egress.Latitude)},
		{Key: "UpstreamEgressLocationLongitude", Value: formatLocationCoordinate(egress.Longitude)},
	}
}

func formatLocationCoordinate(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

type upstreamLocationRefreshRouteResult struct {
	Attempted bool                            `json:"attempted"`
	Updated   bool                            `json:"updated"`
	Profile   upstreamLocationProfileResponse `json:"profile"`
	Error     string                          `json:"error,omitempty"`
}

type upstreamLocationProfileResponse struct {
	PublicIP  string `json:"public_ip"`
	Country   string `json:"country"`
	Region    string `json:"region"`
	City      string `json:"city"`
	Timezone  string `json:"timezone"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
}

type upstreamLocationRefreshResponse struct {
	Host   upstreamLocationRefreshRouteResult `json:"host"`
	Egress upstreamLocationRefreshRouteResult `json:"egress"`
}

func newUpstreamLocationRefreshRouteResult(result common.UpstreamLocationDiscoveryRouteResult) upstreamLocationRefreshRouteResult {
	routeResult := upstreamLocationRefreshRouteResult{
		Attempted: result.Attempted,
		Updated:   result.Updated,
		Profile: upstreamLocationProfileResponse{
			PublicIP:  result.Profile.PublicIP,
			Country:   result.Profile.Country,
			Region:    result.Profile.Region,
			City:      result.Profile.City,
			Timezone:  result.Profile.Timezone,
			Latitude:  formatLocationCoordinate(result.Profile.Latitude),
			Longitude: formatLocationCoordinate(result.Profile.Longitude),
		},
	}
	if result.Err != nil {
		routeResult.Error = result.Err.Error()
	}
	return routeResult
}

func RefreshUpstreamLocationProfiles(c *gin.Context) {
	report, err := common.RefreshUpstreamLocationProfiles(c.Request.Context(), service.GetHttpClient())
	if errors.Is(err, common.ErrUpstreamLocationRefreshInProgress) {
		c.Header("Retry-After", "2")
		c.JSON(http.StatusConflict, gin.H{
			"success": false,
			"message": "上游网络画像正在刷新，请稍后重试",
		})
		return
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if _, proxyErr := service.RefreshChannelProxyLocationProfiles(c.Request.Context()); proxyErr != nil {
		common.SysLog("failed to refresh one or more channel proxy location profiles: " + proxyErr.Error())
	}

	data := upstreamLocationRefreshResponse{
		Host:   newUpstreamLocationRefreshRouteResult(report.Host),
		Egress: newUpstreamLocationRefreshRouteResult(report.Egress),
	}
	if !report.Host.Updated && !report.Egress.Updated {
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false,
			"message": "无法刷新上游网络画像；已保留上次成功的数据",
			"data":    data,
		})
		return
	}

	recordManageAudit(c, "option.upstream_location.refresh", map[string]interface{}{
		"host_updated":       report.Host.Updated,
		"egress_attempted":   report.Egress.Attempted,
		"egress_updated":     report.Egress.Updated,
		"has_host_warning":   report.Host.Err != nil,
		"has_egress_warning": report.Egress.Err != nil,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    data,
	})
}

type OptionUpdateRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type optionBatchUpdateRequest struct {
	Options map[string]string `json:"options"`
}

var routingReliabilityOptionKeys = map[string]struct{}{
	"RetryTimes":                                {},
	"SubscriptionOAuthUpstreamRetryTimes":       {},
	"SubscriptionOAuthCapacityCycleTimes":       {},
	"SubscriptionOAuthCapacityWaitSeconds":      {},
	"SubscriptionOAuthRetry429":                 {},
	"ChannelDisableThreshold":                   {},
	"AutomaticDisableChannelEnabled":            {},
	"AutomaticEnableChannelEnabled":             {},
	"AutomaticDisableKeywords":                  {},
	"AutomaticDisableStatusCodes":               {},
	"AutomaticRetryStatusCodes":                 {},
	"monitor_setting.auto_test_channel_enabled": {},
	"monitor_setting.auto_test_channel_minutes": {},
	"monitor_setting.channel_test_mode":         {},
}

func validateRoutingReliabilityOption(key, value string) error {
	if _, ok := routingReliabilityOptionKeys[key]; !ok {
		return fmt.Errorf("配置项 %s 不支持批量保存", key)
	}

	switch key {
	case "RetryTimes", "SubscriptionOAuthUpstreamRetryTimes", "SubscriptionOAuthCapacityCycleTimes":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 10 {
			return fmt.Errorf("%s 必须是 0 到 10 的整数", key)
		}
	case "SubscriptionOAuthCapacityWaitSeconds":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 30 {
			return fmt.Errorf("%s 必须是 0 到 30 的整数", key)
		}
	case "SubscriptionOAuthRetry429", "AutomaticDisableChannelEnabled", "AutomaticEnableChannelEnabled", "monitor_setting.auto_test_channel_enabled":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("%s 必须是布尔值", key)
		}
	case "ChannelDisableThreshold":
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			parsed, err := strconv.ParseFloat(trimmed, 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
				return fmt.Errorf("%s 必须是非负数或留空", key)
			}
		}
	case "AutomaticDisableStatusCodes", "AutomaticRetryStatusCodes":
		if _, err := operation_setting.ParseHTTPStatusCodeRanges(value); err != nil {
			return err
		}
	case "monitor_setting.auto_test_channel_minutes":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return fmt.Errorf("%s 必须是不小于 1 的整数", key)
		}
	case "monitor_setting.channel_test_mode":
		if value != "scheduled_all" && value != "passive_recovery" {
			return fmt.Errorf("%s 无效", key)
		}
	}
	return nil
}

func UpdateRoutingReliabilityOptions(c *gin.Context) {
	var request optionBatchUpdateRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || len(request.Options) == 0 {
		common.ApiErrorMsg(c, "无效的参数")
		return
	}
	if len(request.Options) > len(routingReliabilityOptionKeys) {
		common.ApiErrorMsg(c, "配置项数量超过限制")
		return
	}
	for key, value := range request.Options {
		if err := validateRoutingReliabilityOption(key, value); err != nil {
			common.ApiErrorMsg(c, err.Error())
			return
		}
	}
	if err := model.UpdateOptionsBulk(request.Options); err != nil {
		common.ApiError(c, err)
		return
	}
	keys := make([]string, 0, len(request.Options))
	for key := range request.Options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	recordManageAudit(c, "option.routing_reliability.update", map[string]interface{}{"keys": keys})
	common.ApiSuccess(c, nil)
}

type modelPricingOptionsUpdateRequest struct {
	Options map[string]string `json:"options"`
}

func validateModelPricingOption(key, value string) error {
	switch key {
	case "ModelPrice", "ModelRatio", "CacheRatio", "CreateCacheRatio", "CompletionRatio", "ImageRatio", "AudioRatio", "AudioCompletionRatio":
		var ratios map[string]float64
		if err := common.UnmarshalJsonStr(value, &ratios); err != nil {
			return fmt.Errorf("%s 必须是数值映射: %w", key, err)
		}
		for modelName, ratio := range ratios {
			if strings.TrimSpace(modelName) == "" {
				return fmt.Errorf("%s 不允许空模型名", key)
			}
			if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
				return fmt.Errorf("%s[%s] 必须是有限的非负数", key, modelName)
			}
		}
	case "ModelPricingInputMode":
		var modes map[string]string
		if err := common.UnmarshalJsonStr(value, &modes); err != nil {
			return fmt.Errorf("ModelPricingInputMode 必须是字符串映射: %w", err)
		}
		for modelName, mode := range modes {
			if strings.TrimSpace(modelName) == "" {
				return fmt.Errorf("ModelPricingInputMode 不允许空模型名")
			}
			if mode != "ratio" && mode != "price" {
				return fmt.Errorf("ModelPricingInputMode[%s] 必须是 ratio 或 price", modelName)
			}
		}
	case "billing_setting.billing_mode":
		var values map[string]string
		if err := common.UnmarshalJsonStr(value, &values); err != nil {
			return fmt.Errorf("%s 必须是字符串映射: %w", key, err)
		}
		for modelName, mode := range values {
			if strings.TrimSpace(modelName) == "" {
				return fmt.Errorf("%s 不允许空模型名", key)
			}
			if mode != billing_setting.BillingModeRatio && mode != billing_setting.BillingModeTieredExpr {
				return fmt.Errorf("%s[%s] 的计费模式 %q 无效", key, modelName, mode)
			}
		}
	case "billing_setting.billing_expr":
		var values map[string]string
		if err := common.UnmarshalJsonStr(value, &values); err != nil {
			return fmt.Errorf("%s 必须是字符串映射: %w", key, err)
		}
		for modelName, expr := range values {
			if strings.TrimSpace(modelName) == "" {
				return fmt.Errorf("%s 不允许空模型名", key)
			}
			if strings.TrimSpace(expr) == "" {
				return fmt.Errorf("%s[%s] 不允许空表达式", key, modelName)
			}
			if err := billing_setting.SmokeTestExpr(expr); err != nil {
				return fmt.Errorf("%s[%s] 校验失败: %w", key, modelName, err)
			}
		}
	case "ExposeRatioEnabled":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("ExposeRatioEnabled 必须是布尔值")
		}
	default:
		return fmt.Errorf("不支持批量修改配置项 %s", key)
	}
	return nil
}

func validateModelPricingOptionSet(options map[string]string, existingModes, existingExprs string) error {
	for key, value := range options {
		if err := validateModelPricingOption(key, value); err != nil {
			return err
		}
	}

	modeValue, hasModes := options["billing_setting.billing_mode"]
	exprValue, hasExprs := options["billing_setting.billing_expr"]
	if !hasModes && !hasExprs {
		return nil
	}
	if !hasModes {
		modeValue = existingModes
	}
	if !hasExprs {
		exprValue = existingExprs
	}
	if strings.TrimSpace(modeValue) == "" {
		modeValue = "{}"
	}
	if strings.TrimSpace(exprValue) == "" {
		exprValue = "{}"
	}
	var modes map[string]string
	var exprs map[string]string
	if err := common.UnmarshalJsonStr(modeValue, &modes); err != nil {
		return err
	}
	if err := common.UnmarshalJsonStr(exprValue, &exprs); err != nil {
		return err
	}
	for modelName, mode := range modes {
		if mode == billing_setting.BillingModeTieredExpr && strings.TrimSpace(exprs[modelName]) == "" {
			return fmt.Errorf("模型 %s 启用了分层计费，但未配置计费表达式", modelName)
		}
	}
	return nil
}

func UpdateModelPricingOptions(c *gin.Context) {
	var request modelPricingOptionsUpdateRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || len(request.Options) == 0 {
		common.ApiErrorMsg(c, "无效的参数")
		return
	}
	if len(request.Options) > 12 {
		common.ApiErrorMsg(c, "模型价格配置项数量超过限制")
		return
	}

	common.OptionMapRWMutex.RLock()
	existingModes := common.OptionMap["billing_setting.billing_mode"]
	existingExprs := common.OptionMap["billing_setting.billing_expr"]
	common.OptionMapRWMutex.RUnlock()
	if err := validateModelPricingOptionSet(request.Options, existingModes, existingExprs); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	keys := make([]string, 0, len(request.Options))
	for key := range request.Options {
		keys = append(keys, key)
	}
	if err := model.UpdateOptionsBulk(request.Options); err != nil {
		common.ApiError(c, err)
		return
	}
	sort.Strings(keys)
	recordManageAudit(c, "option.model_pricing.update", map[string]interface{}{
		"keys": keys,
	})
	common.ApiSuccess(c, nil)
}

func UpdateOption(c *gin.Context) {
	var option OptionUpdateRequest
	err := common.DecodeJson(c.Request.Body, &option)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	switch option.Value.(type) {
	case bool:
		option.Value = common.Interface2String(option.Value.(bool))
	case float64:
		option.Value = common.Interface2String(option.Value.(float64))
	case int:
		option.Value = common.Interface2String(option.Value.(int))
	default:
		option.Value = fmt.Sprintf("%v", option.Value)
	}
	switch option.Key {
	case "QuotaForInviter", "QuotaForInvitee":
		if isPositiveOptionValue(option.Value.(string)) && !operation_setting.IsPaymentComplianceConfirmed() {
			common.ApiErrorI18n(c, i18n.MsgPaymentComplianceRequired)
			return
		}
	default:
		if isPaymentComplianceOptionKey(option.Key) {
			common.ApiErrorMsg(c, "合规确认字段不允许通过通用设置接口修改")
			return
		}
	}
	switch option.Key {
	case "GitHubOAuthEnabled":
		if option.Value == "true" && common.GitHubClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 GitHub OAuth，请先填入 GitHub Client Id 以及 GitHub Client Secret！",
			})
			return
		}
	case "discord.enabled":
		if option.Value == "true" && system_setting.GetDiscordSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Discord OAuth，请先填入 Discord Client Id 以及 Discord Client Secret！",
			})
			return
		}
	case "oidc.enabled":
		if option.Value == "true" && system_setting.GetOIDCSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 OIDC 登录，请先填入 OIDC Client Id 以及 OIDC Client Secret！",
			})
			return
		}
	case "LinuxDOOAuthEnabled":
		if option.Value == "true" && common.LinuxDOClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 LinuxDO OAuth，请先填入 LinuxDO Client Id 以及 LinuxDO Client Secret！",
			})
			return
		}
	case "EmailDomainRestrictionEnabled":
		if option.Value == "true" && len(common.EmailDomainWhitelist) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用邮箱域名限制，请先填入限制的邮箱域名！",
			})
			return
		}
	case "WeChatAuthEnabled":
		if option.Value == "true" && common.WeChatServerAddress == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用微信登录，请先填入微信登录相关配置信息！",
			})
			return
		}
	case "TurnstileCheckEnabled":
		if option.Value == "true" && common.TurnstileSiteKey == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Turnstile 校验，请先填入 Turnstile 校验相关配置信息！",
			})

			return
		}
	case "TelegramOAuthEnabled":
		if option.Value == "true" && common.TelegramBotToken == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Telegram OAuth，请先填入 Telegram Bot Token！",
			})
			return
		}
	case "theme.frontend":
		if option.Value != "default" && option.Value != "classic" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无效的主题值，可选值：default（新版前端）、classic（经典前端）",
			})
			return
		}
	case "GroupRatio":
		err = ratio_setting.CheckGroupRatio(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "ImageRatio":
		err = ratio_setting.UpdateImageRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "图片倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioRatio":
		err = ratio_setting.UpdateAudioRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioCompletionRatio":
		err = ratio_setting.UpdateAudioCompletionRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频补全倍率设置失败: " + err.Error(),
			})
			return
		}
	case "CreateCacheRatio":
		err = ratio_setting.UpdateCreateCacheRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "缓存创建倍率设置失败: " + err.Error(),
			})
			return
		}
	case "ModelRequestRateLimitGroup":
		err = setting.CheckModelRequestRateLimitGroup(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutomaticDisableStatusCodes":
		_, err = operation_setting.ParseHTTPStatusCodeRanges(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutomaticRetryStatusCodes":
		_, err = operation_setting.ParseHTTPStatusCodeRanges(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "SubscriptionOAuthUpstreamRetryTimes", "SubscriptionOAuthCapacityCycleTimes":
		value, parseErr := strconv.Atoi(option.Value.(string))
		if parseErr != nil || value < 0 || value > 10 {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "value must be an integer between 0 and 10"})
			return
		}
	case "SubscriptionOAuthCapacityWaitSeconds":
		value, parseErr := strconv.Atoi(option.Value.(string))
		if parseErr != nil || value < 0 || value > 30 {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "value must be an integer between 0 and 30"})
			return
		}
	case "console_setting.api_info":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "ApiInfo")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.announcements":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "Announcements")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.faq":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "FAQ")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.uptime_kuma_groups":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "UptimeKumaGroups")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	}
	err = model.UpdateOption(option.Key, option.Value.(string))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	// 出于安全考虑只记录被修改的配置项名称，不记录配置值（可能含密钥等敏感信息）。
	recordManageAudit(c, "option.update", map[string]interface{}{
		"key": option.Key,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

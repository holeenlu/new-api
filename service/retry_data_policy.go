package service

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

const (
	relayProviderHeader       = "X-Relay-Upstream-Provider"
	relayAttemptHeader        = "X-Relay-Attempt"
	relayRetryCountHeader     = "X-Relay-Retry-Count"
	relayRetryIsolationHeader = "X-Relay-Retry-Isolation"
	relayDataRegionHeader     = "X-Relay-Data-Region"
	relayDataRetentionHeader  = "X-Relay-Data-Retention"
	relayDataTrainingHeader   = "X-Relay-Data-Training"
)

type ResolvedChannelDataPolicy struct {
	Provider         string
	Region           string
	Retention        string
	Training         string
	RetryIsolation   dto.RetryIsolation
	RetryPolicyGroup string
	Endpoint         string
}

type RetryBoundary struct {
	initialChannelID int
	initialType      int
	policy           ResolvedChannelDataPolicy
	usedChannelIDs   map[int]struct{}
	usedKeyIndexes   map[int]map[int]struct{}
}

func ResolveChannelDataPolicy(channel *model.Channel) ResolvedChannelDataPolicy {
	policy := ResolvedChannelDataPolicy{
		Provider:  "Unknown",
		Region:    "provider_account_policy",
		Retention: "provider_account_policy",
		Training:  string(dto.DataTrainingProviderDefault),
		Endpoint:  normalizedUpstreamEndpoint(""),
	}
	if channel == nil {
		policy.RetryIsolation = dto.RetryIsolationChannel
		return policy
	}

	policy.Provider = constant.GetChannelTypeName(channel.Type)
	policy.Endpoint = normalizedUpstreamEndpoint(channel.GetBaseURL())
	settings := channel.GetOtherSettings().DataPolicy
	if settings != nil {
		validRetryPolicy := settings.Validate() == nil
		if value := strings.TrimSpace(settings.Provider); value != "" {
			policy.Provider = value
		}
		if value := strings.TrimSpace(settings.Region); value != "" {
			policy.Region = value
		}
		if value := strings.TrimSpace(settings.Retention); value != "" {
			policy.Retention = value
		}
		if settings.Training != "" {
			policy.Training = string(settings.Training)
		}
		policy.RetryIsolation = settings.RetryIsolation
		policy.RetryPolicyGroup = strings.TrimSpace(settings.RetryPolicyGroup)
		if !validRetryPolicy {
			policy.RetryIsolation = dto.RetryIsolationChannel
			policy.RetryPolicyGroup = ""
		}
	}
	if policy.RetryIsolation == "" {
		policy.RetryIsolation = dto.RetryIsolationChannel
	}
	return policy
}

func normalizedUpstreamEndpoint(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(strings.TrimRight(rawURL, "/"))
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(path.Clean(parsed.Path), "/")
	if parsed.Path == "." {
		parsed.Path = ""
	}
	return parsed.String()
}

func NewRetryBoundary(channel *model.Channel) *RetryBoundary {
	if channel == nil {
		return nil
	}
	return &RetryBoundary{
		initialChannelID: channel.Id,
		initialType:      channel.Type,
		policy:           ResolveChannelDataPolicy(channel),
		usedChannelIDs:   make(map[int]struct{}),
		usedKeyIndexes:   make(map[int]map[int]struct{}),
	}
}

func (b *RetryBoundary) MarkAttempt(channel *model.Channel, multiKeyIndex ...int) {
	if b == nil || channel == nil {
		return
	}
	b.usedChannelIDs[channel.Id] = struct{}{}
	if channel.ChannelInfo.IsMultiKey && len(multiKeyIndex) > 0 && multiKeyIndex[0] >= 0 {
		indexes := b.usedKeyIndexes[channel.Id]
		if indexes == nil {
			indexes = make(map[int]struct{})
			b.usedKeyIndexes[channel.Id] = indexes
		}
		indexes[multiKeyIndex[0]] = struct{}{}
	}
}

func (b *RetryBoundary) UsedMultiKeyIndexes(channelID int) map[int]struct{} {
	if b == nil || len(b.usedKeyIndexes[channelID]) == 0 {
		return nil
	}
	result := make(map[int]struct{}, len(b.usedKeyIndexes[channelID]))
	for index := range b.usedKeyIndexes[channelID] {
		result[index] = struct{}{}
	}
	return result
}

func (b *RetryBoundary) hasUntriedEnabledKey(channel *model.Channel) bool {
	lock := model.GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()

	used := b.usedKeyIndexes[channel.Id]
	keys := channel.GetKeys()
	for index := range keys {
		status, hasStatus := channel.ChannelInfo.MultiKeyStatusList[index]
		if hasStatus && status != common.ChannelStatusEnabled {
			continue
		}
		if _, attempted := used[index]; !attempted {
			return true
		}
	}
	return false
}

func (b *RetryBoundary) Allows(channel *model.Channel) bool {
	if b == nil || channel == nil {
		return false
	}
	if _, used := b.usedChannelIDs[channel.Id]; used {
		if !channel.ChannelInfo.IsMultiKey || !b.hasUntriedEnabledKey(channel) {
			return false
		}
	}
	candidate := ResolveChannelDataPolicy(channel)
	if candidate.Provider != b.policy.Provider || candidate.Region != b.policy.Region ||
		candidate.Retention != b.policy.Retention || candidate.Training != b.policy.Training {
		return false
	}
	switch b.policy.RetryIsolation {
	case dto.RetryIsolationProvider:
		return candidate.RetryIsolation == dto.RetryIsolationProvider &&
			channel.Type == b.initialType && candidate.Endpoint == b.policy.Endpoint
	case dto.RetryIsolationPolicyGroup:
		return candidate.RetryIsolation == dto.RetryIsolationPolicyGroup &&
			b.policy.RetryPolicyGroup != "" && candidate.RetryPolicyGroup == b.policy.RetryPolicyGroup
	default:
		return channel.Id == b.initialChannelID && channel.ChannelInfo.IsMultiKey
	}
}

func ApplyRelayDataPolicyHeaders(c *gin.Context, channel *model.Channel, attempt int) {
	if c == nil || channel == nil {
		return
	}
	policy := ResolveChannelDataPolicy(channel)
	c.Header(relayProviderHeader, safeDisclosureValue(policy.Provider))
	c.Header(relayAttemptHeader, strconv.Itoa(attempt))
	c.Header(relayRetryCountHeader, strconv.Itoa(max(attempt-1, 0)))
	c.Header(relayRetryIsolationHeader, string(policy.RetryIsolation))
	c.Header(relayDataRegionHeader, safeDisclosureValue(policy.Region))
	c.Header(relayDataRetentionHeader, safeDisclosureValue(policy.Retention))
	c.Header(relayDataTrainingHeader, safeDisclosureValue(policy.Training))
}

func safeDisclosureValue(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > 128 {
		value = string(runes[:128])
	}
	return value
}

func RelayDisclosureHeaders() []string {
	return []string{
		relayProviderHeader,
		relayAttemptHeader,
		relayRetryCountHeader,
		relayRetryIsolationHeader,
		relayDataRegionHeader,
		relayDataRetentionHeader,
		relayDataTrainingHeader,
		"X-New-Api-Version",
	}
}

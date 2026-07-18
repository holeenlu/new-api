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
	ProviderIdentity string
	Region           string
	Retention        string
	Training         string
	RetryIsolation   dto.RetryIsolation
	RetryPolicyGroup string
	Endpoint         string
}

type RetryBoundary struct {
	initialChannelID    int
	initialType         int
	initialTag          string
	effectiveGroup      string
	subscriptionOAuth   bool
	policy              ResolvedChannelDataPolicy
	usedChannelIDs      map[int]struct{}
	usedKeyIndexes      map[int]map[int]struct{}
	excludedCredentials map[string]struct{}
	failedCredentials   map[string]struct{}
	capacityAttempts    map[string]map[int]map[int]struct{}
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
	policy.ProviderIdentity = "endpoint:" + policy.Endpoint
	settings := channel.GetOtherSettings().DataPolicy
	if settings != nil {
		validRetryPolicy := settings.Validate() == nil
		if value := strings.TrimSpace(settings.Provider); value != "" {
			policy.Provider = value
			policy.ProviderIdentity = "declared:" + strings.ToLower(value)
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
	isSubscriptionOAuth := constant.IsSubscriptionOAuthChannel(channel.Type)
	if isSubscriptionOAuth && (policy.RetryIsolation == "" || policy.RetryIsolation == dto.RetryIsolationTag) {
		policy.RetryIsolation = dto.RetryIsolationGroup
	} else if policy.RetryIsolation == "" {
		if strings.TrimSpace(channel.GetTag()) != "" {
			policy.RetryIsolation = dto.RetryIsolationTag
		} else {
			policy.RetryIsolation = dto.RetryIsolationChannel
		}
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

func NewRetryBoundary(channel *model.Channel, effectiveGroup string) *RetryBoundary {
	if channel == nil {
		return nil
	}
	effectiveGroup = model.NormalizeChannelGroupFilter(effectiveGroup)
	if effectiveGroup == "" {
		groups := channel.GetGroups()
		if len(groups) == 1 {
			effectiveGroup = groups[0]
		}
	}
	return &RetryBoundary{
		initialChannelID:    channel.Id,
		initialType:         channel.Type,
		initialTag:          strings.TrimSpace(channel.GetTag()),
		effectiveGroup:      effectiveGroup,
		subscriptionOAuth:   constant.IsSubscriptionOAuthChannel(channel.Type),
		policy:              ResolveChannelDataPolicy(channel),
		usedChannelIDs:      make(map[int]struct{}),
		usedKeyIndexes:      make(map[int]map[int]struct{}),
		excludedCredentials: make(map[string]struct{}),
		failedCredentials:   make(map[string]struct{}),
		capacityAttempts:    make(map[string]map[int]map[int]struct{}),
	}
}

func (b *RetryBoundary) EffectiveGroup() string {
	if b == nil {
		return ""
	}
	return b.effectiveGroup
}

func (b *RetryBoundary) IsSubscriptionOAuth() bool {
	return b != nil && b.subscriptionOAuth
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

func (b *RetryBoundary) ExcludeCredential(fingerprint string, channelID, keyIndex int) {
	if b == nil || strings.TrimSpace(fingerprint) == "" {
		return
	}
	b.excludedCredentials[fingerprint] = struct{}{}
	channels := b.capacityAttempts[fingerprint]
	if channels == nil {
		channels = make(map[int]map[int]struct{})
		b.capacityAttempts[fingerprint] = channels
	}
	indexes := channels[channelID]
	if indexes == nil {
		indexes = make(map[int]struct{})
		channels[channelID] = indexes
	}
	indexes[keyIndex] = struct{}{}
}

func (b *RetryBoundary) FailCredential(fingerprint string) {
	if b == nil || strings.TrimSpace(fingerprint) == "" {
		return
	}
	b.failedCredentials[fingerprint] = struct{}{}
	delete(b.excludedCredentials, fingerprint)
}

// RestartCapacityCycle makes channels skipped only because of local OAuth
// concurrency available for a later retry cycle. It deliberately retains
// channels that previously failed for another reason.
func (b *RetryBoundary) RestartCapacityCycle() bool {
	if b == nil || len(b.excludedCredentials) == 0 {
		return false
	}
	for fingerprint := range b.excludedCredentials {
		for channelID, indexes := range b.capacityAttempts[fingerprint] {
			for keyIndex := range indexes {
				delete(b.usedKeyIndexes[channelID], keyIndex)
			}
			if len(b.usedKeyIndexes[channelID]) == 0 {
				delete(b.usedKeyIndexes, channelID)
				delete(b.usedChannelIDs, channelID)
			}
		}
		delete(b.capacityAttempts, fingerprint)
	}
	clear(b.excludedCredentials)
	return true
}

func (b *RetryBoundary) HasCapacityExclusions() bool {
	return b != nil && len(b.excludedCredentials) > 0
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

func (b *RetryBoundary) ExcludedKeyIndexes(channel *model.Channel) map[int]struct{} {
	if b == nil || channel == nil {
		return nil
	}
	result := b.UsedMultiKeyIndexes(channel.Id)
	if result == nil {
		result = make(map[int]struct{})
	}
	for index, key := range channel.GetKeys() {
		fingerprint := SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, index, key)
		if _, excluded := b.excludedCredentials[fingerprint]; excluded {
			result[index] = struct{}{}
			continue
		}
		if _, failed := b.failedCredentials[fingerprint]; failed {
			result[index] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
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
	if b == nil || channel == nil || channel.Status != common.ChannelStatusEnabled {
		return false
	}
	if b.subscriptionOAuth && (channel.Type != b.initialType || !channelBelongsToGroup(channel, b.effectiveGroup)) {
		return false
	}
	candidate := ResolveChannelDataPolicy(channel)
	if candidate.ProviderIdentity != b.policy.ProviderIdentity || candidate.Region != b.policy.Region ||
		candidate.Retention != b.policy.Retention || candidate.Training != b.policy.Training {
		return false
	}
	allowedByPolicy := false
	switch b.policy.RetryIsolation {
	case dto.RetryIsolationChannel:
		allowedByPolicy = channel.Id == b.initialChannelID
	case dto.RetryIsolationGroup:
		allowedByPolicy = b.subscriptionOAuth && candidate.RetryIsolation == dto.RetryIsolationGroup
	case dto.RetryIsolationProvider:
		allowedByPolicy = candidate.RetryIsolation == dto.RetryIsolationProvider &&
			channel.Type == b.initialType && candidate.Endpoint == b.policy.Endpoint
	case dto.RetryIsolationPolicyGroup:
		allowedByPolicy = candidate.RetryIsolation == dto.RetryIsolationPolicyGroup &&
			b.policy.RetryPolicyGroup != "" && candidate.RetryPolicyGroup == b.policy.RetryPolicyGroup
	case dto.RetryIsolationTag:
		allowedByPolicy = !b.subscriptionOAuth && candidate.RetryIsolation == dto.RetryIsolationTag &&
			channel.Type == b.initialType && b.initialTag != "" &&
			b.initialTag == strings.TrimSpace(channel.GetTag())
	default:
		allowedByPolicy = false
	}
	if !allowedByPolicy {
		return false
	}
	if constant.IsSubscriptionOAuthChannel(b.initialType) {
		return b.hasEligibleSubscriptionOAuthCredential(channel)
	}
	if _, used := b.usedChannelIDs[channel.Id]; used {
		return channel.ChannelInfo.IsMultiKey && b.hasUntriedEnabledKey(channel)
	}
	return true
}

func channelBelongsToGroup(channel *model.Channel, group string) bool {
	if channel == nil || strings.TrimSpace(group) == "" {
		return false
	}
	for _, candidate := range channel.GetGroups() {
		if candidate == group {
			return true
		}
	}
	return false
}

func (b *RetryBoundary) hasEligibleSubscriptionOAuthCredential(channel *model.Channel) bool {
	lock := model.GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()

	keys := channel.GetKeys()
	if len(keys) == 0 {
		return false
	}
	for index, key := range keys {
		if channel.ChannelInfo.IsMultiKey {
			status, exists := channel.ChannelInfo.MultiKeyStatusList[index]
			if exists && status != common.ChannelStatusEnabled {
				continue
			}
		}
		fingerprint := SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, index, key)
		if _, excluded := b.excludedCredentials[fingerprint]; excluded {
			continue
		}
		if _, failed := b.failedCredentials[fingerprint]; failed {
			continue
		}
		return true
	}
	return false
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

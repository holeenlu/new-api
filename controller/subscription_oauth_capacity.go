package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"
)

// acquireSubscriptionOAuthManagementCapacity applies the same credential
// limits to account-management calls without changing circuit-breaker state.
func acquireSubscriptionOAuthManagementCapacity(
	ctx context.Context,
	channelType int,
	channelID int,
	keyIndex int,
	key string,
) (*service.SubscriptionOAuthLease, error) {
	maxConcurrency := 0
	minRequestInterval := time.Duration(0)
	switch channelType {
	case constant.ChannelTypeCodex:
		maxConcurrency = codex.CodexOAuthMaxConcurrency
		minRequestInterval = codex.CodexOAuthMinRequestInterval
	case constant.ChannelTypeClaudeCode:
		maxConcurrency = claude.ClaudeCodeOAuthMaxConcurrency
		minRequestInterval = claude.ClaudeCodeOAuthMinRequestInterval
	default:
		return nil, nil
	}

	fingerprint := service.SubscriptionOAuthCredentialFingerprint(channelType, channelID, keyIndex, key)
	lease, err := service.AcquireSubscriptionOAuthManagementCapacity(ctx, fingerprint, maxConcurrency, minRequestInterval)
	if err == nil {
		return lease, nil
	}
	if service.IsSubscriptionOAuthCapacityError(err) {
		return nil, fmt.Errorf(
			"subscription OAuth credential is busy; retry after %d seconds: %w",
			service.SubscriptionOAuthCapacityRetryAfterSeconds(err),
			err,
		)
	}
	return nil, err
}

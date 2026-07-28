package service

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageFromClaudeBillingUsageNormalizesCacheCreationSplit(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		known5m   int
		known1h   int
		want5m    int
		want1h    int
		wantTotal int
	}{
		{name: "aggregate only", total: 100, want5m: 100, wantTotal: 100},
		{name: "partial split", total: 100, known1h: 20, want5m: 80, want1h: 20, wantTotal: 100},
		{name: "full split", total: 100, known5m: 80, known1h: 20, want5m: 80, want1h: 20, wantTotal: 100},
		{name: "missing aggregate", known5m: 30, known1h: 20, want5m: 30, want1h: 20, wantTotal: 50},
		{name: "known split exceeds aggregate", total: 50, known5m: 40, known1h: 30, want5m: 40, want1h: 30, wantTotal: 70},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := usageFromClaudeBillingUsage(&dto.BillingUsage{
				Source:   dto.BillingUsageSourceClaudeMessages,
				Semantic: dto.BillingUsageSemanticAnthropic,
				ClaudeUsage: &dto.ClaudeUsage{
					InputTokens:                 10,
					CacheCreationInputTokens:    tt.total,
					ClaudeCacheCreation5mTokens: tt.known5m,
					ClaudeCacheCreation1hTokens: tt.known1h,
				},
			})

			require.NotNil(t, usage)
			assert.Equal(t, tt.want5m, usage.ClaudeCacheCreation5mTokens)
			assert.Equal(t, tt.want1h, usage.ClaudeCacheCreation1hTokens)
			assert.Equal(t, tt.wantTotal, usage.PromptTokensDetails.CachedCreationTokens)
			assert.Equal(t, 10+tt.wantTotal, usage.InputTokens)
			assert.Equal(t, tt.total, usage.BillingUsage.ClaudeUsage.CacheCreationInputTokens)
		})
	}
}

func TestUsageFromClaudeBillingUsageUsesCacheCreationObjectWhenPresent(t *testing.T) {
	usage := usageFromClaudeBillingUsage(&dto.BillingUsage{
		Source:   dto.BillingUsageSourceClaudeMessages,
		Semantic: dto.BillingUsageSemanticAnthropic,
		ClaudeUsage: &dto.ClaudeUsage{
			CacheCreationInputTokens: 100,
			CacheCreation: &dto.ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: 30,
				Ephemeral1hInputTokens: 20,
			},
			ClaudeCacheCreation5mTokens: 99,
			ClaudeCacheCreation1hTokens: 99,
		},
	})

	require.NotNil(t, usage)
	assert.Equal(t, 80, usage.ClaudeCacheCreation5mTokens)
	assert.Equal(t, 20, usage.ClaudeCacheCreation1hTokens)
}

func TestUsageFromClaudeBillingUsagePrefersExplicitCacheCreationObjectZero(t *testing.T) {
	usage := usageFromClaudeBillingUsage(&dto.BillingUsage{
		ClaudeUsage: &dto.ClaudeUsage{
			CacheCreationInputTokens: 100,
			CacheCreation: &dto.ClaudeCacheCreationUsage{
				Ephemeral1hInputTokens: 20,
			},
			ClaudeCacheCreation5mTokens: 99,
			ClaudeCacheCreation1hTokens: 99,
		},
	})

	require.NotNil(t, usage)
	assert.Equal(t, 80, usage.ClaudeCacheCreation5mTokens)
	assert.Equal(t, 20, usage.ClaudeCacheCreation1hTokens)
	assert.Equal(t, 100, usage.PromptTokensDetails.CachedCreationTokens)
}

func TestUsageFromClaudeBillingUsageSaturatesCacheCreationTotal(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	usage := usageFromClaudeBillingUsage(&dto.BillingUsage{
		ClaudeUsage: &dto.ClaudeUsage{
			CacheCreationInputTokens:    0,
			ClaudeCacheCreation5mTokens: maxInt,
			ClaudeCacheCreation1hTokens: 2,
		},
	})

	require.NotNil(t, usage)
	assert.Equal(t, maxInt, usage.ClaudeCacheCreation5mTokens)
	assert.Equal(t, 2, usage.ClaudeCacheCreation1hTokens)
	assert.Equal(t, maxInt, usage.PromptTokensDetails.CachedCreationTokens)
	assert.Equal(t, maxInt, usage.InputTokens)
}

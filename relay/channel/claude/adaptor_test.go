package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/require"
)

func TestSetupClaudeCodeOAuthHeader(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-api-key", "stale-key")

	err := SetupClaudeCodeOAuthHeader(&headers, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-oauth-token")

	require.NoError(t, err)
	require.Empty(t, headers.Get("x-api-key"))
	require.Equal(t, "Bearer sk-ant-oat01-oauth-token", headers.Get("Authorization"))
	require.Equal(t, ClaudeCodeOAuthBeta, headers.Get("anthropic-beta"))
	require.Equal(t, ClaudeCodeOAuthUserAgent, headers.Get("user-agent"))
	require.Equal(t, "cli", headers.Get("x-app"))
}

func TestClaudeCodeOAuthPacing(t *testing.T) {
	originalInterval := ClaudeCodeOAuthMinRequestInterval
	ClaudeCodeOAuthMinRequestInterval = 20 * time.Millisecond
	t.Cleanup(func() { ClaudeCodeOAuthMinRequestInterval = originalInterval })

	channelID := 900003
	require.NoError(t, waitForClaudeCodeOAuthTurn(context.Background(), channelID))
	started := time.Now()
	require.NoError(t, waitForClaudeCodeOAuthTurn(context.Background(), channelID))
	require.GreaterOrEqual(t, time.Since(started), 15*time.Millisecond)
}

func TestParseClaudeCodeOAuthToken(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "raw token", input: "sk-ant-oat01-token", want: "sk-ant-oat01-token"},
		{name: "environment assignment", input: "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-token", want: "sk-ant-oat01-token"},
		{name: "exported quoted assignment", input: `export CLAUDE_CODE_OAUTH_TOKEN="sk-ant-oat01-token"`, want: "sk-ant-oat01-token"},
		{name: "regular api key", input: "sk-ant-api03-token", wantErr: true},
		{name: "embedded whitespace", input: "sk-ant-oat01-token value", wantErr: true},
		{name: "empty", input: "CLAUDE_CODE_OAUTH_TOKEN=", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseClaudeCodeOAuthToken(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEnsureClaudeCodeIdentitySystem(t *testing.T) {
	// Bare channel-test request: no system -> identity becomes the system.
	bare := &dto.ClaudeRequest{}
	ensureClaudeCodeIdentitySystem(bare)
	require.True(t, bare.IsStringSystem())
	require.Equal(t, ClaudeCodeIdentitySystem, bare.GetStringSystem())

	// Existing string system that is not the identity -> identity prepended as
	// the first block, original content preserved as the second block.
	strSys := &dto.ClaudeRequest{System: "custom instructions"}
	ensureClaudeCodeIdentitySystem(strSys)
	blocks := strSys.ParseSystem()
	require.Len(t, blocks, 2)
	require.Equal(t, ClaudeCodeIdentitySystem, blocks[0].GetText())
	require.Equal(t, "custom instructions", blocks[1].GetText())

	// Real Claude Code client already sends the identity first -> no-op.
	identityBlock := newClaudeTextBlock(ClaudeCodeIdentitySystem)
	extraBlock := newClaudeTextBlock("more context")
	already := &dto.ClaudeRequest{System: []dto.ClaudeMediaMessage{identityBlock, extraBlock}}
	ensureClaudeCodeIdentitySystem(already)
	blocks = already.ParseSystem()
	require.Len(t, blocks, 2)
	require.Equal(t, ClaudeCodeIdentitySystem, blocks[0].GetText())
	require.Equal(t, "more context", blocks[1].GetText())
}

func TestClaudeCodeOAuthConcurrencySlots(t *testing.T) {
	originalMaxConcurrency := ClaudeCodeOAuthMaxConcurrency
	ClaudeCodeOAuthMaxConcurrency = 2
	t.Cleanup(func() { ClaudeCodeOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 900001
	releaseFirst, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)
	releaseSecond, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)

	_, ok = acquireClaudeCodeOAuthSlot(channelID)
	require.False(t, ok)

	releaseFirst()
	releaseThird, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)
	releaseSecond()
	releaseThird()
}

func TestClaudeCodeOAuthResponseBodyReleasesSlotOnce(t *testing.T) {
	originalMaxConcurrency := ClaudeCodeOAuthMaxConcurrency
	ClaudeCodeOAuthMaxConcurrency = 2
	t.Cleanup(func() { ClaudeCodeOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 900002
	release, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)
	body := &claudeCodeOAuthResponseBody{
		ReadCloser: io.NopCloser(strings.NewReader("ok")),
		release:    release,
	}

	require.NoError(t, body.Close())
	require.NoError(t, body.Close())

	first, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)
	second, ok := acquireClaudeCodeOAuthSlot(channelID)
	require.True(t, ok)
	_, ok = acquireClaudeCodeOAuthSlot(channelID)
	require.False(t, ok)
	first()
	second()
}

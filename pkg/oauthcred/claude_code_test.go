package oauthcred

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeClaudeCodeToken(t *testing.T) {
	for _, input := range []string{
		"sk-ant-oat-token",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-token",
		`CLAUDE_CODE_OAUTH_TOKEN= "sk-ant-oat-token"`,
		`export CLAUDE_CODE_OAUTH_TOKEN='sk-ant-oat-token'`,
	} {
		require.Equal(t, "sk-ant-oat-token", NormalizeClaudeCodeToken(input))
	}
}

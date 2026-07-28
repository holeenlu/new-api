package types

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReclassifiedClaudeErrorPreservesProtocolTypeAndGatewayCode(t *testing.T) {
	upstream := WithClaudeError(ClaudeError{
		Type:    "rate_limit_error",
		Message: "temporary limit",
	}, http.StatusTooManyRequests)

	classified := upstream.Reclassify(errors.New("temporary limit"), ErrorCodeUpstreamRateLimited)
	got := classified.ToClaudeError()

	require.Equal(t, "rate_limit_error", got.Type)
	require.Equal(t, string(ErrorCodeUpstreamRateLimited), got.Code)
	require.Equal(t, "temporary limit", got.Message)
}

func TestGatewayClaudeErrorUsesAnthropicCompatibleType(t *testing.T) {
	err := NewErrorWithStatusCode(errors.New("credential expired"), ErrorCodeOAuthUnauthorized, http.StatusUnauthorized)

	got := err.ToClaudeError()

	require.Equal(t, "authentication_error", got.Type)
	require.Equal(t, string(ErrorCodeOAuthUnauthorized), got.Code)
}

func TestMaskSensitiveErrorRedactsCredentialShapes(t *testing.T) {
	input := `Authorization: Bearer bearer-secret {"access_token":"access-secret","refresh_token":"refresh-secret"} CLAUDE_CODE_OAUTH_TOKEN=claude-secret upstream=https://example.com/v1?key=query-secret proxy=socks5://proxy-user:proxy-secret@127.0.0.1:1080`
	apiErr := NewErrorWithStatusCode(errors.New(input), ErrorCodeBadResponseStatusCode, http.StatusBadGateway)

	masked := apiErr.MaskSensitiveError()

	for _, secret := range []string{
		"bearer-secret",
		"access-secret",
		"refresh-secret",
		"claude-secret",
		"query-secret",
		"proxy-secret",
	} {
		assert.NotContains(t, masked, secret)
	}
}

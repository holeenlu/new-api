package types

import (
	"errors"
	"net/http"
	"testing"

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

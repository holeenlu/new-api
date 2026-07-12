package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

func TestApplyChannelErrorPolicySkipsClaudeCodeRetries(t *testing.T) {
	err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

	require.Same(t, err, got)
	require.True(t, types.IsSkipRetryError(got))
}

func TestApplyChannelErrorPolicyLeavesOtherChannelsUnchanged(t *testing.T) {
	err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeAnthropic, err)

	require.Same(t, err, got)
	require.False(t, types.IsSkipRetryError(got))
}

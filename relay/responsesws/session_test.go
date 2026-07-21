package responsesws

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResetChannelForRetryClearsSessionBinding(t *testing.T) {
	session := &Session{
		channelID:    41,
		model:        "gpt-test",
		httpFallback: true,
		pendingID:    42,
		pendingModel: "gpt-backup",
	}

	session.ResetChannelForRetry()

	require.Zero(t, session.ChannelID())
	require.Empty(t, session.model)
	require.False(t, session.httpFallback)
	require.Zero(t, session.pendingID)
	require.Empty(t, session.pendingModel)
}

package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingRedisHonorsDisabledAndUninitializedStates(t *testing.T) {
	originalEnabled, originalClient := RedisEnabled, RDB
	t.Cleanup(func() {
		RedisEnabled = originalEnabled
		RDB = originalClient
	})

	RedisEnabled = false
	RDB = nil
	require.NoError(t, PingRedis(context.Background()))

	RedisEnabled = true
	require.Error(t, PingRedis(context.Background()))
}

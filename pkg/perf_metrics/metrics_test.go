package perfmetrics

import (
	"errors"
	"net/http"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotRelaySampleDoesNotRetainMutableRelayInfo(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	info := &relaycommon.RelayInfo{
		OriginModelName:   "mapped-compact-model",
		UsingGroup:        "paid",
		IsStream:          true,
		StartTime:         start,
		FirstResponseTime: start.Add(time.Second),
	}

	sample, ok := SnapshotRelaySample(info, true, 17)
	require.True(t, ok)

	info.OriginModelName = "restored-client-model"
	info.UsingGroup = "changed"
	info.StartTime = time.Now()
	info.FirstResponseTime = time.Time{}

	assert.Equal(t, "mapped-compact-model", sample.Model)
	assert.Equal(t, "paid", sample.Group)
	assert.True(t, sample.Success)
	assert.Equal(t, int64(17), sample.OutputTokens)
	assert.True(t, sample.HasTtft)
	assert.GreaterOrEqual(t, sample.TtftMs, int64(1000))
}

func TestSnapshotRelaySampleTreatsCommittedUpstreamErrorAsFailure(t *testing.T) {
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-test",
		UsingGroup:      "default",
		StartTime:       time.Now(),
	}
	info.MarkCommittedUpstreamError(types.NewErrorWithStatusCode(
		errors.New("upstream terminal failure"),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	))

	sample, ok := SnapshotRelaySample(info, true, 9)

	require.True(t, ok)
	assert.False(t, sample.Success)
	assert.Equal(t, int64(9), sample.OutputTokens)
}

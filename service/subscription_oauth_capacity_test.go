package service

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionOAuthLocalCapacityUsesCredentialGlobalLimit(t *testing.T) {
	first := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		1,
		0,
		`{"access_token":"token-a","account_id":"global-account"}`,
	)
	second := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		2,
		0,
		`{"access_token":"token-b","account_id":"global-account"}`,
	)
	replaceSubscriptionOAuthStateForTest(t, first)
	require.Equal(t, first, second)

	leases := make([]*SubscriptionOAuthLease, 0, 10)
	for range 10 {
		lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), first, 10, 0)
		require.NoError(t, err)
		leases = append(leases, lease)
	}
	_, err := AcquireSubscriptionOAuthCapacity(context.Background(), second, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCapacityReached)
	for _, lease := range leases {
		lease.Release()
	}
}

func TestSubscriptionOAuthCapacityIsSafeUnderConcurrentAcquire(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"concurrent-account"}`)
	replaceSubscriptionOAuthStateForTest(t, fingerprint)

	start := make(chan struct{})
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	var acquired atomic.Int32
	var wg sync.WaitGroup
	attempted := make(chan struct{}, 40)
	results := make(chan error, 40)
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
			attempted <- struct{}{}
			if err != nil {
				results <- err
				return
			}
			acquired.Add(1)
			current := active.Add(1)
			for {
				previous := peak.Load()
				if current <= previous || peak.CompareAndSwap(previous, current) {
					break
				}
			}
			<-release
			active.Add(-1)
			lease.Release()
			results <- nil
		}()
	}
	close(start)
	for range 40 {
		<-attempted
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			require.ErrorIs(t, err, errSubscriptionOAuthCapacityReached)
		}
	}
	require.EqualValues(t, 10, acquired.Load())
	require.LessOrEqual(t, peak.Load(), int32(10))
}

func TestSubscriptionOAuthCooldownAllowsOnlyOneProbePerWindow(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 1, 0, "sk-ant-oat01-half-open")
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Hour))

	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	probe, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.True(t, probe.IsRecoveryProbe())

	_, err = AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)
	probe.Release()
	_, err = AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)

	require.True(t, MarkSubscriptionOAuthCredentialHealthy(fingerprint, probe.Generation()))
	next, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.False(t, next.IsRecoveryProbe())
	next.Release()
}

func TestSubscriptionOAuthManagementCallDoesNotConsumeRecoveryProbe(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"management-recovery-account"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Hour))
	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	_, err := AcquireSubscriptionOAuthManagementCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)

	state.mu.Lock()
	require.Zero(t, state.active)
	require.True(t, state.recoveryPending)
	require.False(t, state.recoveryInFlight)
	state.mu.Unlock()

	probe, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.True(t, probe.IsRecoveryProbe())
	probe.Release()
}

func TestSubscriptionOAuthCooldownReportsRemainingRetryAfter(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"retry-after-account"}`)
	replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, 12*time.Second))

	_, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)
	retryAfter := SubscriptionOAuthCapacityRetryAfter(err)
	require.Greater(t, retryAfter, 10*time.Second)
	require.LessOrEqual(t, retryAfter, 12*time.Second)
	retryAfterSeconds := SubscriptionOAuthCapacityRetryAfterSeconds(err)
	require.GreaterOrEqual(t, retryAfterSeconds, 11)
	require.LessOrEqual(t, retryAfterSeconds, 12)
}

func TestSubscriptionOAuthCancelledPacingAbandonsRecoveryProbe(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 1, 0, "sk-ant-oat01-abandoned-probe")
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Minute))
	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.nextStart = time.Now().Add(time.Second)
	state.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AcquireSubscriptionOAuthCapacity(ctx, fingerprint, 10, time.Second)
	require.ErrorIs(t, err, context.Canceled)

	state.mu.Lock()
	require.Zero(t, state.active)
	require.True(t, state.recoveryPending)
	require.False(t, state.recoveryInFlight)
	require.True(t, state.cooldownUntil.Before(time.Now()))
	state.nextStart = time.Time{}
	state.mu.Unlock()

	probe, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.True(t, probe.IsRecoveryProbe())
	probe.Release()
}

func TestSubscriptionOAuthSuccessfulResponseResolvesRecoveryBeforeRelease(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"response-recovery-account"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Hour))
	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	probe, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.True(t, probe.IsRecoveryProbe())
	probe.ReleaseResponseBody()
	state.mu.Lock()
	require.Equal(t, 1, state.active)
	require.True(t, state.recoveryInFlight)
	state.mu.Unlock()

	probe.ResolveResponse(true)
	state.mu.Lock()
	require.Zero(t, state.active)
	require.False(t, state.recoveryPending)
	require.False(t, state.recoveryInFlight)
	state.mu.Unlock()

	next, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	next.Release()
}

func TestResolveBoundSubscriptionOAuthResponseCompletesRecoveryProbe(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"bound-recovery-account"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Hour))
	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	probe, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	BindSubscriptionOAuthResponseLease(c, probe)

	ResolveBoundSubscriptionOAuthResponse(c, true)

	state.mu.Lock()
	require.Zero(t, state.active)
	require.False(t, state.recoveryPending)
	require.False(t, state.recoveryInFlight)
	state.mu.Unlock()
}

func TestSubscriptionOAuthStaleSuccessCannotClearNewCooldown(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"generation-account"}`)
	replaceSubscriptionOAuthStateForTest(t, fingerprint)
	lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)

	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, lease.Generation(), time.Minute))
	require.False(t, MarkSubscriptionOAuthCredentialHealthy(fingerprint, lease.Generation()))
	_, err = AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)
	lease.Release()
}

func TestSubscriptionOAuthOrdinarySuccessDoesNotInvalidateConcurrentFailure(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"concurrent-outcome-account"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	first, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	second, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.Equal(t, first.Generation(), second.Generation())

	first.ReleaseResponseBody()
	first.ResolveResponse(true)
	state.mu.Lock()
	require.EqualValues(t, 1, state.generation)
	state.mu.Unlock()

	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, second.Generation(), time.Minute))
	second.Release()
	_, err = AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)
}

func TestSubscriptionOAuthPacingPreservesReservedStartOrder(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 1, 0, "sk-ant-oat01-pacing")
	replaceSubscriptionOAuthStateForTest(t, fingerprint)
	first, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 20*time.Millisecond)
	require.NoError(t, err)
	first.Release()

	started := time.Now()
	second, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 20*time.Millisecond)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 15*time.Millisecond)
	second.Release()
}

func TestSubscriptionOAuthCleanupRetiresIdleStateBeforeReplacement(t *testing.T) {
	now := time.Now()
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 1, 0, `{"account_id":"idle-cleanup-account"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	state.mu.Lock()
	state.lastTouched = now.Add(-subscriptionOAuthStateRetention - time.Minute)
	state.mu.Unlock()
	loadedBeforeCleanup, ok := subscriptionOAuthLocalStates.Load(fingerprint)
	require.True(t, ok)
	require.Same(t, state, loadedBeforeCleanup)

	cleanupSubscriptionOAuthLocalStates(now, nil)

	_, ok = subscriptionOAuthLocalStates.Load(fingerprint)
	require.False(t, ok)
	state.mu.Lock()
	require.True(t, state.retired)
	state.mu.Unlock()

	lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 1, 0)
	require.NoError(t, err)
	loadedAfterCleanup, ok := subscriptionOAuthLocalStates.Load(fingerprint)
	require.True(t, ok)
	require.NotSame(t, state, loadedAfterCleanup)
	lease.Release()
}

func TestSubscriptionOAuthCleanupPreservesUnsafeStates(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		setup func(*subscriptionOAuthLocalState)
	}{
		{
			name: "active lease",
			setup: func(state *subscriptionOAuthLocalState) {
				state.active = 1
			},
		},
		{
			name: "recovery probe",
			setup: func(state *subscriptionOAuthLocalState) {
				state.recoveryInFlight = true
			},
		},
		{
			name: "cooldown",
			setup: func(state *subscriptionOAuthLocalState) {
				state.recoveryPending = true
				state.cooldownUntil = now.Add(time.Minute)
			},
		},
		{
			name: "pacing reservation",
			setup: func(state *subscriptionOAuthLocalState) {
				state.nextStart = now.Add(time.Second)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 100+index, 0, test.name)
			state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
			state.mu.Lock()
			state.lastTouched = now.Add(-subscriptionOAuthStateRetention - time.Minute)
			test.setup(state)
			state.mu.Unlock()

			cleanupSubscriptionOAuthLocalStates(now, nil)

			loaded, ok := subscriptionOAuthLocalStates.Load(fingerprint)
			require.True(t, ok)
			require.Same(t, state, loaded)
			state.mu.Lock()
			require.False(t, state.retired)
			state.mu.Unlock()
		})
	}
}

func TestSubscriptionOAuthCleanupReclaimsExpiredRecoveryStateAfterRetention(t *testing.T) {
	now := time.Now()
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 1, 0, "sk-ant-oat01-expired-recovery")
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	state.mu.Lock()
	state.lastTouched = now.Add(-subscriptionOAuthStateRetention - time.Minute)
	state.recoveryPending = true
	state.cooldownUntil = now.Add(-time.Minute)
	state.mu.Unlock()

	cleanupSubscriptionOAuthLocalStates(now, nil)

	_, ok := subscriptionOAuthLocalStates.Load(fingerprint)
	require.False(t, ok)
}

func TestSubscriptionOAuthCleanupPreservesConfiguredIdleCredential(t *testing.T) {
	now := time.Now()
	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 301, 0, `{"account_id":"configured-idle"}`)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	state.mu.Lock()
	state.lastTouched = now.Add(-subscriptionOAuthStateRetention - time.Minute)
	state.mu.Unlock()

	cleanupSubscriptionOAuthLocalStates(now, map[string]struct{}{fingerprint: {}})

	loaded, ok := subscriptionOAuthLocalStates.Load(fingerprint)
	require.True(t, ok)
	require.Same(t, state, loaded)
}

func TestSubscriptionOAuthFingerprintsForConfiguredChannels(t *testing.T) {
	codexKey := `{"access_token":"token-a","account_id":"configured-codex"}`
	claudeFirst := "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-first"
	claudeSecond := "sk-ant-oat01-second"
	channels := []*model.Channel{
		{Id: 401, Type: constant.ChannelTypeCodex, Key: codexKey},
		{
			Id:   402,
			Type: constant.ChannelTypeClaudeCode,
			Key:  claudeFirst + "\n" + claudeSecond,
			ChannelInfo: model.ChannelInfo{
				IsMultiKey: true,
			},
		},
		{Id: 403, Type: constant.ChannelTypeOpenAI, Key: "ordinary-api-key"},
	}

	fingerprints := subscriptionOAuthFingerprintsForChannels(channels)

	require.Len(t, fingerprints, 3)
	require.Contains(t, fingerprints, SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, 401, 0, codexKey))
	require.Contains(t, fingerprints, SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 402, 0, claudeFirst))
	require.Contains(t, fingerprints, SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 402, 1, claudeSecond))
}

func replaceSubscriptionOAuthStateForTest(t *testing.T, fingerprint string) *subscriptionOAuthLocalState {
	t.Helper()
	state := newSubscriptionOAuthLocalState(time.Now())
	previous, existed := subscriptionOAuthLocalStates.Load(fingerprint)
	subscriptionOAuthLocalStates.Store(fingerprint, state)
	t.Cleanup(func() {
		if existed {
			subscriptionOAuthLocalStates.Store(fingerprint, previous)
		} else {
			subscriptionOAuthLocalStates.Delete(fingerprint)
		}
	})
	return state
}

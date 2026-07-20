package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	subscriptionOAuthCredentialCooldown   = 30 * time.Second
	subscriptionOAuthStateRetention       = time.Hour
	subscriptionOAuthStateCleanupInterval = 10 * time.Minute
)

var (
	errSubscriptionOAuthCapacityReached = errors.New("subscription OAuth credential concurrency limit reached")
	errSubscriptionOAuthCredentialCool  = errors.New("subscription OAuth credential is temporarily unavailable")

	subscriptionOAuthLocalStates sync.Map
	subscriptionOAuthJanitorOnce sync.Once
	subscriptionOAuthJanitorDone = make(chan struct{})
)

type subscriptionOAuthCapacityError struct {
	cause      error
	retryAfter time.Duration
}

func (e *subscriptionOAuthCapacityError) Error() string {
	return e.cause.Error()
}

func (e *subscriptionOAuthCapacityError) Unwrap() error {
	return e.cause
}

type subscriptionOAuthLeaseOutcome int

const (
	subscriptionOAuthLeaseFailed subscriptionOAuthLeaseOutcome = iota
	subscriptionOAuthLeaseSucceeded
	subscriptionOAuthLeaseAbandoned
)

type subscriptionOAuthLocalState struct {
	mu sync.Mutex

	active      int
	nextStart   time.Time
	lastTouched time.Time
	retired     bool

	generation       uint64
	cooldownUntil    time.Time
	cooldownDuration time.Duration
	recoveryPending  bool
	recoveryInFlight bool
}

func newSubscriptionOAuthLocalState(now time.Time) *subscriptionOAuthLocalState {
	return &subscriptionOAuthLocalState{
		generation:       1,
		cooldownDuration: subscriptionOAuthCredentialCooldown,
		lastTouched:      now,
	}
}

// cleanupSubscriptionOAuthLocalStates retires idle entries before deleting
// them. A concurrent caller that already loaded the old pointer observes the
// retired flag under the same lock and retries against the map, preventing two
// independent concurrency counters for one credential.
func cleanupSubscriptionOAuthLocalStates(now time.Time, referencedFingerprints map[string]struct{}) {
	subscriptionOAuthLocalStates.Range(func(key, value any) bool {
		fingerprint, ok := key.(string)
		if !ok {
			return true
		}
		if _, referenced := referencedFingerprints[fingerprint]; referenced {
			return true
		}
		state, ok := value.(*subscriptionOAuthLocalState)
		if !ok || state == nil {
			return true
		}
		state.mu.Lock()
		idleLongEnough := state.lastTouched.IsZero() || now.Sub(state.lastTouched) >= subscriptionOAuthStateRetention
		if state.retired || !idleLongEnough || state.active != 0 || state.recoveryInFlight ||
			state.cooldownUntil.After(now) || state.nextStart.After(now) {
			state.mu.Unlock()
			return true
		}
		state.retired = true
		subscriptionOAuthLocalStates.CompareAndDelete(key, state)
		state.mu.Unlock()
		return true
	})
}

func configuredSubscriptionOAuthFingerprints() (map[string]struct{}, error) {
	var channels []*model.Channel
	err := model.DB.
		Select("id", "type", "key", "channel_info").
		Where("type IN ?", []int{constant.ChannelTypeCodex, constant.ChannelTypeClaudeCode}).
		Find(&channels).Error
	if err != nil {
		return nil, err
	}
	return subscriptionOAuthFingerprintsForChannels(channels), nil
}

func subscriptionOAuthFingerprintsForChannels(channels []*model.Channel) map[string]struct{} {
	fingerprints := make(map[string]struct{})
	for _, channel := range channels {
		if channel == nil || !constant.IsSubscriptionOAuthChannel(channel.Type) ||
			strings.TrimSpace(channel.Key) == "" {
			continue
		}
		keys := []string{channel.Key}
		if channel.ChannelInfo.IsMultiKey {
			keys = channel.GetKeys()
		}
		for index, key := range keys {
			if strings.TrimSpace(key) == "" {
				continue
			}
			fingerprint := SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, index, key)
			fingerprints[fingerprint] = struct{}{}
		}
	}
	return fingerprints
}

// StartSubscriptionOAuthStateJanitor starts one process-local conditional
// cleanup loop. Every gateway process owns an independent state map, so this
// task intentionally runs on every node rather than only on the master node.
func StartSubscriptionOAuthStateJanitor(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	subscriptionOAuthJanitorOnce.Do(func() {
		gopool.Go(func() {
			defer close(subscriptionOAuthJanitorDone)
			ticker := time.NewTicker(subscriptionOAuthStateCleanupInterval)
			defer ticker.Stop()
			for {
				select {
				case now := <-ticker.C:
					referencedFingerprints, err := configuredSubscriptionOAuthFingerprints()
					if err != nil {
						common.SysError("subscription OAuth state cleanup: load configured credentials failed: " + err.Error())
						continue
					}
					cleanupSubscriptionOAuthLocalStates(now, referencedFingerprints)
				case <-ctx.Done():
					return
				}
			}
		})
	})
	return subscriptionOAuthJanitorDone
}

// SubscriptionOAuthLease owns one credential-scoped concurrency slot. Release
// is idempotent and must be called when the upstream response body or WebSocket
// session is closed.
type SubscriptionOAuthLease struct {
	releaseOnce   sync.Once
	resultOnce    sync.Once
	release       func(subscriptionOAuthLeaseOutcome)
	generation    uint64
	recoveryProbe bool
	fingerprint   string
}

func (l *SubscriptionOAuthLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.releaseOnce.Do(func() { l.release(subscriptionOAuthLeaseFailed) })
}

// Abandon releases capacity without treating a recovery probe as failed. It is
// used only when no request reached the provider, such as cancellation while
// waiting for the credential's minimum request interval.
func (l *SubscriptionOAuthLease) Abandon() {
	if l == nil || l.release == nil {
		return
	}
	l.releaseOnce.Do(func() { l.release(subscriptionOAuthLeaseAbandoned) })
}

// FinishFailedAttempt releases the lease after a failed request attempt. A
// request that reached the provider (written) is Released so a recovery probe
// counts as a real failure; one that never reached it is Abandoned so the
// circuit stays retryable. This encodes the release invariant once for every
// subscription OAuth adaptor.
func (l *SubscriptionOAuthLease) FinishFailedAttempt(written bool) {
	if written {
		l.Release()
		return
	}
	l.Abandon()
}

// ReleaseResponseBody releases ordinary request capacity as soon as the body
// closes. A half-open recovery probe is retained until ResolveResponse records
// its outcome, preventing a successful probe from briefly reopening cooldown.
func (l *SubscriptionOAuthLease) ReleaseResponseBody() {
	if l == nil || l.recoveryProbe {
		return
	}
	l.Release()
}

// ResolveResponse records the final HTTP/SSE outcome. Recovery probes update
// circuit state and release capacity in one critical section. Ordinary leases
// are already released by the response body and only need a success marker.
func (l *SubscriptionOAuthLease) ResolveResponse(success bool) {
	if l == nil {
		return
	}
	l.resultOnce.Do(func() {
		if l.recoveryProbe {
			outcome := subscriptionOAuthLeaseFailed
			if success {
				outcome = subscriptionOAuthLeaseSucceeded
			}
			l.releaseOnce.Do(func() { l.release(outcome) })
			return
		}
		if success {
			MarkSubscriptionOAuthCredentialHealthy(l.fingerprint, l.generation)
		}
	})
}

func (l *SubscriptionOAuthLease) Generation() uint64 {
	if l == nil {
		return 0
	}
	return l.generation
}

func (l *SubscriptionOAuthLease) IsRecoveryProbe() bool {
	return l != nil && l.recoveryProbe
}

// SubscriptionOAuthCredentialFingerprint returns a stable, non-secret identity
// for concurrency, retry and cooldown decisions. Codex account_id is preferred;
// Claude Code falls back to the normalized OAuth token because that protocol
// does not expose a separate account identifier.
func SubscriptionOAuthCredentialFingerprint(channelType, channelID, keyIndex int, key string) string {
	return model.SubscriptionOAuthCredentialFingerprint(channelType, channelID, keyIndex, key)
}

// AcquireSubscriptionOAuthCapacity reserves one process-local slot for a
// credential and enforces its request-start interval. Each deployed server runs
// one gateway process, so servers intentionally maintain independent limits.
func AcquireSubscriptionOAuthCapacity(
	ctx context.Context,
	fingerprint string,
	maxConcurrency int,
	minRequestInterval time.Duration,
) (*SubscriptionOAuthLease, error) {
	return acquireSubscriptionOAuthCapacity(ctx, fingerprint, maxConcurrency, minRequestInterval, true)
}

// AcquireSubscriptionOAuthManagementCapacity shares credential capacity and
// pacing with inference requests without consuming the half-open recovery
// probe or changing circuit state.
func AcquireSubscriptionOAuthManagementCapacity(
	ctx context.Context,
	fingerprint string,
	maxConcurrency int,
	minRequestInterval time.Duration,
) (*SubscriptionOAuthLease, error) {
	return acquireSubscriptionOAuthCapacity(ctx, fingerprint, maxConcurrency, minRequestInterval, false)
}

func acquireSubscriptionOAuthCapacity(
	ctx context.Context,
	fingerprint string,
	maxConcurrency int,
	minRequestInterval time.Duration,
	allowRecoveryProbe bool,
) (*SubscriptionOAuthLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(fingerprint) == "" {
		return nil, errors.New("subscription OAuth credential fingerprint is empty")
	}
	if maxConcurrency < 1 {
		maxConcurrency = 1
	} else if maxConcurrency > 10 {
		maxConcurrency = 10
	}
	if minRequestInterval < 0 {
		minRequestInterval = 0
	} else if minRequestInterval > 5*time.Second {
		minRequestInterval = 5 * time.Second
	}

	now := time.Now()
	var state *subscriptionOAuthLocalState
	for {
		value, _ := subscriptionOAuthLocalStates.LoadOrStore(fingerprint, newSubscriptionOAuthLocalState(now))
		state = value.(*subscriptionOAuthLocalState)
		state.mu.Lock()
		if state.retired {
			state.mu.Unlock()
			continue
		}
		state.lastTouched = now
		break
	}
	if state.generation == 0 {
		state.generation = 1
	}
	if state.recoveryPending {
		if state.cooldownUntil.After(now) {
			retryAfter := time.Until(state.cooldownUntil)
			state.mu.Unlock()
			return nil, &subscriptionOAuthCapacityError{
				cause:      errSubscriptionOAuthCredentialCool,
				retryAfter: retryAfter,
			}
		}
		if !allowRecoveryProbe {
			state.mu.Unlock()
			return nil, &subscriptionOAuthCapacityError{
				cause:      errSubscriptionOAuthCredentialCool,
				retryAfter: time.Second,
			}
		}
		if state.recoveryInFlight {
			state.mu.Unlock()
			return nil, &subscriptionOAuthCapacityError{
				cause:      errSubscriptionOAuthCredentialCool,
				retryAfter: time.Second,
			}
		}
	}
	if state.active >= maxConcurrency {
		state.mu.Unlock()
		return nil, errSubscriptionOAuthCapacityReached
	}

	recoveryProbe := state.recoveryPending
	if recoveryProbe {
		state.recoveryInFlight = true
	}
	state.active++
	generation := state.generation
	startAt := now
	if state.nextStart.After(startAt) {
		startAt = state.nextStart
	}
	previousNextStart := state.nextStart
	advancedNextStart := startAt.Add(minRequestInterval)
	state.nextStart = advancedNextStart
	state.mu.Unlock()

	lease := &SubscriptionOAuthLease{
		generation:    generation,
		recoveryProbe: recoveryProbe,
		fingerprint:   fingerprint,
	}
	lease.release = func(outcome subscriptionOAuthLeaseOutcome) {
		state.mu.Lock()
		if state.active > 0 {
			state.active--
		}
		now := time.Now()
		state.lastTouched = now
		if outcome == subscriptionOAuthLeaseAbandoned && minRequestInterval > 0 &&
			state.nextStart.Equal(advancedNextStart) {
			// This attempt never reached the provider and we were the last to
			// advance the pacing cursor, so return the reserved slot; otherwise a
			// cancelled request permanently delays the next request on the
			// credential.
			state.nextStart = previousNextStart
		}
		if recoveryProbe && state.generation == generation && state.recoveryInFlight {
			state.recoveryInFlight = false
			switch outcome {
			case subscriptionOAuthLeaseSucceeded:
				state.generation++
				state.cooldownUntil = time.Time{}
				state.recoveryPending = false
			case subscriptionOAuthLeaseAbandoned:
				// Keep the circuit half-open and immediately eligible for another
				// probe because this attempt never reached the provider.
				state.recoveryPending = true
			case subscriptionOAuthLeaseFailed:
				state.recoveryPending = true
				duration := state.cooldownDuration
				if duration <= 0 {
					duration = subscriptionOAuthCredentialCooldown
				}
				state.cooldownUntil = now.Add(duration)
			}
		}
		state.mu.Unlock()
	}

	if delay := time.Until(startAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			lease.Abandon()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return lease, nil
}

func IsSubscriptionOAuthCapacityError(err error) bool {
	return errors.Is(err, errSubscriptionOAuthCapacityReached) || errors.Is(err, errSubscriptionOAuthCredentialCool)
}

// SubscriptionOAuthCapacityRetryAfter returns when a rejected credential can
// reasonably be retried. Active-concurrency saturation has no exact release
// time, so callers retain the one-second fallback.
func SubscriptionOAuthCapacityRetryAfter(err error) time.Duration {
	var capacityError *subscriptionOAuthCapacityError
	if errors.As(err, &capacityError) && capacityError.retryAfter > 0 {
		return capacityError.retryAfter
	}
	return time.Second
}

func SubscriptionOAuthCapacityRetryAfterSeconds(err error) int {
	retryAfter := SubscriptionOAuthCapacityRetryAfter(err)
	return max(int((retryAfter+time.Second-1)/time.Second), 1)
}

// CooldownSubscriptionOAuthCredential opens the credential circuit only when
// the failure belongs to the current generation. A zero expected generation is
// reserved for administrative/test use and applies unconditionally.
func CooldownSubscriptionOAuthCredential(fingerprint string, expectedGeneration uint64, duration time.Duration) bool {
	if strings.TrimSpace(fingerprint) == "" || duration <= 0 {
		return false
	}
	now := time.Now()
	var state *subscriptionOAuthLocalState
	for {
		value, _ := subscriptionOAuthLocalStates.LoadOrStore(fingerprint, newSubscriptionOAuthLocalState(now))
		state = value.(*subscriptionOAuthLocalState)
		state.mu.Lock()
		if state.retired {
			state.mu.Unlock()
			continue
		}
		break
	}
	defer state.mu.Unlock()
	state.lastTouched = now
	if state.generation == 0 {
		state.generation = 1
	}
	if expectedGeneration != 0 && state.generation != expectedGeneration {
		return false
	}
	state.generation++
	state.cooldownDuration = duration
	state.cooldownUntil = now.Add(duration)
	state.recoveryPending = true
	state.recoveryInFlight = false
	return true
}

// MarkSubscriptionOAuthCredentialHealthy closes only the circuit generation
// observed by the successful attempt. An older in-flight success therefore
// cannot erase a newer cooldown.
func MarkSubscriptionOAuthCredentialHealthy(fingerprint string, expectedGeneration uint64) bool {
	if strings.TrimSpace(fingerprint) == "" || expectedGeneration == 0 {
		return false
	}
	value, ok := subscriptionOAuthLocalStates.Load(fingerprint)
	if !ok {
		return false
	}
	state := value.(*subscriptionOAuthLocalState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired {
		return false
	}
	state.lastTouched = time.Now()
	if state.generation != expectedGeneration {
		return false
	}
	if !state.recoveryPending && !state.recoveryInFlight && state.cooldownUntil.IsZero() {
		return true
	}
	state.generation++
	state.cooldownUntil = time.Time{}
	state.recoveryPending = false
	state.recoveryInFlight = false
	return true
}

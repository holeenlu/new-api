package service

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const maximumSubscriptionOAuthRetryAfter = 15 * time.Minute

const maximumSubscriptionOAuthRequestAttempts = 10

// A plan/usage-limit exhaustion (multi-hour or weekly cap) resets in hours-to-days,
// so its credential is cooled down far longer than a transient burst 429. The
// default applies when the upstream sends no usable reset window; the maximum
// caps a misread window so a credential cannot be sidelined indefinitely — the
// next probe after expiry self-corrects if the account is still limited.
const (
	subscriptionOAuthUsageLimitCooldown        = 1 * time.Hour
	maximumSubscriptionOAuthUsageLimitCooldown = 8 * 24 * time.Hour
)

type SubscriptionOAuthRetryDecision int

const (
	SubscriptionOAuthRetryStop SubscriptionOAuthRetryDecision = iota
	SubscriptionOAuthRetryCurrentCredential
	SubscriptionOAuthSwitchCredential
)

// SubscriptionOAuthAttemptReservation explains why a credential was or was not
// reserved for the next upstream attempt. The relay must distinguish a
// per-credential exhaustion (which should select a backup) from the request
// ceiling (which must finish the client request).
type SubscriptionOAuthAttemptReservation int

const (
	SubscriptionOAuthAttemptReserved SubscriptionOAuthAttemptReservation = iota
	SubscriptionOAuthAttemptCredentialExhausted
	SubscriptionOAuthAttemptRequestExhausted
)

type SubscriptionOAuthAttemptTarget struct {
	ChannelID      int
	KeyIndex       int
	Fingerprint    string
	Generation     uint64
	RecoveryProbe  bool
	ResponseScoped bool
	lease          *SubscriptionOAuthLease
}

type subscriptionOAuthCredentialRetry struct {
	attempts int
	failures int
}

type subscriptionOAuthTracker struct {
	current        *SubscriptionOAuthAttemptTarget
	credentials    map[string]*subscriptionOAuthCredentialRetry
	refreshTried   map[string]struct{}
	capacityCycles int
	capacityWait   time.Duration
	capacityOrder  []SubscriptionOAuthAttemptTarget
	capacitySeen   map[string]struct{}
	capacityReplay bool
	capacityCursor int
}

type SubscriptionOAuthRetryObservation struct {
	ChannelType             int
	Error                   *types.NewAPIError
	UpstreamRequestWritten  bool
	UpstreamResponseStarted bool
	ExplicitFailureResponse bool
	DownstreamStarted       bool
	RetryDisabled           bool
	SpecificChannel         bool
	Retryable               bool
}

func DisableSubscriptionOAuthRetry(c *gin.Context) {
	if state := subscriptionOAuthState(c, true); state != nil {
		state.retryDisabled = true
	}
}

func IsSubscriptionOAuthRetryDisabled(c *gin.Context) bool {
	state := subscriptionOAuthState(c, false)
	return state != nil && state.retryDisabled
}

func BindSubscriptionOAuthLease(c *gin.Context, lease *SubscriptionOAuthLease) {
	if c == nil || lease == nil {
		return
	}
	state := subscriptionOAuthState(c, true)
	if state.attempt == nil {
		state.attempt = &SubscriptionOAuthAttemptTarget{}
	}
	state.attempt.Generation = lease.Generation()
	state.attempt.RecoveryProbe = lease.IsRecoveryProbe()
	state.attempt.lease = lease
}

func BindSubscriptionOAuthResponseLease(c *gin.Context, lease *SubscriptionOAuthLease) {
	BindSubscriptionOAuthLease(c, lease)
	if state := subscriptionOAuthState(c, false); state != nil && lease != nil {
		state.attempt.ResponseScoped = true
	}
}

func ClearSubscriptionOAuthAttemptMetadata(c *gin.Context) {
	if c == nil {
		return
	}
	state := subscriptionOAuthState(c, true)
	state.attempt = nil
}

// ResolveBoundSubscriptionOAuthResponse records the outcome of a response
// lease outside the normal relay retry loop, such as an administrator channel
// test. This is required for half-open recovery probes, whose capacity remains
// reserved until the response outcome is known.
func ResolveBoundSubscriptionOAuthResponse(c *gin.Context, success bool) {
	state := subscriptionOAuthState(c, false)
	if state == nil || state.attempt == nil || !state.attempt.ResponseScoped {
		return
	}
	if state.attempt.lease != nil {
		state.attempt.lease.ResolveResponse(success)
	}
}

func (p *RetryParam) ensureSubscriptionOAuthTracker() *subscriptionOAuthTracker {
	if p.oauth == nil {
		p.oauth = &subscriptionOAuthTracker{
			credentials:  make(map[string]*subscriptionOAuthCredentialRetry),
			refreshTried: make(map[string]struct{}),
			capacitySeen: make(map[string]struct{}),
		}
	}
	return p.oauth
}

// ClaimSubscriptionOAuthCredentialRefresh permits one refresh attempt for the
// current credential during a client request. The credential fingerprint is
// stable across Codex access-token rotation, so concurrent retry branches in
// the same request cannot refresh repeatedly.
func (p *RetryParam) ClaimSubscriptionOAuthCredentialRefresh() bool {
	if p == nil || p.totalAttempts >= maximumSubscriptionOAuthRequestAttempts {
		return false
	}
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil || target.Fingerprint == "" {
		return false
	}
	tracker := p.ensureSubscriptionOAuthTracker()
	if _, exists := tracker.refreshTried[target.Fingerprint]; exists {
		return false
	}
	tracker.refreshTried[target.Fingerprint] = struct{}{}
	return true
}

func (p *RetryParam) NextCapacityCycleBackoff() (time.Duration, bool) {
	tracker := p.ensureSubscriptionOAuthTracker()
	if tracker.capacityCycles == 0 {
		tracker.capacityCycles = 1
	}
	if tracker.capacityCycles >= common.SubscriptionOAuthCapacityCycleTimes {
		return 0, false
	}
	maxWait := time.Duration(common.SubscriptionOAuthCapacityWaitSeconds) * time.Second
	remaining := maxWait - tracker.capacityWait
	if remaining <= 0 {
		return 0, false
	}
	tracker.capacityCycles++
	shift := min(tracker.capacityCycles-2, 3)
	base := 250 * time.Millisecond * time.Duration(1<<shift)
	jitterLimit := min(int(base/(4*time.Millisecond)), 100)
	delay := base + time.Duration(common.GetRandomInt(jitterLimit+1))*time.Millisecond
	if delay > remaining {
		delay = remaining
	}
	tracker.capacityWait += delay
	return delay, true
}

func (p *RetryParam) recordCapacityTarget(target *SubscriptionOAuthAttemptTarget) {
	if p == nil || target == nil || target.Fingerprint == "" {
		return
	}
	tracker := p.ensureSubscriptionOAuthTracker()
	if _, exists := tracker.capacitySeen[target.Fingerprint]; exists {
		return
	}
	tracker.capacitySeen[target.Fingerprint] = struct{}{}
	tracker.capacityOrder = append(tracker.capacityOrder, SubscriptionOAuthAttemptTarget{
		ChannelID:   target.ChannelID,
		KeyIndex:    target.KeyIndex,
		Fingerprint: target.Fingerprint,
	})
}

func (p *RetryParam) advanceCapacityReplay() {
	if p == nil || p.oauth == nil {
		return
	}
	tracker := p.oauth
	if !tracker.capacityReplay || tracker.capacityCursor >= len(tracker.capacityOrder) {
		tracker.capacityReplay = false
		return
	}
	target := tracker.capacityOrder[tracker.capacityCursor]
	tracker.capacityCursor++
	p.setSubscriptionOAuthAttemptTarget(&target)
	if tracker.credentials[target.Fingerprint] == nil {
		tracker.credentials[target.Fingerprint] = &subscriptionOAuthCredentialRetry{}
	}
}

func (p *RetryParam) startCapacityReplay() bool {
	if p == nil || p.oauth == nil || len(p.oauth.capacityOrder) == 0 {
		return false
	}
	p.oauth.capacityReplay = true
	p.oauth.capacityCursor = 0
	p.advanceCapacityReplay()
	return p.SubscriptionOAuthAttemptTarget() != nil
}

// RestartSubscriptionOAuthCapacityCycle reopens only credentials excluded by
// local capacity and then replays them in their original selection order.
func (p *RetryParam) RestartSubscriptionOAuthCapacityCycle() bool {
	if p == nil || p.Boundary == nil || !p.Boundary.RestartCapacityCycle() {
		return false
	}
	return p.startCapacityReplay()
}

// ReserveSubscriptionOAuthAttempt reserves one request-local attempt for a
// credential. It is the final guard around every relay path: even if an error
// path fails before normal retry accounting, the same credential cannot be
// selected more than the configured per-credential limit.
func (p *RetryParam) ReserveSubscriptionOAuthAttempt(channelID, keyIndex int, fingerprint string) SubscriptionOAuthAttemptReservation {
	state := p.ensureSubscriptionOAuthTracker()
	if p.totalAttempts >= maximumSubscriptionOAuthRequestAttempts {
		p.clearSubscriptionOAuthAttemptTarget()
		return SubscriptionOAuthAttemptRequestExhausted
	}
	credential := state.credentials[fingerprint]
	if credential == nil {
		credential = &subscriptionOAuthCredentialRetry{}
		state.credentials[fingerprint] = credential
	}
	maxAttempts := common.SubscriptionOAuthUpstreamRetryTimes
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if credential.attempts >= maxAttempts {
		if p.Boundary != nil {
			p.Boundary.FailCredential(fingerprint)
		}
		p.clearSubscriptionOAuthAttemptTarget()
		return SubscriptionOAuthAttemptCredentialExhausted
	}
	credential.attempts++
	p.setSubscriptionOAuthAttemptTarget(&SubscriptionOAuthAttemptTarget{
		ChannelID:   channelID,
		KeyIndex:    keyIndex,
		Fingerprint: fingerprint,
	})
	return SubscriptionOAuthAttemptReserved
}

func (p *RetryParam) setSubscriptionOAuthAttemptTarget(target *SubscriptionOAuthAttemptTarget) {
	if p == nil || p.oauth == nil {
		return
	}
	p.oauth.current = target
	if p.Ctx != nil {
		if state := subscriptionOAuthState(p.Ctx, true); state != nil {
			state.attempt = target
		}
	}
}

func (p *RetryParam) clearSubscriptionOAuthAttemptTarget() {
	p.setSubscriptionOAuthAttemptTarget(nil)
}

func (p *RetryParam) SubscriptionOAuthAttemptTarget() *SubscriptionOAuthAttemptTarget {
	if p == nil || p.oauth == nil || p.oauth.current == nil {
		return nil
	}
	target := *p.oauth.current
	return &target
}

func (p *RetryParam) CaptureSubscriptionOAuthAttemptMetadata(c *gin.Context) {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil || c == nil {
		return
	}
	state := subscriptionOAuthState(c, false)
	if state != nil && state.attempt != p.oauth.current {
		state.attempt = p.oauth.current
	}
}

func (p *RetryParam) handleSubscriptionOAuthCapacityFailure() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.ExcludeCredential(target.Fingerprint, target.ChannelID, target.KeyIndex)
	}
	if credential := p.oauth.credentials[target.Fingerprint]; credential != nil && credential.attempts > 0 {
		credential.attempts--
	}
	p.recordCapacityTarget(target)
	p.clearSubscriptionOAuthAttemptTarget()
	p.advanceCapacityReplay()
}

func (p *RetryParam) handleSubscriptionOAuthKnownCooldown() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.FailCredential(target.Fingerprint)
	}
	p.clearSubscriptionOAuthAttemptTarget()
}

// RejectSubscriptionOAuthReplayTarget advances past a stale replay target
// that can no longer be loaded or matched to its original credential.
func (p *RetryParam) RejectSubscriptionOAuthReplayTarget() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	p.clearSubscriptionOAuthAttemptTarget()
	p.advanceCapacityReplay()
}

// handleSubscriptionOAuthAccountUnavailable removes an invalid or forbidden
// account from this request and opens its short circuit for subsequent requests.
func (p *RetryParam) handleSubscriptionOAuthAccountUnavailable() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown, subscriptionOAuthCooldownTransient, types.SubscriptionOAuthUsageWindows{})
}

// handleSubscriptionOAuthModelUnavailable excludes the credential only from
// the current request. Model entitlement is account- and model-specific, so it
// must not make the account unavailable for other models.
func (p *RetryParam) handleSubscriptionOAuthModelUnavailable() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.FailCredential(target.Fingerprint)
	}
	p.clearSubscriptionOAuthAttemptTarget()
	p.advanceCapacityReplay()
}

// handleSubscriptionOAuthModelCapacity excludes only the credential/model
// combination represented by this request. A provider model-capacity signal
// must not make unrelated models on the same account unavailable.
func (p *RetryParam) handleSubscriptionOAuthModelCapacity() {
	p.handleSubscriptionOAuthModelUnavailable()
}

// DecideSubscriptionOAuthContinuation is the single state transition entry
// for subscription OAuth retries. Callers report what happened; this method
// updates the credential ledger and returns whether to retry the current
// credential, switch credentials, or stop.
func (p *RetryParam) DecideSubscriptionOAuthContinuation(
	observation SubscriptionOAuthRetryObservation,
) (SubscriptionOAuthRetryDecision, time.Duration) {
	if p == nil || observation.Error == nil {
		return SubscriptionOAuthRetryStop, 0
	}

	stop := observation.DownstreamStarted || observation.RetryDisabled || observation.SpecificChannel ||
		p.totalAttempts >= maximumSubscriptionOAuthRequestAttempts
	if IsSubscriptionOAuthAccountUnavailable(observation.ChannelType, observation.Error) {
		p.handleSubscriptionOAuthAccountUnavailable()
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthModelAtCapacity(observation.ChannelType, observation.Error) {
		if !observation.DownstreamStarted {
			p.handleSubscriptionOAuthModelCapacity()
		}
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthModelUnavailable(observation.ChannelType, observation.Error) {
		if !observation.DownstreamStarted {
			p.handleSubscriptionOAuthModelUnavailable()
		}
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthUsageLimit(observation.ChannelType, observation.Error) {
		cooldown := SubscriptionOAuthCredentialCooldownForError(observation.ChannelType, observation.Error)
		if IsSubscriptionOAuthKnownCooldownFailure(observation.ChannelType, observation.Error) {
			p.handleSubscriptionOAuthKnownCooldown()
		} else {
			target := p.SubscriptionOAuthAttemptTarget()
			if target == nil {
				return SubscriptionOAuthRetryStop, cooldown
			}
			p.failCurrentSubscriptionOAuthCredential(target, cooldown, subscriptionOAuthCooldownUsageLimit, observation.Error.UsageWindows)
		}
		if stop {
			return SubscriptionOAuthRetryStop, cooldown
		}
		return SubscriptionOAuthSwitchCredential, cooldown
	}
	if IsSubscriptionOAuthKnownCooldownFailure(observation.ChannelType, observation.Error) {
		p.handleSubscriptionOAuthKnownCooldown()
		if stop {
			return SubscriptionOAuthRetryStop, observation.Error.RetryAfter
		}
		return SubscriptionOAuthSwitchCredential, observation.Error.RetryAfter
	}
	if IsSubscriptionOAuthActiveCapacityFailure(observation.ChannelType, observation.Error) {
		p.handleSubscriptionOAuthCapacityFailure()
		if stop {
			return SubscriptionOAuthRetryStop, observation.Error.RetryAfter
		}
		return SubscriptionOAuthSwitchCredential, observation.Error.RetryAfter
	}

	statusCode := observation.Error.GetUpstreamStatusCode()
	if statusCode == http.StatusTooManyRequests {
		cooldown := SubscriptionOAuthCredentialCooldownForError(observation.ChannelType, observation.Error)
		decision := p.decideSubscriptionOAuthRetry(
			observation.UpstreamRequestWritten,
			observation.UpstreamResponseStarted,
			observation.ExplicitFailureResponse,
			stop,
			statusCode,
			cooldown,
		)
		return decision, cooldown
	}
	if stop || !observation.Retryable {
		return SubscriptionOAuthRetryStop, 0
	}
	return p.decideSubscriptionOAuthRetry(
		observation.UpstreamRequestWritten,
		observation.UpstreamResponseStarted,
		observation.ExplicitFailureResponse,
		false,
		statusCode,
		0,
	), 0
}

// decideSubscriptionOAuthRetry applies a per-credential retry budget. A local
// capacity failure is handled separately and never consumes this budget.
func (p *RetryParam) decideSubscriptionOAuthRetry(
	written bool,
	responseStarted bool,
	explicitFailureResponse bool,
	downstreamStarted bool,
	statusCode int,
	cooldown time.Duration,
) SubscriptionOAuthRetryDecision {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return SubscriptionOAuthRetryStop
	}
	if statusCode == http.StatusTooManyRequests {
		p.failCurrentSubscriptionOAuthCredential(target, cooldown, subscriptionOAuthCooldownRateLimit, types.SubscriptionOAuthUsageWindows{})
		if downstreamStarted || !common.SubscriptionOAuthRetry429 {
			return SubscriptionOAuthRetryStop
		}
		return SubscriptionOAuthSwitchCredential
	}
	if downstreamStarted {
		return SubscriptionOAuthRetryStop
	}

	state := p.ensureSubscriptionOAuthTracker()
	credential := state.credentials[target.Fingerprint]
	if credential == nil {
		credential = &subscriptionOAuthCredentialRetry{}
		state.credentials[target.Fingerprint] = credential
	}
	credential.failures++
	if target.RecoveryProbe {
		p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown, subscriptionOAuthCooldownTransient, types.SubscriptionOAuthUsageWindows{})
		return SubscriptionOAuthSwitchCredential
	}
	maxFailures := common.SubscriptionOAuthUpstreamRetryTimes
	if maxFailures < 1 {
		return SubscriptionOAuthRetryStop
	}
	if credential.failures >= maxFailures {
		p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown, subscriptionOAuthCooldownTransient, types.SubscriptionOAuthUsageWindows{})
		return SubscriptionOAuthSwitchCredential
	}

	// Once bytes may have reached the provider, the gateway has no idempotency
	// proof. Replaying to this or another credential can duplicate execution or
	// billing, so preserve the original upstream error for the client.
	if written {
		return SubscriptionOAuthRetryStop
	}

	return SubscriptionOAuthRetryCurrentCredential
}

func SubscriptionOAuthRetryCooldown(retryAfter time.Duration) time.Duration {
	if retryAfter <= 0 {
		return subscriptionOAuthCredentialCooldown
	}
	if retryAfter > maximumSubscriptionOAuthRetryAfter {
		return maximumSubscriptionOAuthRetryAfter
	}
	return retryAfter
}

// SubscriptionOAuthCredentialCooldownForError picks how long a rejected
// credential stays out of routing. A plan/usage-limit exhaustion is cooled down
// for its exact reset window, or the conservative default when none is known,
// so requests fail over to other channels until it recovers, instead of
// the short transient-429 cooldown that re-probed the exhausted account within
// minutes. All other 429s keep the transient cooldown.
func SubscriptionOAuthCredentialCooldownForError(channelType int, err *types.NewAPIError) time.Duration {
	if err == nil {
		return subscriptionOAuthCredentialCooldown
	}
	if IsSubscriptionOAuthUsageLimit(channelType, err) {
		cooldown := err.RetryAfter
		if cooldown <= 0 {
			cooldown = subscriptionOAuthUsageLimitCooldown
		}
		if cooldown > maximumSubscriptionOAuthUsageLimitCooldown {
			cooldown = maximumSubscriptionOAuthUsageLimitCooldown
		}
		return cooldown
	}
	return SubscriptionOAuthRetryCooldown(err.RetryAfter)
}

func (p *RetryParam) failCurrentSubscriptionOAuthCredential(
	target *SubscriptionOAuthAttemptTarget,
	cooldown time.Duration,
	reason subscriptionOAuthCooldownReason,
	usageWindows types.SubscriptionOAuthUsageWindows,
) {
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.FailCredential(target.Fingerprint)
	}
	cooldownSubscriptionOAuthCredentialWithUsageWindows(target.Fingerprint, target.Generation, cooldown, reason, usageWindows)
	p.clearSubscriptionOAuthAttemptTarget()
	p.advanceCapacityReplay()
}

func (p *RetryParam) MarkSubscriptionOAuthSuccess() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if target.ResponseScoped && target.lease != nil {
		target.lease.ResolveResponse(true)
		return
	}
	MarkSubscriptionOAuthCredentialHealthy(target.Fingerprint, target.Generation)
}

func (p *RetryParam) MarkSubscriptionOAuthFailure() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target != nil && target.ResponseScoped && target.lease != nil {
		target.lease.ResolveResponse(false)
	}
}

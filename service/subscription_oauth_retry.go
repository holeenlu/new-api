package service

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const maximumSubscriptionOAuthRetryAfter = 15 * time.Minute

type SubscriptionOAuthRetryDecision int

const (
	SubscriptionOAuthRetryStop SubscriptionOAuthRetryDecision = iota
	SubscriptionOAuthRetryCurrentCredential
	SubscriptionOAuthSwitchCredential
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
	attempts         int
	failures         int
	postWriteRetries int
}

type SubscriptionOAuthRetryState struct {
	current     *SubscriptionOAuthAttemptTarget
	credentials map[string]*subscriptionOAuthCredentialRetry
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
	state.generation = lease.Generation()
	state.recoveryProbe = lease.IsRecoveryProbe()
	state.lease = lease
}

func BindSubscriptionOAuthResponseLease(c *gin.Context, lease *SubscriptionOAuthLease) {
	BindSubscriptionOAuthLease(c, lease)
	if state := subscriptionOAuthState(c, false); state != nil && lease != nil {
		state.responseScoped = true
	}
}

func ClearSubscriptionOAuthAttemptMetadata(c *gin.Context) {
	if c == nil {
		return
	}
	state := subscriptionOAuthState(c, true)
	state.generation = 0
	state.recoveryProbe = false
	state.lease = nil
	state.responseScoped = false
}

// ResolveBoundSubscriptionOAuthResponse records the outcome of a response
// lease outside the normal relay retry loop, such as an administrator channel
// test. This is required for half-open recovery probes, whose capacity remains
// reserved until the response outcome is known.
func ResolveBoundSubscriptionOAuthResponse(c *gin.Context, success bool) {
	state := subscriptionOAuthState(c, false)
	if state == nil || !state.responseScoped {
		return
	}
	if state.lease != nil {
		state.lease.ResolveResponse(success)
	}
}

func (p *RetryParam) ensureSubscriptionOAuthRetryState() *SubscriptionOAuthRetryState {
	if p.oauthRetry == nil {
		p.oauthRetry = &SubscriptionOAuthRetryState{
			credentials: make(map[string]*subscriptionOAuthCredentialRetry),
		}
	}
	return p.oauthRetry
}

// SetSubscriptionOAuthAttempt reserves one request-local attempt for a
// credential. It is the final guard around every relay path: even if an error
// path fails before normal retry accounting, the same credential cannot be
// selected more than the configured per-credential limit.
func (p *RetryParam) SetSubscriptionOAuthAttempt(channelID, keyIndex int, fingerprint string) bool {
	state := p.ensureSubscriptionOAuthRetryState()
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
		state.current = nil
		return false
	}
	credential.attempts++
	state.current = &SubscriptionOAuthAttemptTarget{
		ChannelID:   channelID,
		KeyIndex:    keyIndex,
		Fingerprint: fingerprint,
	}
	return true
}

func (p *RetryParam) SubscriptionOAuthAttemptTarget() *SubscriptionOAuthAttemptTarget {
	if p == nil || p.oauthRetry == nil || p.oauthRetry.current == nil {
		return nil
	}
	target := *p.oauthRetry.current
	return &target
}

func (p *RetryParam) CaptureSubscriptionOAuthAttemptMetadata(c *gin.Context) {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil || c == nil {
		return
	}
	state := subscriptionOAuthState(c, false)
	if state == nil {
		return
	}
	p.oauthRetry.current.Generation = state.generation
	p.oauthRetry.current.RecoveryProbe = state.recoveryProbe
	p.oauthRetry.current.lease = state.lease
	p.oauthRetry.current.ResponseScoped = state.responseScoped
}

func (p *RetryParam) HandleSubscriptionOAuthCapacityFailure() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.ExcludeCredential(target.Fingerprint, target.ChannelID, target.KeyIndex)
	}
	if credential := p.oauthRetry.credentials[target.Fingerprint]; credential != nil && credential.attempts > 0 {
		credential.attempts--
	}
	p.recordCapacityTarget(target)
	p.oauthRetry.current = nil
	p.advanceCapacityReplay()
}

func (p *RetryParam) HandleSubscriptionOAuthCredentialUnavailable() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	p.oauthRetry.current = nil
	p.advanceCapacityReplay()
}

// HandleSubscriptionOAuthAccountUnavailable removes an invalid or forbidden
// account from this request and opens its short circuit for subsequent requests.
func (p *RetryParam) HandleSubscriptionOAuthAccountUnavailable() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown)
}

// HandleSubscriptionOAuthModelUnavailable excludes the credential only from
// the current request. Model entitlement is account- and model-specific, so it
// must not make the account unavailable for other models.
func (p *RetryParam) HandleSubscriptionOAuthModelUnavailable() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.FailCredential(target.Fingerprint)
	}
	if p.oauthRetry != nil {
		p.oauthRetry.current = nil
	}
	p.advanceCapacityReplay()
}

// HandleSubscriptionOAuthModelCapacity removes a temporarily saturated
// credential from the current request and opens its short cooldown before the
// next same-group credential is selected. This is intentionally distinct from
// a model-entitlement failure, which is request-local and does not cool down
// the OAuth account.
func (p *RetryParam) HandleSubscriptionOAuthModelCapacity() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown)
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

	stop := observation.DownstreamStarted || observation.RetryDisabled || observation.SpecificChannel
	if IsSubscriptionOAuthAccountUnavailable(observation.ChannelType, observation.Error) {
		p.HandleSubscriptionOAuthAccountUnavailable()
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthModelAtCapacity(observation.ChannelType, observation.Error) {
		if !observation.DownstreamStarted {
			p.HandleSubscriptionOAuthModelCapacity()
		}
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthModelUnavailable(observation.ChannelType, observation.Error) {
		if !observation.DownstreamStarted {
			p.HandleSubscriptionOAuthModelUnavailable()
		}
		if stop {
			return SubscriptionOAuthRetryStop, 0
		}
		return SubscriptionOAuthSwitchCredential, 0
	}
	if IsSubscriptionOAuthConcurrencyLimit(observation.ChannelType, observation.Error) {
		p.HandleSubscriptionOAuthCapacityFailure()
		if stop {
			return SubscriptionOAuthRetryStop, observation.Error.RetryAfter
		}
		return SubscriptionOAuthSwitchCredential, observation.Error.RetryAfter
	}

	statusCode := observation.Error.GetUpstreamStatusCode()
	if statusCode == http.StatusTooManyRequests {
		retryAfter := SubscriptionOAuthRetryCooldown(observation.Error.RetryAfter)
		decision := p.DecideSubscriptionOAuthRetry(
			observation.UpstreamRequestWritten,
			observation.UpstreamResponseStarted,
			observation.ExplicitFailureResponse,
			stop,
			statusCode,
			observation.Error.RetryAfter,
		)
		return decision, retryAfter
	}
	if stop || !observation.Retryable {
		return SubscriptionOAuthRetryStop, 0
	}
	return p.DecideSubscriptionOAuthRetry(
		observation.UpstreamRequestWritten,
		observation.UpstreamResponseStarted,
		observation.ExplicitFailureResponse,
		false,
		statusCode,
		observation.Error.RetryAfter,
	), 0
}

// DecideSubscriptionOAuthRetry applies a per-credential retry budget. A local
// capacity failure is handled separately and never consumes this budget.
func (p *RetryParam) DecideSubscriptionOAuthRetry(
	written bool,
	responseStarted bool,
	explicitFailureResponse bool,
	downstreamStarted bool,
	statusCode int,
	retryAfter time.Duration,
) SubscriptionOAuthRetryDecision {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return SubscriptionOAuthRetryStop
	}
	if statusCode == http.StatusTooManyRequests {
		p.failCurrentSubscriptionOAuthCredential(target, SubscriptionOAuthRetryCooldown(retryAfter))
		if downstreamStarted || !common.SubscriptionOAuthRetry429 {
			return SubscriptionOAuthRetryStop
		}
		return SubscriptionOAuthSwitchCredential
	}
	if downstreamStarted {
		return SubscriptionOAuthRetryStop
	}

	state := p.ensureSubscriptionOAuthRetryState()
	credential := state.credentials[target.Fingerprint]
	if credential == nil {
		credential = &subscriptionOAuthCredentialRetry{}
		state.credentials[target.Fingerprint] = credential
	}
	credential.failures++
	if target.RecoveryProbe {
		p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown)
		return SubscriptionOAuthSwitchCredential
	}
	maxFailures := common.SubscriptionOAuthUpstreamRetryTimes
	if maxFailures < 1 {
		return SubscriptionOAuthRetryStop
	}
	if credential.failures >= maxFailures {
		p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown)
		return SubscriptionOAuthSwitchCredential
	}

	// A request that may already have reached the provider is allowed only one
	// ambiguous retry, and it still has to fit within the general failure budget.
	if written && (!responseStarted || !explicitFailureResponse) {
		if credential.postWriteRetries >= 1 {
			p.failCurrentSubscriptionOAuthCredential(target, subscriptionOAuthCredentialCooldown)
			return SubscriptionOAuthSwitchCredential
		}
		credential.postWriteRetries++
		return SubscriptionOAuthRetryCurrentCredential
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

func (p *RetryParam) failCurrentSubscriptionOAuthCredential(target *SubscriptionOAuthAttemptTarget, cooldown time.Duration) {
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.FailCredential(target.Fingerprint)
	}
	CooldownSubscriptionOAuthCredential(target.Fingerprint, target.Generation, cooldown)
	if p.oauthRetry != nil {
		p.oauthRetry.current = nil
	}
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

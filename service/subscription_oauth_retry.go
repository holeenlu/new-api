package service

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
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
	failures         int
	postWriteRetries int
}

type SubscriptionOAuthRetryState struct {
	current     *SubscriptionOAuthAttemptTarget
	credentials map[string]*subscriptionOAuthCredentialRetry
}

const (
	subscriptionOAuthGenerationContextKey = "subscription_oauth_capacity_generation"
	subscriptionOAuthRecoveryContextKey   = "subscription_oauth_capacity_recovery_probe"
	subscriptionOAuthLeaseContextKey      = "subscription_oauth_capacity_lease"
	subscriptionOAuthResponseContextKey   = "subscription_oauth_response_scoped"
)

func BindSubscriptionOAuthLease(c *gin.Context, lease *SubscriptionOAuthLease) {
	if c == nil || lease == nil {
		return
	}
	c.Set(subscriptionOAuthGenerationContextKey, lease.Generation())
	c.Set(subscriptionOAuthRecoveryContextKey, lease.IsRecoveryProbe())
	c.Set(subscriptionOAuthLeaseContextKey, lease)
}

func BindSubscriptionOAuthResponseLease(c *gin.Context, lease *SubscriptionOAuthLease) {
	BindSubscriptionOAuthLease(c, lease)
	if c != nil && lease != nil {
		c.Set(subscriptionOAuthResponseContextKey, true)
	}
}

func ClearSubscriptionOAuthAttemptMetadata(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(subscriptionOAuthGenerationContextKey, uint64(0))
	c.Set(subscriptionOAuthRecoveryContextKey, false)
	c.Set(subscriptionOAuthLeaseContextKey, (*SubscriptionOAuthLease)(nil))
	c.Set(subscriptionOAuthResponseContextKey, false)
}

// ResolveBoundSubscriptionOAuthResponse records the outcome of a response
// lease outside the normal relay retry loop, such as an administrator channel
// test. This is required for half-open recovery probes, whose capacity remains
// reserved until the response outcome is known.
func ResolveBoundSubscriptionOAuthResponse(c *gin.Context, success bool) {
	if c == nil || !c.GetBool(subscriptionOAuthResponseContextKey) {
		return
	}
	value, exists := c.Get(subscriptionOAuthLeaseContextKey)
	if !exists {
		return
	}
	lease, _ := value.(*SubscriptionOAuthLease)
	if lease != nil {
		lease.ResolveResponse(success)
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

func (p *RetryParam) SetSubscriptionOAuthAttempt(channelID, keyIndex int, fingerprint string) {
	state := p.ensureSubscriptionOAuthRetryState()
	state.current = &SubscriptionOAuthAttemptTarget{
		ChannelID:   channelID,
		KeyIndex:    keyIndex,
		Fingerprint: fingerprint,
	}
	if state.credentials[fingerprint] == nil {
		state.credentials[fingerprint] = &subscriptionOAuthCredentialRetry{}
	}
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
	if value, exists := c.Get(subscriptionOAuthGenerationContextKey); exists {
		if generation, ok := value.(uint64); ok {
			p.oauthRetry.current.Generation = generation
		}
	}
	p.oauthRetry.current.RecoveryProbe = c.GetBool(subscriptionOAuthRecoveryContextKey)
	if value, exists := c.Get(subscriptionOAuthLeaseContextKey); exists {
		p.oauthRetry.current.lease, _ = value.(*SubscriptionOAuthLease)
	}
	p.oauthRetry.current.ResponseScoped = c.GetBool(subscriptionOAuthResponseContextKey)
}

func (p *RetryParam) HandleSubscriptionOAuthCapacityFailure() {
	target := p.SubscriptionOAuthAttemptTarget()
	if target == nil {
		return
	}
	if p.Boundary != nil {
		p.Boundary.ExcludeCredential(target.Fingerprint, target.ChannelID, target.KeyIndex)
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

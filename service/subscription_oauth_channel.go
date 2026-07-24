package service

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// SubscriptionOAuthCapacityBounds are the supported ranges for per-credential
// concurrency and request pacing. Every subscription OAuth channel (Claude
// Code, Codex) shares these limits so their runtime behavior stays identical.
const (
	subscriptionOAuthMaxConcurrencyCeiling = 10
	subscriptionOAuthMinIntervalCeiling    = 5 * time.Second
)

// ClampSubscriptionOAuthCapacity normalizes configured concurrency and pacing
// values into the supported range. It is the single definition of those bounds
// shared by each adaptor's runtime-settings initializer and by capacity
// acquisition.
func ClampSubscriptionOAuthCapacity(maxConcurrency int, minRequestInterval time.Duration) (int, time.Duration) {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	} else if maxConcurrency > subscriptionOAuthMaxConcurrencyCeiling {
		maxConcurrency = subscriptionOAuthMaxConcurrencyCeiling
	}
	if minRequestInterval < 0 {
		minRequestInterval = 0
	} else if minRequestInterval > subscriptionOAuthMinIntervalCeiling {
		minRequestInterval = subscriptionOAuthMinIntervalCeiling
	}
	return maxConcurrency, minRequestInterval
}

// SubscriptionOAuthResponseBody wraps an upstream response body so its OAuth
// capacity lease is released when the body is closed. Shared by every
// subscription OAuth adaptor to avoid one wrapper type per channel.
type SubscriptionOAuthResponseBody struct {
	io.ReadCloser
	lease *SubscriptionOAuthLease
}

// NewSubscriptionOAuthResponseBody wraps rc so closing it releases the lease.
func NewSubscriptionOAuthResponseBody(rc io.ReadCloser, lease *SubscriptionOAuthLease) *SubscriptionOAuthResponseBody {
	return &SubscriptionOAuthResponseBody{ReadCloser: rc, lease: lease}
}

func (b *SubscriptionOAuthResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.lease.ReleaseResponseBody()
	return err
}

// AcquireSubscriptionOAuthChannelCapacity reserves one credential-scoped slot
// for a subscription OAuth channel request, binds the lease to the request
// context, and translates capacity/pacing failures into client-facing API
// errors. Sharing it across adaptors keeps concurrency, pacing, and error
// semantics identical for every subscription OAuth channel type.
func AcquireSubscriptionOAuthChannelCapacity(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	maxConcurrency int,
	minRequestInterval time.Duration,
) (*SubscriptionOAuthLease, error) {
	if info != nil && info.IsChannelTest {
		return &SubscriptionOAuthLease{}, nil
	}
	fingerprint := SubscriptionOAuthCredentialFingerprint(
		info.ChannelType,
		info.ChannelId,
		info.ChannelMultiKeyIndex,
		info.ApiKey,
	)
	lease, err := AcquireSubscriptionOAuthCapacity(
		c.Request.Context(),
		fingerprint,
		maxConcurrency,
		minRequestInterval,
	)
	if err == nil {
		BindSubscriptionOAuthLease(c, lease)
		return lease, nil
	}
	if IsSubscriptionOAuthCapacityError(err) {
		if c != nil {
			c.Header("Retry-After", strconv.Itoa(SubscriptionOAuthCapacityRetryAfterSeconds(err)))
		}
		errorCode := types.ErrorCodeOAuthChannelConcurrencyLimit
		statusCode := http.StatusServiceUnavailable
		if IsSubscriptionOAuthUsageLimitCapacityError(err) {
			errorCode = types.ErrorCodeUpstreamUsageLimit
			statusCode = http.StatusTooManyRequests
		} else if IsSubscriptionOAuthRateLimitCapacityError(err) {
			errorCode = types.ErrorCodeUpstreamRateLimited
			statusCode = http.StatusTooManyRequests
		}
		apiError := types.NewErrorWithStatusCode(
			err,
			errorCode,
			statusCode,
			types.ErrOptionWithNoRecordErrorLog(),
		)
		apiError.RetryAfter = SubscriptionOAuthCapacityRetryAfter(err)
		var capacityError *subscriptionOAuthCapacityError
		if errors.As(err, &capacityError) {
			apiError.UsageWindows = capacityError.usageWindows
		}
		return nil, apiError
	}
	status := http.StatusServiceUnavailable
	if c.Request.Context().Err() != nil {
		status = 499
	}
	return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, status, types.ErrOptionWithSkipRetry())
}

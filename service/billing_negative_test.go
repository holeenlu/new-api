package service

import (
	"net/http"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingBillingSettler struct {
	settleCalls int
}

func (s *recordingBillingSettler) Settle(_ int) error {
	s.settleCalls++
	return nil
}

func (s *recordingBillingSettler) Refund(_ *gin.Context) {}

func (s *recordingBillingSettler) NeedsRefund() bool {
	return false
}

func (s *recordingBillingSettler) FundingCommitted() bool {
	return false
}

func (s *recordingBillingSettler) GetPreConsumedQuota() int {
	return 0
}

func (s *recordingBillingSettler) Reserve(_ int) error {
	return nil
}

func TestSettleBillingRejectsNegativeActualQuotaBeforeDelegating(t *testing.T) {
	settler := &recordingBillingSettler{}
	relayInfo := &relaycommon.RelayInfo{Billing: settler}

	err := SettleBilling(nil, relayInfo, -1)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "actual quota cannot be negative")
	assert.Zero(t, settler.settleCalls)
}

func TestBillingSessionSettleRejectsNegativeActualQuota(t *testing.T) {
	tests := []struct {
		name           string
		alreadySettled bool
	}{
		{name: "open session"},
		{name: "already settled session", alreadySettled: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &BillingSession{settled: test.alreadySettled}

			err := session.Settle(-1)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "actual quota cannot be negative")
			assert.Equal(t, test.alreadySettled, session.settled)
		})
	}
}

func TestBillingSessionReserveRejectsNegativeTargetQuotaBeforeStateChecks(t *testing.T) {
	tests := []struct {
		name    string
		session *BillingSession
	}{
		{name: "open session", session: &BillingSession{preConsumedQuota: 10}},
		{name: "settled session", session: &BillingSession{preConsumedQuota: 10, settled: true}},
		{name: "refunded session", session: &BillingSession{preConsumedQuota: 10, refunded: true}},
		{name: "trusted session", session: &BillingSession{preConsumedQuota: 10, trusted: true}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.session.Reserve(-1)

			require.Error(t, err)
			var apiError *types.NewAPIError
			require.ErrorAs(t, err, &apiError)
			assert.Equal(t, types.ErrorCodeModelPriceError, apiError.GetErrorCode())
			assert.Equal(t, http.StatusBadRequest, apiError.StatusCode)
			assert.True(t, types.IsSkipRetryError(apiError))
			assert.Contains(t, apiError.Error(), "reserve target quota cannot be negative")
		})
	}
}

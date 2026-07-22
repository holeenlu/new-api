package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingBillingSettler struct {
	settleCalls int
	actualQuota int
}

type recordingFundingSource struct {
	settleCalls int
	settleErr   error
}

func (*recordingFundingSource) Source() string { return BillingSourceWallet }

func (*recordingFundingSource) PreConsume(int) error { return nil }

func (*recordingFundingSource) PreConsumeStrict(int) error { return nil }

func (s *recordingFundingSource) Settle(int) error {
	s.settleCalls++
	return s.settleErr
}

func (*recordingFundingSource) Refund() error { return nil }

func (s *recordingBillingSettler) Settle(actualQuota int) error {
	s.settleCalls++
	s.actualQuota = actualQuota
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

func (s *recordingBillingSettler) ReserveStrict(_ int) error {
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
			funding := &recordingFundingSource{}
			session := &BillingSession{
				relayInfo: &relaycommon.RelayInfo{},
				funding:   funding,
				settled:   test.alreadySettled,
			}

			err := session.Settle(-1)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "actual quota cannot be negative")
			assert.Zero(t, funding.settleCalls)
		})
	}
}

func TestBillingSessionSettleRejectsRefundedSessionBeforeFundingMutation(t *testing.T) {
	funding := &recordingFundingSource{}
	session := &BillingSession{
		funding:          funding,
		preConsumedQuota: 100,
		refunded:         true,
	}

	err := session.Settle(500)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already refunded")
	assert.Zero(t, funding.settleCalls)
}

func TestBillingSessionFundingSettlementFailureIsSticky(t *testing.T) {
	settlementErr := errors.New("funding settlement commit is unknown")
	funding := &recordingFundingSource{settleErr: settlementErr}
	session := &BillingSession{
		relayInfo:        &relaycommon.RelayInfo{},
		funding:          funding,
		preConsumedQuota: 100,
		tokenConsumed:    100,
	}

	firstErr := session.Settle(200)
	repeatedErr := session.Settle(200)

	require.ErrorIs(t, firstErr, settlementErr)
	require.ErrorIs(t, repeatedErr, settlementErr)
	assert.Equal(t, 1, funding.settleCalls, "a commit-unknown funding delta must not be issued twice")
	assert.False(t, session.FundingCommitted())
	assert.True(t, session.NeedsRefund())
}

func TestBillingSessionReserveRejectsNegativeTargetQuotaBeforeStateChecks(t *testing.T) {
	tests := []struct {
		name    string
		session *BillingSession
	}{
		{name: "open session", session: &BillingSession{preConsumedQuota: 10}},
		{name: "settled session", session: &BillingSession{preConsumedQuota: 10, settled: true}},
		{name: "refunded session", session: &BillingSession{preConsumedQuota: 10, refunded: true}},
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

func TestBillingSessionReserveRejectsClosedSessions(t *testing.T) {
	tests := []struct {
		name    string
		session *BillingSession
		message string
	}{
		{
			name:    "settled",
			session: &BillingSession{preConsumedQuota: 10, settled: true},
			message: "already settled",
		},
		{
			name:    "refunded",
			session: &BillingSession{preConsumedQuota: 10, refunded: true},
			message: "already refunded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, reserve := range []struct {
				name string
				call func(int) error
			}{
				{name: "ordinary", call: test.session.Reserve},
				{name: "strict", call: test.session.ReserveStrict},
			} {
				t.Run(reserve.name, func(t *testing.T) {
					err := reserve.call(20)
					require.Error(t, err)
					assert.Contains(t, err.Error(), test.message)
				})
			}
		})
	}
}

func TestBillingSessionStrictPreConsumeSettlesPositiveActualDelta(t *testing.T) {
	truncate(t)
	const (
		userID        = 845
		tokenID       = 846
		tokenKey      = "strict-positive-settlement"
		reservedQuota = 100
		actualQuota   = 250
	)
	seedUser(t, userID, 1000)
	seedToken(t, tokenID, userID, tokenKey, 1000)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	info := &relaycommon.RelayInfo{
		RequestId: "strict-positive-settlement-request",
		UserId:    userID,
		TokenId:   tokenID,
		TokenKey:  tokenKey,
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
	}

	require.Nil(t, PreConsumeBillingStrict(ctx, reservedQuota, info))
	require.NoError(t, info.Billing.Settle(actualQuota))
	assert.True(t, info.Billing.FundingCommitted())
	assert.False(t, info.Billing.NeedsRefund())

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 1000-actualQuota, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 1000-actualQuota, token.RemainQuota)
	assert.Equal(t, actualQuota, token.UsedQuota)
}

func TestBillingSessionReserveStrictDoesNotLeavePartialLedgerMutation(t *testing.T) {
	tests := []struct {
		name          string
		userQuota     int
		tokenQuota    int
		wantErrorCode types.ErrorCode
	}{
		{
			name:          "token quota rejects before wallet mutation",
			userQuota:     1000,
			tokenQuota:    100,
			wantErrorCode: types.ErrorCodePreConsumeTokenQuotaFailed,
		},
		{
			name:          "wallet rejection rolls token reservation back",
			userQuota:     100,
			tokenQuota:    1000,
			wantErrorCode: types.ErrorCodeInsufficientUserQuota,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			userID := 850 + index*10
			tokenID := userID + 1
			tokenKey := "strict-reserve-token-" + test.name
			seedUser(t, userID, test.userQuota)
			seedToken(t, tokenID, userID, tokenKey, test.tokenQuota)
			info := &relaycommon.RelayInfo{
				UserId:   userID,
				TokenId:  tokenID,
				TokenKey: tokenKey,
			}
			funding := &WalletFunding{userId: userID}
			session := &BillingSession{relayInfo: info, funding: funding}

			err := session.ReserveStrict(500)

			require.Error(t, err)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, test.wantErrorCode, apiErr.GetErrorCode())
			var user model.User
			require.NoError(t, model.DB.First(&user, userID).Error)
			assert.Equal(t, test.userQuota, user.Quota)
			var token model.Token
			require.NoError(t, model.DB.First(&token, tokenID).Error)
			assert.Equal(t, test.tokenQuota, token.RemainQuota)
			assert.Zero(t, token.UsedQuota)
		})
	}
}

func TestWalletFundingPreConsumeCommitsAtMostOneCompetingReservation(t *testing.T) {
	truncate(t)
	const userID = 890
	seedUser(t, userID, 1000)

	fundings := []*WalletFunding{{userId: userID}, {userId: userID}}
	errs := make([]error, len(fundings))
	var wait sync.WaitGroup
	for index := range fundings {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs[index] = fundings[index].PreConsume(700)
		}()
	}
	wait.Wait()

	successes := 0
	insufficient := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, model.ErrInsufficientUserQuota):
			insufficient++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, insufficient)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 300, user.Quota)
}

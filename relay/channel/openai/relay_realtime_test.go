package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type realtimeBillingRecorder struct {
	mu               sync.Mutex
	reserves         []int
	failReserveCall  int
	blockReserveCall int
	reserveStarted   chan struct{}
	releaseReserve   chan struct{}
	settleCalls      int
	refundCalls      int
}

func (b *realtimeBillingRecorder) Settle(int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settleCalls++
	return nil
}

func (b *realtimeBillingRecorder) Refund(*gin.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refundCalls++
}

func (b *realtimeBillingRecorder) NeedsRefund() bool      { return false }
func (b *realtimeBillingRecorder) FundingCommitted() bool { return false }
func (b *realtimeBillingRecorder) GetPreConsumedQuota() int {
	return 0
}

func (b *realtimeBillingRecorder) Reserve(target int) error {
	b.mu.Lock()
	b.reserves = append(b.reserves, target)
	call := len(b.reserves)
	block := b.blockReserveCall > 0 && call == b.blockReserveCall
	fail := b.failReserveCall > 0 && call == b.failReserveCall
	started := b.reserveStarted
	release := b.releaseReserve
	b.mu.Unlock()
	if block {
		if started != nil {
			started <- struct{}{}
		}
		if release != nil {
			<-release
		}
	}
	if fail {
		return errors.New("realtime reservation rejected")
	}
	return nil
}

func (b *realtimeBillingRecorder) ReserveStrict(target int) error {
	return b.Reserve(target)
}

func (b *realtimeBillingRecorder) snapshot() (reserves []int, settleCalls int, refundCalls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.reserves...), b.settleCalls, b.refundCalls
}

func TestRealtimeUsageAggregationBillsEachTurnExactlyOnce(t *testing.T) {
	local := dto.RealtimeUsage{}
	total := dto.RealtimeUsage{}

	input := dto.RealtimeUsage{TotalTokens: 10, InputTokens: 10}
	input.InputTokenDetails.TextTokens = 10
	output := dto.RealtimeUsage{TotalTokens: 5, OutputTokens: 5}
	output.OutputTokenDetails.AudioTokens = 5
	require.NoError(t, applyRealtimeUsageEvent(&local, &total, realtimeUsageEvent{usage: input}))
	require.NoError(t, applyRealtimeUsageEvent(&local, &total, realtimeUsageEvent{usage: output}))

	authoritative := dto.RealtimeUsage{TotalTokens: 12, InputTokens: 7, OutputTokens: 5}
	authoritative.InputTokenDetails.CachedTokens = 1
	authoritative.InputTokenDetails.CachedCreationTokens = 2
	authoritative.InputTokenDetails.CacheWriteTokens = 3
	authoritative.InputTokenDetails.TextTokens = 7
	authoritative.InputTokenDetails.ImageTokens = 4
	authoritative.OutputTokenDetails.AudioTokens = 5
	authoritative.OutputTokenDetails.ImageTokens = 6
	authoritative.OutputTokenDetails.ReasoningTokens = 7
	require.NoError(t, applyRealtimeUsageEvent(&local, &total, realtimeUsageEvent{
		usage:         authoritative,
		responseDone:  true,
		authoritative: true,
	}))

	assert.Equal(t, authoritative, total, "provider usage replaces the local estimate for that turn")
	assert.Equal(t, dto.RealtimeUsage{}, local)

	fallback := dto.RealtimeUsage{TotalTokens: 3, InputTokens: 2, OutputTokens: 1}
	fallback.InputTokenDetails.AudioTokens = 2
	fallback.OutputTokenDetails.TextTokens = 1
	require.NoError(t, applyRealtimeUsageEvent(&local, &total, realtimeUsageEvent{
		usage:        fallback,
		responseDone: true,
	}))

	assert.Equal(t, 15, total.TotalTokens)
	assert.Equal(t, 9, total.InputTokens)
	assert.Equal(t, 6, total.OutputTokens)
	assert.Equal(t, 2, total.InputTokenDetails.AudioTokens)
	assert.Equal(t, 1, total.OutputTokenDetails.TextTokens)
	assert.Equal(t, dto.RealtimeUsage{}, local)
}

func TestAddRealtimeUsageRejectsEveryNegativeFieldAndInt32Overflow(t *testing.T) {
	fields := []struct {
		name string
		set  func(*dto.RealtimeUsage, int)
	}{
		{name: "total_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.TotalTokens = value }},
		{name: "input_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokens = value }},
		{name: "output_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.OutputTokens = value }},
		{name: "input_token_details.cached_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.CachedTokens = value }},
		{name: "input_token_details.cached_creation_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.CachedCreationTokens = value }},
		{name: "input_token_details.cache_write_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.CacheWriteTokens = value }},
		{name: "input_token_details.text_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.TextTokens = value }},
		{name: "input_token_details.audio_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.AudioTokens = value }},
		{name: "input_token_details.image_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.InputTokenDetails.ImageTokens = value }},
		{name: "output_token_details.text_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.OutputTokenDetails.TextTokens = value }},
		{name: "output_token_details.audio_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.OutputTokenDetails.AudioTokens = value }},
		{name: "output_token_details.image_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.OutputTokenDetails.ImageTokens = value }},
		{name: "output_token_details.reasoning_tokens", set: func(usage *dto.RealtimeUsage, value int) { usage.OutputTokenDetails.ReasoningTokens = value }},
	}

	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			negative := dto.RealtimeUsage{}
			field.set(&negative, -1)
			total := dto.RealtimeUsage{}
			err := addRealtimeUsage(&total, negative)
			require.Error(t, err)
			assert.Contains(t, err.Error(), field.name)
			assert.Equal(t, dto.RealtimeUsage{}, total, "a rejected event must not partially mutate the accumulator")

			atLimit := dto.RealtimeUsage{}
			field.set(&atLimit, common.MaxQuota)
			require.NoError(t, addRealtimeUsage(&atLimit, dto.RealtimeUsage{}), "the exact database boundary remains representable")

			oneMore := dto.RealtimeUsage{}
			field.set(&oneMore, 1)
			before := atLimit
			err = addRealtimeUsage(&atLimit, oneMore)
			require.Error(t, err)
			assert.Contains(t, err.Error(), field.name)
			assert.Equal(t, before, atLimit, "overflow must not wrap or partially mutate the accumulator")
		})
	}
}

func TestNewRealtimeUsageDeltaRejectsCombinedInt32Overflow(t *testing.T) {
	_, err := newRealtimeUsageDelta(common.MaxQuota, 1, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds int32")

	usage, err := newRealtimeUsageDelta(common.MaxQuota-1, 1, false)
	require.NoError(t, err)
	assert.Equal(t, common.MaxQuota, usage.TotalTokens)
	assert.Equal(t, common.MaxQuota, usage.OutputTokens)
}

func TestApplyRealtimeUsageEventRejectsInvalidAuthoritativeUsageAtomically(t *testing.T) {
	tests := []struct {
		name  string
		total dto.RealtimeUsage
		event dto.RealtimeUsage
	}{
		{
			name:  "negative detail",
			total: dto.RealtimeUsage{TotalTokens: 7, InputTokens: 7},
			event: dto.RealtimeUsage{
				TotalTokens:       3,
				InputTokens:       3,
				InputTokenDetails: dto.InputTokenDetails{CachedTokens: -1},
			},
		},
		{
			name:  "cumulative overflow",
			total: dto.RealtimeUsage{TotalTokens: common.MaxQuota, InputTokens: common.MaxQuota},
			event: dto.RealtimeUsage{TotalTokens: 1, InputTokens: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := dto.RealtimeUsage{TotalTokens: 2, OutputTokens: 2}
			beforeLocal := local
			beforeTotal := tt.total

			err := applyRealtimeUsageEvent(&local, &tt.total, realtimeUsageEvent{
				usage:         tt.event,
				responseDone:  true,
				authoritative: true,
			})

			require.Error(t, err)
			assert.Equal(t, beforeLocal, local, "a rejected authoritative event must not clear the local estimate")
			assert.Equal(t, beforeTotal, tt.total, "a rejected authoritative event must not partially update cumulative usage")
		})
	}
}

func newRealtimeWebSocketPair(t *testing.T) (serverConn *websocket.Conn, clientConn *websocket.Conn) {
	t.Helper()

	accepted := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket pair: %v", err)
			return
		}
		accepted <- conn
		<-release
		_ = conn.Close()
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	serverConn = <-accepted
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		close(release)
		server.Close()
	})
	return serverConn, clientConn
}

func TestOpenaiRealtimeHandlerProgressivelyReservesCumulativeAuthoritativeUsage(t *testing.T) {
	downstreamHandlerConn, downstreamPeer := newRealtimeWebSocketPair(t)
	upstreamPeer, upstreamHandlerConn := newRealtimeWebSocketPair(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	billing := &realtimeBillingRecorder{}
	info := &relaycommon.RelayInfo{
		ClientWs: downstreamHandlerConn,
		TargetWs: upstreamHandlerConn,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o-realtime-preview",
		},
		InputAudioFormat:  "pcm16",
		OutputAudioFormat: "pcm16",
		IsFirstRequest:    true,
		Billing:           billing,
		PriceData: types.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}

	type handlerResult struct {
		apiErr *types.NewAPIError
		usage  *dto.RealtimeUsage
	}
	result := make(chan handlerResult, 1)
	go func() {
		apiErr, usage := OpenaiRealtimeHandler(c, info)
		result <- handlerResult{apiErr: apiErr, usage: usage}
	}()

	deadline := time.Now().Add(2 * time.Second)
	require.NoError(t, downstreamPeer.SetWriteDeadline(deadline))
	require.NoError(t, downstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"session.update","session":{"instructions":"local estimate must be replaced"}}`,
	)))
	require.NoError(t, upstreamPeer.SetReadDeadline(deadline))
	_, forwarded, err := upstreamPeer.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(forwarded), "session.update")

	require.NoError(t, upstreamPeer.SetWriteDeadline(deadline))
	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.function_call_arguments.delta","delta":"local output estimate"}`,
	)))
	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.done","response":{"usage":{"total_tokens":12,"input_tokens":12,"output_tokens":0,"input_token_details":{"text_tokens":12}}}}`,
	)))
	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.done","response":{"usage":{"total_tokens":8,"input_tokens":8,"output_tokens":0,"input_token_details":{"text_tokens":8}}}}`,
	)))
	require.NoError(t, downstreamPeer.SetReadDeadline(deadline))
	for range 3 {
		_, _, err = downstreamPeer.ReadMessage()
		require.NoError(t, err)
	}

	require.NoError(t, upstreamPeer.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		deadline,
	))

	select {
	case got := <-result:
		require.Nil(t, got.apiErr)
		require.NotNil(t, got.usage)
		assert.Equal(t, 20, got.usage.TotalTokens)
		assert.Equal(t, 20, got.usage.InputTokens)
		assert.Zero(t, got.usage.OutputTokens)
		assert.Equal(t, 20, got.usage.InputTokenDetails.TextTokens)
		assert.False(t, info.IsFirstRequest)
		reserves, settleCalls, refundCalls := billing.snapshot()
		assert.Equal(t, []int{12, 20}, reserves)
		assert.Zero(t, settleCalls, "the outer WSS lifecycle owns the single final settlement")
		assert.Zero(t, refundCalls)
	case <-time.After(2 * time.Second):
		t.Fatal("realtime handler did not stop after upstream close")
	}
}

func TestOpenaiRealtimeHandlerRejectsNegativeAuthoritativeUsage(t *testing.T) {
	downstreamHandlerConn, downstreamPeer := newRealtimeWebSocketPair(t)
	upstreamPeer, upstreamHandlerConn := newRealtimeWebSocketPair(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	billing := &realtimeBillingRecorder{}
	info := &relaycommon.RelayInfo{
		ClientWs: downstreamHandlerConn,
		TargetWs: upstreamHandlerConn,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o-realtime-preview",
		},
		Billing: billing,
		PriceData: types.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}

	type handlerResult struct {
		apiErr *types.NewAPIError
		usage  *dto.RealtimeUsage
	}
	result := make(chan handlerResult, 1)
	go func() {
		apiErr, usage := OpenaiRealtimeHandler(c, info)
		result <- handlerResult{apiErr: apiErr, usage: usage}
	}()

	deadline := time.Now().Add(2 * time.Second)
	require.NoError(t, upstreamPeer.SetWriteDeadline(deadline))
	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.done","response":{"usage":{"total_tokens":10,"input_tokens":10,"input_token_details":{"text_tokens":10,"cached_tokens":-1}}}}`,
	)))

	select {
	case got := <-result:
		require.Nil(t, got.apiErr)
		require.NotNil(t, got.usage)
		assert.Equal(t, dto.RealtimeUsage{}, *got.usage)
		reserves, _, _ := billing.snapshot()
		assert.Empty(t, reserves, "invalid upstream usage must never reach billing")
		require.NoError(t, downstreamPeer.SetReadDeadline(deadline))
		_, forwarded, readErr := downstreamPeer.ReadMessage()
		if readErr == nil {
			t.Fatalf("invalid response.done was forwarded to the client: %s", forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("realtime handler did not fail closed on negative upstream usage")
	}
}

func TestOpenaiRealtimeHandlerReservationFailureTerminatesWithoutDroppingUsage(t *testing.T) {
	downstreamHandlerConn, downstreamPeer := newRealtimeWebSocketPair(t)
	upstreamPeer, upstreamHandlerConn := newRealtimeWebSocketPair(t)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	reserveStarted := make(chan struct{}, 1)
	releaseReserve := make(chan struct{})
	billing := &realtimeBillingRecorder{
		failReserveCall:  2,
		blockReserveCall: 2,
		reserveStarted:   reserveStarted,
		releaseReserve:   releaseReserve,
	}
	info := &relaycommon.RelayInfo{
		ClientWs: downstreamHandlerConn,
		TargetWs: upstreamHandlerConn,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o-realtime-preview",
		},
		IsFirstRequest: true,
		Billing:        billing,
		PriceData: types.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}

	type handlerResult struct {
		apiErr *types.NewAPIError
		usage  *dto.RealtimeUsage
	}
	result := make(chan handlerResult, 1)
	go func() {
		apiErr, usage := OpenaiRealtimeHandler(c, info)
		result <- handlerResult{apiErr: apiErr, usage: usage}
	}()

	deadline := time.Now().Add(2 * time.Second)
	require.NoError(t, upstreamPeer.SetWriteDeadline(deadline))
	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.done","response":{"usage":{"total_tokens":10,"input_tokens":10,"input_token_details":{"text_tokens":10}}}}`,
	)))
	require.Eventually(t, func() bool {
		reserves, _, _ := billing.snapshot()
		return len(reserves) == 1
	}, time.Second, time.Millisecond)
	require.NoError(t, downstreamPeer.SetReadDeadline(deadline))
	_, firstDone, err := downstreamPeer.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(firstDone), "response.done")

	require.NoError(t, upstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"response.done","response":{"usage":{"total_tokens":20,"input_tokens":20,"input_token_details":{"text_tokens":20}}}}`,
	)))
	select {
	case <-reserveStarted:
	case <-time.After(time.Second):
		t.Fatal("second cumulative reservation did not start")
	}
	require.NoError(t, downstreamPeer.SetWriteDeadline(deadline))
	require.NoError(t, downstreamPeer.WriteMessage(websocket.TextMessage, []byte(
		`{"type":"session.update","session":{"instructions":"this token-bearing request must remain behind the failed reservation gate"}}`,
	)))
	close(releaseReserve)

	select {
	case got := <-result:
		require.Nil(t, got.apiErr)
		require.NotNil(t, got.usage)
		assert.Equal(t, 30, got.usage.TotalTokens, "the token-bearing next turn must not enter the aggregator while reserve is blocked")
		assert.Equal(t, 30, got.usage.InputTokens)
		assert.Equal(t, 30, got.usage.InputTokenDetails.TextTokens)
		reserves, settleCalls, refundCalls := billing.snapshot()
		assert.Equal(t, []int{10, 30}, reserves)
		assert.Zero(t, settleCalls, "failed progressive reserve must still leave settlement to WssHelper")
		assert.Zero(t, refundCalls, "committed upstream usage must not take the request refund path")
		require.NoError(t, upstreamPeer.SetReadDeadline(deadline))
		_, forwarded, readErr := upstreamPeer.ReadMessage()
		if readErr == nil {
			t.Fatalf("request crossed the failed response.done reservation gate: %s", forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("realtime handler did not terminate after cumulative reservation failed")
	}
}

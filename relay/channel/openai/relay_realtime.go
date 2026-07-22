package openai

import (
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type realtimeUsageEvent struct {
	usage         dto.RealtimeUsage
	responseDone  bool
	authoritative bool
	processed     chan error
}

func addRealtimeUsage(total *dto.RealtimeUsage, delta dto.RealtimeUsage) error {
	if total == nil {
		return fmt.Errorf("realtime usage total is nil")
	}

	next := *total
	fields := []struct {
		name  string
		value *int
		delta int
	}{
		{name: "total_tokens", value: &next.TotalTokens, delta: delta.TotalTokens},
		{name: "input_tokens", value: &next.InputTokens, delta: delta.InputTokens},
		{name: "output_tokens", value: &next.OutputTokens, delta: delta.OutputTokens},
		{name: "input_token_details.cached_tokens", value: &next.InputTokenDetails.CachedTokens, delta: delta.InputTokenDetails.CachedTokens},
		{name: "input_token_details.cached_creation_tokens", value: &next.InputTokenDetails.CachedCreationTokens, delta: delta.InputTokenDetails.CachedCreationTokens},
		{name: "input_token_details.cache_write_tokens", value: &next.InputTokenDetails.CacheWriteTokens, delta: delta.InputTokenDetails.CacheWriteTokens},
		{name: "input_token_details.text_tokens", value: &next.InputTokenDetails.TextTokens, delta: delta.InputTokenDetails.TextTokens},
		{name: "input_token_details.audio_tokens", value: &next.InputTokenDetails.AudioTokens, delta: delta.InputTokenDetails.AudioTokens},
		{name: "input_token_details.image_tokens", value: &next.InputTokenDetails.ImageTokens, delta: delta.InputTokenDetails.ImageTokens},
		{name: "output_token_details.text_tokens", value: &next.OutputTokenDetails.TextTokens, delta: delta.OutputTokenDetails.TextTokens},
		{name: "output_token_details.audio_tokens", value: &next.OutputTokenDetails.AudioTokens, delta: delta.OutputTokenDetails.AudioTokens},
		{name: "output_token_details.image_tokens", value: &next.OutputTokenDetails.ImageTokens, delta: delta.OutputTokenDetails.ImageTokens},
		{name: "output_token_details.reasoning_tokens", value: &next.OutputTokenDetails.ReasoningTokens, delta: delta.OutputTokenDetails.ReasoningTokens},
	}
	for _, field := range fields {
		if *field.value < 0 || field.delta < 0 {
			return fmt.Errorf("realtime usage %s must not be negative", field.name)
		}
		if *field.value > common.MaxQuota || field.delta > common.MaxQuota-*field.value {
			return fmt.Errorf("realtime usage %s exceeds int32", field.name)
		}
		*field.value += field.delta
	}
	*total = next
	return nil
}

func applyRealtimeUsageEvent(localUsage, totalUsage *dto.RealtimeUsage, event realtimeUsageEvent) error {
	if localUsage == nil || totalUsage == nil {
		return fmt.Errorf("realtime usage accumulator is nil")
	}
	if !event.responseDone {
		nextLocal := *localUsage
		if err := addRealtimeUsage(&nextLocal, event.usage); err != nil {
			return err
		}
		// Keep the unsettled turn representable together with all prior turns.
		// Logs persist token counts as int32 on supported databases.
		combined := *totalUsage
		if err := addRealtimeUsage(&combined, nextLocal); err != nil {
			return err
		}
		*localUsage = nextLocal
		return nil
	}

	nextTotal := *totalUsage
	if event.authoritative {
		if err := addRealtimeUsage(&nextTotal, event.usage); err != nil {
			return err
		}
	} else {
		nextLocal := *localUsage
		if err := addRealtimeUsage(&nextLocal, event.usage); err != nil {
			return err
		}
		if err := addRealtimeUsage(&nextTotal, nextLocal); err != nil {
			return err
		}
	}
	*totalUsage = nextTotal
	*localUsage = dto.RealtimeUsage{}
	return nil
}

func newRealtimeUsageDelta(textTokens, audioTokens int, input bool) (dto.RealtimeUsage, error) {
	if textTokens < 0 || audioTokens < 0 {
		return dto.RealtimeUsage{}, fmt.Errorf("realtime token count must not be negative: text=%d audio=%d", textTokens, audioTokens)
	}
	if textTokens > common.MaxQuota || audioTokens > common.MaxQuota-textTokens {
		return dto.RealtimeUsage{}, fmt.Errorf("realtime token count exceeds int32: text=%d audio=%d", textTokens, audioTokens)
	}

	totalTokens := textTokens + audioTokens
	delta := dto.RealtimeUsage{TotalTokens: totalTokens}
	if input {
		delta.InputTokens = totalTokens
		delta.InputTokenDetails.TextTokens = textTokens
		delta.InputTokenDetails.AudioTokens = audioTokens
	} else {
		delta.OutputTokens = totalTokens
		delta.OutputTokenDetails.TextTokens = textTokens
		delta.OutputTokenDetails.AudioTokens = audioTokens
	}
	return delta, nil
}

func sendRealtimeTermination(termination chan<- error, err error) {
	select {
	case termination <- err:
	default:
	}
}

func sendRealtimeUsageEvent(stop <-chan struct{}, events chan<- realtimeUsageEvent, event realtimeUsageEvent) bool {
	if event.responseDone {
		event.processed = make(chan error, 1)
	}
	select {
	case events <- event:
	case <-stop:
		return false
	}
	if event.processed == nil {
		return true
	}
	select {
	case err := <-event.processed:
		return err == nil
	case <-stop:
		return false
	}
}

func sendRealtimeResponseDone(
	stop <-chan struct{},
	events chan<- realtimeUsageEvent,
	turnGate *sync.RWMutex,
	event realtimeUsageEvent,
) bool {
	turnGate.Lock()
	defer turnGate.Unlock()
	if sendRealtimeUsageEvent(stop, events, event) {
		return true
	}
	// On reservation failure the aggregation owner closes stop before joining
	// the readers. Keep the exclusive gate until then so a queued next-turn
	// client message cannot slip upstream between the failed ack and shutdown.
	<-stop
	return false
}

func OpenaiRealtimeHandler(c *gin.Context, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.RealtimeUsage) {
	if info == nil || info.ClientWs == nil || info.TargetWs == nil {
		return types.NewError(fmt.Errorf("invalid websocket connection"), types.ErrorCodeBadResponse), nil
	}

	info.IsStream = true
	clientConn := info.ClientWs
	targetConn := info.TargetWs

	termination := make(chan error, 2)
	usageEvents := make(chan realtimeUsageEvent, 256)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var stateMu sync.Mutex
	var turnGate sync.RWMutex

	readers.Add(1)
	gopool.Go(func() {
		defer readers.Done()
		defer func() {
			if r := recover(); r != nil {
				sendRealtimeTermination(termination, fmt.Errorf("panic in client reader: %v", r))
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
				_, message, err := clientConn.ReadMessage()
				if err != nil {
					if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						sendRealtimeTermination(termination, fmt.Errorf("error reading from client: %v", err))
					} else {
						sendRealtimeTermination(termination, nil)
					}
					return
				}

				realtimeEvent := &dto.RealtimeEvent{}
				err = common.Unmarshal(message, realtimeEvent)
				if err != nil {
					sendRealtimeTermination(termination, fmt.Errorf("error unmarshalling message: %v", err))
					return
				}

				turnGate.RLock()
				select {
				case <-stop:
					turnGate.RUnlock()
					return
				default:
				}
				stateMu.Lock()
				if realtimeEvent.Type == dto.RealtimeEventTypeSessionUpdate {
					if realtimeEvent.Session != nil {
						if realtimeEvent.Session.Tools != nil {
							info.RealtimeTools = realtimeEvent.Session.Tools
						}
					}
				}

				textToken, audioToken, err := service.CountTokenRealtime(info, *realtimeEvent, info.UpstreamModelName)
				stateMu.Unlock()
				if err != nil {
					turnGate.RUnlock()
					sendRealtimeTermination(termination, fmt.Errorf("error counting text token: %v", err))
					return
				}
				logger.LogInfo(c, fmt.Sprintf("type: %s, textToken: %d, audioToken: %d", realtimeEvent.Type, textToken, audioToken))
				delta, err := newRealtimeUsageDelta(textToken, audioToken, true)
				if err != nil {
					turnGate.RUnlock()
					sendRealtimeTermination(termination, err)
					return
				}
				if !sendRealtimeUsageEvent(stop, usageEvents, realtimeUsageEvent{usage: delta}) {
					turnGate.RUnlock()
					return
				}

				select {
				case <-stop:
					turnGate.RUnlock()
					return
				default:
				}
				err = helper.WssString(c, targetConn, string(message))
				turnGate.RUnlock()
				if err != nil {
					sendRealtimeTermination(termination, fmt.Errorf("error writing to target: %v", err))
					return
				}
			}
		}
	})

	readers.Add(1)
	gopool.Go(func() {
		defer readers.Done()
		defer func() {
			if r := recover(); r != nil {
				sendRealtimeTermination(termination, fmt.Errorf("panic in target reader: %v", r))
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
				_, message, err := targetConn.ReadMessage()
				if err != nil {
					if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						sendRealtimeTermination(termination, fmt.Errorf("error reading from target: %v", err))
					} else {
						sendRealtimeTermination(termination, nil)
					}
					return
				}
				info.SetFirstResponseTime()
				realtimeEvent := &dto.RealtimeEvent{}
				err = common.Unmarshal(message, realtimeEvent)
				if err != nil {
					sendRealtimeTermination(termination, fmt.Errorf("error unmarshalling message: %v", err))
					return
				}

				if realtimeEvent.Type == dto.RealtimeEventTypeResponseDone {
					var realtimeUsage *dto.RealtimeUsage
					if realtimeEvent.Response != nil {
						realtimeUsage = realtimeEvent.Response.Usage
					}
					if realtimeUsage != nil {
						stateMu.Lock()
						info.IsFirstRequest = false
						stateMu.Unlock()
						if !sendRealtimeResponseDone(stop, usageEvents, &turnGate, realtimeUsageEvent{
							usage:         *realtimeUsage,
							responseDone:  true,
							authoritative: true,
						}) {
							return
						}
					} else {
						stateMu.Lock()
						textToken, audioToken, err := service.CountTokenRealtime(info, *realtimeEvent, info.UpstreamModelName)
						if err != nil {
							stateMu.Unlock()
							sendRealtimeTermination(termination, fmt.Errorf("error counting text token: %v", err))
							return
						}
						logger.LogInfo(c, fmt.Sprintf("type: %s, textToken: %d, audioToken: %d", realtimeEvent.Type, textToken, audioToken))
						info.IsFirstRequest = false
						stateMu.Unlock()
						delta, err := newRealtimeUsageDelta(textToken, audioToken, true)
						if err != nil {
							sendRealtimeTermination(termination, err)
							return
						}
						if !sendRealtimeResponseDone(stop, usageEvents, &turnGate, realtimeUsageEvent{
							usage:        delta,
							responseDone: true,
						}) {
							return
						}
					}
				} else if realtimeEvent.Type == dto.RealtimeEventTypeSessionUpdated || realtimeEvent.Type == dto.RealtimeEventTypeSessionCreated {
					realtimeSession := realtimeEvent.Session
					if realtimeSession != nil {
						// update audio format
						stateMu.Lock()
						info.InputAudioFormat = common.GetStringIfEmpty(realtimeSession.InputAudioFormat, info.InputAudioFormat)
						info.OutputAudioFormat = common.GetStringIfEmpty(realtimeSession.OutputAudioFormat, info.OutputAudioFormat)
						stateMu.Unlock()
					}
				} else {
					stateMu.Lock()
					textToken, audioToken, err := service.CountTokenRealtime(info, *realtimeEvent, info.UpstreamModelName)
					stateMu.Unlock()
					if err != nil {
						sendRealtimeTermination(termination, fmt.Errorf("error counting text token: %v", err))
						return
					}
					logger.LogInfo(c, fmt.Sprintf("type: %s, textToken: %d, audioToken: %d", realtimeEvent.Type, textToken, audioToken))
					delta, err := newRealtimeUsageDelta(textToken, audioToken, false)
					if err != nil {
						sendRealtimeTermination(termination, err)
						return
					}
					if !sendRealtimeUsageEvent(stop, usageEvents, realtimeUsageEvent{usage: delta}) {
						return
					}
				}

				err = helper.WssString(c, clientConn, string(message))
				if err != nil {
					sendRealtimeTermination(termination, fmt.Errorf("error writing to client: %v", err))
					return
				}
			}
		}
	})

	localUsage := dto.RealtimeUsage{}
	totalUsage := dto.RealtimeUsage{}
	var terminalErr error
	reservationFailed := false
	processUsageEvent := func(event realtimeUsageEvent) (processErr error) {
		defer func() {
			if event.processed != nil {
				event.processed <- processErr
			}
		}()
		if err := applyRealtimeUsageEvent(&localUsage, &totalUsage, event); err != nil {
			return fmt.Errorf("invalid realtime usage: %w", err)
		}
		if !event.responseDone || reservationFailed {
			return nil
		}
		if err := service.ReserveWssQuota(info, info.UpstreamModelName, &totalUsage); err != nil {
			reservationFailed = true
			return fmt.Errorf("error reserving cumulative realtime usage: %w", err)
		}
		return nil
	}
running:
	for {
		select {
		case event := <-usageEvents:
			if err := processUsageEvent(event); err != nil {
				terminalErr = err
				break running
			}
		case terminalErr = <-termination:
			break running
		case <-c.Done():
			terminalErr = c.Err()
			break running
		}
	}
	close(stop)
	_ = clientConn.Close()
	_ = targetConn.Close()
	readers.Wait()
	for {
		select {
		case event := <-usageEvents:
			if err := processUsageEvent(event); err != nil {
				terminalErr = err
			}
		default:
			goto drained
		}
	}

drained:
	if localUsage.TotalTokens != 0 {
		if err := addRealtimeUsage(&totalUsage, localUsage); err != nil {
			terminalErr = fmt.Errorf("invalid final realtime usage: %w", err)
		}
	}
	if terminalErr != nil {
		logger.LogError(c, "realtime error: "+terminalErr.Error())
	}
	return nil, &totalUsage
}

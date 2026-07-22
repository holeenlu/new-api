package responsesws

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const continuationRequiredKey = "responses_websocket_continuation_required"
const sessionResponseKey = "responses_websocket_session_response"

// MarkContinuationRequired records that the final upstream request contains a
// previous_response_id and therefore must use this client's live upstream
// WebSocket instead of an HTTP or alternate-adaptor path.
func MarkContinuationRequired(c *gin.Context) {
	if c != nil {
		c.Set(continuationRequiredKey, true)
	}
}

// IsContinuationRequired reports whether the current turn must use the live
// upstream WebSocket that owns its previous_response_id.
func IsContinuationRequired(c *gin.Context) bool {
	return c != nil && c.GetBool(continuationRequiredKey)
}

// IsSessionResponse reports whether the current upstream response is the SSE
// facade produced by a live Responses WebSocket. Its producer closes the facade
// at a terminal event while intentionally keeping the upstream connection alive,
// so stream consumers must drain to producer EOF instead of closing the body at
// the terminal data line.
func IsSessionResponse(c *gin.Context) bool {
	return c != nil && c.GetBool(sessionResponseKey)
}

// NewContinuationUnavailableError is the shared fail-closed result for a turn
// whose previous_response_id has lost its owning upstream WebSocket.
func NewContinuationUnavailableError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New("responses websocket continuation requires its original live upstream connection; reconnect and retry with full input context"),
		types.ErrorCodeWebSocketConnectionLimitReached,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	)
}

// NormalizeResponseDoneEvent translates the provider-specific response.done
// terminal into the corresponding Responses WebSocket terminal event. The
// nested response.error takes precedence over response.status, and an absent or
// unknown status fails closed so a malformed terminal can never be reported as
// a successful response.completed event.
func NormalizeResponseDoneEvent(payload []byte) ([]byte, string, error) {
	var event map[string]any
	if err := common.UnmarshalWithNumber(payload, &event); err != nil {
		return nil, "", err
	}

	eventType, _ := event["type"].(string)
	if eventType != "response.done" {
		return payload, eventType, nil
	}

	normalizedType := "response.failed"
	hasTopLevelError := event["error"] != nil
	if response, ok := event["response"].(map[string]any); ok && !hasTopLevelError {
		_, hasError := response["error"]
		if !hasError || response["error"] == nil {
			status, _ := response["status"].(string)
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "completed":
				normalizedType = "response.completed"
			case "incomplete":
				normalizedType = "response.incomplete"
			}
		}
	}

	event["type"] = normalizedType
	normalizedPayload, err := common.Marshal(event)
	if err != nil {
		return nil, "", err
	}
	return normalizedPayload, normalizedType, nil
}

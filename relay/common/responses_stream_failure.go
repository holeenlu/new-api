package common

import "github.com/gin-gonic/gin"

const responsesStreamPreflightFailureEventKey = "responses_stream_preflight_failure_event"
const responsesStreamFailureEmittedKey = "responses_stream_failure_emitted"

// SetResponsesStreamPreflightFailureEvent preserves a valid upstream
// response.failed event until the retry loop has exhausted eligible OAuth
// credentials. It is never used after any downstream event has been written.
func SetResponsesStreamPreflightFailureEvent(c *gin.Context, data string) {
	if c == nil || data == "" {
		return
	}
	c.Set(responsesStreamPreflightFailureEventKey, data)
}

func GetResponsesStreamPreflightFailureEvent(c *gin.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	data, ok := c.Get(responsesStreamPreflightFailureEventKey)
	value, isString := data.(string)
	return value, ok && isString && value != ""
}

func ClearResponsesStreamPreflightFailureEvent(c *gin.Context) {
	if c != nil {
		c.Set(responsesStreamPreflightFailureEventKey, "")
	}
}

func MarkResponsesStreamFailureEmitted(c *gin.Context) {
	if c != nil {
		c.Set(responsesStreamFailureEmittedKey, true)
	}
}

func IsResponsesStreamFailureEmitted(c *gin.Context) bool {
	return c != nil && c.GetBool(responsesStreamFailureEmittedKey)
}

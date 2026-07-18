package common

import "github.com/gin-gonic/gin"

const responsesStreamPreflightFailureEventKey = "responses_stream_preflight_failure_event"

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

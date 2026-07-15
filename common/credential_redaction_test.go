package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactSensitiveCredentials(t *testing.T) {
	input := `Authorization: Bearer access-secret {"access_token":"access-secret","refresh_token":"refresh-secret"} CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-secret callback=http://localhost:1455/auth/callback?code=callback-secret&state=state-secret`

	redacted := RedactSensitiveCredentials(input)

	for _, secret := range []string{"access-secret", "refresh-secret", "sk-ant-oat01-secret", "callback-secret", "state-secret", "localhost:1455"} {
		assert.NotContains(t, redacted, secret)
	}
	assert.Contains(t, redacted, "[REDACTED]")
	assert.Contains(t, redacted, "[REDACTED_OAUTH_CALLBACK_URL]")
}

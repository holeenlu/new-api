package common

import (
	"regexp"
	"strings"
)

var (
	oauthCallbackURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]*(?:[?&](?:code|state|access_token|refresh_token)=[^\s"'<>]*)+`)
	bearerTokenPattern      = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s,"'}]+`)
	oauthJSONSecretPattern  = regexp.MustCompile(`(?i)(["']?(?:access_token|refresh_token|id_token|oauth_token|code_verifier|client_secret)["']?\s*[:=]\s*["']?)[^\s,"'&}]+`)
	claudeOAuthTokenPattern = regexp.MustCompile(`(?i)(CLAUDE_CODE_OAUTH_TOKEN\s*=\s*["']?)[^\s"']+`)
	secretQueryPattern      = regexp.MustCompile(`(?i)([?&](?:key|api[_-]?key|apikey|x-api-key|access_token|refresh_token|id_token|token|authorization|auth|client_secret|secret|password|passwd|signature|sig|awsaccesskeyid|x-amz-credential|x-amz-security-token|x-amz-signature)=)[^&\s"'<>]+`)
	proxyUserinfoPattern    = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/@\s:]+:)[^@/\s]+@`)
)

// redactionTriggerNeedles is a strict lowercase superset of every substring the
// redaction patterns above can match. If a log line contains none of them, no
// pattern can match, so the six regex passes are skipped. Keep this list in sync
// when adding a pattern.
var redactionTriggerNeedles = []string{
	"://", "bearer", "token", "code", "secret", "key", "auth", "passw", "sig", "credential",
}

// RedactSensitiveCredentials removes OAuth credentials and callback URLs from log text.
func RedactSensitiveCredentials(value string) string {
	if !mightContainSensitiveCredential(value) {
		return value
	}
	value = oauthCallbackURLPattern.ReplaceAllString(value, "[REDACTED_OAUTH_CALLBACK_URL]")
	value = bearerTokenPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = oauthJSONSecretPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = claudeOAuthTokenPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = secretQueryPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	return proxyUserinfoPattern.ReplaceAllString(value, `${1}[REDACTED]@`)
}

func mightContainSensitiveCredential(value string) bool {
	lower := strings.ToLower(value)
	for _, needle := range redactionTriggerNeedles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

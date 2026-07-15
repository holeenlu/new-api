package common

import "regexp"

var (
	oauthCallbackURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]*(?:[?&](?:code|state|access_token|refresh_token)=[^\s"'<>]*)+`)
	bearerTokenPattern      = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s,"'}]+`)
	oauthJSONSecretPattern  = regexp.MustCompile(`(?i)(["']?(?:access_token|refresh_token|id_token|oauth_token|code_verifier|client_secret)["']?\s*[:=]\s*["']?)[^\s,"'&}]+`)
	claudeOAuthTokenPattern = regexp.MustCompile(`(?i)(CLAUDE_CODE_OAUTH_TOKEN\s*=\s*["']?)[^\s"']+`)
)

// RedactSensitiveCredentials removes OAuth credentials and callback URLs from log text.
func RedactSensitiveCredentials(value string) string {
	value = oauthCallbackURLPattern.ReplaceAllString(value, "[REDACTED_OAUTH_CALLBACK_URL]")
	value = bearerTokenPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = oauthJSONSecretPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	return claudeOAuthTokenPattern.ReplaceAllString(value, `${1}[REDACTED]`)
}

package common

import kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"

// RedactSensitiveCredentials removes OAuth credentials and callback URLs from log text.
func RedactSensitiveCredentials(value string) string {
	return kitutil.RedactSensitiveCredentials(value)
}

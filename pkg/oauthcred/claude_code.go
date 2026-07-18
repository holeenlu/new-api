package oauthcred

import "strings"

// NormalizeClaudeCodeToken removes the shell assignment forms accepted by the
// Claude Code channel and returns the canonical token value.
func NormalizeClaudeCodeToken(raw string) string {
	token := strings.TrimSpace(raw)
	token = strings.TrimPrefix(token, "export ")
	token = strings.TrimPrefix(token, "CLAUDE_CODE_OAUTH_TOKEN=")
	token = strings.TrimSpace(token)
	if len(token) >= 2 && ((token[0] == '"' && token[len(token)-1] == '"') ||
		(token[0] == '\'' && token[len(token)-1] == '\'')) {
		token = token[1 : len(token)-1]
	}
	return token
}

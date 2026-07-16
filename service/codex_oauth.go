package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

const (
	defaultCodexOAuthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultCodexOAuthRedirectURI = "http://localhost:1455/auth/callback"
	defaultCodexOAuthScope       = "openid profile email offline_access"
	codexOAuthAuthorizeURL       = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL           = "https://auth.openai.com/oauth/token"
	codexJWTClaimPath            = "https://api.openai.com/auth"
	defaultHTTPTimeout           = 20 * time.Second
)

type codexOAuthConfig struct {
	ClientID    string
	RedirectURI string
	Scope       string
}

type CodexOAuthUpstreamError struct {
	StatusCode int
	Code       types.ErrorCode
	Message    string
	Cause      error
}

func (e *CodexOAuthUpstreamError) Error() string {
	return e.Message
}

func (e *CodexOAuthUpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func loadCodexOAuthConfig() (codexOAuthConfig, error) {
	config := codexOAuthConfig{
		ClientID:    strings.TrimSpace(common.GetEnvOrDefaultString("CODEX_OAUTH_CLIENT_ID", defaultCodexOAuthClientID)),
		RedirectURI: strings.TrimSpace(common.GetEnvOrDefaultString("CODEX_OAUTH_REDIRECT_URI", defaultCodexOAuthRedirectURI)),
		Scope:       strings.Join(strings.Fields(common.GetEnvOrDefaultString("CODEX_OAUTH_SCOPE", defaultCodexOAuthScope)), " "),
	}
	if config.ClientID == "" {
		return codexOAuthConfig{}, errors.New("CODEX_OAUTH_CLIENT_ID must not be empty")
	}
	redirectURL, err := url.ParseRequestURI(config.RedirectURI)
	if err != nil || redirectURL.Host == "" || (redirectURL.Scheme != "http" && redirectURL.Scheme != "https") {
		return codexOAuthConfig{}, errors.New("CODEX_OAUTH_REDIRECT_URI must be an absolute HTTP or HTTPS URL")
	}
	if config.Scope == "" {
		return codexOAuthConfig{}, errors.New("CODEX_OAUTH_SCOPE must not be empty")
	}
	return config, nil
}

func newCodexOAuthUpstreamError(statusCode int, operation string) error {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		message := "OAuth request was rejected"
		if operation == "code exchange" {
			message = "OAuth authorization code was rejected; start a new authorization and verify the redirect URI configuration"
		} else if operation == "refresh" {
			message = "OAuth credential refresh was rejected; reauthorize the account"
		}
		return &CodexOAuthUpstreamError{
			StatusCode: statusCode,
			Code:       types.ErrorCodeInvalidRequest,
			Message:    message,
		}
	case http.StatusUnauthorized:
		return &CodexOAuthUpstreamError{StatusCode: statusCode, Code: types.ErrorCodeOAuthUnauthorized, Message: "OAuth credential is invalid or expired"}
	case http.StatusForbidden:
		return &CodexOAuthUpstreamError{StatusCode: statusCode, Code: types.ErrorCodeOAuthForbidden, Message: "OAuth account is not permitted to access this resource"}
	case http.StatusTooManyRequests:
		return &CodexOAuthUpstreamError{StatusCode: statusCode, Code: types.ErrorCodeBadResponseStatusCode, Message: "OAuth authorization is temporarily rate limited; retry after a short delay"}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &CodexOAuthUpstreamError{StatusCode: statusCode, Code: types.ErrorCodeBadResponseStatusCode, Message: "OAuth authorization service is temporarily unavailable; retry later"}
	default:
		return &CodexOAuthUpstreamError{StatusCode: statusCode, Code: types.ErrorCodeBadResponseStatusCode, Message: fmt.Sprintf("codex oauth %s failed: status=%d", operation, statusCode)}
	}
}

func newCodexOAuthTransportError(err error, operation string) error {
	message := "OAuth authorization could not reach auth.openai.com"
	if errors.Is(err, context.DeadlineExceeded) {
		message = "OAuth authorization timed out while contacting auth.openai.com"
	} else if errors.Is(err, context.Canceled) {
		message = "OAuth authorization request was cancelled"
	}
	return &CodexOAuthUpstreamError{
		Code:    types.ErrorCodeDoRequestFailed,
		Message: message + " during " + operation,
		Cause:   err,
	}
}

func newCodexOAuthInvalidResponseError(operation string, cause error) error {
	return &CodexOAuthUpstreamError{
		Code:    types.ErrorCodeBadResponseBody,
		Message: "OAuth authorization returned an invalid success response during " + operation,
		Cause:   cause,
	}
}

type CodexOAuthTokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type CodexOAuthAuthorizationFlow struct {
	State        string
	Verifier     string
	AuthorizeURL string
}

func RefreshCodexOAuthToken(ctx context.Context, refreshToken string) (*CodexOAuthTokenResult, error) {
	return RefreshCodexOAuthTokenWithProxy(ctx, refreshToken, "")
}

func RefreshCodexOAuthTokenWithProxy(ctx context.Context, refreshToken string, proxyURL string) (*CodexOAuthTokenResult, error) {
	config, err := loadCodexOAuthConfig()
	if err != nil {
		return nil, err
	}
	client, err := getCodexOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return refreshCodexOAuthToken(ctx, client, codexOAuthTokenURL, config.ClientID, refreshToken)
}

func CreateCodexOAuthAuthorizationFlow() (*CodexOAuthAuthorizationFlow, error) {
	config, err := loadCodexOAuthConfig()
	if err != nil {
		return nil, err
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeSum := sha256.Sum256([]byte(verifier))

	u, err := url.Parse(codexOAuthAuthorizeURL)
	if err != nil {
		return nil, err
	}
	state := fmt.Sprintf("%x", stateBytes)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", config.ClientID)
	q.Set("redirect_uri", config.RedirectURI)
	q.Set("scope", config.Scope)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challengeSum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "codex_cli_rs")
	u.RawQuery = q.Encode()

	return &CodexOAuthAuthorizationFlow{State: state, Verifier: verifier, AuthorizeURL: u.String()}, nil
}

func ExchangeCodexAuthorizationCode(ctx context.Context, code string, verifier string) (*CodexOAuthTokenResult, error) {
	code = strings.TrimSpace(code)
	verifier = strings.TrimSpace(verifier)
	if code == "" || verifier == "" {
		return nil, errors.New("authorization code and code_verifier are required")
	}
	config, err := loadCodexOAuthConfig()
	if err != nil {
		return nil, err
	}
	client, err := getCodexOAuthHTTPClient("")
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", config.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", config.RedirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, newCodexOAuthTransportError(err, "code exchange")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCodexOAuthUpstreamError(resp.StatusCode, "code exchange")
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := common.DecodeJson(resp.Body, &payload); err != nil {
		return nil, newCodexOAuthInvalidResponseError("code exchange", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" || strings.TrimSpace(payload.RefreshToken) == "" || payload.ExpiresIn <= 0 {
		return nil, newCodexOAuthInvalidResponseError("code exchange", errors.New("required token fields are missing"))
	}
	return &CodexOAuthTokenResult{AccessToken: strings.TrimSpace(payload.AccessToken), RefreshToken: strings.TrimSpace(payload.RefreshToken), ExpiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)}, nil
}

func refreshCodexOAuthToken(
	ctx context.Context,
	client *http.Client,
	tokenURL string,
	clientID string,
	refreshToken string,
) (*CodexOAuthTokenResult, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return nil, errors.New("empty refresh_token")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, newCodexOAuthTransportError(err, "credential refresh")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCodexOAuthUpstreamError(resp.StatusCode, "refresh")
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := common.DecodeJson(resp.Body, &payload); err != nil {
		return nil, newCodexOAuthInvalidResponseError("credential refresh", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" || payload.ExpiresIn <= 0 {
		return nil, newCodexOAuthInvalidResponseError("credential refresh", errors.New("required token fields are missing"))
	}
	rotatedRefreshToken := strings.TrimSpace(payload.RefreshToken)
	if rotatedRefreshToken == "" {
		rotatedRefreshToken = rt
	}

	return &CodexOAuthTokenResult{
		AccessToken:  strings.TrimSpace(payload.AccessToken),
		RefreshToken: rotatedRefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

func getCodexOAuthHTTPClient(proxyURL string) (*http.Client, error) {
	baseClient, err := GetHttpClientWithProxy(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, err
	}
	if baseClient == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}, nil
	}
	clientCopy := *baseClient
	clientCopy.Timeout = defaultHTTPTimeout
	return &clientCopy, nil
}

func ExtractCodexAccountIDFromJWT(token string) (string, bool) {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return "", false
	}
	raw, ok := claims[codexJWTClaimPath]
	if !ok {
		return "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := obj["chatgpt_account_id"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

func ExtractEmailFromJWT(token string) (string, bool) {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return "", false
	}
	v, ok := claims["email"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

func decodeJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := common.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

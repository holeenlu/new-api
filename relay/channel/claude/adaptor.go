package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

const (
	ClaudeCodeOAuthBeta      = "claude-code-20250219,oauth-2025-04-20"
	ClaudeCodeOAuthUserAgent = "claude-cli"
)

var (
	ClaudeCodeOAuthMaxConcurrency     = rootcommon.GetEnvOrDefault("CLAUDE_CODE_OAUTH_MAX_CONCURRENCY", 5)
	ClaudeCodeOAuthMinRequestInterval = time.Duration(rootcommon.GetEnvOrDefault("CLAUDE_CODE_OAUTH_MIN_REQUEST_INTERVAL_MS", 750)) * time.Millisecond
	claudeCodeOAuthSlots              sync.Map
	claudeCodeOAuthPacing             sync.Map
)

func init() {
	if ClaudeCodeOAuthMaxConcurrency < 1 {
		ClaudeCodeOAuthMaxConcurrency = 1
	} else if ClaudeCodeOAuthMaxConcurrency > 8 {
		ClaudeCodeOAuthMaxConcurrency = 8
	}
	if ClaudeCodeOAuthMinRequestInterval < 0 {
		ClaudeCodeOAuthMinRequestInterval = 0
	} else if ClaudeCodeOAuthMinRequestInterval > 5*time.Second {
		ClaudeCodeOAuthMinRequestInterval = 5 * time.Second
	}
}

type claudeCodeOAuthPacingState struct {
	mu        sync.Mutex
	nextStart time.Time
}

type claudeCodeOAuthResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *claudeCodeOAuthResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func acquireClaudeCodeOAuthSlot(channelID int) (func(), bool) {
	value, _ := claudeCodeOAuthSlots.LoadOrStore(channelID, make(chan struct{}, ClaudeCodeOAuthMaxConcurrency))
	slots := value.(chan struct{})
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-slots })
		}, true
	default:
		return nil, false
	}
}

func waitForClaudeCodeOAuthTurn(ctx context.Context, channelID int) error {
	value, _ := claudeCodeOAuthPacing.LoadOrStore(channelID, &claudeCodeOAuthPacingState{})
	state := value.(*claudeCodeOAuthPacingState)

	state.mu.Lock()
	now := time.Now()
	startAt := now
	if state.nextStart.After(startAt) {
		startAt = state.nextStart
	}
	state.nextStart = startAt.Add(ClaudeCodeOAuthMinRequestInterval)
	state.mu.Unlock()

	delay := time.Until(startAt)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ClaudeCodeIdentitySystem is the exact first system block Anthropic requires on
// Claude Code OAuth (subscription) requests. An OAuth-token request whose first
// system block is not this identity is rejected upstream with a masked
// 429 rate_limit_error (message: "Error"), which is why a bare channel-test
// request — or any non-Claude-Code client — fails against this channel type.
const ClaudeCodeIdentitySystem = "You are Claude Code, Anthropic's official CLI for Claude."

// ensureClaudeCodeIdentitySystem guarantees the Claude Code identity is the first
// system block so an OAuth-token request is accepted upstream. Real Claude Code
// clients already send it (the call is then a no-op); channel tests and other
// clients do not, so it is prepended ahead of any existing system content.
func ensureClaudeCodeIdentitySystem(request *dto.ClaudeRequest) {
	if request == nil {
		return
	}
	if request.System == nil {
		request.SetStringSystem(ClaudeCodeIdentitySystem)
		return
	}
	if request.IsStringSystem() {
		existing := strings.TrimSpace(request.GetStringSystem())
		if existing == "" || existing == ClaudeCodeIdentitySystem {
			request.SetStringSystem(ClaudeCodeIdentitySystem)
			return
		}
		request.System = []dto.ClaudeMediaMessage{
			newClaudeTextBlock(ClaudeCodeIdentitySystem),
			newClaudeTextBlock(existing),
		}
		return
	}
	blocks := request.ParseSystem()
	if len(blocks) > 0 && strings.TrimSpace(blocks[0].GetText()) == ClaudeCodeIdentitySystem {
		return
	}
	request.System = append([]dto.ClaudeMediaMessage{newClaudeTextBlock(ClaudeCodeIdentitySystem)}, blocks...)
}

func newClaudeTextBlock(text string) dto.ClaudeMediaMessage {
	block := dto.ClaudeMediaMessage{Type: "text"}
	block.SetText(text)
	return block
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	if info != nil && info.ChannelType == constant.ChannelTypeClaudeCode {
		ensureClaudeCodeIdentitySystem(request)
	}
	return request, nil
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	requestURL := fmt.Sprintf("%s/v1/messages", info.ChannelBaseUrl)
	if !shouldAppendClaudeBetaQuery(info) {
		return requestURL, nil
	}

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}
	query := parsedURL.Query()
	query.Set("beta", "true")
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String(), nil
}

func shouldAppendClaudeBetaQuery(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	if info.IsClaudeBetaQuery {
		return true
	}
	if info.ChannelOtherSettings.ClaudeBetaQuery {
		return true
	}
	return false
}

func CommonClaudeHeadersOperation(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) {
	// common headers operation
	anthropicBeta := c.Request.Header.Get("anthropic-beta")
	if anthropicBeta != "" {
		req.Set("anthropic-beta", anthropicBeta)
	}
	model_setting.GetClaudeSettings().WriteHeaders(info.OriginModelName, req)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)
	if info != nil && info.ChannelType == constant.ChannelTypeClaudeCode {
		req.Set("anthropic-version", "2023-06-01")
		return SetupClaudeCodeOAuthHeader(req, info.ApiKey)
	}
	req.Set("x-api-key", info.ApiKey)
	anthropicVersion := c.Request.Header.Get("anthropic-version")
	if anthropicVersion == "" {
		anthropicVersion = "2023-06-01"
	}
	req.Set("anthropic-version", anthropicVersion)
	CommonClaudeHeadersOperation(c, req, info)
	return nil
}

func SetupClaudeCodeOAuthHeader(req *http.Header, key string) error {
	token, err := ParseClaudeCodeOAuthToken(key)
	if err != nil {
		return err
	}
	req.Del("x-api-key")
	req.Set("Authorization", "Bearer "+token)
	req.Set("anthropic-beta", ClaudeCodeOAuthBeta)
	req.Set("user-agent", ClaudeCodeOAuthUserAgent)
	req.Set("x-app", "cli")
	return nil
}

func ParseClaudeCodeOAuthToken(key string) (string, error) {
	token := strings.TrimSpace(key)
	token = strings.TrimPrefix(token, "export ")
	token = strings.TrimPrefix(token, "CLAUDE_CODE_OAUTH_TOKEN=")
	token = strings.TrimSpace(token)
	if len(token) >= 2 {
		if (token[0] == '"' && token[len(token)-1] == '"') ||
			(token[0] == '\'' && token[len(token)-1] == '\'') {
			token = token[1 : len(token)-1]
		}
	}
	if token == "" {
		return "", errors.New("claude code channel: CLAUDE_CODE_OAUTH_TOKEN is required")
	}
	if len(token) > 4096 || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", errors.New("claude code channel: OAuth token contains invalid whitespace or is too long")
	}
	if !strings.HasPrefix(token, "sk-ant-oat") {
		return "", errors.New("claude code channel: an OAuth token beginning with sk-ant-oat is required")
	}
	return token, nil
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	result, err := relayconvert.ConvertRequest(c, info, types.RelayFormatClaude, request)
	if err != nil {
		return nil, err
	}
	if info != nil && info.ChannelType == constant.ChannelTypeClaudeCode {
		if claudeReq, ok := result.Value.(*dto.ClaudeRequest); ok {
			ensureClaudeCodeIdentitySystem(claudeReq)
		}
	}
	return result.Value, nil
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	// TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	if info == nil || info.ChannelType != constant.ChannelTypeClaudeCode {
		return channel.DoApiRequest(a, c, info, requestBody)
	}

	release, acquired := acquireClaudeCodeOAuthSlot(info.ChannelId)
	if !acquired {
		c.Header("Retry-After", "1")
		return nil, types.NewErrorWithStatusCode(
			errors.New("claude code channel concurrency limit reached; retry later"),
			types.ErrorCodeDoRequestFailed,
			http.StatusTooManyRequests,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if err := waitForClaudeCodeOAuthTurn(c.Request.Context(), info.ChannelId); err != nil {
		release()
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}

	resp, err := channel.DoApiRequest(a, c, info, requestBody)
	if err != nil {
		release()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		release()
		return resp, nil
	}
	resp.Body = &claudeCodeOAuthResponseBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	info.FinalRequestRelayFormat = types.RelayFormatClaude
	if info.IsStream {
		return ClaudeStreamHandler(c, resp, info)
	} else {
		return ClaudeHandler(c, resp, info)
	}
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

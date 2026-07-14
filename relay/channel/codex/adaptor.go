package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

var codexClientHeaders = []string{
	"session-id",
	"thread-id",
	"x-client-request-id",
	"x-codex-beta-features",
	"x-codex-parent-thread-id",
	"x-codex-turn-state",
	"x-codex-turn-metadata",
	"x-codex-window-id",
	"x-openai-memgen-request",
	"x-openai-subagent",
	"x-responsesapi-include-timing-metrics",
}

const (
	CodexOAuthUserAgent            = codexModelsUserAgent
	CodexOAuthOriginator           = "codex_cli_rs"
	codexChannelTestSessionID      = "019f4cb7-4452-79b2-92c0-f105ef46cc15"
	codexChannelTestTurnID         = "019f4cb7-44a0-79d0-9824-ceb1437e84b8"
	codexChannelTestInstallationID = "f6c6d912-945f-4584-bcf8-2cac04e84a1c"
	codexChannelTestTurnMetadata   = `{"installation_id":"f6c6d912-945f-4584-bcf8-2cac04e84a1c","session_id":"019f4cb7-4452-79b2-92c0-f105ef46cc15","thread_id":"019f4cb7-4452-79b2-92c0-f105ef46cc15","turn_id":"019f4cb7-44a0-79d0-9824-ceb1437e84b8","window_id":"019f4cb7-4452-79b2-92c0-f105ef46cc15:0","request_kind":"turn","thread_source":"user","sandbox":"seatbelt","turn_started_at_unix_ms":1783698506913}`
)

var (
	CodexOAuthMaxConcurrency     = common.GetEnvOrDefault("CODEX_OAUTH_MAX_CONCURRENCY", 5)
	CodexOAuthMinRequestInterval = time.Duration(common.GetEnvOrDefault("CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS", 750)) * time.Millisecond
	codexOAuthSlots              sync.Map
	codexOAuthPacing             sync.Map
)

func init() {
	if CodexOAuthMaxConcurrency < 1 {
		CodexOAuthMaxConcurrency = 1
	} else if CodexOAuthMaxConcurrency > 8 {
		CodexOAuthMaxConcurrency = 8
	}
	if CodexOAuthMinRequestInterval < 0 {
		CodexOAuthMinRequestInterval = 0
	} else if CodexOAuthMinRequestInterval > 5*time.Second {
		CodexOAuthMinRequestInterval = 5 * time.Second
	}
}

type codexOAuthPacingState struct {
	mu        sync.Mutex
	nextStart time.Time
}

type codexOAuthResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *codexOAuthResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func acquireCodexOAuthSlot(channelID int) (func(), bool) {
	value, _ := codexOAuthSlots.LoadOrStore(channelID, make(chan struct{}, CodexOAuthMaxConcurrency))
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

func waitForCodexOAuthTurn(ctx context.Context, channelID int) error {
	value, _ := codexOAuthPacing.LoadOrStore(channelID, &codexOAuthPacingState{})
	state := value.(*codexOAuthPacingState)

	state.mu.Lock()
	now := time.Now()
	startAt := now
	if state.nextStart.After(startAt) {
		startAt = state.nextStart
	}
	state.nextStart = startAt.Add(CodexOAuthMinRequestInterval)
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

func isCodexLiteModel(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	model := info.UpstreamModelName
	if model == "" {
		model = info.OriginModelName
	}
	return strings.HasPrefix(strings.ToLower(model), "gpt-5.6-")
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/messages endpoint not supported")
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/chat/completions endpoint not supported")
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/rerank endpoint not supported")
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/embeddings endpoint not supported")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	isCompact := info != nil && info.RelayMode == relayconstant.RelayModeResponsesCompact

	if info != nil && info.ChannelSetting.SystemPrompt != "" {
		systemPrompt := info.ChannelSetting.SystemPrompt

		if len(request.Instructions) == 0 {
			if b, err := common.Marshal(systemPrompt); err == nil {
				request.Instructions = b
			} else {
				return nil, err
			}
		} else if info.ChannelSetting.SystemPromptOverride {
			var existing string
			if err := common.Unmarshal(request.Instructions, &existing); err == nil {
				existing = strings.TrimSpace(existing)
				if existing == "" {
					if b, err := common.Marshal(systemPrompt); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				} else {
					if b, err := common.Marshal(systemPrompt + "\n" + existing); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				}
			} else {
				if b, err := common.Marshal(systemPrompt); err == nil {
					request.Instructions = b
				} else {
					return nil, err
				}
			}
		}
	}
	// Codex backend requires the `instructions` field to be present.
	// Keep it consistent with Codex CLI behavior by defaulting to an empty string.
	if len(request.Instructions) == 0 {
		request.Instructions = json.RawMessage(`""`)
	}
	if info != nil && info.IsChannelTest && isCodexLiteModel(info) {
		clientMetadata := map[string]any{
			"session_id":              codexChannelTestSessionID,
			"thread_id":               codexChannelTestSessionID,
			"turn_id":                 codexChannelTestTurnID,
			"x-codex-installation-id": codexChannelTestInstallationID,
			"x-codex-turn-metadata":   codexChannelTestTurnMetadata,
			"x-codex-window-id":       codexChannelTestSessionID + ":0",
		}
		clientMetadataJSON, err := common.Marshal(clientMetadata)
		if err != nil {
			return nil, err
		}
		request.ClientMetadata = clientMetadataJSON
		request.Include = json.RawMessage(`["reasoning.encrypted_content"]`)
		request.ParallelToolCalls = json.RawMessage(`false`)
		request.ToolChoice = json.RawMessage(`"auto"`)
		request.Text = json.RawMessage(`{"verbosity":"low"}`)
		request.Reasoning = &dto.Reasoning{Effort: "medium", Context: json.RawMessage(`"all_turns"`)}
	}

	if isCompact {
		return request, nil
	}
	// codex: store must be false
	request.Store = json.RawMessage("false")
	// rm max_output_tokens
	request.MaxOutputTokens = nil
	request.Temperature = nil
	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	release, acquired := acquireCodexOAuthSlot(info.ChannelId)
	if !acquired {
		c.Header("Retry-After", "1")
		return nil, types.NewErrorWithStatusCode(
			errors.New("codex OAuth channel concurrency limit reached; retry later"),
			types.ErrorCodeDoRequestFailed,
			http.StatusTooManyRequests,
		)
	}
	if err := waitForCodexOAuthTurn(c.Request.Context(), info.ChannelId); err != nil {
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
	resp.Body = &codexOAuthResponseBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact {
		return nil, types.NewError(errors.New("codex channel: endpoint not supported"), types.ErrorCodeInvalidRequest)
	}

	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		return openai.OaiResponsesCompactionHandler(c, resp)
	}

	if info.IsStream {
		return openai.OaiResponsesStreamHandler(c, info, resp)
	}
	return openai.OaiResponsesHandler(c, info, resp)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact {
		return "", errors.New("codex channel: only /v1/responses and /v1/responses/compact are supported")
	}
	path := "/backend-api/codex/responses"
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		path = "/backend-api/codex/responses/compact"
	}
	return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, path, info.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)
	for _, name := range codexClientHeaders {
		if value := c.GetHeader(name); value != "" {
			req.Set(name, value)
		}
	}
	if info.IsChannelTest && isCodexLiteModel(info) {
		req.Set("originator", "Codex Desktop")
		req.Set("x-openai-internal-codex-responses-lite", "true")
		req.Set("x-codex-beta-features", "remote_compaction_v2")
		req.Set("session-id", codexChannelTestSessionID)
		req.Set("thread-id", codexChannelTestSessionID)
		req.Set("x-codex-turn-metadata", codexChannelTestTurnMetadata)
		req.Set("x-codex-window-id", codexChannelTestSessionID+":0")
	}

	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return errors.New("codex channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return err
	}

	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	accountID := strings.TrimSpace(oauthKey.AccountID)

	if accessToken == "" {
		return errors.New("codex channel: access_token is required")
	}
	if accountID == "" {
		return errors.New("codex channel: account_id is required")
	}

	req.Set("Authorization", "Bearer "+accessToken)
	req.Set("chatgpt-account-id", accountID)

	if req.Get("OpenAI-Beta") == "" {
		req.Set("OpenAI-Beta", "responses=experimental")
	}
	if req.Get("originator") == "" {
		req.Set("originator", CodexOAuthOriginator)
	}
	req.Set("User-Agent", CodexOAuthUserAgent)

	// chatgpt.com/backend-api/codex/responses is strict about Content-Type.
	// Clients may omit it or include parameters like `application/json; charset=utf-8`,
	// which can be rejected by the upstream. Force the exact media type.
	req.Set("Content-Type", "application/json")
	if info.IsStream {
		req.Set("Accept", "text/event-stream")
	} else if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}

	return nil
}

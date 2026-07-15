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
	"github.com/google/uuid"
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
	CodexOAuthUserAgent  = codexModelsUserAgent
	CodexOAuthOriginator = "codex_cli_rs"
	codexTestMetadataKey = "codex_channel_test_metadata"
	codexLiteEnabledKey  = "codex_responses_lite_enabled"
)

type codexChannelTestMetadata struct {
	SessionID      string
	ThreadID       string
	TurnID         string
	InstallationID string
	WindowID       string
	TurnMetadata   string
}

func getOrCreateCodexResponsesMetadata(c *gin.Context) (*codexChannelTestMetadata, error) {
	if c != nil {
		if existing, ok := c.Get(codexTestMetadataKey); ok {
			if metadata, valid := existing.(*codexChannelTestMetadata); valid {
				return metadata, nil
			}
		}
	}
	var sessionID, threadID, windowID, suppliedTurnMetadata string
	if c != nil {
		sessionID = strings.TrimSpace(c.GetHeader("session-id"))
		threadID = strings.TrimSpace(c.GetHeader("thread-id"))
		windowID = strings.TrimSpace(c.GetHeader("x-codex-window-id"))
		suppliedTurnMetadata = strings.TrimSpace(c.GetHeader("x-codex-turn-metadata"))
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if threadID == "" {
		threadID = sessionID
	}
	if windowID == "" {
		windowID = sessionID + ":0"
	}
	metadata := &codexChannelTestMetadata{
		SessionID:      sessionID,
		ThreadID:       threadID,
		TurnID:         uuid.NewString(),
		InstallationID: uuid.NewString(),
		WindowID:       windowID,
	}
	if supplied := suppliedTurnMetadata; supplied != "" {
		var suppliedMetadata map[string]any
		if err := common.Unmarshal([]byte(supplied), &suppliedMetadata); err == nil {
			if value, ok := suppliedMetadata["turn_id"].(string); ok && strings.TrimSpace(value) != "" {
				metadata.TurnID = strings.TrimSpace(value)
			}
			if value, ok := suppliedMetadata["installation_id"].(string); ok && strings.TrimSpace(value) != "" {
				metadata.InstallationID = strings.TrimSpace(value)
			}
		}
	}
	turnMetadata, err := common.Marshal(map[string]any{
		"installation_id":         metadata.InstallationID,
		"session_id":              metadata.SessionID,
		"thread_id":               metadata.ThreadID,
		"turn_id":                 metadata.TurnID,
		"window_id":               metadata.WindowID,
		"request_kind":            "turn",
		"thread_source":           "user",
		"sandbox":                 "seatbelt",
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	metadata.TurnMetadata = string(turnMetadata)
	if c != nil {
		c.Set(codexTestMetadataKey, metadata)
	}
	return metadata, nil
}

func shouldUseCodexResponsesLite(info *relaycommon.RelayInfo) bool {
	return info != nil &&
		info.RelayMode == relayconstant.RelayModeResponses &&
		isCodexLiteModel(info)
}

func codexResponsesLiteToolsCompatible(raw json.RawMessage) (bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return true, nil
	}

	var tools []json.RawMessage
	if err := common.Unmarshal(raw, &tools); err != nil {
		return false, types.NewErrorWithStatusCode(
			errors.New("Codex Responses Lite requires tools to be a JSON array"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	for _, rawTool := range tools {
		var tool struct {
			Type      string `json:"type"`
			Execution string `json:"execution"`
		}
		if err := common.Unmarshal(rawTool, &tool); err != nil {
			return false, types.NewErrorWithStatusCode(
				errors.New("Codex Responses Lite tool definitions must be JSON objects"),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
			)
		}
		toolType := strings.ToLower(strings.TrimSpace(tool.Type))
		if toolType == "function" || toolType == "custom" {
			continue
		}
		if toolType == "tool_search" && strings.EqualFold(strings.TrimSpace(tool.Execution), "client") {
			continue
		}
		return false, nil
	}
	return true, nil
}

func setCodexResponsesLiteEnabled(c *gin.Context, enabled bool) {
	if c != nil {
		c.Set(codexLiteEnabledKey, enabled)
	}
}

func isCodexResponsesLiteEnabled(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if !shouldUseCodexResponsesLite(info) || c == nil {
		return false
	}
	enabled, exists := c.Get(codexLiteEnabledKey)
	if !exists {
		return false
	}
	value, ok := enabled.(bool)
	return ok && value
}

func ensureCodexLiteInclude(raw json.RawMessage) (json.RawMessage, error) {
	const encryptedReasoning = "reasoning.encrypted_content"
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`["reasoning.encrypted_content"]`), nil
	}

	var include []string
	if err := common.Unmarshal(raw, &include); err != nil {
		return nil, types.NewErrorWithStatusCode(
			errors.New("Codex Responses Lite requires include to be an array of strings"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	for _, item := range include {
		if item == encryptedReasoning {
			return raw, nil
		}
	}
	include = append(include, encryptedReasoning)
	encoded, err := common.Marshal(include)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

var (
	CodexOAuthMaxConcurrency     = 5
	CodexOAuthMinRequestInterval = 750 * time.Millisecond
	codexOAuthSlots              sync.Map
	codexOAuthPacing             sync.Map
)

func InitOAuthRuntimeSettings() {
	CodexOAuthMaxConcurrency = common.GetEnvOrDefault("CODEX_OAUTH_MAX_CONCURRENCY", 5)
	CodexOAuthMinRequestInterval = time.Duration(common.GetEnvOrDefault("CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS", 750)) * time.Millisecond
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
	useLite := false
	if shouldUseCodexResponsesLite(info) {
		if !info.IsStream {
			return nil, types.NewErrorWithStatusCode(
				errors.New("Codex Responses Lite requires stream=true"),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
			)
		}
		var err error
		useLite, err = codexResponsesLiteToolsCompatible(request.Tools)
		if err != nil {
			return nil, err
		}
	}
	setCodexResponsesLiteEnabled(c, useLite)
	if useLite {
		metadata, err := getOrCreateCodexResponsesMetadata(c)
		if err != nil {
			return nil, err
		}
		clientMetadata := make(map[string]any)
		if trimmed := strings.TrimSpace(string(request.ClientMetadata)); trimmed != "" && trimmed != "null" {
			if err := common.Unmarshal(request.ClientMetadata, &clientMetadata); err != nil {
				return nil, types.NewErrorWithStatusCode(
					errors.New("Codex Responses Lite requires client_metadata to be a JSON object"),
					types.ErrorCodeInvalidRequest,
					http.StatusBadRequest,
					types.ErrOptionWithSkipRetry(),
				)
			}
		}
		generatedMetadata := map[string]any{
			"session_id":              metadata.SessionID,
			"thread_id":               metadata.ThreadID,
			"turn_id":                 metadata.TurnID,
			"x-codex-installation-id": metadata.InstallationID,
			"x-codex-turn-metadata":   metadata.TurnMetadata,
			"x-codex-window-id":       metadata.WindowID,
		}
		for key, value := range generatedMetadata {
			clientMetadata[key] = value
		}
		clientMetadataJSON, err := common.Marshal(clientMetadata)
		if err != nil {
			return nil, err
		}
		request.ClientMetadata = clientMetadataJSON
		request.Include, err = ensureCodexLiteInclude(request.Include)
		if err != nil {
			return nil, err
		}
		request.ParallelToolCalls = json.RawMessage(`false`)
		if len(request.ToolChoice) == 0 {
			request.ToolChoice = json.RawMessage(`"auto"`)
		}
		if len(request.Text) == 0 {
			request.Text = json.RawMessage(`{"verbosity":"low"}`)
		}
		if request.Reasoning == nil {
			request.Reasoning = &dto.Reasoning{Effort: "medium"}
		} else if request.Reasoning.Effort == "" {
			request.Reasoning.Effort = "medium"
		}
		request.Reasoning.Context = json.RawMessage(`"all_turns"`)
		stream := true
		request.Stream = &stream
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
			http.StatusServiceUnavailable,
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
	return ConfiguredModelList()
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
	if isCodexResponsesLiteEnabled(c, info) {
		metadata, err := getOrCreateCodexResponsesMetadata(c)
		if err != nil {
			return err
		}
		req.Set("originator", "Codex Desktop")
		req.Set("x-openai-internal-codex-responses-lite", "true")
		req.Set("x-codex-beta-features", "remote_compaction_v2")
		req.Set("session-id", metadata.SessionID)
		req.Set("thread-id", metadata.ThreadID)
		req.Set("x-codex-turn-metadata", metadata.TurnMetadata)
		req.Set("x-codex-window-id", metadata.WindowID)
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

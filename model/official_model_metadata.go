package model

import (
	"strings"

	"github.com/QuantumNous/new-api/dto"
)

const conservativeModelContextWindow = 128_000

const (
	claudeCurrentContextWindow = 1_000_000
	claudeStandardContextWindow = 200_000
	geminiLargeContextWindow = 1_048_576
	openAILargeContextWindow = 1_047_576
	openAIGPT5ContextWindow = 400_000
	openAIGPT54ContextWindow = 1_050_000
)

var conservativeModelMetadata = dto.UpstreamModelMetadata{
	ContextWindow:    conservativeModelContextWindow,
	MaxContextWindow: conservativeModelContextWindow,
	Complete:         true,
}

// codexCompatibilityModelMetadata covers the ChatGPT subscription model IDs
// that do not have a public API specification. It is intentionally separate
// from officialModelContextWindows: a successful Codex model catalog remains
// authoritative, while this preserves the Codex client's 272K active-window
// and 1M maximum-window compatibility contract when the catalog omits limits.
var codexCompatibilityModelMetadata = map[string]dto.UpstreamModelMetadata{
	"gpt-5.5":       {ContextWindow: 272_000, MaxContextWindow: 1_000_000, Complete: true},
	"gpt-5.6-sol":   {ContextWindow: 272_000, MaxContextWindow: 1_000_000, Complete: true},
	"gpt-5.6-terra": {ContextWindow: 272_000, MaxContextWindow: 1_000_000, Complete: true},
	"gpt-5.6-luna":  {ContextWindow: 272_000, MaxContextWindow: 1_000_000, Complete: true},
}

// officialModelContextWindows is a conservative offline baseline maintained
// from vendor-published specifications. Entries must be concrete public model
// IDs or documented aliases. The live upstream catalog always takes priority.
// Do not add prefix rules here: provider-specific or future model names must
// fall back instead of inheriting an unrelated model's context window.
var officialModelContextWindows = map[string]int{
	// Anthropic: https://platform.claude.com/docs/en/about-claude/models/overview
	"claude-fable-5":             claudeCurrentContextWindow,
	"claude-opus-4-8":             claudeCurrentContextWindow,
	"claude-sonnet-5":             claudeCurrentContextWindow,
	"claude-haiku-4-5":            claudeStandardContextWindow,
	"claude-haiku-4-5-20251001":   claudeStandardContextWindow,
	"claude-opus-4-7":             claudeStandardContextWindow,
	"claude-opus-4-6":             claudeStandardContextWindow,
	"claude-sonnet-4-6":           claudeStandardContextWindow,
	"claude-sonnet-4-5":           claudeStandardContextWindow,
	"claude-sonnet-4-5-20250929":  claudeStandardContextWindow,
	"claude-opus-4-5":             claudeStandardContextWindow,
	"claude-opus-4-5-20251101":    claudeStandardContextWindow,
	"claude-opus-4-1":             claudeStandardContextWindow,
	"claude-opus-4-1-20250805":    claudeStandardContextWindow,
	"claude-opus-4-20250514":      claudeStandardContextWindow,
	"claude-sonnet-4-20250514":    claudeStandardContextWindow,
	"claude-3-7-sonnet":           claudeStandardContextWindow,
	"claude-3-7-sonnet-20250219":  claudeStandardContextWindow,
	"claude-3-5-sonnet":           claudeStandardContextWindow,
	"claude-3-5-sonnet-latest":    claudeStandardContextWindow,
	"claude-3-5-sonnet-20241022":  claudeStandardContextWindow,
	"claude-3-5-sonnet-20240620":  claudeStandardContextWindow,
	"claude-3-5-haiku":            claudeStandardContextWindow,
	"claude-3-5-haiku-latest":     claudeStandardContextWindow,
	"claude-3-5-haiku-20241022":   claudeStandardContextWindow,
	"claude-3-opus":               claudeStandardContextWindow,
	"claude-3-opus-20240229":      claudeStandardContextWindow,
	"claude-3-sonnet":             claudeStandardContextWindow,
	"claude-3-sonnet-20240229":    claudeStandardContextWindow,
	"claude-3-haiku":              claudeStandardContextWindow,
	"claude-3-haiku-20240307":     claudeStandardContextWindow,

	// Google: https://ai.google.dev/gemini-api/docs/models
	"gemini-2.5-pro":        geminiLargeContextWindow,
	"gemini-2.5-flash":      geminiLargeContextWindow,
	"gemini-2.5-flash-lite": geminiLargeContextWindow,
	"gemini-2.0-flash":      geminiLargeContextWindow,
	"gemini-2.0-flash-lite": geminiLargeContextWindow,
	"gemini-3.1-pro-preview": geminiLargeContextWindow,
	"gemini-3-pro-preview":   geminiLargeContextWindow,
	"gemini-3-flash-preview": geminiLargeContextWindow,
	"gemini-3.5-flash":      geminiLargeContextWindow,
	"gemini-3.1-flash-lite": geminiLargeContextWindow,
	"gemini-3.1-flash-image": 131_072,
	"gemini-2.5-flash-image": 65_536,

	// OpenAI: https://developers.openai.com/api/docs/models
	"gpt-5":                    openAIGPT5ContextWindow,
	"gpt-5-2025-08-07":         openAIGPT5ContextWindow,
	"gpt-5-mini":               openAIGPT5ContextWindow,
	"gpt-5-mini-2025-08-07":    openAIGPT5ContextWindow,
	"gpt-5-nano":               openAIGPT5ContextWindow,
	"gpt-5-nano-2025-08-07":    openAIGPT5ContextWindow,
	"gpt-5-pro":                openAIGPT5ContextWindow,
	"gpt-5-pro-2025-10-06":     openAIGPT5ContextWindow,
	"gpt-5-chat-latest":        openAIGPT5ContextWindow,
	"gpt-5-codex":              openAIGPT5ContextWindow,
	"gpt-5-search-api":         openAIGPT5ContextWindow,
	"gpt-5-search-api-2025-10-14": openAIGPT5ContextWindow,
	"gpt-5.1":                  openAIGPT5ContextWindow,
	"gpt-5.1-2025-11-13":       openAIGPT5ContextWindow,
	"gpt-5.1-chat-latest":      openAIGPT5ContextWindow,
	"gpt-5.1-codex":            openAIGPT5ContextWindow,
	"gpt-5.1-codex-max":        openAIGPT5ContextWindow,
	"gpt-5.1-codex-mini":       openAIGPT5ContextWindow,
	"gpt-5.2":                  openAIGPT5ContextWindow,
	"gpt-5.2-2025-12-11":       openAIGPT5ContextWindow,
	"gpt-5.2-chat-latest":      openAIGPT5ContextWindow,
	"gpt-5.2-codex":            openAIGPT5ContextWindow,
	"gpt-5.2-pro":              openAIGPT5ContextWindow,
	"gpt-5.2-pro-2025-12-11":   openAIGPT5ContextWindow,
	"gpt-5.3-chat-latest":      openAIGPT5ContextWindow,
	"gpt-5.3":                  openAIGPT5ContextWindow,
	"gpt-5.3-codex":            openAIGPT5ContextWindow,
	"gpt-5.4":                  openAIGPT54ContextWindow,
	"gpt-5.4-2026-03-05":       openAIGPT54ContextWindow,
	"gpt-5.4-pro":              openAIGPT54ContextWindow,
	"gpt-5.4-pro-2026-03-05":   openAIGPT54ContextWindow,
	"gpt-4.1":                  openAILargeContextWindow,
	"gpt-4.1-2025-04-14":       openAILargeContextWindow,
	"gpt-4.1-mini":             openAILargeContextWindow,
	"gpt-4.1-mini-2025-04-14":  openAILargeContextWindow,
	"gpt-4.1-nano":             openAILargeContextWindow,
	"gpt-4.1-nano-2025-04-14":  openAILargeContextWindow,
	"gpt-4o":                   128_000,
	"gpt-4o-2024-05-13":        128_000,
	"gpt-4o-2024-08-06":        128_000,
	"gpt-4o-2024-11-20":        128_000,
	"gpt-4o-mini":              128_000,
	"gpt-4o-mini-2024-07-18":   128_000,
	"chatgpt-4o-latest":        128_000,
	"gpt-4.5-preview":          128_000,
	"gpt-4.5-preview-2025-02-27": 128_000,
	"gpt-4-turbo":              128_000,
	"gpt-4-turbo-2024-04-09":   128_000,
	"gpt-4-turbo-preview":      128_000,
	"gpt-4-0125-preview":       128_000,
	"gpt-4-1105-preview":       128_000,
	"gpt-4-1106-preview":       128_000,
	"gpt-4-1106-vision-preview": 128_000,
	"gpt-4-vision-preview":     128_000,
	"gpt-4-32k":                32_768,
	"gpt-4-32k-0613":           32_768,
	"gpt-4":                    8_192,
	"gpt-4-0613":               8_192,
	"gpt-4-0314":               8_192,
	"gpt-3.5-turbo":            16_385,
	"gpt-3.5-turbo-16k":        16_385,
	"gpt-3.5-turbo-16k-0613":   16_385,
	"gpt-3.5-turbo-0125":       16_385,
	"gpt-3.5-turbo-1106":       16_385,
	"gpt-3.5-turbo-0301":       4_096,
	"gpt-3.5-turbo-0613":       4_096,
	"gpt-3.5-turbo-instruct":   4_096,
	"gpt-3.5-turbo-instruct-0914": 4_096,
}

// GetOfficialModelMetadata returns the maintained baseline from vendor-published
// model specifications. It deliberately uses the generally available window,
// not a larger preview or beta-only limit.
func GetOfficialModelMetadata(modelName string) (dto.UpstreamModelMetadata, bool) {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	if modelName == "" {
		return dto.UpstreamModelMetadata{}, false
	}
	contextWindow, ok := officialModelContextWindows[modelName]
	if ok {
		return modelMetadataForContextWindow(contextWindow), true
	}
	metadata, ok := codexCompatibilityModelMetadata[modelName]
	return metadata, ok
}

func ConservativeModelMetadata() dto.UpstreamModelMetadata {
	return conservativeModelMetadata
}

func modelMetadataForContextWindow(contextWindow int) dto.UpstreamModelMetadata {
	return dto.UpstreamModelMetadata{
		ContextWindow:    contextWindow,
		MaxContextWindow: contextWindow,
		Complete:         true,
	}
}

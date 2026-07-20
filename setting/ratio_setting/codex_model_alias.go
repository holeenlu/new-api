package ratio_setting

import "strings"

// codexResponsesModelAliases maps user-facing Codex model shorthands to their
// canonical gpt-5.6 family names. Billing ratios, cache ratios, and context
// window metadata are all keyed on the canonical names, so the shorthand must
// be resolved before channel selection and quota calculation. This is the
// single source of truth for the mapping; both the shared Responses request
// validator and the Codex collaboration stream rewriter reference it.
var codexResponsesModelAliases = map[string]string{
	"sol":   "gpt-5.6-sol",
	"terra": "gpt-5.6-terra",
	"luna":  "gpt-5.6-luna",
}

// CanonicalCodexModelAlias returns the canonical model name for a Codex model
// shorthand, or the input unchanged when it is not a known alias.
func CanonicalCodexModelAlias(model string) string {
	if canonical, ok := codexResponsesModelAliases[strings.ToLower(strings.TrimSpace(model))]; ok {
		return canonical
	}
	return model
}

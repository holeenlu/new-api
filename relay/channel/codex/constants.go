package codex

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
)

func ConfiguredModelList() []string {
	raw := common.GetEnvOrDefaultString("CODEX_MODEL_LIST", "")
	models := make([]string, 0)
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		model := strings.TrimSpace(item)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}

const ChannelName = "codex"

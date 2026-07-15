package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfiguredModelList(t *testing.T) {
	t.Setenv("CODEX_MODEL_LIST", "gpt-custom-a, gpt-custom-b,gpt-custom-a")

	require.Equal(t, []string{"gpt-custom-a", "gpt-custom-b"}, ConfiguredModelList())
}

func TestConfiguredModelListDefaultsToUpstreamDiscovery(t *testing.T) {
	t.Setenv("CODEX_MODEL_LIST", "")

	require.Empty(t, ConfiguredModelList())
}

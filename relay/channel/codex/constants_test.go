package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelList(t *testing.T) {
	require.Contains(t, ModelList, "gpt-5.5")
	require.Contains(t, ModelList, "gpt-5.6-sol")
	require.Contains(t, ModelList, "gpt-5.6-terra")
	require.Contains(t, ModelList, "gpt-5.6-luna")
}

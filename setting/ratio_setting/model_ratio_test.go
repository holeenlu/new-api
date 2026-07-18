package ratio_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletionRatioKeepsLegacyLocksAndAllowsGPT54Overrides(t *testing.T) {
	original := CompletionRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, UpdateCompletionRatioByJSONString(original))
	})
	require.NoError(t, UpdateCompletionRatioByJSONString(`{"o1":99,"gpt-5.3":9,"gpt-5.4":7,"gpt-5.6-sol":8,"gpt-5.10":10}`))

	assert.Equal(t, 4.0, GetCompletionRatio("o1"))
	assert.Equal(t, CompletionRatioInfo{Ratio: 4, Locked: true}, GetCompletionRatioInfo("o1"))
	assert.Equal(t, 6.0, GetCompletionRatio("gpt-5.3"))
	assert.Equal(t, CompletionRatioInfo{Ratio: 6, Locked: true}, GetCompletionRatioInfo("gpt-5.3"))
	assert.Equal(t, 7.0, GetCompletionRatio("gpt-5.4"))
	assert.Equal(t, CompletionRatioInfo{Ratio: 7, Locked: false}, GetCompletionRatioInfo("gpt-5.4"))
	assert.Equal(t, 8.0, GetCompletionRatio("gpt-5.6-sol"))
	assert.Equal(t, CompletionRatioInfo{Ratio: 8, Locked: false}, GetCompletionRatioInfo("gpt-5.6-sol"))
	assert.Equal(t, 10.0, GetCompletionRatio("gpt-5.10"))
	assert.Equal(t, CompletionRatioInfo{Ratio: 10, Locked: false}, GetCompletionRatioInfo("gpt-5.10"))
}

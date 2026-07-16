package common

import (
	"io"
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestNewPrivacyFilteredPassThroughJSONBodyFiltersNormalizedSensitiveKeys(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))
	storage, err := rootcommon.CreateBodyStorage([]byte(`{"metadata":{"TRUE-CLIENT-IP":"203.0.113.10","keep":"value"}}`))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	body, _, closer, err := NewPrivacyFilteredPassThroughJSONBody(storage)

	require.NoError(t, err)
	if closer != nil {
		t.Cleanup(func() { require.NoError(t, closer.Close()) })
	}
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"keep":"value"}}`, string(output))
}

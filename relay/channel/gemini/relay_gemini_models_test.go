package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func TestFetchGeminiModelCatalogReadsInputTokenLimit(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1beta/models", r.URL.Path)
		require.Equal(t, "test-key", r.Header.Get("x-goog-api-key"))
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/gemini-2.5-pro","inputTokenLimit":1000000},
			{"name":"models/gemini-invalid","inputTokenLimit":0}
		]}`))
	}))
	t.Cleanup(server.Close)

	catalog, err := FetchGeminiModelCatalog(context.Background(), server.URL, "test-key", "")

	require.NoError(t, err)
	require.Len(t, catalog, 2)
	require.Equal(t, "gemini-2.5-pro", catalog[0].ID)
	require.True(t, catalog[0].Metadata.Valid())
	require.Equal(t, 1_000_000, catalog[0].Metadata.ContextWindow)
	require.False(t, catalog[1].Metadata.Valid())
}

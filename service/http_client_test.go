package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetHttpClientWithResponseHeaderTimeoutOnlyBoundsHeaders(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: &http.Transport{}}
	responseTimeoutClients = make(map[string]*http.Client)
	t.Cleanup(func() {
		ResetProxyClientCache()
		httpClient = oldClient
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(75 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}))
	defer server.Close()

	client, err := GetHttpClientWithResponseHeaderTimeout("", 25*time.Millisecond)
	require.NoError(t, err)
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "done", string(body))
}

func TestGetHttpClientWithResponseHeaderTimeoutFailsBeforeLateHeaders(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: &http.Transport{}}
	responseTimeoutClients = make(map[string]*http.Client)
	t.Cleanup(func() {
		ResetProxyClientCache()
		httpClient = oldClient
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(75 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := GetHttpClientWithResponseHeaderTimeout("", 25*time.Millisecond)
	require.NoError(t, err)
	resp, err := client.Get(server.URL)
	require.Error(t, err)
	require.Nil(t, resp)
}

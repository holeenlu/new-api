package newapi

import (
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRequestURLRemovesConsumedGeminiKey(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:    constant.ChannelTypeNewAPI,
			ChannelBaseUrl: "https://new-api.example",
		},
		RelayFormat:    types.RelayFormatGemini,
		RequestURLPath: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?key=downstream-token&alt=sse&api-version=2024-02-01",
	}

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)

	assert.Equal(t, "https", parsedURL.Scheme)
	assert.Equal(t, "new-api.example", parsedURL.Host)
	assert.Equal(t, "/v1beta/models/gemini-2.5-flash:streamGenerateContent", parsedURL.Path)
	assert.Empty(t, parsedURL.Query().Get("key"))
	assert.Equal(t, "sse", parsedURL.Query().Get("alt"))
	assert.Equal(t, "2024-02-01", parsedURL.Query().Get("api-version"))
}

func TestGetRequestURLPreservesNonGeminiKeyParameter(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:    constant.ChannelTypeNewAPI,
			ChannelBaseUrl: "https://new-api.example",
		},
		RelayFormat:    types.RelayFormatOpenAI,
		RequestURLPath: "/v1/chat/completions?key=upstream-routing-key",
	}

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://new-api.example/v1/chat/completions?key=upstream-routing-key", requestURL)
}

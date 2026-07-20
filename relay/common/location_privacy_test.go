package common

import (
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestFilterUpstreamLocationDataStripRemovesProtocolLocationAndIP(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	input := []byte(`{
		"model":"gpt-5",
		"inference_geo":"eu",
		"web_search_options":{"search_context_size":"medium","user_location":{"type":"approximate","approximate":{"country":"CN","city":"Shanghai","timezone":"Asia/Shanghai"}}},
		"tools":[
			{"type":"web_search","user_location":{"type":"approximate","country":"CN","city":"Shanghai"}},
			{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}
		],
		"toolConfig":{"retrievalConfig":{"latLng":{"latitude":31.2304,"longitude":121.4737},"languageCode":"zh"}},
		"client_metadata":{"installation_id":"install-1","client_ip":"203.0.113.10","timezone":"Asia/Shanghai","nested":{"city":"Shanghai","keep":"value"}},
		"metadata":{"user_id":"user-1","x-forwarded-for":"203.0.113.10"},
		"messages":[{"role":"user","content":"Find weather for city Shanghai and IP 203.0.113.10"}]
	}`)

	out, changed, err := FilterUpstreamLocationData(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"model":"gpt-5",
		"web_search_options":{"search_context_size":"medium"},
		"tools":[
			{"type":"web_search"},
			{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}
		],
		"toolConfig":{"retrievalConfig":{"languageCode":"zh"}},
		"client_metadata":{"installation_id":"install-1","nested":{"keep":"value"}},
		"metadata":{"user_id":"user-1"},
		"messages":[{"role":"user","content":"Find weather for city Shanghai and IP 203.0.113.10"}]
	}`, string(out))
}

func TestFilterUpstreamLocationDataRelayRewritesKnownLocationContainers(t *testing.T) {
	restoreLocationSettings(t)
	latitude := 34.0522
	longitude := -118.2437
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeEgress))
	rootcommon.UpstreamEgressLocationSettings = rootcommon.UpstreamLocationProfile{
		Country:   "US",
		Region:    "California",
		City:      "Los Angeles",
		Timezone:  "America/Los_Angeles",
		Latitude:  &latitude,
		Longitude: &longitude,
	}

	input := []byte(`{
		"inference_geo":"eu",
		"web_search_options":{"user_location":{"type":"approximate","approximate":{"country":"CN","city":"Shanghai"}}},
		"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"CN","city":"Shanghai"}}],
		"toolConfig":{"retrievalConfig":{"latLng":{"latitude":31.2304,"longitude":121.4737}}},
		"client_metadata":{"country":"CN","city":"Shanghai","timezone":"Asia/Shanghai","client_ip":"203.0.113.10"}
	}`)

	out, changed, err := FilterUpstreamLocationData(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"web_search_options":{"user_location":{"type":"approximate","approximate":{"country":"US","region":"California","city":"Los Angeles","timezone":"America/Los_Angeles"}}},
		"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"US","region":"California","city":"Los Angeles","timezone":"America/Los_Angeles"}}],
		"toolConfig":{"retrievalConfig":{"latLng":{"latitude":34.0522,"longitude":-118.2437}}},
		"client_metadata":{"country":"US","city":"Los Angeles","timezone":"America/Los_Angeles"}
	}`, string(out))
}

func TestFilterUpstreamLocationDataRelayWithoutStaticLocationStripsFields(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeEgress))
	rootcommon.UpstreamEgressLocationSettings = rootcommon.UpstreamLocationProfile{}

	out, changed, err := FilterUpstreamLocationData([]byte(`{
		"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"CN"}}],
		"toolConfig":{"retrievalConfig":{"latLng":{"latitude":31.2,"longitude":121.4}}}
	}`))

	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"tools":[{"type":"web_search"}],"toolConfig":{"retrievalConfig":{}}}`, string(out))
}

func TestFilterUpstreamLocationDataClientAllowsLocationButStillRemovesIP(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeClient))
	input := []byte(`{
		"web_search_options":{"user_location":{"country":"CN","city":"Shanghai","ip":"203.0.113.10"}},
		"client_ip":"203.0.113.10",
		"x_real_ip":"203.0.113.10",
		"client_metadata":{"timezone":"Asia/Shanghai","x-forwarded-for":"203.0.113.10"}
	}`)

	out, changed, err := FilterUpstreamLocationData(input)

	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"web_search_options":{"user_location":{"country":"CN","city":"Shanghai"}},
		"client_metadata":{"timezone":"Asia/Shanghai"}
	}`, string(out))
}

func TestFilterUpstreamLocationDataNormalizesSensitiveKeyVariants(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	input := []byte(`{
		"REMOTE_ADDR":"203.0.113.10",
		"Cf-Connecting-Ip":"203.0.113.11",
		"metadata":{
			"Remote.Addr":"203.0.113.12",
			"nested":[{"TRUE-CLIENT-IP":"203.0.113.13","keep":"value"}]
		},
		"model":"gpt-5"
	}`)

	out, changed, err := FilterUpstreamLocationData(input)

	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"metadata":{"nested":[{"keep":"value"}]},"model":"gpt-5"}`, string(out))
}

func TestFilterUpstreamLocationDataParsesEveryJSONRequest(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	_, _, err := FilterUpstreamLocationData([]byte(`{"model":`))

	require.Error(t, err)
}

func TestFilterUpstreamLocationDataAutoSelectsHostOrProxyProfile(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeAuto))
	rootcommon.UpstreamSystemProxyEnabled = false
	rootcommon.UpstreamHostLocationSettings = rootcommon.UpstreamLocationProfile{Country: "CN", City: "Beijing"}
	rootcommon.UpstreamEgressLocationSettings = rootcommon.UpstreamLocationProfile{Country: "US", City: "Los Angeles"}
	proxyURL := "http://proxy.example:8080"
	require.True(t, rootcommon.SetChannelProxyLocationProfile(proxyURL, rootcommon.UpstreamLocationProfile{Country: "JP", City: "Tokyo"}))
	input := []byte(`{"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"SG"}}]}`)

	hostOut, hostChanged, err := FilterUpstreamLocationData(input)
	require.NoError(t, err)
	require.True(t, hostChanged)
	require.JSONEq(t, `{"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"CN","city":"Beijing"}}]}`, string(hostOut))

	proxyOut, proxyChanged, err := FilterUpstreamLocationData(input, proxyURL)
	require.NoError(t, err)
	require.True(t, proxyChanged)
	require.JSONEq(t, `{"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"JP","city":"Tokyo"}}]}`, string(proxyOut))

	rootcommon.UpstreamSystemProxyEnabled = true
	systemProxyOut, systemProxyChanged, err := FilterUpstreamLocationData(input)
	require.NoError(t, err)
	require.True(t, systemProxyChanged)
	require.JSONEq(t, `{"tools":[{"type":"web_search","user_location":{"type":"approximate","country":"US","city":"Los Angeles"}}]}`, string(systemProxyOut))
}

func TestFilterUpstreamLocationDataSanitizesDirectLocationFields(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	out, changed, err := FilterUpstreamLocationData([]byte(`{
		"model":"gpt-5",
		"country":"CN",
		"city":"Shanghai",
		"latitude":31.2,
		"timezone":"Asia/Shanghai",
		"lat_lng":{"latitude":31.2,"longitude":121.4},
		"metadata":{"latlng":{"latitude":31.2,"longitude":121.4}}
	}`))

	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"model":"gpt-5","metadata":{}}`, string(out))
}

func TestFilterUpstreamLocationDataPreservesLargeIntegerFields(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	// 9007199254740993 == 2^53 + 1 is not representable as a float64, so a
	// map[string]interface{} round-trip that decodes numbers to float64 would
	// corrupt it to 9007199254740992. The filter must preserve it exactly even
	// though it re-marshals the body to strip the location field.
	input := []byte(`{"model":"gpt-5","seed":9007199254740993,"user_location":{"type":"approximate","country":"CN"}}`)

	out, changed, err := FilterUpstreamLocationData(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(out), "9007199254740993")
	require.NotContains(t, string(out), "9007199254740992")
	require.NotContains(t, string(out), "user_location")
}

func restoreLocationSettings(t *testing.T) {
	t.Helper()
	originalMode := rootcommon.GetUpstreamLocationMode()
	originalSystemProxyEnabled := rootcommon.UpstreamSystemProxyEnabled
	originalHostLocation := rootcommon.UpstreamHostLocationSettings
	originalLocation := rootcommon.UpstreamEgressLocationSettings
	t.Cleanup(func() {
		require.NoError(t, rootcommon.SetUpstreamLocationMode(originalMode))
		rootcommon.UpstreamSystemProxyEnabled = originalSystemProxyEnabled
		rootcommon.UpstreamHostLocationSettings = originalHostLocation
		rootcommon.UpstreamEgressLocationSettings = originalLocation
	})
}

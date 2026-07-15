package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestUpstreamLocationRuntimeOptionsExposeConfiguredProfiles(t *testing.T) {
	originalSystemProxyEnabled := common.UpstreamSystemProxyEnabled
	originalHost := common.UpstreamHostLocationSettings
	originalEgress := common.UpstreamEgressLocationSettings
	t.Cleanup(func() {
		common.UpstreamSystemProxyEnabled = originalSystemProxyEnabled
		common.UpstreamHostLocationSettings = originalHost
		common.UpstreamEgressLocationSettings = originalEgress
	})

	latitude := 34.0522
	longitude := -118.2437
	common.UpstreamSystemProxyEnabled = true
	common.UpstreamHostLocationSettings = common.UpstreamLocationProfile{PublicIP: "198.51.100.10", Country: "CN", City: "Beijing"}
	common.UpstreamEgressLocationSettings = common.UpstreamLocationProfile{
		PublicIP:  "203.0.113.20",
		Country:   "US",
		City:      "Los Angeles",
		Latitude:  &latitude,
		Longitude: &longitude,
	}

	values := make(map[string]string)
	for _, option := range upstreamLocationRuntimeOptions() {
		values[option.Key] = option.Value
	}

	require.Equal(t, "true", values["UpstreamSystemProxyEnabled"])
	require.Equal(t, "198.51.100.10", values["UpstreamHostPublicIP"])
	require.Equal(t, "CN", values["UpstreamHostLocationCountry"])
	require.Equal(t, "Beijing", values["UpstreamHostLocationCity"])
	require.Equal(t, "US", values["UpstreamEgressLocationCountry"])
	require.Equal(t, "203.0.113.20", values["UpstreamEgressPublicIP"])
	require.Equal(t, "Los Angeles", values["UpstreamEgressLocationCity"])
	require.Equal(t, "34.0522", values["UpstreamEgressLocationLatitude"])
	require.Equal(t, "-118.2437", values["UpstreamEgressLocationLongitude"])
}

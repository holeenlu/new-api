package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitUpstreamLocationSettingsEgress(t *testing.T) {
	restoreUpstreamLocationSettings(t)
	t.Setenv("UPSTREAM_LOCATION_MODE", "egress")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_COUNTRY", "US")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_REGION", "California")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_CITY", "Los Angeles")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_TIMEZONE", "America/Los_Angeles")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_LATITUDE", "34.0522")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_LONGITUDE", "-118.2437")

	initUpstreamLocationSettings()

	require.Equal(t, UpstreamLocationModeEgress, GetUpstreamLocationMode())
	require.Equal(t, "US", UpstreamEgressLocationSettings.Country)
	require.Equal(t, "California", UpstreamEgressLocationSettings.Region)
	require.Equal(t, "Los Angeles", UpstreamEgressLocationSettings.City)
	require.Equal(t, "America/Los_Angeles", UpstreamEgressLocationSettings.Timezone)
	require.NotNil(t, UpstreamEgressLocationSettings.Latitude)
	require.NotNil(t, UpstreamEgressLocationSettings.Longitude)
	require.InDelta(t, 34.0522, *UpstreamEgressLocationSettings.Latitude, 0.000001)
	require.InDelta(t, -118.2437, *UpstreamEgressLocationSettings.Longitude, 0.000001)
}

func TestInitUpstreamLocationSettingsAutoLoadsHostAndProxyProfiles(t *testing.T) {
	restoreUpstreamLocationSettings(t)
	t.Setenv("UPSTREAM_LOCATION_MODE", "auto")
	t.Setenv("UPSTREAM_SYSTEM_PROXY_ENABLED", "true")
	t.Setenv("UPSTREAM_HOST_LOCATION_COUNTRY", "CN")
	t.Setenv("UPSTREAM_HOST_LOCATION_CITY", "Beijing")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_COUNTRY", "US")
	t.Setenv("UPSTREAM_EGRESS_LOCATION_CITY", "Los Angeles")

	initUpstreamLocationSettings()

	require.Equal(t, UpstreamLocationModeAuto, GetUpstreamLocationMode())
	require.True(t, UpstreamSystemProxyEnabled)
	require.Equal(t, "CN", UpstreamHostLocationSettings.Country)
	require.Equal(t, "Beijing", UpstreamHostLocationSettings.City)
	require.Equal(t, "US", UpstreamEgressLocationSettings.Country)
	require.Equal(t, "Los Angeles", UpstreamEgressLocationSettings.City)
}

func TestInitUpstreamLocationSettingsSupportsLegacyRelayAliases(t *testing.T) {
	restoreUpstreamLocationSettings(t)
	t.Setenv("UPSTREAM_LOCATION_MODE", "relay")
	t.Setenv("RELAY_LOCATION_COUNTRY", "JP")

	initUpstreamLocationSettings()

	require.Equal(t, UpstreamLocationModeEgress, GetUpstreamLocationMode())
	require.Equal(t, "JP", UpstreamEgressLocationSettings.Country)
}

func TestInitUpstreamLocationSettingsInvalidValuesFailClosed(t *testing.T) {
	restoreUpstreamLocationSettings(t)
	t.Setenv("UPSTREAM_LOCATION_MODE", "unexpected")
	t.Setenv("RELAY_LOCATION_LATITUDE", "91")
	t.Setenv("RELAY_LOCATION_LONGITUDE", "not-a-number")

	initUpstreamLocationSettings()

	require.Equal(t, UpstreamLocationModeStrip, GetUpstreamLocationMode())
	require.Nil(t, UpstreamEgressLocationSettings.Latitude)
	require.Nil(t, UpstreamEgressLocationSettings.Longitude)
}

func TestDiscoverUpstreamLocationProfileRequiresBothUpstreamsAndEnrichesPublicIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chatgpt", "/claude":
			w.WriteHeader(http.StatusUnauthorized)
		case "/trace":
			_, _ = fmt.Fprint(w, "ip=8.8.8.8\nloc=US\n")
		case "/geo/8.8.8.8":
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","success":true,"country_code":"US","region":"California","city":"Mountain View","latitude":37.4056,"longitude":-122.0775,"timezone":{"id":"America/Los_Angeles"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	profile, err := discoverUpstreamLocationProfile(context.Background(), server.Client(), upstreamLocationDiscoveryEndpoints{
		ProbeURLs:            []string{server.URL + "/chatgpt", server.URL + "/claude"},
		PublicIPTraceURL:     server.URL + "/trace",
		GeoLookupURLTemplate: server.URL + "/geo/{ip}",
	})

	require.NoError(t, err)
	require.Equal(t, "8.8.8.8", profile.PublicIP)
	require.Equal(t, "US", profile.Country)
	require.Equal(t, "California", profile.Region)
	require.Equal(t, "Mountain View", profile.City)
	require.Equal(t, "America/Los_Angeles", profile.Timezone)
	require.NotNil(t, profile.Latitude)
	require.NotNil(t, profile.Longitude)
	require.InDelta(t, 37.4056, *profile.Latitude, 0.000001)
	require.InDelta(t, -122.0775, *profile.Longitude, 0.000001)
}

func TestDiscoverUpstreamLocationProfileRejectsIncompleteConnectivityCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	server.Close()

	profile, err := discoverUpstreamLocationProfile(context.Background(), http.DefaultClient, upstreamLocationDiscoveryEndpoints{
		ProbeURLs:            []string{server.URL},
		PublicIPTraceURL:     server.URL,
		GeoLookupURLTemplate: server.URL + "/{ip}",
	})

	require.Error(t, err)
	require.Empty(t, profile)
}

func TestDiscoverUpstreamLocationProfileDoesNotFollowRedirects(t *testing.T) {
	redirectTargetReached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			redirectTargetReached = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	defer server.Close()

	_, err := discoverUpstreamLocationProfile(context.Background(), server.Client(), upstreamLocationDiscoveryEndpoints{
		ProbeURLs:            []string{server.URL + "/probe"},
		PublicIPTraceURL:     server.URL + "/trace",
		GeoLookupURLTemplate: server.URL + "/geo/{ip}",
	})

	require.Error(t, err)
	require.False(t, redirectTargetReached)
}

func TestMergeUpstreamLocationProfilePreservesExplicitConfiguration(t *testing.T) {
	discoveredLatitude := 37.4056
	discoveredLongitude := -122.0775
	profile := mergeUpstreamLocationProfile(
		UpstreamLocationProfile{Country: "CA", City: "Toronto"},
		UpstreamLocationProfile{
			PublicIP:  "8.8.8.8",
			Country:   "US",
			Region:    "California",
			City:      "Mountain View",
			Timezone:  "America/Los_Angeles",
			Latitude:  &discoveredLatitude,
			Longitude: &discoveredLongitude,
		},
	)

	require.Equal(t, "8.8.8.8", profile.PublicIP)
	require.Equal(t, "CA", profile.Country)
	require.Equal(t, "California", profile.Region)
	require.Equal(t, "Toronto", profile.City)
	require.Equal(t, "America/Los_Angeles", profile.Timezone)
}

func TestRefreshUpstreamLocationProfilesReplacesDiscoveredValuesAndPreservesExplicitBaseline(t *testing.T) {
	restoreUpstreamLocationSettings(t)
	t.Setenv("UPSTREAM_LOCATION_DISCOVERY_ENABLED", "false")
	t.Setenv("UPSTREAM_HOST_LOCATION_COUNTRY", "CA")
	initUpstreamLocationSettings()

	var version atomic.Int32
	version.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chatgpt", "/claude":
			w.WriteHeader(http.StatusUnauthorized)
		case "/trace":
			switch version.Load() {
			case 1:
				_, _ = fmt.Fprint(w, "ip=8.8.8.8\nloc=US\n")
			case 2:
				_, _ = fmt.Fprint(w, "ip=1.1.1.1\nloc=AU\n")
			default:
				_, _ = fmt.Fprint(w, "ip=9.9.9.9\nloc=CH\n")
			}
		case "/geo/8.8.8.8":
			_, _ = fmt.Fprint(w, `{"ip":"8.8.8.8","success":true,"country_code":"US","city":"Mountain View","timezone":{"id":"America/Los_Angeles"}}`)
		case "/geo/1.1.1.1":
			_, _ = fmt.Fprint(w, `{"ip":"1.1.1.1","success":true,"country_code":"AU","city":"Sydney","timezone":{"id":"Australia/Sydney"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restoreEndpoints := defaultUpstreamLocationDiscoveryEndpoints
	defaultUpstreamLocationDiscoveryEndpoints = upstreamLocationDiscoveryEndpoints{
		ProbeURLs:            []string{server.URL + "/chatgpt", server.URL + "/claude"},
		PublicIPTraceURL:     server.URL + "/trace",
		GeoLookupURLTemplate: server.URL + "/geo/{ip}",
	}
	t.Cleanup(func() { defaultUpstreamLocationDiscoveryEndpoints = restoreEndpoints })

	first, err := RefreshUpstreamLocationProfiles(context.Background(), server.Client())
	require.NoError(t, err)
	require.True(t, first.Host.Updated)
	require.Equal(t, "8.8.8.8", first.Host.Profile.PublicIP)
	require.Equal(t, "CA", first.Host.Profile.Country)
	require.Equal(t, "Mountain View", first.Host.Profile.City)

	version.Store(2)
	second, err := RefreshUpstreamLocationProfiles(context.Background(), server.Client())
	require.NoError(t, err)
	require.True(t, second.Host.Updated)
	require.Equal(t, "1.1.1.1", second.Host.Profile.PublicIP)
	require.Equal(t, "CA", second.Host.Profile.Country)
	require.Equal(t, "Sydney", second.Host.Profile.City)

	version.Store(3)
	third, err := RefreshUpstreamLocationProfiles(context.Background(), server.Client())
	require.NoError(t, err)
	require.True(t, third.Host.Updated)
	require.Error(t, third.Host.Err)
	require.Equal(t, "9.9.9.9", third.Host.Profile.PublicIP)
	require.Equal(t, "CA", third.Host.Profile.Country)
	require.Equal(t, "Sydney", third.Host.Profile.City)
}

func TestSetUpstreamLocationModeValidatesAndUpdatesAtomically(t *testing.T) {
	restoreUpstreamLocationSettings(t)

	require.NoError(t, SetUpstreamLocationMode(UpstreamLocationModeAuto))
	require.Equal(t, UpstreamLocationModeAuto, GetUpstreamLocationMode())
	require.Error(t, SetUpstreamLocationMode("unexpected"))
	require.Equal(t, UpstreamLocationModeAuto, GetUpstreamLocationMode())
}

func restoreUpstreamLocationSettings(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("UPSTREAM_LOCATION_DISCOVERY_ENABLED", "false")
	originalMode := GetUpstreamLocationMode()
	originalSystemProxyEnabled := UpstreamSystemProxyEnabled
	originalDiscoveryEnabled := UpstreamLocationDiscoveryEnabled
	originalDiscoveryTimeout := UpstreamLocationDiscoveryTimeout
	originalHostLocation := UpstreamHostLocationSettings
	originalLocation := UpstreamEgressLocationSettings
	originalHostBaseline := upstreamHostLocationBaseline
	originalEgressBaseline := upstreamEgressLocationBaseline
	t.Cleanup(func() {
		require.NoError(t, SetUpstreamLocationMode(originalMode))
		UpstreamSystemProxyEnabled = originalSystemProxyEnabled
		UpstreamLocationDiscoveryEnabled = originalDiscoveryEnabled
		UpstreamLocationDiscoveryTimeout = originalDiscoveryTimeout
		upstreamLocationProfilesMutex.Lock()
		upstreamHostLocationBaseline = originalHostBaseline
		upstreamEgressLocationBaseline = originalEgressBaseline
		UpstreamHostLocationSettings = originalHostLocation
		UpstreamEgressLocationSettings = originalLocation
		upstreamLocationProfilesMutex.Unlock()
	})
}

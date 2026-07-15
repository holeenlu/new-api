package common

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type upstreamLocationDiscoveryEndpoints struct {
	ProbeURLs            []string
	PublicIPTraceURL     string
	GeoLookupURLTemplate string
}

var defaultUpstreamLocationDiscoveryEndpoints = upstreamLocationDiscoveryEndpoints{
	ProbeURLs: []string{
		"https://chatgpt.com/backend-api/codex/models",
		"https://api.anthropic.com/v1/models",
	},
	PublicIPTraceURL:     "https://www.cloudflare.com/cdn-cgi/trace",
	GeoLookupURLTemplate: "https://ipwho.is/{ip}",
}

func initUpstreamLocationSettings() {
	mode := strings.ToLower(strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_LOCATION_MODE", UpstreamLocationModeStrip)))
	switch mode {
	case UpstreamLocationModeStrip, UpstreamLocationModeAuto, UpstreamLocationModeHost,
		UpstreamLocationModeEgress, UpstreamLocationModeClient:
		_ = SetUpstreamLocationMode(mode)
	case UpstreamLocationModeRelay:
		_ = SetUpstreamLocationMode(UpstreamLocationModeEgress)
	default:
		log.Printf("WARNING: invalid UPSTREAM_LOCATION_MODE %q; falling back to strip", mode)
		_ = SetUpstreamLocationMode(UpstreamLocationModeStrip)
	}
	UpstreamSystemProxyEnabled = GetEnvOrDefaultBool("UPSTREAM_SYSTEM_PROXY_ENABLED", false)
	if hasSystemProxyEnvironment() {
		UpstreamSystemProxyEnabled = true
	}
	UpstreamLocationDiscoveryEnabled = GetEnvOrDefaultBool("UPSTREAM_LOCATION_DISCOVERY_ENABLED", false)
	UpstreamLocationDiscoveryTimeout = GetEnvOrDefault("UPSTREAM_LOCATION_DISCOVERY_TIMEOUT", 8)
	if UpstreamLocationDiscoveryTimeout < 1 || UpstreamLocationDiscoveryTimeout > 30 {
		log.Printf("WARNING: UPSTREAM_LOCATION_DISCOVERY_TIMEOUT must be between 1 and 30 seconds; using 8 seconds")
		UpstreamLocationDiscoveryTimeout = 8
	}

	UpstreamHostLocationSettings = UpstreamLocationProfile{
		PublicIP: strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_PUBLIC_IP", "")),
		Country:  strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_COUNTRY", "")),
		Region:   strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_REGION", "")),
		City:     strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_CITY", "")),
		Timezone: strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_TIMEZONE", "")),
	}
	UpstreamHostLocationSettings.Latitude = parseLocationCoordinate("UPSTREAM_HOST_LOCATION_LATITUDE", "", -90, 90)
	UpstreamHostLocationSettings.Longitude = parseLocationCoordinate("UPSTREAM_HOST_LOCATION_LONGITUDE", "", -180, 180)

	UpstreamEgressLocationSettings = UpstreamLocationProfile{
		PublicIP: locationEnvValue("UPSTREAM_EGRESS_PUBLIC_IP", ""),
		Country:  locationEnvValue("UPSTREAM_EGRESS_LOCATION_COUNTRY", "RELAY_LOCATION_COUNTRY"),
		Region:   locationEnvValue("UPSTREAM_EGRESS_LOCATION_REGION", "RELAY_LOCATION_REGION"),
		City:     locationEnvValue("UPSTREAM_EGRESS_LOCATION_CITY", "RELAY_LOCATION_CITY"),
		Timezone: locationEnvValue("UPSTREAM_EGRESS_LOCATION_TIMEZONE", "RELAY_LOCATION_TIMEZONE"),
	}
	UpstreamEgressLocationSettings.Latitude = parseLocationCoordinate(
		"UPSTREAM_EGRESS_LOCATION_LATITUDE", "RELAY_LOCATION_LATITUDE", -90, 90,
	)
	UpstreamEgressLocationSettings.Longitude = parseLocationCoordinate(
		"UPSTREAM_EGRESS_LOCATION_LONGITUDE", "RELAY_LOCATION_LONGITUDE", -180, 180,
	)

	if !UpstreamLocationDiscoveryEnabled {
		warnMissingUpstreamLocationProfiles()
	}
}

func GetUpstreamLocationMode() string {
	return upstreamLocationModeValue.Load().(string)
}

func ValidateUpstreamLocationMode(mode string) error {
	switch mode {
	case UpstreamLocationModeStrip, UpstreamLocationModeAuto, UpstreamLocationModeHost,
		UpstreamLocationModeEgress, UpstreamLocationModeClient:
		return nil
	default:
		return fmt.Errorf("invalid upstream location mode %q", mode)
	}
}

func SetUpstreamLocationMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if err := ValidateUpstreamLocationMode(mode); err != nil {
		return err
	}
	upstreamLocationModeValue.Store(mode)
	return nil
}

// DiscoverUpstreamLocationProfiles identifies the public network profile only
// after both subscription upstreams are reachable through the tested route.
// Explicit environment values take precedence over discovered values.
func DiscoverUpstreamLocationProfiles(routedClient *http.Client) {
	if !UpstreamLocationDiscoveryEnabled {
		return
	}

	timeout := time.Duration(UpstreamLocationDiscoveryTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	directTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || directTransport == nil {
		log.Printf("WARNING: upstream location discovery cannot create a direct HTTP transport")
		warnMissingUpstreamLocationProfiles()
		return
	}
	directTransport = directTransport.Clone()
	directTransport.Proxy = nil
	directClient := &http.Client{
		Transport: directTransport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	type discoveryResult struct {
		profile UpstreamLocationProfile
		err     error
	}
	hostResultChannel := make(chan discoveryResult, 1)
	go func() {
		profile, err := discoverUpstreamLocationProfile(ctx, directClient, defaultUpstreamLocationDiscoveryEndpoints)
		hostResultChannel <- discoveryResult{profile: profile, err: err}
	}()

	discoverEgress := UpstreamSystemProxyEnabled
	var egressResultChannel chan discoveryResult
	if discoverEgress {
		egressResultChannel = make(chan discoveryResult, 1)
		if routedClient == nil {
			routedClient = http.DefaultClient
		}
		routedClientCopy := *routedClient
		routedClientCopy.Timeout = timeout
		go func() {
			profile, err := discoverUpstreamLocationProfile(ctx, &routedClientCopy, defaultUpstreamLocationDiscoveryEndpoints)
			egressResultChannel <- discoveryResult{profile: profile, err: err}
		}()
	}

	hostResult := <-hostResultChannel
	if hostResult.profile.PublicIP != "" {
		UpstreamHostLocationSettings = mergeUpstreamLocationProfile(UpstreamHostLocationSettings, hostResult.profile)
	}
	if hostResult.err != nil {
		log.Printf("WARNING: direct upstream network profile discovery failed: %v", hostResult.err)
	} else {
		log.Printf("upstream host network profile discovered after ChatGPT and Claude connectivity checks")
	}

	if discoverEgress {
		egressResult := <-egressResultChannel
		if egressResult.profile.PublicIP != "" {
			UpstreamEgressLocationSettings = mergeUpstreamLocationProfile(UpstreamEgressLocationSettings, egressResult.profile)
		}
		if egressResult.err != nil {
			log.Printf("WARNING: proxy or VPN egress network profile discovery failed: %v", egressResult.err)
		} else {
			log.Printf("upstream proxy or VPN egress profile discovered after ChatGPT and Claude connectivity checks")
		}
	}

	warnMissingUpstreamLocationProfiles()
}

func discoverUpstreamLocationProfile(ctx context.Context, client *http.Client, endpoints upstreamLocationDiscoveryEndpoints) (UpstreamLocationProfile, error) {
	if client == nil {
		return UpstreamLocationProfile{}, fmt.Errorf("HTTP client is nil")
	}
	for _, probeURL := range endpoints.ProbeURLs {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return UpstreamLocationProfile{}, fmt.Errorf("create upstream connectivity request: %w", err)
		}
		request.Header.Set("User-Agent", "new-api-upstream-location-discovery")
		response, err := client.Do(request)
		if err != nil {
			return UpstreamLocationProfile{}, fmt.Errorf("connect to %s: %w", request.URL.Hostname(), err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.PublicIPTraceURL, nil)
	if err != nil {
		return UpstreamLocationProfile{}, fmt.Errorf("create public IP trace request: %w", err)
	}
	request.Header.Set("User-Agent", "new-api-upstream-location-discovery")
	response, err := client.Do(request)
	if err != nil {
		return UpstreamLocationProfile{}, fmt.Errorf("discover public IP: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return UpstreamLocationProfile{}, fmt.Errorf("public IP trace returned HTTP %d", response.StatusCode)
	}
	traceData, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return UpstreamLocationProfile{}, fmt.Errorf("read public IP trace: %w", err)
	}
	traceFields := make(map[string]string)
	for _, line := range strings.Split(string(traceData), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			traceFields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	publicIP := net.ParseIP(traceFields["ip"])
	if publicIP == nil || !publicIP.IsGlobalUnicast() || publicIP.IsPrivate() || publicIP.IsLoopback() {
		return UpstreamLocationProfile{}, fmt.Errorf("public IP trace returned an invalid address")
	}
	profile := UpstreamLocationProfile{
		PublicIP: publicIP.String(),
		Country:  strings.TrimSpace(traceFields["loc"]),
	}

	geoProfile, err := lookupUpstreamLocationProfile(ctx, client, endpoints.GeoLookupURLTemplate, publicIP)
	if err != nil {
		return profile, fmt.Errorf("public IP discovered but geolocation enrichment failed: %w", err)
	}
	return mergeUpstreamLocationProfile(profile, geoProfile), nil
}

func lookupUpstreamLocationProfile(ctx context.Context, client *http.Client, lookupURLTemplate string, publicIP net.IP) (UpstreamLocationProfile, error) {
	lookupURL := strings.ReplaceAll(lookupURLTemplate, "{ip}", url.PathEscape(publicIP.String()))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL, nil)
	if err != nil {
		return UpstreamLocationProfile{}, fmt.Errorf("create geolocation request: %w", err)
	}
	request.Header.Set("User-Agent", "new-api-upstream-location-discovery")
	response, err := client.Do(request)
	if err != nil {
		return UpstreamLocationProfile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return UpstreamLocationProfile{}, fmt.Errorf("geolocation service returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		IP          string   `json:"ip"`
		Success     bool     `json:"success"`
		Message     string   `json:"message"`
		CountryCode string   `json:"country_code"`
		Region      string   `json:"region"`
		City        string   `json:"city"`
		Latitude    *float64 `json:"latitude"`
		Longitude   *float64 `json:"longitude"`
		Timezone    struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if err := DecodeJson(io.LimitReader(response.Body, 256*1024), &payload); err != nil {
		return UpstreamLocationProfile{}, err
	}
	if !payload.Success {
		return UpstreamLocationProfile{}, fmt.Errorf("geolocation service rejected the lookup: %s", strings.TrimSpace(payload.Message))
	}
	lookupIP := net.ParseIP(strings.TrimSpace(payload.IP))
	if lookupIP == nil || !lookupIP.Equal(publicIP) {
		return UpstreamLocationProfile{}, fmt.Errorf("geolocation service returned a different public IP")
	}
	if payload.Latitude != nil && (math.IsNaN(*payload.Latitude) || math.IsInf(*payload.Latitude, 0) || *payload.Latitude < -90 || *payload.Latitude > 90) {
		payload.Latitude = nil
	}
	if payload.Longitude != nil && (math.IsNaN(*payload.Longitude) || math.IsInf(*payload.Longitude, 0) || *payload.Longitude < -180 || *payload.Longitude > 180) {
		payload.Longitude = nil
	}
	return UpstreamLocationProfile{
		Country:   strings.TrimSpace(payload.CountryCode),
		Region:    strings.TrimSpace(payload.Region),
		City:      strings.TrimSpace(payload.City),
		Timezone:  strings.TrimSpace(payload.Timezone.ID),
		Latitude:  payload.Latitude,
		Longitude: payload.Longitude,
	}, nil
}

func mergeUpstreamLocationProfile(preferred UpstreamLocationProfile, fallback UpstreamLocationProfile) UpstreamLocationProfile {
	if preferred.PublicIP == "" {
		preferred.PublicIP = fallback.PublicIP
	}
	if preferred.Country == "" {
		preferred.Country = fallback.Country
	}
	if preferred.Region == "" {
		preferred.Region = fallback.Region
	}
	if preferred.City == "" {
		preferred.City = fallback.City
	}
	if preferred.Timezone == "" {
		preferred.Timezone = fallback.Timezone
	}
	if preferred.Latitude == nil {
		preferred.Latitude = fallback.Latitude
	}
	if preferred.Longitude == nil {
		preferred.Longitude = fallback.Longitude
	}
	return preferred
}

func hasSystemProxyEnvironment() bool {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func warnMissingUpstreamLocationProfiles() {
	mode := GetUpstreamLocationMode()
	if (mode == UpstreamLocationModeHost || mode == UpstreamLocationModeAuto) &&
		!hasConfiguredLocation(UpstreamHostLocationSettings) {
		log.Printf("WARNING: host location profile is unavailable; matching client location fields will be stripped")
	}
	if (mode == UpstreamLocationModeEgress || mode == UpstreamLocationModeAuto) &&
		!hasConfiguredLocation(UpstreamEgressLocationSettings) {
		log.Printf("WARNING: proxy egress location profile is unavailable; matching client location fields will be stripped")
	}
}

func locationEnvValue(name string, legacyName string) string {
	value := strings.TrimSpace(GetEnvOrDefaultString(name, ""))
	if value != "" {
		return value
	}
	return strings.TrimSpace(GetEnvOrDefaultString(legacyName, ""))
}

func parseLocationCoordinate(name string, legacyName string, minValue float64, maxValue float64) *float64 {
	raw := locationEnvValue(name, legacyName)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < minValue || value > maxValue {
		log.Printf("WARNING: invalid %s %q; coordinate will not be forwarded", name, raw)
		return nil
	}
	return &value
}

func hasConfiguredLocation(location UpstreamLocationProfile) bool {
	return location.Country != "" || location.Region != "" || location.City != "" ||
		location.Timezone != "" || location.Latitude != nil || location.Longitude != nil
}

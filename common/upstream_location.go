package common

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"sync"
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

var ErrUpstreamLocationRefreshInProgress = errors.New("upstream location profile refresh is already in progress")

type UpstreamLocationDiscoveryRouteResult struct {
	Attempted bool
	Updated   bool
	Profile   UpstreamLocationProfile
	Err       error
}

type UpstreamLocationDiscoveryReport struct {
	Host   UpstreamLocationDiscoveryRouteResult
	Egress UpstreamLocationDiscoveryRouteResult
}

var (
	upstreamLocationProfilesMutex  sync.RWMutex
	upstreamLocationRefreshMutex   sync.Mutex
	upstreamHostLocationBaseline   UpstreamLocationProfile
	upstreamEgressLocationBaseline UpstreamLocationProfile
	channelProxyLocationProfiles   sync.Map
)

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

	hostProfile := UpstreamLocationProfile{
		PublicIP: strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_PUBLIC_IP", "")),
		Country:  strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_COUNTRY", "")),
		Region:   strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_REGION", "")),
		City:     strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_CITY", "")),
		Timezone: strings.TrimSpace(GetEnvOrDefaultString("UPSTREAM_HOST_LOCATION_TIMEZONE", "")),
	}
	hostProfile.Latitude = parseLocationCoordinate("UPSTREAM_HOST_LOCATION_LATITUDE", "", -90, 90)
	hostProfile.Longitude = parseLocationCoordinate("UPSTREAM_HOST_LOCATION_LONGITUDE", "", -180, 180)

	egressProfile := UpstreamLocationProfile{
		PublicIP: locationEnvValue("UPSTREAM_EGRESS_PUBLIC_IP", ""),
		Country:  locationEnvValue("UPSTREAM_EGRESS_LOCATION_COUNTRY", "RELAY_LOCATION_COUNTRY"),
		Region:   locationEnvValue("UPSTREAM_EGRESS_LOCATION_REGION", "RELAY_LOCATION_REGION"),
		City:     locationEnvValue("UPSTREAM_EGRESS_LOCATION_CITY", "RELAY_LOCATION_CITY"),
		Timezone: locationEnvValue("UPSTREAM_EGRESS_LOCATION_TIMEZONE", "RELAY_LOCATION_TIMEZONE"),
	}
	egressProfile.Latitude = parseLocationCoordinate(
		"UPSTREAM_EGRESS_LOCATION_LATITUDE", "RELAY_LOCATION_LATITUDE", -90, 90,
	)
	egressProfile.Longitude = parseLocationCoordinate(
		"UPSTREAM_EGRESS_LOCATION_LONGITUDE", "RELAY_LOCATION_LONGITUDE", -180, 180,
	)
	setInitialUpstreamLocationProfiles(hostProfile, egressProfile)

	if !UpstreamLocationDiscoveryEnabled {
		warnMissingUpstreamLocationProfiles()
	}
}

func setInitialUpstreamLocationProfiles(host UpstreamLocationProfile, egress UpstreamLocationProfile) {
	upstreamLocationProfilesMutex.Lock()
	defer upstreamLocationProfilesMutex.Unlock()
	upstreamHostLocationBaseline = host
	upstreamEgressLocationBaseline = egress
	UpstreamHostLocationSettings = host
	UpstreamEgressLocationSettings = egress
}

func GetUpstreamLocationProfiles() (UpstreamLocationProfile, UpstreamLocationProfile) {
	upstreamLocationProfilesMutex.RLock()
	defer upstreamLocationProfilesMutex.RUnlock()
	return UpstreamHostLocationSettings, UpstreamEgressLocationSettings
}

func channelProxyLocationProfileKey(proxyURL string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(proxyURL))))
}

func SetChannelProxyLocationProfile(proxyURL string, profile UpstreamLocationProfile) bool {
	if strings.TrimSpace(proxyURL) == "" || !hasConfiguredLocation(profile) {
		return false
	}
	channelProxyLocationProfiles.Store(channelProxyLocationProfileKey(proxyURL), profile)
	return true
}

func GetChannelProxyLocationProfile(proxyURL string) (UpstreamLocationProfile, bool) {
	if strings.TrimSpace(proxyURL) == "" {
		return UpstreamLocationProfile{}, false
	}
	value, ok := channelProxyLocationProfiles.Load(channelProxyLocationProfileKey(proxyURL))
	if !ok {
		return UpstreamLocationProfile{}, false
	}
	profile, ok := value.(UpstreamLocationProfile)
	return profile, ok
}

func DiscoverChannelProxyLocationProfile(ctx context.Context, client *http.Client) (UpstreamLocationProfile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(UpstreamLocationDiscoveryTimeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return discoverUpstreamLocationProfile(ctx, client, defaultUpstreamLocationDiscoveryEndpoints)
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
	upstreamLocationRefreshMutex.Lock()
	defer upstreamLocationRefreshMutex.Unlock()

	timeout := time.Duration(UpstreamLocationDiscoveryTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	report := discoverAndUpdateUpstreamLocationProfiles(ctx, routedClient)
	logUpstreamLocationDiscoveryReport(report)
	warnMissingUpstreamLocationProfiles()
}

// RefreshUpstreamLocationProfiles performs an operator-requested refresh even
// when automatic startup discovery is disabled. Fixed discovery endpoints and
// the server-configured routed client are used; callers cannot supply URLs.
func RefreshUpstreamLocationProfiles(ctx context.Context, routedClient *http.Client) (UpstreamLocationDiscoveryReport, error) {
	if !upstreamLocationRefreshMutex.TryLock() {
		return UpstreamLocationDiscoveryReport{}, ErrUpstreamLocationRefreshInProgress
	}
	defer upstreamLocationRefreshMutex.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(UpstreamLocationDiscoveryTimeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report := discoverAndUpdateUpstreamLocationProfiles(ctx, routedClient)
	logUpstreamLocationDiscoveryReport(report)
	warnMissingUpstreamLocationProfiles()
	return report, nil
}

func discoverAndUpdateUpstreamLocationProfiles(ctx context.Context, routedClient *http.Client) UpstreamLocationDiscoveryReport {
	currentHost, currentEgress := GetUpstreamLocationProfiles()
	report := UpstreamLocationDiscoveryReport{
		Host:   UpstreamLocationDiscoveryRouteResult{Attempted: true, Profile: currentHost},
		Egress: UpstreamLocationDiscoveryRouteResult{Profile: currentEgress},
	}

	directTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || directTransport == nil {
		report.Host.Err = errors.New("upstream location discovery cannot create a direct HTTP transport")
		return report
	}
	directTransport = directTransport.Clone()
	directTransport.Proxy = nil
	directClient := &http.Client{
		Transport: directTransport,
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
	report.Egress.Attempted = discoverEgress
	var egressResultChannel chan discoveryResult
	if discoverEgress {
		egressResultChannel = make(chan discoveryResult, 1)
		if routedClient == nil {
			routedClient = http.DefaultClient
		}
		go func() {
			profile, err := discoverUpstreamLocationProfile(ctx, routedClient, defaultUpstreamLocationDiscoveryEndpoints)
			egressResultChannel <- discoveryResult{profile: profile, err: err}
		}()
	}

	hostResult := <-hostResultChannel
	report.Host.Profile = hostResult.profile
	report.Host.Err = hostResult.err
	report.Host.Updated = hostResult.profile.PublicIP != ""

	if discoverEgress {
		egressResult := <-egressResultChannel
		report.Egress.Profile = egressResult.profile
		report.Egress.Err = egressResult.err
		report.Egress.Updated = egressResult.profile.PublicIP != ""
	}

	upstreamLocationProfilesMutex.Lock()
	if report.Host.Updated {
		discoveredProfile := mergeUpstreamLocationProfile(report.Host.Profile, currentHost)
		UpstreamHostLocationSettings = mergeUpstreamLocationProfile(upstreamHostLocationBaseline, discoveredProfile)
	}
	if report.Egress.Updated {
		discoveredProfile := mergeUpstreamLocationProfile(report.Egress.Profile, currentEgress)
		UpstreamEgressLocationSettings = mergeUpstreamLocationProfile(upstreamEgressLocationBaseline, discoveredProfile)
	}
	report.Host.Profile = UpstreamHostLocationSettings
	report.Egress.Profile = UpstreamEgressLocationSettings
	upstreamLocationProfilesMutex.Unlock()

	return report
}

func logUpstreamLocationDiscoveryReport(report UpstreamLocationDiscoveryReport) {
	if report.Host.Err != nil {
		log.Printf("WARNING: direct upstream network profile discovery failed: %v", report.Host.Err)
	} else if report.Host.Updated {
		log.Printf("upstream host network profile discovered after ChatGPT and Claude connectivity checks")
	}
	if report.Egress.Attempted {
		if report.Egress.Err != nil {
			log.Printf("WARNING: proxy or VPN egress network profile discovery failed: %v", report.Egress.Err)
		} else if report.Egress.Updated {
			log.Printf("upstream proxy or VPN egress profile discovered after ChatGPT and Claude connectivity checks")
		}
	}
}

func discoverUpstreamLocationProfile(ctx context.Context, client *http.Client, endpoints upstreamLocationDiscoveryEndpoints) (UpstreamLocationProfile, error) {
	if client == nil {
		return UpstreamLocationProfile{}, fmt.Errorf("HTTP client is nil")
	}
	discoveryClient := *client
	discoveryClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for _, probeURL := range endpoints.ProbeURLs {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return UpstreamLocationProfile{}, fmt.Errorf("create upstream connectivity request: %w", err)
		}
		request.Header.Set("User-Agent", "new-api-upstream-location-discovery")
		response, err := discoveryClient.Do(request)
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
	response, err := discoveryClient.Do(request)
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

	geoProfile, err := lookupUpstreamLocationProfile(ctx, &discoveryClient, endpoints.GeoLookupURLTemplate, publicIP)
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
	host, egress := GetUpstreamLocationProfiles()
	if (mode == UpstreamLocationModeHost || mode == UpstreamLocationModeAuto) &&
		!hasConfiguredLocation(host) {
		log.Printf("WARNING: host location profile is unavailable; matching client location fields will be stripped")
	}
	if (mode == UpstreamLocationModeEgress || mode == UpstreamLocationModeAuto) &&
		!hasConfiguredLocation(egress) {
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

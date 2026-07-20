package service

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"golang.org/x/net/proxy"
)

var (
	httpClient              *http.Client
	ssrfProtectedHTTPClient *http.Client
	responseTimeoutLock     sync.Mutex
	proxyClients            = proxyHTTPClientCache{
		clients: make(map[string]*http.Client),
		aliases: make(map[string]string),
	}
	legacyProxyURLWarnings sync.Map
	responseTimeoutClients = make(map[string]*http.Client)
)

type proxyHTTPClientCache struct {
	mutex   sync.RWMutex
	clients map[string]*http.Client
	aliases map[string]string
}

type proxyURLConfig struct {
	parsedURL *url.URL
	cacheKey  string
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	urlStr := req.URL.String()
	if err := validateURLWithCurrentFetchSetting(urlStr, true); err != nil {
		return fmt.Errorf("redirect to %s blocked: %v", urlStr, err)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

func checkProtectedFetchRedirect(req *http.Request, via []*http.Request) error {
	urlStr := req.URL.String()
	if err := ValidateSSRFProtectedFetchURL(urlStr); err != nil {
		return fmt.Errorf("redirect to %s blocked: %v", urlStr, err)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

func validateURLWithCurrentFetchSetting(urlStr string, applyDomainIPFilter bool) error {
	fetchSetting := system_setting.GetFetchSetting()
	return common.ValidateURLWithFetchSetting(urlStr, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, applyDomainIPFilter && fetchSetting.ApplyIPFilterForDomain)
}

func ValidateSSRFProtectedFetchURL(urlStr string) error {
	return validateURLWithCurrentFetchSetting(urlStr, true)
}

func newRelayHTTPTransport() *http.Transport {
	var transport *http.Transport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && defaultTransport != nil {
		transport = defaultTransport.Clone()
	} else {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	transport.MaxIdleConns = common.RelayMaxIdleConns
	transport.MaxIdleConnsPerHost = common.RelayMaxIdleConnsPerHost
	transport.IdleConnTimeout = time.Duration(common.RelayIdleConnTimeout) * time.Second
	transport.ForceAttemptHTTP2 = true
	if common.TLSInsecureSkipVerify {
		transport.TLSClientConfig = common.InsecureTLSConfig
	}
	return transport
}

func newRelayHTTPClient(transport *http.Transport) *http.Client {
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}
	if common.RelayTimeout != 0 {
		client.Timeout = time.Duration(common.RelayTimeout) * time.Second
	}
	return client
}

func InitHttpClient() {
	transport := newRelayHTTPTransport()
	transport.Proxy = http.ProxyFromEnvironment
	httpClient = newRelayHTTPClient(transport)
	ssrfProtectedHTTPClient = newProtectedFetchHTTPClient()
}

// GetHttpClient returns the general outbound client used by relay/provider
// integrations. Do not attach the SSRF-protected dialer here: provider base URLs
// are root/operator-managed deployment targets, not arbitrary user-controlled
// input, and may legitimately point at private networks, private-link endpoints,
// self-hosted services, or local proxies. Code paths that fetch arbitrary
// user-controlled URLs must use GetSSRFProtectedHTTPClient or
// ValidateSSRFProtectedFetchURL instead.
func GetHttpClient() *http.Client {
	return httpClient
}

// GetSSRFProtectedHTTPClient 返回带拨号时 SSRF 校验的客户端。
// ssrfProtectedHTTPClient 由 InitHttpClient 在启动时初始化，运行期只读。
func GetSSRFProtectedHTTPClient() *http.Client {
	if fetchSetting := system_setting.GetFetchSetting(); fetchSetting != nil && !fetchSetting.EnableSSRFProtection {
		return GetHttpClient()
	}
	return ssrfProtectedHTTPClient
}

func newProxyURLConfig(parsedURL *url.URL) *proxyURLConfig {
	return &proxyURLConfig{
		parsedURL: parsedURL,
		cacheKey:  parsedURL.String(),
	}
}

func warnLegacyProxyURLOnce(config *proxyURLConfig) {
	if _, loaded := legacyProxyURLWarnings.LoadOrStore(config.cacheKey, struct{}{}); loaded {
		return
	}
	logger.LogWarn(
		context.Background(),
		fmt.Sprintf(
			"legacy proxy URL suffix ignored at runtime: scheme=%s host=%s; update the channel proxy setting",
			config.parsedURL.Scheme,
			config.parsedURL.Host,
		),
	)
}

// NormalizeProxyURL validates a proxy URL using runtime-compatible rules and returns its canonical cache key.
func NormalizeProxyURL(rawProxyURL string) (string, error) {
	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(rawProxyURL)
	if err != nil {
		return "", err
	}
	if parsedURL == nil {
		return "", nil
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	return config.cacheKey, nil
}

// ValidateProxyURL validates a channel proxy URL without connecting to it.
func ValidateProxyURL(rawProxyURL string) error {
	_, err := common.ParseProxyURLStrict(rawProxyURL)
	return err
}

func (cache *proxyHTTPClientCache) get(rawCacheKey string) (*http.Client, bool) {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()
	cacheKey := rawCacheKey
	if canonicalKey, ok := cache.aliases[rawCacheKey]; ok {
		cacheKey = canonicalKey
	}
	client, ok := cache.clients[cacheKey]
	return client, ok
}

func (cache *proxyHTTPClientCache) getOrCreate(rawCacheKey string, config *proxyURLConfig) (*http.Client, error) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if client, ok := cache.clients[config.cacheKey]; ok {
		cache.aliases[rawCacheKey] = config.cacheKey
		return client, nil
	}

	client, err := newProxyHTTPClient(config.parsedURL)
	if err != nil {
		return nil, err
	}
	cache.clients[config.cacheKey] = client
	cache.aliases[rawCacheKey] = config.cacheKey
	return client, nil
}

func (cache *proxyHTTPClientCache) remove(cacheKey string) *http.Client {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	client := cache.clients[cacheKey]
	delete(cache.clients, cacheKey)
	for alias, canonicalKey := range cache.aliases {
		if canonicalKey == cacheKey {
			delete(cache.aliases, alias)
		}
	}
	return client
}

func (cache *proxyHTTPClientCache) reset() map[string]*http.Client {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	oldClients := cache.clients
	cache.clients = make(map[string]*http.Client)
	cache.aliases = make(map[string]string)
	return oldClients
}

func newProxyHTTPClient(proxyURL *url.URL) (*http.Client, error) {
	transport := newRelayHTTPTransport()

	switch proxyURL.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)

	case "socks5", "socks5h":
		transport.Proxy = nil
		forwardDialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialer, err := proxy.FromURL(proxyURL, forwardDialer)
		if err != nil {
			return nil, err
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS proxy dialer does not support context cancellation")
		}
		transport.DialContext = contextDialer.DialContext

	default:
		return nil, fmt.Errorf("unsupported proxy scheme")
	}

	return newRelayHTTPClient(transport), nil
}

// GetHttpClientWithProxy returns the default client or a cached proxy-enabled client.
func GetHttpClientWithProxy(rawProxyURL string) (*http.Client, error) {
	trimmedProxyURL := strings.TrimSpace(rawProxyURL)
	if trimmedProxyURL == "" {
		if client := GetHttpClient(); client != nil {
			return client, nil
		}
		return http.DefaultClient, nil
	}
	if client, ok := proxyClients.get(trimmedProxyURL); ok {
		return client, nil
	}

	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(trimmedProxyURL)
	if err != nil {
		return nil, err
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	return proxyClients.getOrCreate(trimmedProxyURL, config)
}

// InvalidateProxyClient removes one proxy client and closes its idle connections.
func InvalidateProxyClient(rawProxyURL string) {
	parsedURL, legacySuffixStripped, err := common.ParseProxyURLRuntime(rawProxyURL)
	if err != nil || parsedURL == nil {
		return
	}
	config := newProxyURLConfig(parsedURL)
	if legacySuffixStripped {
		warnLegacyProxyURLOnce(config)
	}
	if client := proxyClients.remove(config.cacheKey); client != nil {
		client.CloseIdleConnections()
	}
	responseTimeoutLock.Lock()
	for cacheKey, client := range responseTimeoutClients {
		if strings.HasPrefix(cacheKey, config.cacheKey+"\x00") {
			client.CloseIdleConnections()
			delete(responseTimeoutClients, cacheKey)
		}
	}
	responseTimeoutLock.Unlock()
}

// ResetProxyClientCache clears all cached proxy clients.
func ResetProxyClientCache() {
	for _, client := range proxyClients.reset() {
		client.CloseIdleConnections()
	}
	responseTimeoutLock.Lock()
	oldTimeoutClients := responseTimeoutClients
	responseTimeoutClients = make(map[string]*http.Client)
	responseTimeoutLock.Unlock()
	for _, client := range oldTimeoutClients {
		client.CloseIdleConnections()
	}
}

// GetHttpClientWithResponseHeaderTimeout returns a cached client whose timeout
// only covers the wait for upstream response headers. Streaming response bodies
// remain unbounded and continue to use the normal streaming idle timeout.
func GetHttpClientWithResponseHeaderTimeout(proxyURL string, timeout time.Duration) (*http.Client, error) {
	baseClient, err := GetHttpClientWithProxy(proxyURL)
	if err != nil || timeout <= 0 {
		return baseClient, err
	}

	// Derive the cache key from the normalized proxy URL so InvalidateProxyClient,
	// which evicts these variants by normalized cacheKey prefix, can reliably drop
	// them; keying on the raw URL would leave stale timeout clients pointing at an
	// old proxy after invalidation.
	canonicalKey, err := NormalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	cacheKey := canonicalKey + "\x00" + timeout.String()
	responseTimeoutLock.Lock()
	defer responseTimeoutLock.Unlock()
	if client, ok := responseTimeoutClients[cacheKey]; ok {
		return client, nil
	}

	transport, ok := baseClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		return nil, fmt.Errorf("response header timeout requires an HTTP transport")
	}
	clientCopy := *baseClient
	transportCopy := transport.Clone()
	transportCopy.ResponseHeaderTimeout = timeout
	clientCopy.Transport = transportCopy
	responseTimeoutClients[cacheKey] = &clientCopy
	return &clientCopy, nil
}

// NewProxyHttpClient is kept for compatibility.
// Deprecated: use GetHttpClientWithProxy.
func NewProxyHttpClient(proxyURL string) (*http.Client, error) {
	return GetHttpClientWithProxy(proxyURL)
}

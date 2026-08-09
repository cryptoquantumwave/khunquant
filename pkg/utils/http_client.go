package utils

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// CreateHTTPClient creates an HTTP client with optional proxy support.
// If proxyURL is empty, it uses the system environment proxy settings.
// Supported proxy schemes: http, https, socks5, socks5h.
// This client is NOT wrapped in the sandbox transport and should only be used
// for non-exchange traffic (e.g., web search, data fetching).
func CreateHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  false,
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}

	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		scheme := strings.ToLower(proxy.Scheme)
		switch scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf(
				"unsupported proxy scheme %q (supported: http, https, socks5, socks5h)",
				proxy.Scheme,
			)
		}
		if proxy.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL: missing host")
		}
		client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxy)
	} else {
		client.Transport.(*http.Transport).Proxy = http.ProxyFromEnvironment
	}

	return client, nil
}

// CreateExchangeHTTPClient creates an HTTP client for exchange/broker requests.
// When sandbox mode is enabled, requests are rewritten to the sandbox server.
// The underlying transport is wrapped in a sandbox RoundTripper that reads
// the live global sandbox state on each request, ensuring the client remains
// correct even if sandbox is toggled at runtime.
func CreateExchangeHTTPClient(venue, proxyURL string, timeout time.Duration) (*http.Client, error) {
	// Delegate to CreateHTTPClient first to get a properly configured base client.
	client, err := CreateHTTPClient(proxyURL, timeout)
	if err != nil {
		return nil, err
	}

	// Wrap the transport with the sandbox RoundTripper. This ensures that when
	// sandbox mode is enabled, requests go to the sandbox server. The RoundTripper
	// reads global state on each request, so the client remains correct across
	// runtime toggles without needing to rebuild the client.
	client.Transport = sandbox.NewRoundTripper(venue, client.Transport)

	return client, nil
}

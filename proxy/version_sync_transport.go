package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// versionSyncTransport 为 HTTPS 版本源使用 Chrome uTLS，HTTP 供本地源和重定向使用。
type versionSyncTransport struct {
	chrome *utlsRoundTripper
	plain  *http.Transport
}

func newVersionSyncClient(endpoint, proxyURL string, timeout time.Duration) (*http.Client, func()) {
	chromeProxy := strings.TrimSpace(proxyURL)
	if chromeProxy == "" {
		if target, err := url.Parse(endpoint); err == nil {
			if envProxy, err := http.ProxyFromEnvironment(&http.Request{URL: target}); err == nil && envProxy != nil {
				chromeProxy = envProxy.String()
			}
		}
	}
	transport := &versionSyncTransport{
		chrome: NewUTLSTransport(chromeProxy).(*utlsRoundTripper),
		plain:  newCodexStandardTransport(proxyURL).(*http.Transport),
	}
	return &http.Client{Transport: transport, Timeout: timeout}, transport.close
}

func (t *versionSyncTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Scheme {
	case "https":
		return t.chrome.RoundTrip(req)
	case "http":
		return t.plain.RoundTrip(req)
	default:
		return nil, fmt.Errorf("unsupported version source scheme %q", req.URL.Scheme)
	}
}

func (t *versionSyncTransport) close() {
	t.chrome.CloseAllConnections()
	t.plain.CloseIdleConnections()
}

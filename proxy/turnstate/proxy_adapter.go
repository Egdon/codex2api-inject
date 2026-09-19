package turnstate

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	providerZoo     = "zooproxy"
	providerLitport = "litport"
)

// ValidateHarvestProxyConfig validates routing, not credential completeness, so
// settings can be saved before credentials are supplied. Errors contain no input.
func ValidateHarvestProxyConfig(cfg Config) error {
	cfg = NormalizeConfig(cfg)
	var host string
	switch cfg.HarvestProxyProvider {
	case providerZoo:
		host = cfg.ZooHost
	case providerLitport:
		host = cfg.LitportHost
		if len(cfg.LitportRegion) != 2 || cfg.LitportRegion[0] < 'A' || cfg.LitportRegion[0] > 'Z' || cfg.LitportRegion[1] < 'A' || cfg.LitportRegion[1] > 'Z' {
			return fmt.Errorf("Litport region must be a two-letter country code")
		}
	default:
		return fmt.Errorf("unknown harvest proxy provider; select zooproxy or litport")
	}
	if _, err := parseHarvestProxyHost(host); err != nil {
		return fmt.Errorf("selected harvest proxy endpoint is invalid")
	}
	return nil
}

// Legacy storage may contain credentials in an endpoint. Invalid endpoints are
// never echoed publicly, including inactive-provider endpoints.
func publicHarvestHost(host string) string {
	if _, err := parseHarvestProxyHost(host); err != nil {
		return ""
	}
	return host
}

func parseHarvestProxyHost(host string) (*url.URL, error) {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, &probeFailure{kind: probeProxyConfig}
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, &probeFailure{kind: probeProxyConfig}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, &probeFailure{kind: probeProxyConfig}
		}
	}
	return u, nil
}

// buildHarvestProxyURL is pure: the caller owns the session's lifetime. Zoo
// callers supply a fresh SID per probe; Litport supplies one per attempt pair.
func buildHarvestProxyURL(cfg Config, sid string) (*url.URL, error) {
	cfg = NormalizeConfig(cfg)
	if !HarvestReady(cfg) || sid == "" {
		return nil, &probeFailure{kind: probeProxyConfig}
	}
	var host, username, password string
	switch cfg.HarvestProxyProvider {
	case providerZoo:
		host, password = cfg.ZooHost, cfg.ZooPassword
		username = fmt.Sprintf("%s-region-%s-sid-%s-t-%d", cfg.ZooUserPrefix, cfg.ZooRegion, sid, cfg.ZooStickyMinutes)
	case providerLitport:
		if len(sid) != 12 || strings.IndexFunc(sid, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}) >= 0 {
			return nil, &probeFailure{kind: probeProxyConfig}
		}
		host, password = cfg.LitportHost, cfg.LitportPassword
		username = fmt.Sprintf("%s_country-%s_sid-%s_sttl-%d", cfg.LitportUsername, strings.ToLower(cfg.LitportRegion), sid, cfg.LitportSessionSeconds)
	default:
		return nil, &probeFailure{kind: probeProxyConfig}
	}
	u, err := parseHarvestProxyHost(host)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(username, password)
	return u, nil
}

func harvestProxyURL(cfg Config) (*url.URL, error) {
	cfg = NormalizeConfig(cfg)
	sid := cfg.harvestSID
	if cfg.HarvestProxyProvider != providerLitport {
		sid = randomSID() // Preserve Zoo's fresh session for every probe.
	} else if sid == "" {
		sid = randomSID()[:12]
	}
	return buildHarvestProxyURL(cfg, sid)
}

func zooProxyURL(cfg Config) (*url.URL, error) {
	cfg.HarvestProxyProvider = providerZoo
	return buildHarvestProxyURL(cfg, randomSID())
}

// Only CONNECT headers identify the proxy reliably. Never interpret similarly
// named headers on an origin response, nor expose X-Proxy-Error-Message.
func proxyConnectFailure(provider string, resp *http.Response) error {
	if provider == providerLitport {
		switch strings.TrimSpace(resp.Header.Get("X-Proxy-Error-Code")) {
		case "4":
			return &probeFailure{kind: probeProxyAuth}
		case "5":
			return &probeFailure{kind: probeProxyConfig, proxyCode: 5}
		case "11":
			return &probeFailure{kind: probeProxyConfig, proxyCode: 11}
		case "13":
			return &probeFailure{kind: probeProxyConfig, proxyCode: 13}
		case "12", "16":
			return &probeFailure{kind: probeProxyConnect, retryAfter: boundedRetryAfter(resp.Header)}
		}
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return &probeFailure{kind: probeProxyAuth}
	}
	if resp.StatusCode != http.StatusOK {
		return &probeFailure{kind: probeProxyConnect, retryAfter: boundedRetryAfter(resp.Header)}
	}
	return nil
}

func selectedHarvestRegion(cfg Config) string {
	if cfg.HarvestProxyProvider == providerLitport {
		return cfg.LitportRegion
	}
	return cfg.ZooRegion
}

func preserveHarvestPasswords(req *Config, saved Config, keepZooPassword bool) {
	if keepZooPassword || strings.TrimSpace(req.ZooPassword) == "" {
		req.ZooPassword = saved.ZooPassword
	}
	if strings.TrimSpace(req.LitportPassword) == "" {
		req.LitportPassword = saved.LitportPassword
	}
}

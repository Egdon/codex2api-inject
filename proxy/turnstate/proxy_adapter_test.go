package turnstate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func proxyFixture(provider string) Config {
	cfg := DefaultConfig()
	cfg.HarvestProxyProvider = provider
	cfg.ZooUserPrefix, cfg.ZooPassword = "zoo+user", "zoo:@/?# secret"
	cfg.LitportUsername, cfg.LitportPassword = "lit+user", "lit:@/?# secret"
	return cfg
}

func TestHarvestProviderDefaultsLegacyAndRedaction(t *testing.T) {
	cfg, err := ParseConfigJSON(`{"zoo_user_prefix":"legacy","zoo_password":"secret","zoo_region":"fr","zoo_sticky_minutes":90}`)
	if err != nil || cfg.HarvestProxyProvider != providerZoo || cfg.ZooRegion != "FR" || cfg.ZooStickyMinutes != 90 || !HarvestReady(cfg) {
		t.Fatal("legacy config changed", err)
	}
	if cfg.LitportHost != "hub-us-10.litport.net:1337" || cfg.LitportRegion != "DE" || cfg.LitportRegionMode != "fixed" || cfg.LitportSessionSeconds != 600 || cfg.LitportUsername != "" || cfg.LitportPassword != "" {
		t.Fatal("incorrect Litport defaults")
	}
	for _, tc := range []struct{ input, want int }{{0, 600}, {-1, 1}, {1, 1}, {86400, 86400}, {86401, 86400}} {
		if got := NormalizeConfig(Config{LitportSessionSeconds: tc.input}).LitportSessionSeconds; got != tc.want {
			t.Fatalf("session clamp %d: %d", tc.input, got)
		}
	}
	cfg = proxyFixture(providerLitport)
	public := Publicize(cfg)
	if !public.ZooPasswordSet || !public.LitportPasswordSet || public.ZooPassword != "" || public.LitportPassword != "" {
		t.Fatal("password public state")
	}
	b, err := json.Marshal(public)
	if err != nil || strings.Contains(string(b), `"zoo_password":`) || strings.Contains(string(b), `"litport_password":`) || strings.Contains(string(b), "secret") {
		t.Fatal("public JSON exposed credentials")
	}
	cfg.ZooHost = "http://legacy:secret@host:1234"
	if Publicize(cfg).ZooHost != "" {
		t.Fatal("inactive legacy endpoint credentials exposed")
	}
}

func TestHarvestProviderReadinessAndValidation(t *testing.T) {
	cfg := proxyFixture(providerLitport)
	cfg.ZooUserPrefix, cfg.ZooPassword = "", ""
	if !HarvestReady(cfg) {
		t.Fatal("Litport incorrectly depends on Zoo")
	}
	cfg.HarvestProxyProvider = providerZoo
	if HarvestReady(cfg) {
		t.Fatal("Zoo fell back to Litport")
	}
	cfg.HarvestProxyProvider = "direct"
	if ValidateHarvestProxyConfig(cfg) == nil || HarvestReady(cfg) {
		t.Fatal("unknown provider accepted")
	}
	if _, err := buildHarvestProxyURL(cfg, "123456abcdef"); err == nil {
		t.Fatal("unknown provider got a route")
	}
	if _, err := EncodeConfig(cfg); err == nil {
		t.Fatal("unknown provider persisted")
	}
	cfg = proxyFixture(providerLitport)
	for _, host := range []string{"http://", "file:///tmp/proxy", "http://secret@host:1337", "http://host:99999", "http://host:1337/path?secret=x"} {
		cfg.LitportHost = host
		if err := ValidateHarvestProxyConfig(cfg); err == nil || strings.Contains(err.Error(), host) {
			t.Fatal("invalid endpoint accepted or echoed")
		}
	}
	cfg = proxyFixture(providerLitport)
	cfg.LitportRegion = "DE_sid-injected"
	if ValidateHarvestProxyConfig(cfg) == nil {
		t.Fatal("country selector injection accepted")
	}
}

func TestHarvestPasswordsPreservedIndependently(t *testing.T) {
	saved := proxyFixture(providerLitport)
	req := Config{}
	preserveHarvestPasswords(&req, saved, false)
	if req.ZooPassword != saved.ZooPassword || req.LitportPassword != saved.LitportPassword {
		t.Fatal("blank password discarded saved secret")
	}
	req = Config{ZooPassword: "ignored", LitportPassword: "new"}
	preserveHarvestPasswords(&req, saved, true)
	if req.ZooPassword != saved.ZooPassword || req.LitportPassword != "new" {
		t.Fatal("legacy keep flag clobbered Litport update")
	}
}

func TestHarvestProxyURLSuffixEscapingAndSessions(t *testing.T) {
	for _, provider := range []string{providerZoo, providerLitport} {
		cfg := proxyFixture(provider)
		cfg.harvestSID = "aB1234cD5678"
		u, err := buildHarvestProxyURL(cfg, cfg.harvestSID)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := url.Parse(u.String())
		if err != nil {
			t.Fatal("invalid escaped URL")
		}
		wantUser, wantPass := "zoo+user-region-DE-sid-aB1234cD5678-t-120", cfg.ZooPassword
		if provider == providerLitport {
			wantUser, wantPass = "lit+user_country-de_sid-aB1234cD5678_sttl-600", cfg.LitportPassword
		}
		pass, _ := decoded.User.Password()
		if decoded.User.Username() != wantUser || pass != wantPass || decoded.RawQuery != "" {
			t.Fatal("URL escaping or selector grammar changed")
		}
		a, _ := harvestProxyURL(cfg)
		b, _ := harvestProxyURL(cfg)
		if (a.User.Username() == b.User.Username()) != (provider == providerLitport) {
			t.Fatal("wrong per-provider SID lifetime")
		}
	}
}

func TestLitportRotationAndPlanSwitch(t *testing.T) {
	cfg := proxyFixture(providerLitport)
	cfg.LitportRegionMode = regionModeRotation
	for _, region := range []string{"DE", "FR", "GB", "NL", "JP"} {
		cfg.LitportRegion = region
		for n := 0; n < 20; n++ {
			plan := newBatchRegionPlan(cfg)
			first := region
			if first == "JP" {
				first = "DE"
			}
			if plan[0] != first || plan[3] != "JP" {
				t.Fatal(plan)
			}
			for i := 0; i < 3; i++ {
				if !slices.Contains([]string{"DE", "FR", "GB", "NL"}, plan[i]) {
					t.Fatal(plan)
				}
				for j := 0; j < i; j++ {
					if plan[i] == plan[j] {
						t.Fatal("duplicate", plan)
					}
				}
			}
		}
	}
	task := scheduledCell{regionPlan: [4]string{"SE", "FI", "CH", "TW"}, regionConfig: harvestRegionConfig(proxyFixture(providerZoo))}
	task.syncRegionPlan(cfg)
	if task.regionConfig.provider != providerLitport || task.regionPlan[3] != "JP" {
		t.Fatal("old provider plan retained")
	}
	before := task.regionPlan
	task.syncRegionPlan(cfg)
	if task.regionPlan != before {
		t.Fatal("stable plan randomized per retry")
	}
	cfg.LitportRegionMode, cfg.LitportRegion = regionModeFixed, "NL"
	task.syncRegionPlan(cfg)
	if task.regionPlan != [4]string{"NL", "NL", "NL", "NL"} {
		t.Fatal("region change not applied")
	}
}

func TestHarvestAttemptFreezesProviderAndSIDAcrossConfigSwitch(t *testing.T) {
	old := GetConfig()
	defer SetConfig(old)
	cfg := proxyFixture(providerLitport)
	SetConfig(cfg)
	h := NewHarvester(nil, stubStore{}, NewCache())
	acc := testAccount(9)
	g, version := h.retainGeneration(acc.ID(), "gpt-6-astra")
	task := scheduledCell{key: ticketKey(acc.ID(), "gpt-6-astra"), accountID: acc.ID(), model: "gpt-6-astra", max: 20, attempt: 1, generation: g, version: version}
	defer h.releaseGeneration(task.key, g)
	task.syncRegionPlan(cfg)
	var seen []Config
	token := fakeFernet(time.Now().Unix(), 160)
	h.probeFn = func(_ context.Context, got Config, _ *auth.Account, _, inject string) (string, http.Header, error) {
		seen = append(seen, got)
		if inject == "" {
			SetConfig(proxyFixture(providerZoo))
		}
		return token, http.Header{}, nil
	}
	for n := 0; n < 2; n++ {
		result := h.attempt(context.Background(), cfg, acc, task, func() {})
		if result.phase != "succeeded" {
			t.Fatal(result.phase)
		}
	}
	if len(seen) != 4 || seen[0].harvestSID == "" || seen[0].harvestSID != seen[1].harvestSID || seen[2].harvestSID != seen[3].harvestSID || seen[0].harvestSID == seen[2].harvestSID {
		t.Fatal("Litport SID not attempt scoped")
	}
	for _, got := range seen {
		if got.HarvestProxyProvider != providerLitport || got.LitportRegion != "DE" {
			t.Fatal("pair crossed provider/config")
		}
	}
	cfg = GetConfig()
	cfg.ZooRegion = "SE"
	task.syncRegionPlan(cfg)
	task.attempt++
	if result := h.attempt(context.Background(), cfg, acc, task, func() {}); result.phase != "succeeded" {
		t.Fatal(result.phase)
	}
	if seen[4].HarvestProxyProvider != providerZoo || seen[5].HarvestProxyProvider != providerZoo || seen[4].ZooRegion != "SE" {
		t.Fatal("next attempt did not switch")
	}
}

func TestProxyFailureClassificationTrustedConnectOnly(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		kind   probeFailureKind
	}{
		{407, "", probeProxyAuth}, {500, "4", probeProxyAuth}, {500, "5", probeProxyConfig}, {500, "11", probeProxyConfig}, {500, "13", probeProxyConfig}, {500, "12", probeProxyConnect}, {500, "16", probeProxyConnect}, {502, "unknown", probeProxyConnect},
	} {
		hdr := http.Header{"X-Proxy-Error-Code": []string{tc.code}, "X-Proxy-Error-Message": []string{"secret"}}
		err := proxyConnectFailure(providerLitport, &http.Response{StatusCode: tc.status, Header: hdr})
		wrapped := &url.Error{Op: "Post", URL: "https://origin", Err: err}
		kind, _ := classifyRetry(wrapped, nil)
		if kind != tc.kind || strings.Contains(probeErrorDetail(wrapped), "secret") || isUnauthorized(err) {
			t.Fatal("wrong CONNECT classification")
		}
		observe, failed := networkOutcome(context.Background(), nil, err)
		if tc.kind == probeProxyConnect {
			if !observe || !failed || (attemptResult{retryKind: kind}).delay(1) <= 0 {
				t.Fatal("CONNECT failure bypassed backoff/breaker")
			}
		} else if observe {
			t.Fatal("config/auth failure blamed network")
		}
	}
	hdr := http.Header{"X-Proxy-Error-Code": []string{"13"}}
	if kind, _ := classifyRetry(nil, hdr); kind != probeProtocol {
		t.Fatal("origin header treated as proxy evidence")
	}
	if observe, _ := networkOutcome(context.Background(), hdr, fmt.Errorf("status 401")); observe {
		t.Fatal("origin401 blamed proxy")
	}
	if err := proxyConnectFailure(providerZoo, &http.Response{StatusCode: 200, Header: hdr}); err != nil {
		t.Fatal("Zoo interpreted Litport header")
	}
}

func TestTerminalProxyFailurePreservesTicketAndCoolsDown(t *testing.T) {
	h := NewHarvester(nil, stubStore{}, NewCache())
	acc := testAccount(9)
	cfg := proxyFixture(providerLitport)
	g, version := h.retainGeneration(acc.ID(), "gpt-6-astra")
	task := scheduledCell{key: ticketKey(acc.ID(), "gpt-6-astra"), accountID: acc.ID(), model: "gpt-6-astra", max: 6, attempt: 1, generation: g, version: version}
	defer h.releaseGeneration(task.key, g)
	h.cache.Put(CachedTicket{AccountID: acc.ID(), Model: task.model, Token: "previous"})
	h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
		return "", nil, &probeFailure{kind: probeProxyAuth}
	}
	result := h.attempt(context.Background(), cfg, acc, task, func() { t.Fatal("confirmed failed acquisition") })
	cached, _ := h.cache.Get(acc.ID(), task.model)
	if result.phase != "skipped" || result.status == "401" || result.ordinaryMiss || cached.Token != "previous" || cached.CooldownUntil <= time.Now().Unix() || cached.Exhausted {
		t.Fatal("proxy auth did not skip safely")
	}
}

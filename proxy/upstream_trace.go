package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	UpstreamSourceBPS   = "bps"
	UpstreamSourceCodex = "codex"
	UpstreamSourceOther = "other"
)

type upstreamTraceContextKey struct{}
type upstreamTraceAttempt struct {
	accountID int64
	source    string
	requestID string
	proxy     auth.ProxyAuditLabel
	// injectedTurnState 是本次尝试实际注入到出站请求上的凭据级 X-Codex-Turn-State；
	// upstreamTurnState 是上游响应回带的观测值。均为空串表示没有。
	injectedTurnState string
	upstreamTurnState string
}

type upstreamTraceSnapshot struct {
	RequestID         string
	UpstreamSource    string
	accountID         int64
	UpstreamRequestID string
	Proxy             auth.ProxyAuditLabel
	InjectedTurnState string
	UpstreamTurnState string
}

func snapshotUpstreamTrace(ctx context.Context) upstreamTraceSnapshot {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return upstreamTraceSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := upstreamTraceSnapshot{RequestID: a.requestID}
	if a.current != nil {
		result.accountID = a.current.accountID
		result.UpstreamSource = a.current.source
		result.UpstreamRequestID = a.current.requestID
		result.Proxy = a.current.proxy
		result.InjectedTurnState = a.current.injectedTurnState
		result.UpstreamTurnState = a.current.upstreamTurnState
	}
	return result
}

func (s upstreamTraceSnapshot) apply(input *database.UsageLogInput) {
	input.RequestID = s.RequestID
	if s.accountID == input.AccountID {
		input.UpstreamSource = s.UpstreamSource
		input.UpstreamRequestID = s.UpstreamRequestID
		input.UpstreamProxyID = s.Proxy.ID
		input.UpstreamProxyName = s.Proxy.Name
		input.InjectedTurnState = s.InjectedTurnState
		input.UpstreamTurnState = s.UpstreamTurnState
	}
}

type upstreamTraceAudit struct {
	mu        sync.Mutex
	requestID string
	store     *auth.Store
	current   *upstreamTraceAttempt
}

func upstreamTraceFromContext(ctx context.Context) *upstreamTraceAudit {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(upstreamTraceContextKey{}).(*upstreamTraceAudit)
	return a
}

func attachUpstreamTrace(c *gin.Context, store *auth.Store) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(ContextWithUpstreamTrace(c.Request.Context(), store))
	a := upstreamTraceFromContext(c.Request.Context())
	c.Header("X-Codex2API-Request-ID", a.requestID)
}

// ContextWithUpstreamTrace creates an isolated attempt recorder for non-HTTP
// probes. Attach it once per test, not once per concurrent batch.
func ContextWithUpstreamTrace(ctx context.Context, store *auth.Store) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamTraceContextKey{}, &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: store})
}

// UpstreamSourceFromContext reports actual dispatch, never an account preference.
func UpstreamSourceFromContext(ctx context.Context) string {
	return snapshotUpstreamTrace(ctx).UpstreamSource
}

// AttachUpstreamTraceForAdmin exposes attachUpstreamTrace for admin probes
// (test-connection / quality-test) that skip the /v1 auth middleware.
func AttachUpstreamTraceForAdmin(c *gin.Context, store *auth.Store) {
	attachUpstreamTrace(c, store)
}

func resetUpstreamRequestTrace(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	if a := upstreamTraceFromContext(c.Request.Context()); a != nil {
		a.mu.Lock()
		a.requestID = NewUpstreamSessionUUID()
		a.current = nil
		a.mu.Unlock()
	}
}

func resetUpstreamAttemptTrace(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		a.current = nil
		a.mu.Unlock()
	}
}

func beginUpstreamTrace(ctx context.Context, account *auth.Account, proxyURL string, ws bool) func(*http.Response) {
	source := UpstreamSourceOther
	if account != nil && !account.IsRelayStyle() {
		source = UpstreamSourceCodex
	}
	return beginUpstreamTraceWithSource(ctx, account, proxyURL, ws, source)
}

func beginUpstreamTraceWithSource(ctx context.Context, account *auth.Account, proxyURL string, ws bool, source string) func(*http.Response) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || account == nil {
		return func(*http.Response) {}
	}
	label := a.store.ProxyAuditForURL(proxyURL)
	if ws && proxyURL == "" {
		label = auth.ProxyAuditLabel{Name: "unknown"}
	}
	if source != UpstreamSourceBPS && resinCarriesEgress(account) {
		label = auth.ProxyAuditLabel{Name: "resin"}
	}
	label.Name = security.MaskSensitiveData(label.Name)
	attempt := &upstreamTraceAttempt{accountID: account.ID(), source: source, proxy: label, injectedTurnState: CodexTurnStateInjectionFromContext(ctx)}
	a.mu.Lock()
	a.current = attempt
	a.mu.Unlock()
	rememberDispatchedResponseRoute(ctx)
	header := account.GetUpstreamRequestIDHeader()
	return func(resp *http.Response) {
		if resp == nil || ws {
			return
		} // A WS handshake ID is not a per-turn ID; WS turn state arrives per frame, see ObserveCodexTurnStateFrame.
		turnState := observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))
		id := ""
		if header != "" && auth.ValidateUpstreamRequestIDHeader(header) == nil {
			id = resp.Header.Get(header)
		} else if header == "" {
			for _, name := range []string{"X-Request-Id", "Request-Id", "X-Goog-Request-Id"} {
				if id = resp.Header.Get(name); strings.TrimSpace(id) != "" {
					break
				}
			}
		}
		id = security.SafeTruncate(strings.TrimSpace(id), 128)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == attempt {
			attempt.requestID = id
			if turnState != "" {
				attempt.upstreamTurnState = turnState
			}
		}
	}
}

// noteUpstreamTurnState 把上游回带的 turn state 记到当前尝试上；WS 路径逐帧调用，
// 后到的值覆盖先到的。
func noteUpstreamTurnState(ctx context.Context, state string) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || state == "" {
		return
	}
	a.mu.Lock()
	if a.current != nil {
		a.current.upstreamTurnState = state
	}
	a.mu.Unlock()
}

func doTracedUpstreamRequest(client *http.Client, req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	record := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	resp, err := client.Do(req)
	record(resp)
	return resp, err
}

// PopulateUpstreamTrace copies the in-flight Codex turn-state audit onto a
// usage log. Test-connection writes its own log and would otherwise leave
// TURN-STATE blank even when ExecuteRequest injected a ticket.
func PopulateUpstreamTrace(c *gin.Context, input *database.UsageLogInput) {
	populateUpstreamTrace(c, input)
}

func populateUpstreamTrace(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	// Populate source even if the caller supplied a provider request ID. A
	// hidden-round snapshot already includes its source and must stay intact.
	if input.UpstreamSource == "" {
		snapshot := snapshotUpstreamTrace(c.Request.Context())
		if snapshot.accountID == input.AccountID {
			input.UpstreamSource = snapshot.UpstreamSource
		}
	}
	if input.RequestID != "" {
		return
	} // Hidden continuation rounds carry their own snapshot.
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	input.RequestID = a.requestID
	if current := a.current; current != nil && current.accountID == input.AccountID {
		input.UpstreamRequestID = current.requestID
		input.UpstreamProxyID = current.proxy.ID
		input.UpstreamProxyName = current.proxy.Name
		input.InjectedTurnState = current.injectedTurnState
		input.UpstreamTurnState = current.upstreamTurnState
	}
}

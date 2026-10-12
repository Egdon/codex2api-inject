package wsrelay

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

type connectionDialRequest struct {
	account       *auth.Account
	url           string
	sessionKey    string
	headers       http.Header
	proxyOverride string
	capacity      *connectionCapacitySlot
}

func (m *Manager) createConnection(ctx context.Context, input connectionDialRequest) (wc *WsConnection, err error) {
	defer func() {
		if err != nil {
			input.capacity.release()
		}
	}()
	dialer, err := m.connectionDialer(input)
	if err != nil {
		return nil, err
	}
	session, poolKey := m.newDialSession(input)
	proxy.RecordWebsocketUpstreamDispatch(ctx, input.account, effectiveProxyURL(input.account, input.proxyOverride))
	conn, resp, err := dialer.DialContext(ctx, input.url, input.headers)
	if err != nil {
		m.sessions.CompareAndDelete(poolKey, session)
		session.Close()
		return nil, formatDialHandshakeError(err, resp)
	}
	logCompressionNegotiation(resp, input.account.ID())
	wc = m.wrapDialedConnection(dialedConnection{conn: conn, response: resp, session: session, poolKey: poolKey, request: input})
	input.capacity.attach(wc)
	select {
	case <-m.stopCleanup:
		m.DiscardConnection(wc)
		return nil, context.Canceled
	default:
		wc.installControlHandlers()
		wc.StartReadPump()
		return wc, nil
	}
}

func (m *Manager) connectionDialer(input connectionDialRequest) (*websocket.Dialer, error) {
	dialer := *m.dialer
	proxyURL := effectiveProxyURL(input.account, input.proxyOverride)
	if dialProxy := proxy.CodexDialProxyURL(input.account, proxyURL); dialProxy != "" {
		if err := configureWebsocketDialerProxy(&dialer, dialProxy); err != nil {
			return nil, err
		}
	}
	return &dialer, nil
}

func (m *Manager) newDialSession(input connectionDialRequest) (*Session, string) {
	poolKey := m.poolKey(input.account.ID(), input.url, input.sessionKey, effectiveProxyURL(input.account, input.proxyOverride))
	if value, ok := m.sessions.Load(poolKey); ok {
		value.(*Session).Close()
	}
	session := NewSession(input.account.ID(), m)
	if key := strings.TrimSpace(input.sessionKey); key != "" {
		session.ID = key
	}
	m.sessions.Store(poolKey, session)
	return session, poolKey
}

type dialedConnection struct {
	conn     *websocket.Conn
	response *http.Response
	session  *Session
	poolKey  string
	request  connectionDialRequest
}

func (m *Manager) wrapDialedConnection(input dialedConnection) *WsConnection {
	wc := NewWsConnection(input.conn, input.session, input.request.url)
	wc.account = input.request.account
	wc.PoolKey = input.poolKey
	wc.upstreamUserAgent = strings.TrimSpace(input.request.headers.Get("User-Agent"))
	wc.upstreamUserAgentKnown = true
	wc.upstreamClientIdentity = websocketClientIdentity(input.request.headers)
	wc.httpResp = input.response
	wc.onDisconnected = m.getOnDisconnected()
	wc.onReadFailure = m.DiscardConnection
	input.session.SetConnected(true)
	return wc
}

// Package websocket implements the WebSocket (WS/WSS) carrier using
// github.com/coder/websocket. Each session message is one binary WebSocket
// message. Client connections honour HTTPS_PROXY/HTTP_PROXY/NO_PROXY.
package websocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ws "github.com/coder/websocket"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (c *Carrier) Name() string {
	if c.opts.Plain {
		return "ws"
	}
	return "wss"
}

func (c *Carrier) Capabilities() carrier.Capabilities {
	// TCP 32 + TLS record 22 + WebSocket header with client mask 14 + slack.
	return carrier.Capabilities{Reliable: true, Ordered: true, Segmenting: true, MaxMessage: carrier.MaxMessage, Overhead: 70}
}

func (c *Carrier) Check(context.Context) error { return nil }

func (c *Carrier) path() string {
	if c.opts.Path == "" {
		return "/tuunel"
	}
	return c.opts.Path
}

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	scheme := "wss"
	if c.opts.Plain {
		scheme = "ws"
	}
	u := fmt.Sprintf("%s://%s%s", scheme, address, c.path())
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 10 * time.Second}
	if !c.opts.Plain {
		tlsConf, err := carrier.ClientTLS(c.opts, []string{"http/1.1"})
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tlsConf
	}
	conn, resp, err := ws.Dial(ctx, u, &ws.DialOptions{HTTPClient: &http.Client{Transport: tr, Timeout: 15 * time.Second}, Host: c.opts.Host})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(carrier.MaxMessage + 16)
	return newConn(conn, nil, nil), nil
}

type conn struct {
	c      *ws.Conn
	local  net.Addr
	remote net.Addr
	wmu    sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

func newConn(c *ws.Conn, local, remote net.Addr) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{c: c, local: local, remote: remote, ctx: ctx, cancel: cancel}
}

func (c *conn) ReadMessage(b []byte) (int, error) {
	for {
		typ, m, err := c.c.Read(c.ctx)
		if err != nil {
			return 0, err
		}
		if typ != ws.MessageBinary || len(m) == 0 {
			continue
		}
		if len(m) > len(b) {
			return 0, carrier.ErrMessageTooLarge
		}
		return copy(b, m), nil
	}
}

func (c *conn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > carrier.MaxMessage {
		return carrier.ErrMessageTooLarge
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, carrier.StreamWriteTimeout)
	defer cancel()
	return c.c.Write(ctx, ws.MessageBinary, b)
}

func (c *conn) Close() error {
	c.cancel()
	return c.c.CloseNow()
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

type listener struct {
	srv   *http.Server
	ln    net.Listener
	q     *carrier.AcceptQueue
	limit *carrier.Limiter
	sem   chan struct{}
}

func (c *Carrier) Listen(ctx context.Context, address string) (carrier.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	max := c.opts.MaxSessions
	if max <= 0 {
		max = 1024
	}
	l := &listener{ln: ln, q: carrier.NewAcceptQueue(64), limit: carrier.NewLimiter(20, 40), sem: make(chan struct{}, max)}
	mux := http.NewServeMux()
	mux.HandleFunc(c.path(), l.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	if !c.opts.Plain {
		tlsConf, err := carrier.ServerTLS(c.opts, []string{"http/1.1"})
		if err != nil {
			ln.Close()
			return nil, err
		}
		l.srv.TLSConfig = tlsConf
		go func() { _ = l.srv.ServeTLS(ln, "", "") }()
	} else {
		go func() { _ = l.srv.Serve(ln) }()
	}
	return l, nil
}

func (l *listener) handle(w http.ResponseWriter, r *http.Request) {
	if !l.limit.Allow(remoteAddr(r), time.Now()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	c, err := ws.Accept(w, r, &ws.AcceptOptions{})
	if err != nil {
		return
	}
	c.SetReadLimit(carrier.MaxMessage + 16)
	cc := newConn(c, l.ln.Addr(), remoteAddr(r))
	if !l.q.Push(cc) {
		_ = c.Close(ws.StatusTryAgainLater, "busy")
		return
	}
	select { // keep the handler alive for the connection lifetime
	case <-cc.ctx.Done():
	case <-l.q.Done():
		cc.Close()
	}
}

type strAddr string

func (s strAddr) Network() string { return "tcp" }
func (s strAddr) String() string  { return string(s) }

func remoteAddr(r *http.Request) net.Addr {
	if a, err := net.ResolveTCPAddr("tcp", r.RemoteAddr); err == nil {
		return a
	}
	return strAddr(strings.TrimSpace(r.RemoteAddr))
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) {
	c, err := l.q.Accept(ctx)
	if errors.Is(err, carrier.ErrClosed) {
		return nil, net.ErrClosed
	}
	return c, err
}

func (l *listener) Close() error {
	l.q.Close()
	return l.srv.Close()
}

func (l *listener) Addr() net.Addr { return l.ln.Addr() }

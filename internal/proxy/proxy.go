// Package proxy implements a small authenticated SOCKS5 + HTTP proxy server.
//
// The built-in "exit proxy" mode uses two instances:
//
//   - edge:   public listener (SOCKS5 and HTTP on one port) with
//     username/password authentication. Every request is relayed through the
//     tunnel to the remote node's backend with a SOCKS5 CONNECT, so DNS
//     resolution and the outbound connection happen on the remote node.
//   - remote: backend listener bound to the tunnel address, without
//     authentication, that only accepts clients from the tunnel subnet and
//     dials the requested destination directly (refusing loopback, link-local
//     and, unless allowed, private destinations).
//
// Only TCP is relayed (SOCKS5 CONNECT, HTTP CONNECT and plain HTTP requests).
package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// HandshakeTimeout bounds the client greeting/request phase.
var HandshakeTimeout = 20 * time.Second

// DialTimeout bounds one outbound connection attempt.
var DialTimeout = 15 * time.Second

type Config struct {
	Listen         string
	Users          map[string]string // empty: no authentication
	Upstream       string            // SOCKS5 server to relay through ("" = dial directly)
	AllowPrivate   bool              // direct mode: allow RFC1918/ULA destinations
	AllowClients   []netip.Prefix    // when non-empty, only these client addresses are served
	MaxConnections int               // default 1024
}

type Stats struct {
	Listen   string `json:"listen"`
	Mode     string `json:"mode"`
	Active   int64  `json:"active"`
	Total    uint64 `json:"total"`
	Rejected uint64 `json:"rejected"`
	AuthFail uint64 `json:"auth_failures"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

type Server struct {
	cfg    Config
	log    *slog.Logger
	ln     net.Listener
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	active                             atomic.Int64
	total, rejected, authFail, in, out atomic.Uint64
}

func New(cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 1024
	}
	return &Server{cfg: cfg, log: log, conns: map[net.Conn]struct{}{}}
}

// Listen binds the listener; retries for up to wait (the tunnel address may
// appear slightly after start).
func (s *Server) Listen(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		ln, err := net.Listen("tcp", s.cfg.Listen)
		if err == nil {
			s.ln = ln
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) Mode() string {
	m := "direct"
	if s.cfg.Upstream != "" {
		m = "relay via " + s.cfg.Upstream
	}
	if len(s.cfg.Users) > 0 {
		m += ", auth"
	}
	return m
}

func (s *Server) Stats() Stats {
	return Stats{Listen: s.cfg.Listen, Mode: s.Mode(), Active: s.active.Load(), Total: s.total.Load(), Rejected: s.rejected.Load(),
		AuthFail: s.authFail.Load(), BytesIn: s.in.Load(), BytesOut: s.out.Load()}
}

// Serve accepts connections until ctx is done or Close is called.
func (s *Server) Serve(ctx context.Context) {
	go func() { <-ctx.Done(); s.Close() }()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		if !s.clientAllowed(c.RemoteAddr()) || s.active.Load() >= int64(s.cfg.MaxConnections) {
			s.rejected.Add(1)
			c.Close()
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.active.Add(1)
		s.total.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.active.Add(-1)
			defer func() { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock(); c.Close() }()
			s.handle(ctx, c)
		}()
	}
}

func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) clientAllowed(a net.Addr) bool {
	if len(s.cfg.AllowClients) == 0 {
		return true
	}
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, _ := netip.AddrFromSlice(ta.IP)
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range s.cfg.AllowClients {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(HandshakeTimeout))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 5 {
		s.socks5(ctx, c, br)
	} else {
		s.http(ctx, c, br)
	}
}

func (s *Server) checkUser(u, p string) bool {
	want, ok := s.cfg.Users[u]
	ok2 := subtle.ConstantTimeCompare([]byte(want), []byte(p)) == 1
	if ok && ok2 {
		return true
	}
	s.authFail.Add(1)
	return false
}

// ---------------------------------------------------------------- SOCKS5

const (
	repOK          = 0
	repFailure     = 1
	repNotAllowed  = 2
	repNetUnreach  = 3
	repHostUnreach = 4
	repRefused     = 5
	repCmdUnsup    = 7
	repAddrUnsup   = 8
)

func (s *Server) socks5(ctx context.Context, c net.Conn, br *bufio.Reader) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	want := byte(0)
	if len(s.cfg.Users) > 0 {
		want = 2
	}
	found := false
	for _, m := range methods {
		if m == want {
			found = true
		}
	}
	if !found {
		c.Write([]byte{5, 0xff})
		if want == 2 {
			s.authFail.Add(1)
		}
		return
	}
	if _, err := c.Write([]byte{5, want}); err != nil {
		return
	}
	if want == 2 {
		// RFC 1929
		v := make([]byte, 2)
		if _, err := io.ReadFull(br, v); err != nil || v[0] != 1 {
			return
		}
		u := make([]byte, int(v[1]))
		if _, err := io.ReadFull(br, u); err != nil {
			return
		}
		pl, err := br.ReadByte()
		if err != nil {
			return
		}
		p := make([]byte, int(pl))
		if _, err := io.ReadFull(br, p); err != nil {
			return
		}
		if !s.checkUser(string(u), string(p)) {
			c.Write([]byte{1, 1})
			return
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return
		}
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 5 {
		return
	}
	target, err := readAddr(br, req[3])
	if err != nil {
		socksReply(c, repAddrUnsup)
		return
	}
	if req[1] != 1 { // CONNECT only
		socksReply(c, repCmdUnsup)
		return
	}
	up, err := s.dial(ctx, target)
	if err != nil {
		var ue upstreamError
		if s.cfg.Upstream != "" && !errors.As(err, &ue) {
			s.log.Warn("proxy upstream unreachable (is the remote proxy listening? tunnelctl proxy on the remote)", "upstream", s.cfg.Upstream, "error", err)
		} else {
			s.log.Debug("proxy dial failed", "target", target, "error", err)
		}
		socksReply(c, replyCode(err))
		return
	}
	defer up.Close()
	if err := socksReply(c, repOK); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	s.relay(c, br, up)
}

func readAddr(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		return "", fmt.Errorf("address type %d", atyp)
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(r, pb); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(pb[0])<<8|int(pb[1]))), nil
}

func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

var errForbidden = errors.New("destination not allowed")

type upstreamError struct{ code byte }

func (e upstreamError) Error() string { return fmt.Sprintf("upstream socks reply %d", e.code) }

func replyCode(err error) byte {
	var ue upstreamError
	switch {
	case errors.As(err, &ue):
		return ue.code
	case errors.Is(err, errForbidden):
		return repNotAllowed
	case errors.Is(err, syscall.ECONNREFUSED):
		return repRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return repNetUnreach
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return repHostUnreach
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return repHostUnreach
	}
	return repFailure
}

// ---------------------------------------------------------------- HTTP

func (s *Server) http(ctx context.Context, c net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if len(s.cfg.Users) > 0 {
		u, p, ok := proxyAuth(req.Header.Get("Proxy-Authorization"))
		if !ok || !s.checkUser(u, p) {
			io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"tuunel\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
	}
	if req.Method == http.MethodConnect {
		up, err := s.dial(ctx, req.Host)
		if err != nil {
			httpError(c, err)
			return
		}
		defer up.Close()
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		_ = c.SetDeadline(time.Time{})
		s.relay(c, br, up)
		return
	}
	if req.URL == nil || req.URL.Host == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "") {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	host := req.URL.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), "80")
	}
	up, err := s.dial(ctx, host)
	if err != nil {
		httpError(c, err)
		return
	}
	defer up.Close()
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	req.Header.Set("Connection", "close")
	req.Close = true
	req.RequestURI = ""
	if err := req.Write(up); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	s.relay(c, br, up)
}

func proxyAuth(h string) (string, string, bool) {
	const p = "Basic "
	if len(h) < len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(p):]))
	if err != nil {
		return "", "", false
	}
	u, pw, ok := strings.Cut(string(b), ":")
	return u, pw, ok
}

func httpError(c net.Conn, err error) {
	status := "502 Bad Gateway"
	if replyCode(err) == repNotAllowed {
		status = "403 Forbidden"
	}
	io.WriteString(c, "HTTP/1.1 "+status+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// ---------------------------------------------------------------- dialing

func (s *Server) dial(ctx context.Context, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	if s.cfg.Upstream != "" {
		return dialSOCKS5(ctx, s.cfg.Upstream, target)
	}
	d := net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return errForbidden
		}
		if !destinationCheck(ap.Addr(), s.cfg.AllowPrivate) {
			return errForbidden
		}
		return nil
	}}
	return d.DialContext(ctx, "tcp", target)
}

// destinationCheck is replaceable in tests.
var destinationCheck = DestinationAllowed

// DestinationAllowed reports whether the direct-mode proxy may connect to ip.
func DestinationAllowed(ip netip.Addr, allowPrivate bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip.Is4() && ip.As4()[0] == 0 { // 0.0.0.0/8
		return false
	}
	if !allowPrivate {
		if ip.IsPrivate() {
			return false
		}
		cgnat := netip.MustParsePrefix("100.64.0.0/10")
		if cgnat.Contains(ip) {
			return false
		}
	}
	return true
}

// dialSOCKS5 opens target through a no-auth SOCKS5 server, passing domain
// names unresolved so the upstream resolves them.
func dialSOCKS5(ctx context.Context, server, target string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("bad port %q", ps)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", server, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	r := make([]byte, 2)
	if _, err := io.ReadFull(c, r); err != nil {
		return fail(err)
	}
	if r[0] != 5 || r[1] != 0 {
		return fail(errors.New("upstream refused no-auth"))
	}
	req := []byte{5, 1, 0}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is4() {
			a := ip.As4()
			req = append(append(req, 1), a[:]...)
		} else {
			a := ip.As16()
			req = append(append(req, 4), a[:]...)
		}
	} else {
		if len(host) > 255 {
			return fail(errors.New("host name too long"))
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return fail(err)
	}
	rep := make([]byte, 4)
	if _, err := io.ReadFull(c, rep); err != nil {
		return fail(err)
	}
	if rep[1] != 0 {
		return fail(upstreamError{rep[1]})
	}
	if _, err := readAddr(c, rep[3]); err != nil {
		return fail(err)
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// ---------------------------------------------------------------- relay

type closeWriter interface{ CloseWrite() error }

func (s *Server) relay(client net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{})
	go func() {
		n, _ := io.Copy(up, br) // client -> upstream (includes buffered bytes)
		s.out.Add(uint64(n))
		if cw, ok := up.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			up.Close()
		}
		close(done)
	}()
	n, _ := io.Copy(client, up)
	s.in.Add(uint64(n))
	if cw, ok := client.(closeWriter); ok {
		cw.CloseWrite()
	} else {
		client.Close()
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		client.Close()
		up.Close()
		<-done
	}
}

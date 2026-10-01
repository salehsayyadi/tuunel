// Package tcp implements the plain TCP carrier. Session messages are already
// authenticated and encrypted, so TCP only provides reliable delivery.
package tcp

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (*Carrier) Name() string { return "tcp" }

func (*Carrier) Capabilities() carrier.Capabilities {
	// 20 byte TCP header + 12 bytes of typical options + 2 byte framing.
	return carrier.Capabilities{Reliable: true, Ordered: true, Segmenting: true, MaxMessage: carrier.MaxMessage, Overhead: 34}
}

func (*Carrier) Check(context.Context) error { return nil }

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	tune(conn)
	return carrier.NewStreamConn(conn, conn.LocalAddr(), conn.RemoteAddr()), nil
}

func tune(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

type listener struct {
	ln    *net.TCPListener
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
	return &listener{ln: ln.(*net.TCPListener), limit: carrier.NewLimiter(20, 40), sem: make(chan struct{}, max)}, nil
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) {
	for {
		_ = l.ln.SetDeadline(time.Now().Add(500 * time.Millisecond))
		conn, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, err
		}
		if !l.limit.Allow(conn.RemoteAddr(), time.Now()) {
			conn.Close()
			continue
		}
		select {
		case l.sem <- struct{}{}:
		default: // connection limit reached
			conn.Close()
			continue
		}
		tune(conn)
		return &released{StreamConn: carrier.NewStreamConn(conn, conn.LocalAddr(), conn.RemoteAddr()), sem: l.sem}, nil
	}
}

func (l *listener) Close() error   { return l.ln.Close() }
func (l *listener) Addr() net.Addr { return l.ln.Addr() }

type released struct {
	*carrier.StreamConn
	sem  chan struct{}
	once sync.Once
}

func (r *released) Close() error {
	err := r.StreamConn.Close()
	r.once.Do(func() { <-r.sem })
	return err
}

// Package faulty wraps a carrier with controllable failures (blackhole,
// dial refusal, random loss). It is used by tests and the failover lab only.
package faulty

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

type Carrier struct {
	inner carrier.Carrier
	name  string
	down  atomic.Bool
	loss  atomic.Uint32 // percent
	stall atomic.Bool
}

func Wrap(c carrier.Carrier, name string) *Carrier {
	return &Carrier{inner: c, name: name}
}

// SetDown blackholes all traffic (existing and new connections) when true.
func (c *Carrier) SetDown(v bool) { c.down.Store(v) }

// SetStall makes writes block until the connection is closed and drops all
// reads, like a black-holed stream carrier whose socket send buffer is full.
func (c *Carrier) SetStall(v bool) { c.stall.Store(v) }

// SetLoss drops the given percentage of messages in both directions.
func (c *Carrier) SetLoss(pct int) { c.loss.Store(uint32(pct)) }

func (c *Carrier) Name() string                       { return c.name }
func (c *Carrier) Capabilities() carrier.Capabilities { return c.inner.Capabilities() }
func (c *Carrier) Check(ctx context.Context) error    { return c.inner.Check(ctx) }

var ErrDown = errors.New("faulty: carrier down")

func (c *Carrier) Dial(ctx context.Context, a string) (carrier.Conn, error) {
	if c.down.Load() {
		return nil, ErrDown
	}
	x, err := c.inner.Dial(ctx, a)
	if err != nil {
		return nil, err
	}
	return newConn(x, c), nil
}

func (c *Carrier) Listen(ctx context.Context, a string) (carrier.Listener, error) {
	l, err := c.inner.Listen(ctx, a)
	if err != nil {
		return nil, err
	}
	return &listener{Listener: l, c: c}, nil
}

type listener struct {
	carrier.Listener
	c *Carrier
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) {
	x, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return newConn(x, l.c), nil
}

type conn struct {
	carrier.Conn
	c      *Carrier
	closed chan struct{}
	once   sync.Once
}

func newConn(x carrier.Conn, c *Carrier) *conn {
	return &conn{Conn: x, c: c, closed: make(chan struct{})}
}

func (x *conn) Close() error {
	x.once.Do(func() { close(x.closed) })
	return x.Conn.Close()
}

func (x *conn) drop() bool {
	if x.c.down.Load() || x.c.stall.Load() {
		return true
	}
	l := x.c.loss.Load()
	return l > 0 && rand.UintN(100) < uint(l)
}

func (x *conn) ReadMessage(b []byte) (int, error) {
	for {
		n, err := x.Conn.ReadMessage(b)
		if err != nil || !x.drop() {
			return n, err
		}
	}
}

func (x *conn) WriteMessage(b []byte) error {
	if x.c.stall.Load() {
		<-x.closed
		return net.ErrClosed
	}
	if x.drop() {
		return nil
	}
	return x.Conn.WriteMessage(b)
}

var _ net.Addr = nil

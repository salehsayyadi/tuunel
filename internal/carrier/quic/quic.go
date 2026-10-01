// Package quic implements the QUIC carrier using github.com/quic-go/quic-go.
// Two modes are supported: a single bidirectional stream with length framing
// (default, reliable) and RFC 9221 DATAGRAM frames (unreliable, no
// head-of-line blocking, size-limited by the QUIC packet size).
package quic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	quicgo "github.com/quic-go/quic-go"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

var alpn = []string{"tuunel/1"}

// DatagramMax is a conservative DATAGRAM payload limit that fits the initial
// QUIC packet size (InitialPacketSize = 1200 bytes of UDP payload, the RFC 9000
// minimum) used before PMTU discovery: 1200 - (1 flags + 20 max conn ID +
// 4 packet number + 16 AEAD tag + 3 frame header) = 1156 >= 1150.
const DatagramMax = 1150

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (c *Carrier) Name() string { return "quic" }

func (c *Carrier) Capabilities() carrier.Capabilities {
	if c.opts.Datagrams {
		// UDP 8 + short header (1 + 4 conn ID + up to 4 PN) + tag 16 + frame 3
		return carrier.Capabilities{Datagram: true, MaxMessage: DatagramMax, Overhead: 36}
	}
	return carrier.Capabilities{Reliable: true, Ordered: true, Segmenting: true, MaxMessage: carrier.MaxMessage, Overhead: 52}
}

func (c *Carrier) Check(context.Context) error { return nil }

func (c *Carrier) config() *quicgo.Config {
	return &quicgo.Config{
		EnableDatagrams: c.opts.Datagrams,
		// RFC 9000 minimum. quic-go's default (1280-byte UDP payload) needs a
		// >=1308-byte IPv4 path and failed the handshake on 1280/1300 paths in
		// the MTU lab. DPLPMTUD still grows the size once connected.
		InitialPacketSize:    1200,
		KeepAlivePeriod:      10 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
		HandshakeIdleTimeout: 10 * time.Second,
	}
}

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	tlsConf, err := carrier.ClientTLS(c.opts, alpn)
	if err != nil {
		return nil, err
	}
	if tlsConf.ServerName == "" {
		tlsConf.ServerName = "tuunel"
	}
	qc, err := quicgo.DialAddr(ctx, address, tlsConf, c.config())
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Most common causes: UDP filtered, or a path MTU below the 1200-byte
			// datagrams QUIC requires (underlay MTU >= 1228 IPv4 / 1248 IPv6).
			return nil, fmt.Errorf("%w (no QUIC handshake: UDP blocked or path MTU < 1228/1248?)", err)
		}
		return nil, err
	}
	if c.opts.Datagrams {
		if !qc.ConnectionState().SupportsDatagrams.Remote {
			_ = qc.CloseWithError(0, "")
			return nil, errors.New("quic: peer does not support DATAGRAM frames")
		}
		return &dgramConn{qc: qc}, nil
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		_ = qc.CloseWithError(0, "")
		return nil, err
	}
	return newStreamConn(qc, st), nil
}

type streamRWC struct {
	*quicgo.Stream
	qc *quicgo.Conn
}

// Close aborts both stream directions before closing the connection.
// CancelWrite (not the graceful Stream.Close) is required: a Write that is
// blocked on flow control / congestion of a dead path is only woken by
// CancelWrite or a deadline. With Stream.Close() first, quic-go left such a
// Write blocked forever after the connection closed, which wedged the
// engine's per-peer sender on the dead link and stopped all traffic after
// failover (found by tests/lab/failover.py, QUIC blocked under load).
func (s streamRWC) Close() error {
	s.Stream.CancelWrite(0)
	s.Stream.CancelRead(0)
	return s.qc.CloseWithError(0, "closed")
}

func newStreamConn(qc *quicgo.Conn, st *quicgo.Stream) carrier.Conn {
	return carrier.NewStreamConn(streamRWC{Stream: st, qc: qc}, qc.LocalAddr(), qc.RemoteAddr())
}

type dgramConn struct {
	qc   *quicgo.Conn
	once sync.Once
}

func (d *dgramConn) ReadMessage(b []byte) (int, error) {
	m, err := d.qc.ReceiveDatagram(context.Background())
	if err != nil {
		return 0, err
	}
	if len(m) > len(b) {
		return 0, carrier.ErrMessageTooLarge
	}
	return copy(b, m), nil
}

func (d *dgramConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > DatagramMax {
		return fmt.Errorf("%w: %d > %d", carrier.ErrMessageTooLarge, len(b), DatagramMax)
	}
	err := d.qc.SendDatagram(b)
	var tooLarge *quicgo.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: %v", carrier.ErrMessageTooLarge, err)
	}
	return err
}

func (d *dgramConn) Close() error {
	d.once.Do(func() { _ = d.qc.CloseWithError(0, "closed") })
	return nil
}
func (d *dgramConn) LocalAddr() net.Addr  { return d.qc.LocalAddr() }
func (d *dgramConn) RemoteAddr() net.Addr { return d.qc.RemoteAddr() }

type listener struct {
	ln    *quicgo.Listener
	opts  carrier.Options
	limit *carrier.Limiter
}

func (c *Carrier) Listen(ctx context.Context, address string) (carrier.Listener, error) {
	tlsConf, err := carrier.ServerTLS(c.opts, alpn)
	if err != nil {
		return nil, err
	}
	ln, err := quicgo.ListenAddr(address, tlsConf, c.config())
	if err != nil {
		return nil, err
	}
	return &listener{ln: ln, opts: c.opts, limit: carrier.NewLimiter(10, 20)}, nil
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) {
	for {
		qc, err := l.ln.Accept(ctx)
		if err != nil {
			return nil, err
		}
		if !l.limit.Allow(qc.RemoteAddr(), time.Now()) {
			_ = qc.CloseWithError(0x10, "rate limited")
			continue
		}
		if l.opts.Datagrams {
			if !qc.ConnectionState().SupportsDatagrams.Remote {
				_ = qc.CloseWithError(0x11, "datagrams required")
				continue
			}
			return &dgramConn{qc: qc}, nil
		}
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := qc.AcceptStream(actx)
		cancel()
		if err != nil {
			_ = qc.CloseWithError(0x12, "no stream")
			continue
		}
		return newStreamConn(qc, st), nil
	}
}

func (l *listener) Close() error   { return l.ln.Close() }
func (l *listener) Addr() net.Addr { return l.ln.Addr() }

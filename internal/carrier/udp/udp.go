// Package udp implements the UDP datagram carrier. Each session message is one
// UDP datagram; the Don't-Fragment bit is set so oversize messages fail
// visibly (EMSGSIZE) instead of being silently fragmented.
package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

// IdleTimeout removes listener-side sessions that have gone silent.
const IdleTimeout = 90 * time.Second

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (*Carrier) Name() string { return "udp" }

func (*Carrier) Capabilities() carrier.Capabilities {
	return carrier.Capabilities{Datagram: true, MaxMessage: 65507, Overhead: 8}
}

func (*Carrier) Check(context.Context) error { return nil }

// setDF enables path MTU discovery (DF bit) on the socket.
func setDF(c *net.UDPConn) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_DO)
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, 23 /* IPV6_MTU_DISCOVER */, 2 /* IPV6_PMTUDISC_DO */)
	})
}

func mapWriteErr(err error) error {
	if errors.Is(err, syscall.EMSGSIZE) {
		return fmt.Errorf("%w: exceeds path MTU (%v)", carrier.ErrMessageTooLarge, err)
	}
	return err
}

type clientConn struct {
	c   *net.UDPConn
	bio *batchIO
}

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	uc := conn.(*net.UDPConn)
	setDF(uc)
	_ = uc.SetReadBuffer(4 << 20)
	_ = uc.SetWriteBuffer(4 << 20)
	return &clientConn{c: uc, bio: newBatchIO(uc)}, nil
}

func (c *clientConn) ReadMessage(b []byte) (int, error) {
	var n int
	var tooBig bool
	for n == 0 && !tooBig {
		if _, err := c.bio.read(1, func(p []byte, _ net.Addr) bool {
			if len(p) > len(b) {
				tooBig = true
				return true
			}
			n = copy(b, p)
			return true
		}); err != nil {
			return 0, err
		}
	}
	if tooBig {
		return 0, carrier.ErrMessageTooLarge
	}
	return n, nil
}

// ReadMessages implements carrier.BatchReader.
func (c *clientConn) ReadMessages(fn func(msg []byte)) error {
	_, err := c.bio.read(256, func(p []byte, _ net.Addr) bool { fn(p); return true })
	return err
}

func (c *clientConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > 65507 {
		return carrier.ErrMessageTooLarge
	}
	_, err := c.c.Write(b)
	return mapWriteErr(err)
}

// WriteMessages implements carrier.BatchWriter (sendmmsg + UDP GSO).
func (c *clientConn) WriteMessages(msgs [][]byte) (int, error) {
	n, err := c.bio.write(msgs, nil)
	return n, mapWriteErr(err)
}

func (c *clientConn) Close() error         { return c.c.Close() }
func (c *clientConn) LocalAddr() net.Addr  { return c.c.LocalAddr() }
func (c *clientConn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

type listener struct {
	pc    *net.UDPConn
	bio   *batchIO
	q     *carrier.AcceptQueue
	limit *carrier.Limiter
	max   int
	mu    sync.Mutex
	conns map[string]*serverConn
	done  chan struct{}
	// lastSC caches the connection of the previous datagram (readLoop only)
	lastSC *serverConn
	once   sync.Once
}

func (c *Carrier) Listen(ctx context.Context, address string) (carrier.Listener, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	uc := pc.(*net.UDPConn)
	setDF(uc)
	_ = uc.SetReadBuffer(4 << 20)
	_ = uc.SetWriteBuffer(4 << 20)
	max := c.opts.MaxSessions
	if max <= 0 {
		max = 1024
	}
	l := &listener{pc: uc, bio: newBatchIO(uc), q: carrier.NewAcceptQueue(64), limit: carrier.NewLimiter(10, 20), max: max,
		conns: make(map[string]*serverConn), done: make(chan struct{})}
	go l.readLoop()
	go l.reaper()
	return l, nil
}

func (l *listener) readLoop() {
	for {
		_, err := l.bio.read(1<<30, func(p []byte, a net.Addr) bool {
			addr, ok := a.(*net.UDPAddr)
			if ok && len(p) > 0 {
				l.dispatch(p, addr)
			}
			return true
		})
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.Close()
				return
			}
			continue
		}
	}
}

func (l *listener) dispatch(p []byte, addr *net.UDPAddr) {
	if sc := l.lastSC; sc != nil && sc.addr.Port == addr.Port && sc.addr.IP.Equal(addr.IP) {
		select {
		case <-sc.done:
		default: // fast path: same sender as the previous datagram
			if now := time.Now().UnixNano(); now-sc.last.Load() > int64(time.Second) {
				sc.last.Store(now)
			}
			select {
			case sc.in <- append([]byte(nil), p...):
			default:
			}
			return
		}
	}
	key := addr.String()
	l.mu.Lock()
	sc := l.conns[key]
	if sc == nil {
		if len(l.conns) >= l.max || !l.limit.Allow(addr, time.Now()) {
			l.mu.Unlock()
			return
		}
		sc = &serverConn{l: l, addr: addr, in: make(chan []byte, 256), done: make(chan struct{})}
		if !l.q.Push(sc) {
			l.mu.Unlock()
			return
		}
		l.conns[key] = sc
	}
	sc.last.Store(time.Now().UnixNano())
	l.mu.Unlock()
	l.lastSC = sc
	msg := append([]byte(nil), p...)
	select {
	case sc.in <- msg:
	default: // receiver is slow: drop like a congested router would
	}
}

func (l *listener) reaper() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, sc := range l.conns {
				if now.Sub(time.Unix(0, sc.last.Load())) > IdleTimeout {
					delete(l.conns, k)
					sc.closeLocked()
				}
			}
			l.mu.Unlock()
		}
	}
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) { return l.q.Accept(ctx) }

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.q.Close()
		_ = l.pc.Close()
		l.mu.Lock()
		for k, sc := range l.conns {
			delete(l.conns, k)
			sc.closeLocked()
		}
		l.mu.Unlock()
	})
	return nil
}

func (l *listener) Addr() net.Addr { return l.pc.LocalAddr() }

type serverConn struct {
	l    *listener
	addr *net.UDPAddr
	in   chan []byte
	done chan struct{}
	once sync.Once
	last atomic.Int64 // unix nanoseconds of the last datagram
}

func (s *serverConn) closeLocked() { s.once.Do(func() { close(s.done) }) }

func (s *serverConn) ReadMessage(b []byte) (int, error) {
	select {
	case m := <-s.in:
		if len(m) > len(b) {
			return 0, carrier.ErrMessageTooLarge
		}
		return copy(b, m), nil
	case <-s.done:
		return 0, net.ErrClosed
	}
}

func (s *serverConn) WriteMessage(b []byte) error {
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	if len(b) == 0 || len(b) > 65507 {
		return carrier.ErrMessageTooLarge
	}
	_, err := s.l.pc.WriteToUDP(b, s.addr)
	return mapWriteErr(err)
}

// WriteMessages implements carrier.BatchWriter (sendmmsg + UDP GSO).
func (s *serverConn) WriteMessages(msgs [][]byte) (int, error) {
	select {
	case <-s.done:
		return 0, net.ErrClosed
	default:
	}
	n, err := s.l.bio.write(msgs, s.addr)
	return n, mapWriteErr(err)
}

// ReadMessages implements carrier.BatchReader: it waits for one message and
// returns it together with whatever else is already queued.
func (s *serverConn) ReadMessages(fn func(msg []byte)) error { return s.ReadOwned(fn) }

// ReadOwned is ReadMessages where fn may keep msg (it is not reused).
func (s *serverConn) ReadOwned(fn func(msg []byte)) error {
	select {
	case m := <-s.in:
		fn(m)
	case <-s.done:
		return net.ErrClosed
	}
	for i := 0; i < 255; i++ {
		select {
		case m := <-s.in:
			fn(m)
		default:
			return nil
		}
	}
	return nil
}

func (s *serverConn) Close() error {
	s.l.mu.Lock()
	if s.l.conns[s.addr.String()] == s {
		delete(s.l.conns, s.addr.String())
	}
	s.closeLocked()
	s.l.mu.Unlock()
	return nil
}

func (s *serverConn) LocalAddr() net.Addr  { return s.l.pc.LocalAddr() }
func (s *serverConn) RemoteAddr() net.Addr { return s.addr }

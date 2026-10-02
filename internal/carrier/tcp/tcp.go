// Package tcp implements the plain TCP carrier. Session messages are already
// authenticated and encrypted, so TCP only provides reliable delivery.
//
// Multi-stream mode (Options.Streams > 1, dialer side) opens several TCP
// connections per link and spreads inner flows across them by flow hash
// (each inner flow stays on one connection, so it is never reordered). One
// loss then stalls only part of the traffic instead of every flow
// (head-of-line blocking), and paths that throttle each TCP connection give
// proportionally more bandwidth. Each member connection starts with a 16-byte
// preamble; listeners detect it and accept classic single-stream dialers too.
package tcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

// MaxStreams bounds parallel connections per link.
const MaxStreams = 16

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (*Carrier) Name() string { return "tcp" }

func (*Carrier) Capabilities() carrier.Capabilities {
	// 20 byte TCP header + 12 bytes of typical options + 2 byte framing.
	return carrier.Capabilities{Reliable: true, Ordered: true, Segmenting: true, MaxMessage: carrier.MaxMessage, Overhead: 34}
}

func (*Carrier) Check(context.Context) error { return nil }

func (c *Carrier) streams() int {
	n := c.opts.Streams
	if n < 1 {
		n = 1
	}
	if n > MaxStreams {
		n = MaxStreams
	}
	return n
}

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	n := c.streams()
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	if n == 1 {
		conn, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		tune(conn)
		return carrier.NewStreamConn(conn, conn.LocalAddr(), conn.RemoteAddr()), nil
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	members := make([]*carrier.StreamConn, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := d.DialContext(ctx, "tcp", address)
			if err != nil {
				errs[i] = err
				return
			}
			tune(conn)
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(preamble(id, i, n)); err != nil {
				conn.Close()
				errs[i] = err
				return
			}
			_ = conn.SetWriteDeadline(time.Time{})
			members[i] = carrier.NewStreamConn(conn, conn.LocalAddr(), conn.RemoteAddr())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			for _, m := range members {
				if m != nil {
					m.Close()
				}
			}
			return nil, err
		}
	}
	return newMulti(members), nil
}

// Preamble: magic "TUMS", 8-byte group id, member index, member count,
// version, reserved. A classic stream starts with a 2-byte frame length of a
// handshake message, never 0x5455 ("TU").
var magic = []byte("TUMS")

const preambleLen = 16

func preamble(id [8]byte, idx, n int) []byte {
	b := make([]byte, preambleLen)
	copy(b, magic)
	copy(b[4:12], id[:])
	b[12], b[13], b[14] = byte(idx), byte(n), 1
	return b
}

// CongestionControl is requested for every carrier TCP socket. BBR keeps a
// long-lived flow fast on lossy long-distance paths where CUBIC collapses
// (each loss halves the window); unavailable algorithms are ignored and the
// system default stays.
var CongestionControl = "bbr"

// NotSentLowat caps unsent data held in the kernel socket buffer. Without it
// a large send buffer queues seconds of data (bufferbloat: every inner flow
// waits behind it); with it the backlog stays in the small carrier queue,
// where data packets are dropped early so inner TCP slows down instead.
var NotSentLowat = 128 << 10

// UserTimeout makes the kernel abort a connection whose sent data stays
// unacknowledged this long, so a black-holed path fails fast.
var UserTimeout = 20 * time.Second

func tune(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		if rc, err := tc.SyscallConn(); err == nil {
			_ = rc.Control(func(fd uintptr) {
				if CongestionControl != "" {
					_ = syscall.SetsockoptString(int(fd), syscall.IPPROTO_TCP, syscall.TCP_CONGESTION, CongestionControl)
				}
				if NotSentLowat > 0 {
					_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, 25 /* TCP_NOTSENT_LOWAT */, NotSentLowat)
				}
				if UserTimeout > 0 {
					_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, 18 /* TCP_USER_TIMEOUT */, int(UserTimeout/time.Millisecond))
				}
			})
		}
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

type listener struct {
	ln    *net.TCPListener
	limit *carrier.Limiter
	sem   chan struct{}
	q     *carrier.AcceptQueue

	mu     sync.Mutex
	groups map[[8]byte]*group
	done   chan struct{}
	once   sync.Once
}

type group struct {
	members []*carrier.StreamConn
	have    int
	release func()
	timer   *time.Timer
}

// GroupTimeout bounds how long a listener waits for all members of a
// multi-stream group.
var GroupTimeout = 10 * time.Second

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
	l := &listener{ln: ln.(*net.TCPListener), limit: carrier.NewLimiter(20, 40), sem: make(chan struct{}, max),
		q: carrier.NewAcceptQueue(64), groups: map[[8]byte]*group{}, done: make(chan struct{})}
	go l.acceptLoop()
	return l, nil
}

func (l *listener) acceptLoop() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.Close()
				return
			}
			select {
			case <-l.done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
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
		go l.classify(conn)
	}
}

// classify peeks at the first bytes: a multi-stream preamble joins a group,
// anything else is a classic single-stream connection.
func (l *listener) classify(conn net.Conn) {
	var relOnce sync.Once
	release := func() { relOnce.Do(func() { <-l.sem }) }
	br := bufio.NewReaderSize(conn, 64<<10)
	_ = conn.SetReadDeadline(time.Now().Add(GroupTimeout))
	head, err := br.Peek(len(magic))
	if err != nil {
		conn.Close()
		release()
		return
	}
	if !bytes.Equal(head, magic) {
		_ = conn.SetReadDeadline(time.Time{})
		sc := carrier.NewStreamConnReader(conn, br, conn.LocalAddr(), conn.RemoteAddr())
		r := &released{Conn: sc, release: release}
		if !l.q.Push(r) {
			r.Close()
		}
		return
	}
	pre := make([]byte, preambleLen)
	if _, err := io.ReadFull(br, pre); err != nil {
		conn.Close()
		release()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var id [8]byte
	copy(id[:], pre[4:12])
	idx, n := int(pre[12]), int(pre[13])
	if n < 2 || n > MaxStreams || idx >= n || pre[14] != 1 {
		conn.Close()
		release()
		return
	}
	sc := carrier.NewStreamConnReader(conn, br, conn.LocalAddr(), conn.RemoteAddr())
	l.mu.Lock()
	g := l.groups[id]
	if g == nil {
		g = &group{members: make([]*carrier.StreamConn, n)}
		g.timer = time.AfterFunc(GroupTimeout, func() { l.expire(id, g) })
		l.groups[id] = g
	}
	if len(g.members) != n || g.members[idx] != nil {
		l.mu.Unlock()
		sc.Close()
		release()
		return
	}
	g.members[idx] = sc
	prev := g.release
	g.release = func() {
		release()
		if prev != nil {
			prev()
		}
	}
	g.have++
	complete := g.have == n
	if complete {
		delete(l.groups, id)
		g.timer.Stop()
	}
	l.mu.Unlock()
	if complete {
		r := &released{Conn: newMulti(g.members), release: g.release}
		if !l.q.Push(r) {
			r.Close()
		}
	}
}

func (l *listener) expire(id [8]byte, g *group) {
	l.mu.Lock()
	if l.groups[id] != g {
		l.mu.Unlock()
		return
	}
	delete(l.groups, id)
	l.mu.Unlock()
	for _, m := range g.members {
		if m != nil {
			m.Close()
		}
	}
	if g.release != nil {
		g.release()
	}
}

func (l *listener) Accept(ctx context.Context) (carrier.Conn, error) { return l.q.Accept(ctx) }

func (l *listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		l.q.Close()
		err = l.ln.Close()
		l.mu.Lock()
		gs := l.groups
		l.groups = map[[8]byte]*group{}
		l.mu.Unlock()
		for _, g := range gs {
			g.timer.Stop()
			for _, m := range g.members {
				if m != nil {
					m.Close()
				}
			}
			if g.release != nil {
				g.release()
			}
		}
	})
	return err
}

func (l *listener) Addr() net.Addr { return l.ln.Addr() }

type released struct {
	carrier.Conn
	release func()
}

func (r *released) Close() error {
	err := r.Conn.Close()
	r.release()
	return err
}

func (r *released) WriteMessageFlow(b []byte, flow uint32) error {
	if fw, ok := r.Conn.(carrier.FlowWriter); ok {
		return fw.WriteMessageFlow(b, flow)
	}
	return r.Conn.WriteMessage(b)
}

// multi bundles member streams into one message connection. Control messages
// use member 0; data messages are spread by flow hash. Messages from all
// members are merged for reading. Any member failure fails the whole bundle
// (the engine then reconnects).
type multi struct {
	members []*carrier.StreamConn
	in      chan []byte
	done    chan struct{}
	once    sync.Once
	errMu   sync.Mutex
	err     error
}

func newMulti(members []*carrier.StreamConn) *multi {
	m := &multi{members: members, in: make(chan []byte, 256), done: make(chan struct{})}
	for _, s := range members {
		go m.reader(s)
	}
	return m
}

func (m *multi) reader(s *carrier.StreamConn) {
	buf := make([]byte, carrier.MaxMessage)
	for {
		n, err := s.ReadMessage(buf)
		if err != nil {
			m.fail(err)
			return
		}
		select {
		case m.in <- append([]byte(nil), buf[:n]...):
		case <-m.done:
			return
		}
	}
}

func (m *multi) fail(err error) {
	m.errMu.Lock()
	if m.err == nil {
		m.err = err
	}
	m.errMu.Unlock()
	m.Close()
}

func (m *multi) ReadMessage(b []byte) (int, error) {
	select {
	case msg := <-m.in:
		if len(msg) > len(b) {
			return 0, carrier.ErrMessageTooLarge
		}
		return copy(b, msg), nil
	case <-m.done:
		// deliver what already arrived before reporting the failure
		select {
		case msg := <-m.in:
			if len(msg) > len(b) {
				return 0, carrier.ErrMessageTooLarge
			}
			return copy(b, msg), nil
		default:
		}
		m.errMu.Lock()
		err := m.err
		m.errMu.Unlock()
		if err == nil {
			err = net.ErrClosed
		}
		return 0, fmt.Errorf("tcp multi-stream: %w", err)
	}
}

func (m *multi) WriteMessage(b []byte) error { return m.members[0].WriteMessage(b) }

func (m *multi) WriteMessageFlow(b []byte, flow uint32) error {
	return m.members[flow%uint32(len(m.members))].WriteMessageFlow(b, flow)
}

func (m *multi) Close() error {
	m.once.Do(func() {
		close(m.done)
		for _, s := range m.members {
			s.Close()
		}
	})
	return nil
}

func (m *multi) LocalAddr() net.Addr  { return m.members[0].LocalAddr() }
func (m *multi) RemoteAddr() net.Addr { return m.members[0].RemoteAddr() }

// streamsOf reports the number of member connections of c (1 for classic).
func streamsOf(c carrier.Conn) int {
	if r, ok := c.(*released); ok {
		c = r.Conn
	}
	if m, ok := c.(*multi); ok {
		return len(m.members)
	}
	return 1
}

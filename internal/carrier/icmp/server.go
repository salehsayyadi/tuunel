package icmp

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

const idleTimeout = 60 * time.Second

type listener struct {
	pc    *xicmp.PacketConn
	q     *carrier.AcceptQueue
	limit *carrier.Limiter
	max   int
	mu    sync.Mutex
	conns map[string]*serverConn
	done  chan struct{}
	once  sync.Once
}

func (c *Carrier) Listen(ctx context.Context, address string) (carrier.Listener, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	pc, err := xicmp.ListenPacket("ip4:icmp", address)
	if err != nil {
		return nil, classify(err)
	}
	max := c.opts.MaxSessions
	if max <= 0 {
		max = 256
	}
	l := &listener{pc: pc, q: carrier.NewAcceptQueue(32), limit: carrier.NewLimiter(5, 10), max: max,
		conns: map[string]*serverConn{}, done: make(chan struct{})}
	go l.readLoop()
	go l.reaper()
	return l, nil
}

func (l *listener) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, from, err := l.pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && !ne.Timeout() {
				l.Close()
				return
			}
			continue
		}
		ia, ok := from.(*net.IPAddr)
		if !ok {
			continue
		}
		t, e, ok := parse(buf[:n])
		if !ok || t != ipv4.ICMPTypeEcho || [4]byte(e.Data[:4]) != magicReq {
			continue // ordinary pings are left to the kernel
		}
		key := ia.IP.String() + "/" + itoa(e.ID)
		l.mu.Lock()
		sc := l.conns[key]
		if sc == nil {
			if len(l.conns) >= l.max || !l.limit.Allow(ipAddr{ia.IP}, time.Now()) {
				l.mu.Unlock()
				continue
			}
			sc = &serverConn{l: l, key: key, ip: ia.IP, id: e.ID, in: make(chan []byte, 256), done: make(chan struct{})}
			if !l.q.Push(sc) {
				l.mu.Unlock()
				continue
			}
			l.conns[key] = sc
		}
		sc.last = time.Now()
		sc.seq.Store(uint32(e.Seq))
		l.mu.Unlock()
		msg := append([]byte(nil), e.Data[4:]...)
		select {
		case sc.in <- msg:
		default:
		}
	}
}

func itoa(i int) string {
	b := make([]byte, 0, 6)
	if i == 0 {
		return "0"
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
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
				if now.Sub(sc.last) > idleTimeout {
					delete(l.conns, k)
					sc.shut()
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
			sc.shut()
		}
		l.mu.Unlock()
	})
	return nil
}

func (l *listener) Addr() net.Addr { return l.pc.LocalAddr() }

type serverConn struct {
	l    *listener
	key  string
	ip   net.IP
	id   int
	seq  atomic.Uint32
	in   chan []byte
	done chan struct{}
	once sync.Once
	last time.Time
}

func (s *serverConn) shut() { s.once.Do(func() { close(s.done) }) }

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
	if len(b) == 0 || len(b) > MaxPayload {
		return carrier.ErrMessageTooLarge
	}
	m, err := marshal(ipv4.ICMPTypeEchoReply, s.id, int(s.seq.Load()), magicRep, b)
	if err != nil {
		return err
	}
	_, err = s.l.pc.WriteTo(m, &net.IPAddr{IP: s.ip})
	return err
}

func (s *serverConn) Close() error {
	s.l.mu.Lock()
	if s.l.conns[s.key] == s {
		delete(s.l.conns, s.key)
	}
	s.l.mu.Unlock()
	s.shut()
	return nil
}

func (s *serverConn) LocalAddr() net.Addr  { return s.l.pc.LocalAddr() }
func (s *serverConn) RemoteAddr() net.Addr { return ipAddr{s.ip} }

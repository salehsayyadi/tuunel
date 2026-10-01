package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/failover"
	"github.com/salehsayyadi/tuunel/internal/health"
	"github.com/salehsayyadi/tuunel/internal/mtu"
	"github.com/salehsayyadi/tuunel/internal/packet"
	"github.com/salehsayyadi/tuunel/internal/session"
)

// maxPingBody bounds echo payloads so pings cannot be used for amplification.
const maxPingBody = 1200

// link is one authenticated session over one carrier connection.
type link struct {
	e         *Engine
	peer      *peer
	conn      carrier.Conn
	pm        *pump
	carrier   carrier.Carrier
	endpoint  string
	cand      *failover.Candidate
	initiator bool
	probe     bool
	limit     int

	mu      sync.Mutex
	cur     *session.Session
	prev    *session.Session
	pending []*session.Initiation
	rekeyAt time.Time

	tracker     *health.Tracker
	established time.Time
	lastRecv    atomic.Int64
	pingSeq     atomic.Uint64
	confirmed   chan struct{}
	confirmOnce sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
	closeReason atomic.Value
	echoBytes   atomic.Uint64
	pongs       atomic.Uint64
	waitMu      sync.Mutex
	waiters     map[uint64]chan time.Duration
}

func newLink(e *Engine, p *peer, conn carrier.Conn, pm *pump, c carrier.Carrier, endpoint string, cand *failover.Candidate, initiator, probe bool) *link {
	outer6 := false
	if a, ok := conn.RemoteAddr().(interface{ String() string }); ok && a != nil {
		if host, _, err := net.SplitHostPort(a.String()); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
				outer6 = true
			}
		}
	}
	l := &link{e: e, peer: p, conn: conn, pm: pm, carrier: c, endpoint: endpoint, cand: cand, initiator: initiator, probe: probe,
		limit:   mtu.LinkLimit(e.cfg.PathMTU, c.Capabilities(), outer6),
		tracker: health.NewTracker(e.cfg.Health), established: time.Now(),
		confirmed: make(chan struct{}), closed: make(chan struct{})}
	l.lastRecv.Store(time.Now().UnixNano())
	return l
}

func (l *link) setSession(s *session.Session) {
	l.mu.Lock()
	l.prev, l.cur = l.cur, s
	l.rekeyAt = time.Now().Add(l.e.cfg.RekeyInterval)
	l.mu.Unlock()
}

func (l *link) close(reason string) {
	l.closeOnce.Do(func() {
		l.closeReason.Store(reason)
		if reason == "probe finished" || reason == "reconnect requested" {
			// best-effort notification on a link believed healthy
			_ = l.send(session.InnerClose, nil)
		}
		close(l.closed)
		_ = l.conn.Close()
		l.peer.linkClosed(l, reason)
	})
}

func (l *link) send(inner byte, body []byte) error {
	l.mu.Lock()
	s := l.cur
	l.mu.Unlock()
	if s == nil {
		return errors.New("no session")
	}
	msg, err := s.Seal(make([]byte, 0, session.DataHeaderLen+1+len(body)+16), inner, body)
	if err != nil {
		return err
	}
	return l.conn.WriteMessage(msg)
}

func (l *link) sendPing(pad int) error {
	id := l.pingSeq.Add(1)
	if pad > maxPingBody-8 {
		pad = maxPingBody - 8
	}
	body := make([]byte, 8+pad)
	binary.BigEndian.PutUint64(body, id)
	l.tracker.OnSent(id, time.Now())
	return l.send(session.InnerPing, body)
}

func (l *link) readLoop(ctx context.Context) {
	defer l.e.wg.Done()
	for {
		var msg []byte
		select {
		case m, ok := <-l.pm.ch:
			if !ok {
				reason := "carrier closed"
				if l.pm.err != nil {
					reason = "carrier error: " + l.pm.err.Error()
				}
				l.close(reason)
				return
			}
			msg = m
		case <-l.closed:
			return
		case <-ctx.Done():
			l.close("shutdown")
			return
		}
		l.handle(msg)
	}
}

func (l *link) handle(msg []byte) {
	switch msg[0] {
	case session.TypeData:
		l.handleData(msg)
	case session.TypeHandshakeInit:
		if l.initiator || l.probe {
			return
		}
		// Rekey (or a retransmitted initiation) on the same connection.
		sess, reply, err := l.e.responder.Respond(msg)
		if err != nil || !session.KeysEqual(sess.PeerKey, l.peer.cfg.PublicKey) {
			l.e.counters.AuthFailures.Add(1)
			return
		}
		if l.conn.WriteMessage(reply) == nil {
			l.setSession(sess)
		}
	case session.TypeHandshakeResp:
		if !l.initiator {
			return
		}
		l.mu.Lock()
		for i, in := range l.pending {
			if s, err := in.Finish(msg); err == nil {
				l.pending = append(l.pending[:i], l.pending[i+1:]...)
				l.prev, l.cur = l.cur, s
				l.rekeyAt = time.Now().Add(l.e.cfg.RekeyInterval)
				break
			}
		}
		l.mu.Unlock()
	default:
		l.e.counters.Malformed.Add(1)
	}
}

func (l *link) handleData(msg []byte) {
	idx, _ := session.ReceiverIndex(msg)
	l.mu.Lock()
	var s *session.Session
	if l.cur != nil && l.cur.LocalIndex == idx {
		s = l.cur
	} else if l.prev != nil && l.prev.LocalIndex == idx {
		s = l.prev
	}
	l.mu.Unlock()
	if s == nil {
		l.e.counters.AuthFailures.Add(1)
		return
	}
	typ, body, err := s.Open(msg)
	if err != nil {
		l.e.counters.AuthFailures.Add(1)
		return
	}
	l.lastRecv.Store(time.Now().UnixNano())
	l.confirmOnce.Do(func() { close(l.confirmed) })
	switch typ {
	case session.InnerIP:
		if l.probe {
			return
		}
		if packet.Validate(body, 65535) != nil {
			l.e.counters.Malformed.Add(1)
			return
		}
		src, _ := packet.Addrs(body)
		if !l.peer.allowed(src) {
			l.e.counters.Spoofed.Add(1)
			return
		}
		l.peer.stats.rxPackets.Add(1)
		l.peer.stats.rxBytes.Add(uint64(len(body)))
		_, _ = l.e.cfg.Device.Write(body)
	case session.InnerPing:
		if len(body) < 8 || len(body) > maxPingBody {
			return
		}
		l.echoBytes.Add(uint64(len(body)))
		_ = l.send(session.InnerPong, body)
	case session.InnerPong:
		if len(body) >= 8 {
			l.pongs.Add(1)
			id := binary.BigEndian.Uint64(body)
			rtt, ok := l.tracker.OnPong(id, time.Now())
			if ok {
				l.waitMu.Lock()
				if ch := l.waiters[id]; ch != nil {
					ch <- rtt
					delete(l.waiters, id)
				}
				l.waitMu.Unlock()
			}
		}
	case session.InnerClose:
		l.close("closed by peer")
	}
}

// healthLoop pings the peer, evaluates link state and rekeys.
func (l *link) healthLoop(ctx context.Context) {
	defer l.e.wg.Done()
	t := time.NewTicker(l.e.cfg.HealthInterval)
	defer t.Stop()
	prev := health.Unknown
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.closed:
			return
		case now := <-t.C:
			if err := l.sendPing(0); err != nil && !errors.Is(err, carrier.ErrMessageTooLarge) {
				l.close("write failed: " + err.Error())
				return
			}
			l.tracker.Expire(now)
			st := l.tracker.Evaluate(now)
			m := l.tracker.Snapshot()
			if st != prev {
				switch st {
				case health.Degraded:
					l.e.log.Warn("carrier degraded", "peer", l.peer.cfg.Name, "carrier", l.carrier.Name(), "loss", round1(m.LossPct), "rtt", m.AvgRTT.Round(time.Millisecond), "jitter", m.Jitter.Round(time.Millisecond))
				case health.Available:
					if prev == health.Degraded {
						l.e.log.Info("carrier recovered", "peer", l.peer.cfg.Name, "carrier", l.carrier.Name())
					}
				}
				prev = st
			}
			if l.initiator {
				l.peer.mgr.ActiveState(st, m.AvgRTT, now)
			}
			idle := now.Sub(time.Unix(0, l.lastRecv.Load()))
			if st == health.Failed || idle > l.e.cfg.IdleTimeout {
				reason := "health check failed"
				if idle > l.e.cfg.IdleTimeout {
					reason = "idle timeout"
				}
				l.e.log.Warn("carrier failed", "peer", l.peer.cfg.Name, "carrier", l.carrier.Name(), "reason", reason, "loss", round1(m.LossPct))
				l.close(reason)
				return
			}
			if l.initiator {
				l.maybeRekey(now)
			}
		}
	}
}

func (l *link) maybeRekey(now time.Time) {
	l.mu.Lock()
	due := now.After(l.rekeyAt) || l.cur != nil && l.cur.SentMessages() > session.RekeyAfterMessages
	if !due || len(l.pending) > 4 {
		l.mu.Unlock()
		return
	}
	l.rekeyAt = now.Add(l.e.cfg.HandshakeTimeout) // retry soon if the response is lost
	l.mu.Unlock()
	in, msg, err := session.Initiate(l.e.cfg.Key, l.peer.cfg.PublicKey, l.e.cfg.PSK, l.e.cfg.NodeID, false)
	if err != nil {
		return
	}
	l.mu.Lock()
	l.pending = append(l.pending, in)
	l.mu.Unlock()
	if err := l.conn.WriteMessage(msg); err == nil {
		l.e.log.Debug("rekey initiated", "peer", l.peer.cfg.Name, "carrier", l.carrier.Name())
	}
}

// probeServe keeps a probe link alive only while the prober is active.
func (l *link) probeServe(ctx context.Context) {
	defer l.e.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			l.close("shutdown")
			return
		case <-l.closed:
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, l.lastRecv.Load())) > 5*time.Second || now.Sub(start) > time.Minute {
				l.close("probe finished")
				return
			}
		}
	}
}

// sendIP transmits an inner IP packet, fragmenting or signalling
// packet-too-big when it exceeds what this link can carry.
func (l *link) sendIP(pkt []byte) error {
	if len(pkt) <= l.limit {
		return l.send(session.InnerIP, pkt)
	}
	if pkt[0]>>4 == 4 && !packet.DontFragment(pkt) {
		frags, err := packet.FragmentIPv4(pkt, l.limit)
		if err == nil {
			l.e.counters.Fragmented.Add(1)
			for _, f := range frags {
				if err := l.send(session.InnerIP, f); err != nil {
					return err
				}
			}
			return nil
		}
	}
	l.e.counters.TooBig.Add(1)
	mtuv := l.limit
	v6 := pkt[0]>>4 == 6
	if v6 && mtuv < packet.MinIPv6MTU {
		return nil // cannot signal below the IPv6 minimum; drop
	}
	if icmp := packet.TooBig(pkt, mtuv, l.e.localAddr(v6)); icmp != nil {
		_, _ = l.e.cfg.Device.Write(icmp)
	}
	return nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// pingWait sends one ping and waits for its pong.
func (l *link) pingWait(timeout time.Duration) (time.Duration, error) {
	id := l.pingSeq.Add(1)
	ch := make(chan time.Duration, 1)
	l.waitMu.Lock()
	if l.waiters == nil {
		l.waiters = map[uint64]chan time.Duration{}
	}
	l.waiters[id] = ch
	l.waitMu.Unlock()
	defer func() {
		l.waitMu.Lock()
		delete(l.waiters, id)
		l.waitMu.Unlock()
	}()
	body := make([]byte, 8)
	binary.BigEndian.PutUint64(body, id)
	l.tracker.OnSent(id, time.Now())
	if err := l.send(session.InnerPing, body); err != nil {
		return 0, err
	}
	select {
	case d := <-ch:
		return d, nil
	case <-time.After(timeout):
		return 0, errors.New("timeout")
	case <-l.closed:
		return 0, errors.New("link closed")
	}
}

// Package engine is the carrier-independent L3 tunnel engine. It reads IP
// packets from a Device (TUN), routes them to peers by allowed IPs, seals
// them with the session layer and writes them to whichever carrier link is
// currently active for that peer. It contains no carrier-specific logic:
// carriers are used exclusively through the carrier.Carrier interface.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/failover"
	"github.com/salehsayyadi/tuunel/internal/health"
	"github.com/salehsayyadi/tuunel/internal/packet"
	"github.com/salehsayyadi/tuunel/internal/session"
)

// Device is the local packet interface (a TUN device in production).
type Device interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

type CandidateConfig struct {
	Endpoint     string
	EndpointRank int
	Carrier      carrier.Carrier
	CarrierRank  int
	Address      string
}

type PeerConfig struct {
	Name       string
	PublicKey  []byte
	AllowedIPs []netip.Prefix
	Candidates []CandidateConfig // empty: this node only accepts the peer
}

type ListenerConfig struct {
	Carrier carrier.Carrier
	Address string
}

type Config struct {
	NodeID  string
	Key     session.KeyPair
	PSK     []byte
	Device  Device
	MTU     int
	PathMTU int
	// DetectPathMTU, when set, is asked for the kernel's path MTU towards
	// each link's remote address (host:port). A smaller value than PathMTU
	// lowers that link's limit, so datagram carriers never emit outer
	// packets larger than the real path (e.g. on a listener, whose plan
	// cannot know the peers' paths in advance).
	DetectPathMTU    func(address string) (int, error)
	LocalAddrs       []netip.Addr
	Peers            []PeerConfig
	Listeners        []ListenerConfig
	Health           health.Thresholds
	HealthInterval   time.Duration
	IdleTimeout      time.Duration
	HandshakeTimeout time.Duration
	RekeyInterval    time.Duration
	FailoverEnabled  bool
	Failover         failover.Policy
	ProbeInterval    time.Duration
	Logger           *slog.Logger
}

func (c *Config) defaults() {
	if c.HealthInterval == 0 {
		c.HealthInterval = 2 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.RekeyInterval == 0 {
		c.RekeyInterval = 10 * time.Minute
	}
	if c.ProbeInterval == 0 {
		c.ProbeInterval = 10 * time.Second
	}
	if c.Health.Window == 0 {
		c.Health = health.DefaultThresholds()
	}
	if c.Failover.BackoffInitial == 0 {
		c.Failover = failover.DefaultPolicy()
	}
	if c.PathMTU == 0 {
		c.PathMTU = 1500
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

type Engine struct {
	cfg       Config
	log       *slog.Logger
	peers     []*peer
	byKey     map[string]*peer
	responder *session.Responder
	listeners []carrier.Listener
	started   time.Time
	hsSem     chan struct{}
	wg        sync.WaitGroup
	counters  Counters
	lnStatus  []ListenerStatus
	stMu      sync.Mutex
}

// Counters are engine-wide drop counters.
type Counters struct {
	NoRoute      atomic.Uint64
	Malformed    atomic.Uint64
	AuthFailures atomic.Uint64
	Spoofed      atomic.Uint64
	TooBig       atomic.Uint64
	Fragmented   atomic.Uint64
}

func New(cfg Config) (*Engine, error) {
	cfg.defaults()
	if cfg.Device == nil {
		return nil, errors.New("engine: device is required")
	}
	if cfg.MTU < packet.MinIPv4MTU || cfg.MTU > 65535-session.Overhead {
		return nil, fmt.Errorf("engine: invalid MTU %d", cfg.MTU)
	}
	e := &Engine{cfg: cfg, log: cfg.Logger, byKey: map[string]*peer{}, hsSem: make(chan struct{}, 64)}
	for _, pc := range cfg.Peers {
		if len(pc.PublicKey) != 32 {
			return nil, fmt.Errorf("engine: peer %s: invalid public key", pc.Name)
		}
		if _, dup := e.byKey[string(pc.PublicKey)]; dup {
			return nil, fmt.Errorf("engine: duplicate peer key for %s", pc.Name)
		}
		p := newPeer(e, pc)
		e.peers = append(e.peers, p)
		e.byKey[string(pc.PublicKey)] = p
	}
	e.responder = session.NewResponder(cfg.Key, cfg.PSK, func(k []byte) bool { _, ok := e.byKey[string(k)]; return ok })
	return e, nil
}

// Run starts listeners, peer supervisors and the device reader, and blocks
// until ctx is cancelled or the device fails.
func (e *Engine) Run(ctx context.Context) error {
	e.stMu.Lock()
	e.started = time.Now()
	e.stMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, lc := range e.cfg.Listeners {
		st := ListenerStatus{Carrier: lc.Carrier.Name(), Address: lc.Address}
		if err := lc.Carrier.Check(ctx); err != nil {
			st.Error = err.Error()
			e.addListenerStatus(st)
			e.log.Warn("carrier listener unavailable", "carrier", lc.Carrier.Name(), "error", err)
			continue
		}
		ln, err := lc.Carrier.Listen(ctx, lc.Address)
		if err != nil {
			st.Error = err.Error()
			e.addListenerStatus(st)
			e.log.Warn("carrier listen failed", "carrier", lc.Carrier.Name(), "address", lc.Address, "error", err)
			continue
		}
		st.Address, st.Listening = ln.Addr().String(), true
		e.addListenerStatus(st)
		e.listeners = append(e.listeners, ln)
		e.log.Info("listening", "carrier", lc.Carrier.Name(), "address", ln.Addr().String())
		e.wg.Add(1)
		go e.acceptLoop(ctx, ln, lc.Carrier)
	}
	if len(e.cfg.Listeners) > 0 && len(e.listeners) == 0 {
		hasDial := false
		for _, p := range e.peers {
			hasDial = hasDial || len(p.cfg.Candidates) > 0
		}
		if !hasDial {
			return errors.New("engine: no listener could be started and no peer to dial")
		}
	}
	for _, p := range e.peers {
		e.wg.Add(2)
		go p.sender(ctx)
		go p.supervise(ctx)
	}
	devErr := make(chan error, 1)
	go func() { devErr <- e.readDevice(ctx) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-devErr:
		if ctx.Err() != nil {
			err = nil
		}
	}
	cancel()
	for _, ln := range e.listeners {
		_ = ln.Close()
	}
	for _, p := range e.peers {
		p.closeAll("shutdown")
	}
	e.wg.Wait()
	return err
}

func (e *Engine) readDevice(ctx context.Context) error {
	buf := make([]byte, 65535)
	for {
		n, err := e.cfg.Device.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("engine: device read: %w", err)
		}
		pkt := buf[:n]
		if packet.Validate(pkt, 65535) != nil {
			e.counters.Malformed.Add(1)
			continue
		}
		_, dst := packet.Addrs(pkt)
		p := e.route(dst)
		if p == nil {
			e.counters.NoRoute.Add(1)
			continue
		}
		p.enqueue(append([]byte(nil), pkt...))
	}
}

// route returns the peer whose allowed IPs contain dst (longest prefix wins).
func (e *Engine) route(dst netip.Addr) *peer {
	var best *peer
	bits := -1
	for _, p := range e.peers {
		for _, pf := range p.cfg.AllowedIPs {
			if pf.Bits() > bits && pf.Contains(dst) {
				best, bits = p, pf.Bits()
			}
		}
	}
	return best
}

func (e *Engine) localAddr(v6 bool) netip.Addr {
	for _, a := range e.cfg.LocalAddrs {
		if a.Is6() == v6 {
			return a
		}
	}
	return netip.Addr{}
}

func (e *Engine) acceptLoop(ctx context.Context, ln carrier.Listener, c carrier.Carrier) {
	defer e.wg.Done()
	backoff := 10 * time.Millisecond
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, carrier.ErrClosed) {
				return
			}
			e.log.Debug("accept error", "carrier", c.Name(), "error", err)
			time.Sleep(backoff)
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 10 * time.Millisecond
		select {
		case e.hsSem <- struct{}{}:
		default: // too many handshakes in progress: shed load
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-e.hsSem }()
			e.respond(ctx, conn, c)
		}()
	}
}

// pump reads messages from conn into a channel so that handshake code and
// the link read loop share a single reader goroutine.
type pump struct {
	ch       chan []byte
	err      error
	done     chan struct{}
	stopOnce sync.Once
	conn     carrier.Conn
}

func startPump(conn carrier.Conn) *pump {
	p := &pump{ch: make(chan []byte, 64), done: make(chan struct{}), conn: conn}
	go func() {
		defer close(p.ch)
		buf := make([]byte, carrier.MaxMessage)
		for {
			n, err := conn.ReadMessage(buf)
			if err != nil {
				p.err = err
				return
			}
			if n == 0 {
				continue
			}
			select {
			case p.ch <- append([]byte(nil), buf[:n]...):
			case <-p.done:
				// Nobody will consume more messages (handshake rejected, link
				// closed). Without this the goroutine and up to 64 buffered
				// messages leaked whenever a peer kept sending.
				return
			}
		}
	}()
	return p
}

// stop closes the connection and releases the reader goroutine.
func (p *pump) stop() {
	p.stopOnce.Do(func() { close(p.done) })
	_ = p.conn.Close()
}

var errTimeout = errors.New("timeout")

func (p *pump) next(d time.Duration) ([]byte, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case m, ok := <-p.ch:
		if !ok {
			if p.err == nil {
				return nil, errors.New("connection closed")
			}
			return nil, p.err
		}
		return m, nil
	case <-t.C:
		return nil, errTimeout
	}
}

func (e *Engine) respond(ctx context.Context, conn carrier.Conn, c carrier.Carrier) {
	pm := startPump(conn)
	msg, err := pm.next(e.cfg.HandshakeTimeout)
	if err != nil {
		pm.stop()
		return
	}
	sess, reply, err := e.responder.Respond(msg)
	if err != nil {
		e.counters.AuthFailures.Add(1)
		e.log.Debug("handshake rejected", "carrier", c.Name(), "remote", addrString(conn), "error", err)
		pm.stop()
		return
	}
	p := e.byKey[string(sess.PeerKey)]
	if err := conn.WriteMessage(reply); err != nil {
		pm.stop()
		return
	}
	l := newLink(e, p, conn, pm, c, "", nil, false, sess.Probe)
	l.setSession(sess)
	e.wg.Add(1)
	go l.readLoop(ctx)
	t := time.NewTimer(e.cfg.HandshakeTimeout)
	defer t.Stop()
	select {
	case <-l.confirmed:
	case <-l.closed:
		return
	case <-t.C:
		l.close("handshake not confirmed")
		return
	case <-ctx.Done():
		l.close("shutdown")
		return
	}
	if sess.Probe {
		e.wg.Add(1)
		go l.probeServe(ctx)
		return
	}
	e.log.Info("peer connected", "peer", p.cfg.Name, "carrier", c.Name(), "remote", addrString(conn), "node", sess.PeerNodeID)
	p.install(l)
	e.wg.Add(1)
	go l.healthLoop(ctx)
}

func addrString(c carrier.Conn) string {
	if a := c.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

// dial establishes an authenticated link to cand. abort cancels the attempt;
// the resulting link lives until ctx (the engine context) ends.
func (e *Engine) dial(ctx context.Context, p *peer, cand *failover.Candidate, cc CandidateConfig, probe bool) (*link, time.Duration, error) {
	return e.dialAbortable(ctx, ctx, p, cand, cc, probe)
}

func (e *Engine) dialAbortable(ctx, abort context.Context, p *peer, cand *failover.Candidate, cc CandidateConfig, probe bool) (*link, time.Duration, error) {
	dctx, cancel := context.WithTimeout(abort, e.cfg.HandshakeTimeout)
	defer cancel()
	if err := cc.Carrier.Check(dctx); err != nil {
		return nil, 0, err
	}
	conn, err := cc.Carrier.Dial(dctx, cc.Address)
	if err != nil {
		return nil, 0, fmt.Errorf("dial: %w", err)
	}
	start := time.Now()
	// Datagram carriers may lose the initiation: retransmit with fresh
	// initiations (never replaying the same message).
	attempts := 1
	if !cc.Carrier.Capabilities().Reliable {
		attempts = 3
	}
	var pend []*session.Initiation
	var sess *session.Session
	pm := startPump(conn)
	stopConn := context.AfterFunc(dctx, func() { _ = conn.Close() })
	per := e.cfg.HandshakeTimeout / time.Duration(attempts)
	for i := 0; i < attempts && sess == nil; i++ {
		in, msg, err := session.Initiate(e.cfg.Key, p.cfg.PublicKey, e.cfg.PSK, e.cfg.NodeID, probe)
		if err != nil {
			pm.stop()
			return nil, 0, err
		}
		pend = append(pend, in)
		if err := conn.WriteMessage(msg); err != nil {
			pm.stop()
			return nil, 0, fmt.Errorf("handshake write: %w", err)
		}
		deadline := time.Now().Add(per)
		for sess == nil && time.Now().Before(deadline) {
			m, err := pm.next(time.Until(deadline))
			if err != nil {
				if errors.Is(err, errTimeout) {
					break
				}
				pm.stop()
				return nil, 0, fmt.Errorf("handshake read: %w", err)
			}
			if len(m) < 1 || m[0] != session.TypeHandshakeResp {
				continue
			}
			for j, in := range pend {
				if s, err := in.Finish(m); err == nil {
					sess = s
					pend = append(pend[:j], pend[j+1:]...)
					break
				}
			}
		}
	}
	if !stopConn() || dctx.Err() != nil {
		pm.stop()
		return nil, 0, fmt.Errorf("handshake: %w", dctx.Err())
	}
	if sess == nil {
		pm.stop()
		return nil, 0, errors.New("handshake failed: no valid response (peer unreachable, blocked, or key mismatch)")
	}
	rtt := time.Since(start)
	l := newLink(e, p, conn, pm, cc.Carrier, cc.Endpoint, cand, true, probe)
	l.setSession(sess)
	l.mu.Lock()
	l.pending = pend
	l.mu.Unlock()
	e.wg.Add(1)
	go l.readLoop(ctx)
	if err := l.sendPing(0); err != nil { // confirms the session to the responder
		l.close("confirm failed")
		return nil, 0, err
	}
	return l, rtt, nil
}

func (e *Engine) addListenerStatus(st ListenerStatus) {
	e.stMu.Lock()
	e.lnStatus = append(e.lnStatus, st)
	e.stMu.Unlock()
}

package engine

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/salehsayyadi/tuunel/internal/failover"
)

type peerStats struct {
	txPackets, txBytes, rxPackets, rxBytes, drops atomic.Uint64
	reconnects                                    atomic.Uint64
}

type peer struct {
	e     *Engine
	cfg   PeerConfig
	mgr   *failover.Manager
	cands map[*failover.Candidate]CandidateConfig
	queue chan []byte
	stats peerStats

	mu       sync.Mutex
	active   *link
	links    map[*link]struct{}
	upSince  time.Time
	connects int

	wake chan struct{}
}

func newPeer(e *Engine, pc PeerConfig) *peer {
	p := &peer{e: e, cfg: pc, queue: make(chan []byte, 1024), links: map[*link]struct{}{}, cands: map[*failover.Candidate]CandidateConfig{}, wake: make(chan struct{}, 1)}
	var list []*failover.Candidate
	for _, cc := range pc.Candidates {
		c := &failover.Candidate{Endpoint: cc.Endpoint, Carrier: cc.Carrier.Name(), Address: cc.Address, EndpointRank: cc.EndpointRank, CarrierRank: cc.CarrierRank}
		p.cands[c] = cc
		list = append(list, c)
	}
	p.mgr = failover.New(e.cfg.Failover, list)
	return p
}

func (p *peer) allowed(a netip.Addr) bool {
	for _, pf := range p.cfg.AllowedIPs {
		if pf.Contains(a) {
			return true
		}
	}
	return false
}

func (p *peer) enqueue(pkt []byte) {
	select {
	case p.queue <- pkt:
	default:
		p.stats.drops.Add(1)
	}
}

func (p *peer) activeLink() *link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *peer) sender(ctx context.Context) {
	defer p.e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-p.queue:
			l := p.activeLink()
			if l == nil {
				p.stats.drops.Add(1)
				continue
			}
			if err := l.sendIP(pkt); err != nil {
				p.stats.drops.Add(1)
				continue
			}
			p.stats.txPackets.Add(1)
			p.stats.txBytes.Add(uint64(len(pkt)))
		}
	}
}

// install makes l the active link, closing any previous one (make-before-break).
func (p *peer) install(l *link) {
	p.mu.Lock()
	old := p.active
	p.active = l
	p.links[l] = struct{}{}
	p.upSince = time.Now()
	p.connects++
	if p.connects > 1 {
		p.stats.reconnects.Add(1)
	}
	p.mu.Unlock()
	if old != nil && old != l {
		old.close("replaced")
	}
}

func (p *peer) linkClosed(l *link, reason string) {
	p.mu.Lock()
	delete(p.links, l)
	wasActive := p.active == l
	if wasActive {
		p.active = nil
	}
	p.mu.Unlock()
	if wasActive && l.initiator && l.cand != nil && reason != "shutdown" && reason != "replaced" {
		p.mgr.Failure(l.cand, time.Now(), errString(reason))
	}
	if wasActive {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func (p *peer) closeAll(reason string) {
	p.mu.Lock()
	var ls []*link
	for l := range p.links {
		ls = append(ls, l)
	}
	p.mu.Unlock()
	for _, l := range ls {
		l.close(reason)
	}
}

func (p *peer) reconnect() {
	p.mgr.Reset()
	if l := p.activeLink(); l != nil {
		l.close("reconnect requested")
	} else {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// supervise maintains a link to the peer when this node is the initiator.
func (p *peer) supervise(ctx context.Context) {
	defer p.e.wg.Done()
	if len(p.cands) == 0 {
		<-ctx.Done()
		return
	}
	probeT := time.NewTicker(p.e.cfg.ProbeInterval)
	defer probeT.Stop()
	for ctx.Err() == nil {
		if p.activeLink() == nil {
			cands, wait := p.mgr.Eligible(time.Now())
			if len(cands) == 0 {
				p.sleep(ctx, wait)
				continue
			}
			if !p.race(ctx, cands) {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-probeT.C:
			if p.e.cfg.FailoverEnabled {
				p.probeAndDecide(ctx)
			}
		}
	}
}

func (p *peer) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		d = 100 * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-p.wake:
	}
}

func (p *peer) connect(ctx context.Context, cand *failover.Candidate, from string) error {
	cc := p.cands[cand]
	l, rtt, err := p.e.dial(ctx, p, cand, cc, false)
	if err != nil {
		if ctx.Err() == nil {
			p.mgr.Failure(cand, time.Now(), err)
			p.e.log.Warn("connect failed", "peer", p.cfg.Name, "endpoint", cand.Endpoint, "carrier", cand.Carrier, "error", err)
		}
		return err
	}
	p.activate(ctx, cand, l, rtt, from)
	return nil
}

// RaceStagger is the delay between starting successive candidates when no
// link is up. Better-ranked candidates start first; the first to complete
// an authenticated handshake wins, so a blocked carrier (which fails only by
// timeout) cannot delay recovery over an unblocked one.
var RaceStagger = 250 * time.Millisecond

type raceResult struct {
	c   *failover.Candidate
	l   *link
	rtt time.Duration
	err error
}

func (p *peer) race(ctx context.Context, cands []*failover.Candidate) bool {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := make(chan raceResult, len(cands))
	for i, c := range cands {
		go func(i int, c *failover.Candidate) {
			if i > 0 {
				select {
				case <-time.After(time.Duration(i) * RaceStagger):
				case <-rctx.Done():
					res <- raceResult{c: c, err: rctx.Err()}
					return
				}
			}
			l, rtt, err := p.e.dialAbortable(ctx, rctx, p, c, p.cands[c], false)
			res <- raceResult{c, l, rtt, err}
		}(i, c)
	}
	var win *raceResult
	for range cands {
		r := <-res
		switch {
		case r.err == nil && win == nil:
			rr := r
			win = &rr
			cancel() // stop remaining attempts
		case r.err == nil:
			r.l.close("replaced") // lost the race
		case rctx.Err() != nil && win != nil:
			// cancelled because another candidate won: not a failure
		case ctx.Err() == nil:
			p.mgr.Failure(r.c, time.Now(), r.err)
			p.e.log.Warn("connect failed", "peer", p.cfg.Name, "endpoint", r.c.Endpoint, "carrier", r.c.Carrier, "error", r.err)
		}
	}
	if win == nil {
		return false
	}
	p.activate(ctx, win.c, win.l, win.rtt, "")
	return true
}

func (p *peer) activate(ctx context.Context, c *failover.Candidate, l *link, rtt time.Duration, from string) {
	p.mgr.Connected(c, time.Now(), rtt)
	if from != "" {
		p.e.log.Info("switching carrier", "peer", p.cfg.Name, "from", from, "to", c.Key())
	}
	p.install(l)
	p.e.log.Info("tunnel established", "peer", p.cfg.Name, "endpoint", c.Endpoint, "carrier", c.Carrier, "handshake_rtt", rtt.Round(time.Microsecond))
	p.e.wg.Add(1)
	go l.healthLoop(ctx)
}

func (p *peer) probeAndDecide(ctx context.Context) {
	now := time.Now()
	for _, c := range p.mgr.ProbeTargets(now) {
		r := p.e.probe(ctx, p, c, p.cands[c], 3, 0)
		p.mgr.ProbeResult(c, r.OK, r.RTT, time.Now(), r.err)
		p.e.log.Debug("probe", "peer", p.cfg.Name, "candidate", c.Key(), "ok", r.OK, "rtt", r.RTT)
	}
	if c, why := p.mgr.Decide(time.Now()); c != nil {
		from := ""
		if a := p.activeLink(); a != nil && a.cand != nil {
			from = a.cand.Key()
		}
		p.e.log.Info("failover decision", "peer", p.cfg.Name, "to", c.Key(), "reason", why)
		_ = p.connect(ctx, c, from)
	}
}

package engine

import (
	"context"
	"time"

	"github.com/salehsayyadi/tuunel/internal/failover"
	"github.com/salehsayyadi/tuunel/internal/session"
)

// ProbeReport is the measured result of a carrier test.
type ProbeReport struct {
	Peer          string        `json:"peer"`
	Endpoint      string        `json:"endpoint"`
	Carrier       string        `json:"carrier"`
	Address       string        `json:"address"`
	OK            bool          `json:"ok"`
	Reachable     bool          `json:"reachable"`
	Handshake     bool          `json:"handshake"`
	HandshakeRTT  time.Duration `json:"handshake_rtt_ns"`
	RTT           time.Duration `json:"rtt_ns"`
	Jitter        time.Duration `json:"jitter_ns"`
	LossPct       float64       `json:"loss_pct"`
	ThroughputBps float64       `json:"throughput_bps,omitempty"`
	Error         string        `json:"error,omitempty"`
	err           error
}

// probe opens a probe-flagged session (which never carries traffic or
// replaces the active link), measures RTT/loss/jitter with `pings` echo
// requests and optionally echo throughput with `bulk` padded pings.
func (e *Engine) probe(ctx context.Context, p *peer, c *failover.Candidate, cc CandidateConfig, pings, bulk int) ProbeReport {
	r := ProbeReport{Peer: p.cfg.Name, Endpoint: c.Endpoint, Carrier: c.Carrier, Address: c.Address}
	l, hs, err := e.dial(ctx, p, c, cc, true)
	if err != nil {
		r.Error, r.err = err.Error(), err
		return r
	}
	defer l.close("probe finished")
	r.Reachable, r.Handshake, r.HandshakeRTT = true, true, hs
	for i := 0; i < pings; i++ {
		_ = l.sendPing(0)
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return r
		}
	}
	deadline := time.Now().Add(e.cfg.Health.PingTimeout)
	for time.Now().Before(deadline) {
		if m := l.tracker.Snapshot(); m.Received >= uint64(pings+1) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.tracker.Expire(time.Now().Add(e.cfg.Health.PingTimeout))
	m := l.tracker.Snapshot()
	r.RTT, r.Jitter = m.AvgRTT, m.Jitter
	if m.Sent > 0 {
		r.LossPct = 100 * float64(m.Sent-m.Received) / float64(m.Sent)
	}
	r.OK = m.Received > 0
	if !r.OK {
		r.Error = "handshake succeeded but no echo replies (path unreliable)"
		r.err = errString(r.Error)
	}
	if bulk > 0 && r.OK {
		r.ThroughputBps = e.bulkEcho(l, bulk)
	}
	return r
}

// bulkEcho sends n padded pings (paced in small bursts) and measures the
// echoed payload rate. The result is a round-trip goodput estimate.
func (e *Engine) bulkEcho(l *link, n int) float64 {
	before := l.pongs.Load()
	pad := maxPingBody - 8
	if max := l.limit - session.Overhead - 8; max < pad {
		pad = max
	}
	if pad < 0 {
		pad = 0
	}
	body := make([]byte, 8+pad)
	start := time.Now()
	for i := 0; i < n; i++ {
		body[0] = 0xff // ids outside the health tracker's range
		if err := l.send(session.InnerPing, body); err != nil {
			break
		}
		if i >= 64 && i%32 == 31 { // avoid overrunning datagram socket buffers
			for l.pongs.Load()-before < uint64(i-64) && time.Since(start) < 5*time.Second {
				time.Sleep(time.Millisecond)
			}
		}
	}
	deadline := start.Add(5 * time.Second)
	for time.Now().Before(deadline) && l.pongs.Load()-before < uint64(n) {
		time.Sleep(2 * time.Millisecond)
	}
	got := l.pongs.Load() - before
	el := time.Since(start).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(got) * float64(len(body)) * 8 / el
}

// TestCarriers probes every configured candidate of every peer.
func (e *Engine) TestCarriers(ctx context.Context, bulk int) []ProbeReport {
	var out []ProbeReport
	for _, p := range e.peers {
		for _, c := range p.mgr.Ranked() {
			r := e.probe(ctx, p, c, p.cands[c], 5, bulk)
			if a := p.activeLink(); a != nil && a.cand == c && !r.OK {
				// do not penalise the active link on a failed side probe
			} else {
				p.mgr.ProbeResult(c, r.OK, r.RTT, time.Now(), r.err)
			}
			out = append(out, r)
		}
	}
	return out
}

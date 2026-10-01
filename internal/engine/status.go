package engine

import (
	"errors"
	"time"

	"github.com/salehsayyadi/tuunel/internal/health"
)

type ListenerStatus struct {
	Carrier   string `json:"carrier"`
	Address   string `json:"address"`
	Listening bool   `json:"listening"`
	Error     string `json:"error,omitempty"`
}

type CandidateStatus struct {
	Endpoint  string        `json:"endpoint"`
	Carrier   string        `json:"carrier"`
	Address   string        `json:"address"`
	State     health.State  `json:"state"`
	Failures  int           `json:"failures"`
	LastRTT   time.Duration `json:"last_rtt_ns"`
	LastError string        `json:"last_error,omitempty"`
	RetryIn   time.Duration `json:"retry_in_ns"`
	Parked    bool          `json:"parked"`
	Active    bool          `json:"active"`
}

type PeerStatus struct {
	Name          string            `json:"name"`
	Up            bool              `json:"up"`
	Role          string            `json:"role"`
	Carrier       string            `json:"carrier,omitempty"`
	Endpoint      string            `json:"endpoint,omitempty"`
	Remote        string            `json:"remote,omitempty"`
	LinkMTU       int               `json:"link_mtu,omitempty"`
	Health        health.Metrics    `json:"health"`
	TxBytes       uint64            `json:"tx_bytes"`
	RxBytes       uint64            `json:"rx_bytes"`
	TxPackets     uint64            `json:"tx_packets"`
	RxPackets     uint64            `json:"rx_packets"`
	Drops         uint64            `json:"drops"`
	Reconnects    uint64            `json:"reconnects"`
	CarrierSwitch int               `json:"carrier_switches"`
	Uptime        time.Duration     `json:"uptime_ns"`
	AllowedIPs    []string          `json:"allowed_ips"`
	Candidates    []CandidateStatus `json:"candidates,omitempty"`
}

type Status struct {
	NodeID    string            `json:"node_id"`
	MTU       int               `json:"mtu"`
	Uptime    time.Duration     `json:"uptime_ns"`
	Peers     []PeerStatus      `json:"peers"`
	Listeners []ListenerStatus  `json:"listeners"`
	Drops     map[string]uint64 `json:"drops"`
}

func (e *Engine) Status() Status {
	e.stMu.Lock()
	started, lns := e.started, append([]ListenerStatus(nil), e.lnStatus...)
	e.stMu.Unlock()
	s := Status{NodeID: e.cfg.NodeID, MTU: e.cfg.MTU, Uptime: time.Since(started), Listeners: lns,
		Drops: map[string]uint64{
			"no_route": e.counters.NoRoute.Load(), "malformed": e.counters.Malformed.Load(),
			"auth_failures": e.counters.AuthFailures.Load(), "spoofed_source": e.counters.Spoofed.Load(),
			"too_big": e.counters.TooBig.Load(), "fragmented": e.counters.Fragmented.Load(),
		}}
	now := time.Now()
	for _, p := range e.peers {
		ps := PeerStatus{Name: p.cfg.Name, Role: "responder", TxBytes: p.stats.txBytes.Load(), RxBytes: p.stats.rxBytes.Load(),
			TxPackets: p.stats.txPackets.Load(), RxPackets: p.stats.rxPackets.Load(), Drops: p.stats.drops.Load(), Reconnects: p.stats.reconnects.Load()}
		if len(p.cands) > 0 {
			ps.Role = "initiator"
		}
		for _, a := range p.cfg.AllowedIPs {
			ps.AllowedIPs = append(ps.AllowedIPs, a.String())
		}
		p.mu.Lock()
		l, up := p.active, p.upSince
		p.mu.Unlock()
		if l != nil {
			ps.Up = true
			ps.Carrier = l.carrier.Name()
			ps.Endpoint = l.endpoint
			ps.Remote = addrString(l.conn)
			ps.LinkMTU = l.limit
			ps.Health = l.tracker.Snapshot()
			ps.Uptime = now.Sub(up)
		}
		snap := p.mgr.Snapshot()
		ps.CarrierSwitch = snap.Switches
		for _, c := range snap.All {
			cs := CandidateStatus{Endpoint: c.Endpoint, Carrier: c.Carrier, Address: c.Address, State: c.State, Failures: c.Failures,
				LastRTT: c.LastRTT, LastError: c.LastError, Parked: c.Parked, Active: snap.Active != nil && snap.Active.Key() == c.Key() && l != nil}
			if c.NextAttempt.After(now) {
				cs.RetryIn = c.NextAttempt.Sub(now)
			}
			ps.Candidates = append(ps.Candidates, cs)
		}
		s.Peers = append(s.Peers, ps)
	}
	return s
}

// Reconnect forces peers (all when name is empty) to re-establish their link.
func (e *Engine) Reconnect(name string) int {
	n := 0
	for _, p := range e.peers {
		if name == "" || p.cfg.Name == name {
			p.reconnect()
			n++
		}
	}
	return n
}

// PingResult is one authenticated in-tunnel echo.
type PingResult struct {
	Seq     int           `json:"seq"`
	RTT     time.Duration `json:"rtt_ns"`
	Error   string        `json:"error,omitempty"`
	Carrier string        `json:"carrier"`
}

// Ping sends count authenticated echo requests over the peer's active link.
func (e *Engine) Ping(name string, count int, interval time.Duration) ([]PingResult, error) {
	var p *peer
	for _, x := range e.peers {
		if name == "" || x.cfg.Name == name {
			p = x
			break
		}
	}
	if p == nil {
		return nil, errors.New("unknown peer")
	}
	if count <= 0 || count > 100 {
		count = 4
	}
	var out []PingResult
	for i := 0; i < count; i++ {
		l := p.activeLink()
		r := PingResult{Seq: i + 1}
		if l == nil {
			r.Error = "tunnel down"
		} else {
			r.Carrier = l.carrier.Name()
			d, err := l.pingWait(e.cfg.Health.PingTimeout)
			r.RTT = d
			if err != nil {
				r.Error = err.Error()
			}
		}
		out = append(out, r)
		if i < count-1 {
			time.Sleep(interval)
		}
	}
	return out, nil
}

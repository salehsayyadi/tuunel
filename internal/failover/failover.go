// Package failover selects which (endpoint, carrier) candidate carries a
// peer's tunnel. It is pure decision logic with an injectable clock so every
// policy (backoff, retry limits, cooldown, hysteresis, preemption) is unit
// tested without a network.
package failover

import (
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/salehsayyadi/tuunel/internal/health"
)

type Policy struct {
	Order             string        // "endpoint" (default): try every carrier of the best endpoint first; "carrier": best carrier across endpoints first
	EndpointSelection string        // "priority" (default) or "latency"
	BackoffInitial    time.Duration // first retry delay after a failure
	BackoffMax        time.Duration
	MaxRetries        int           // consecutive failures before a candidate is parked (0 = never park)
	Cooldown          time.Duration // how long a parked candidate is skipped
	MinHold           time.Duration // minimum time on a link before a voluntary switch
	RecoverySuccesses int           // consecutive successful probes for RECOVERING -> AVAILABLE
	Preempt           bool          // return to a better-ranked candidate once it is AVAILABLE
	SwitchOnDegraded  bool          // leave a DEGRADED link for an AVAILABLE alternative
	// DegradeHoldoff damps flapping between candidates that suffer the same
	// (environmental) loss: a candidate left because it was DEGRADED is not
	// chosen again voluntarily (preemption or degraded-switch) for this long;
	// the hold doubles for each further degrade departure within
	// degradeStreakWindow (capped at 32x). Hard failures are never damped.
	DegradeHoldoff time.Duration
}

const degradeStreakWindow = 10 * time.Minute

func DefaultPolicy() Policy {
	return Policy{Order: "endpoint", EndpointSelection: "priority", BackoffInitial: time.Second, BackoffMax: time.Minute,
		MaxRetries: 0, Cooldown: 5 * time.Minute, MinHold: 30 * time.Second, RecoverySuccesses: 3, Preempt: true, SwitchOnDegraded: true,
		DegradeHoldoff: time.Minute}
}

type Candidate struct {
	Endpoint     string
	Carrier      string
	Address      string
	EndpointRank int
	CarrierRank  int

	State          health.State
	Failures       int
	NextAttempt    time.Time
	LastError      string
	LastRTT        time.Duration
	LastProbe      time.Time
	ProbeOK        int
	AvailableSince time.Time
	Parked         bool
	// HoldUntil: not eligible for voluntary switches before this time
	// (set when the candidate was left because it was DEGRADED).
	HoldUntil      time.Time
	degradeStreak  int
	lastDegradeOut time.Time
}

// Key identifies a candidate in logs and APIs.
func (c *Candidate) Key() string { return c.Endpoint + "/" + c.Carrier }

type Manager struct {
	mu          sync.Mutex
	p           Policy
	c           []*Candidate
	active      *Candidate
	activeSince time.Time
	switches    int
	// endpointSwitches counts switches that changed the endpoint;
	// failureSwitches counts switches after the active candidate failed
	// (as opposed to preemption or a degraded-path switch).
	endpointSwitches, failureSwitches int
	// last is the most recently connected candidate; unlike active it is
	// not cleared on failure, so failure-driven switches are counted too.
	last *Candidate
	rng  *rand.Rand
}

func New(p Policy, cands []*Candidate) *Manager {
	for _, c := range cands {
		if c.State == "" {
			c.State = health.Unknown
		}
	}
	return &Manager{p: p, c: cands, rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))}
}

// endpointRTT returns the best recent RTT measured for an endpoint.
func (m *Manager) endpointRTT(ep string) time.Duration {
	best := time.Duration(0)
	for _, c := range m.c {
		if c.Endpoint == ep && c.LastRTT > 0 && c.State != health.Failed && (best == 0 || c.LastRTT < best) {
			best = c.LastRTT
		}
	}
	return best
}

// rankedLocked returns candidates in preference order.
func (m *Manager) rankedLocked() []*Candidate {
	out := append([]*Candidate(nil), m.c...)
	epKey := func(c *Candidate) int64 {
		if m.p.EndpointSelection == "latency" {
			if r := m.endpointRTT(c.Endpoint); r > 0 {
				return int64(r)
			}
			return int64(time.Hour) + int64(c.EndpointRank) // unmeasured last, by priority
		}
		return int64(c.EndpointRank)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ea, eb := epKey(a), epKey(b)
		if m.p.Order == "carrier" {
			if a.CarrierRank != b.CarrierRank {
				return a.CarrierRank < b.CarrierRank
			}
			return ea < eb
		}
		if ea != eb {
			return ea < eb
		}
		return a.CarrierRank < b.CarrierRank
	})
	return out
}

func (m *Manager) Ranked() []*Candidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rankedLocked()
}

// Pick returns the best candidate that may be dialled now, or the time to
// wait until one becomes eligible.
func (m *Manager) Pick(now time.Time) (*Candidate, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var earliest time.Time
	for _, c := range m.rankedLocked() {
		if c.Parked && now.After(c.NextAttempt) {
			c.Parked, c.Failures = false, 0
		}
		if !now.Before(c.NextAttempt) {
			return c, 0
		}
		if earliest.IsZero() || c.NextAttempt.Before(earliest) {
			earliest = c.NextAttempt
		}
	}
	if earliest.IsZero() {
		return nil, time.Second
	}
	return nil, earliest.Sub(now)
}

// Eligible returns every candidate that may be dialled now, in preference
// order, or the time until the next one becomes eligible.
func (m *Manager) Eligible(now time.Time) ([]*Candidate, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Candidate
	var earliest time.Time
	for _, c := range m.rankedLocked() {
		if c.Parked && now.After(c.NextAttempt) {
			c.Parked, c.Failures = false, 0
		}
		if !now.Before(c.NextAttempt) {
			out = append(out, c)
		} else if earliest.IsZero() || c.NextAttempt.Before(earliest) {
			earliest = c.NextAttempt
		}
	}
	if len(out) > 0 {
		return out, 0
	}
	if earliest.IsZero() {
		return nil, time.Second
	}
	return nil, earliest.Sub(now)
}

func (m *Manager) backoff(failures int) time.Duration {
	d := m.p.BackoffInitial
	for i := 1; i < failures && d < m.p.BackoffMax; i++ {
		d *= 2
	}
	if d > m.p.BackoffMax {
		d = m.p.BackoffMax
	}
	// +/-20% jitter prevents synchronized reconnect storms.
	j := time.Duration(float64(d) * (0.8 + 0.4*m.rng.Float64()))
	return j
}

// Failure records a dial/handshake/link failure for c.
func (m *Manager) Failure(c *Candidate, now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.Failures++
	c.State = health.Failed
	c.ProbeOK = 0
	c.AvailableSince = time.Time{}
	if err != nil {
		c.LastError = err.Error()
	}
	c.NextAttempt = now.Add(m.backoff(c.Failures))
	if m.p.MaxRetries > 0 && c.Failures >= m.p.MaxRetries {
		c.Parked = true
		c.NextAttempt = now.Add(m.p.Cooldown)
	}
	if m.active == c {
		m.active = nil
	}
}

// Connected marks c as the active candidate.
func (m *Manager) Connected(c *Candidate, now time.Time, rtt time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.active; a != nil && a != c && a.State == health.Degraded && m.p.DegradeHoldoff > 0 {
		// voluntary departure from a degraded candidate: hold it down
		if now.Sub(a.lastDegradeOut) < degradeStreakWindow {
			if a.degradeStreak < 5 {
				a.degradeStreak++
			}
		} else {
			a.degradeStreak = 0
		}
		a.lastDegradeOut = now
		a.HoldUntil = now.Add(m.p.DegradeHoldoff << a.degradeStreak)
	}
	if m.last != nil && m.last != c {
		m.switches++
		if m.last.Endpoint != c.Endpoint {
			m.endpointSwitches++
		}
		if m.active == nil { // Failure() cleared it: failure-triggered switch
			m.failureSwitches++
		}
	}
	m.active, m.activeSince, m.last = c, now, c
	c.Failures, c.Parked, c.LastError = 0, false, ""
	c.NextAttempt = time.Time{}
	if rtt > 0 {
		c.LastRTT = rtt
	}
	if c.State != health.Available {
		c.State = health.Available
		c.AvailableSince = now
	}
}

// ActiveState updates the health of the active candidate.
func (m *Manager) ActiveState(s health.State, rtt time.Duration, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return
	}
	if rtt > 0 {
		m.active.LastRTT = rtt
	}
	if s != m.active.State {
		m.active.State = s
		if s == health.Available {
			m.active.AvailableSince = now
		}
	}
}

// ProbeResult records the outcome of a background probe of a non-active candidate.
func (m *Manager) ProbeResult(c *Candidate, ok bool, rtt time.Duration, now time.Time, err error) {
	if !ok {
		m.Failure(c, now, err)
		m.mu.Lock()
		c.LastProbe = now
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c.LastProbe = now
	c.LastRTT = rtt
	c.ProbeOK++
	c.Failures, c.Parked = 0, false
	c.NextAttempt = time.Time{}
	switch {
	case c.ProbeOK >= m.p.RecoverySuccesses:
		if c.State != health.Available {
			c.State = health.Available
			c.AvailableSince = now
		}
	case c.State == health.Failed || c.State == health.Unknown || c.State == health.Recovering:
		c.State = health.Recovering
	}
}

// Decide returns a candidate to switch to voluntarily (make-before-break),
// or nil. It never proposes a switch within MinHold of the last switch.
func (m *Manager) Decide(now time.Time) (*Candidate, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.active
	if a == nil || now.Sub(m.activeSince) < m.p.MinHold {
		return nil, ""
	}
	ranked := m.rankedLocked()
	if m.p.Preempt {
		for _, c := range ranked {
			if c == a {
				break
			}
			if c.State == health.Available && !c.Parked && !now.Before(c.HoldUntil) {
				return c, "preempt: better-ranked candidate recovered"
			}
		}
	}
	if m.p.SwitchOnDegraded && a.State == health.Degraded {
		for _, c := range ranked {
			if c != a && c.State == health.Available && !c.Parked && !now.Before(c.HoldUntil) {
				return c, "active carrier degraded"
			}
		}
	}
	return nil, ""
}

// ProbeTargets lists non-active candidates worth probing now: those ranked
// above the active one (for preemption) and, when the active link is
// degraded, all others. Candidates in backoff are skipped.
func (m *Manager) ProbeTargets(now time.Time) []*Candidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Candidate
	above := m.active != nil
	for _, c := range m.rankedLocked() {
		if c == m.active {
			above = false
			continue
		}
		if now.Before(c.NextAttempt) {
			continue
		}
		degraded := m.active != nil && m.active.State == health.Degraded && m.p.SwitchOnDegraded
		if (above && m.p.Preempt) || degraded {
			if c.State != health.Available || now.Sub(c.LastProbe) > 30*time.Second {
				out = append(out, c)
			}
		}
	}
	return out
}

type Snapshot struct {
	Active   *Candidate
	Since    time.Time
	Switches int
	// EndpointSwitches and FailureSwitches are subsets of Switches.
	EndpointSwitches int
	FailureSwitches  int
	All              []Candidate
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Since: m.activeSince, Switches: m.switches, EndpointSwitches: m.endpointSwitches, FailureSwitches: m.failureSwitches}
	if m.active != nil {
		cp := *m.active
		s.Active = &cp
	}
	for _, c := range m.rankedLocked() {
		s.All = append(s.All, *c)
	}
	return s
}

// Reset clears backoff state so an administrator-triggered reconnect is immediate.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.c {
		c.NextAttempt, c.Parked, c.Failures = time.Time{}, false, 0
	}
}

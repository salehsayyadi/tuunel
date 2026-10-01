package failover

import (
	"errors"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/health"
)

func cands() []*Candidate {
	var out []*Candidate
	for ei, ep := range []string{"germany", "netherlands"} {
		for ci, c := range []string{"tcp", "quic", "wss"} {
			out = append(out, &Candidate{Endpoint: ep, Carrier: c, EndpointRank: ei, CarrierRank: ci})
		}
	}
	return out
}

var errX = errors.New("x")

func TestOrderEndpointFirst(t *testing.T) {
	m := New(DefaultPolicy(), cands())
	r := m.Ranked()
	if r[0].Key() != "germany/tcp" || r[2].Key() != "germany/wss" || r[3].Key() != "netherlands/tcp" {
		t.Fatalf("order: %s %s %s", r[0].Key(), r[2].Key(), r[3].Key())
	}
	p := DefaultPolicy()
	p.Order = "carrier"
	r = New(p, cands()).Ranked()
	if r[1].Key() != "netherlands/tcp" {
		t.Fatalf("carrier order: %s", r[1].Key())
	}
}

func TestFailoverChainAndBackoff(t *testing.T) {
	m := New(DefaultPolicy(), cands())
	now := time.Unix(1000, 0)
	var seen []string
	for i := 0; i < 6; i++ {
		c, wait := m.Pick(now)
		if c == nil {
			t.Fatalf("no candidate at step %d (wait %v)", i, wait)
		}
		seen = append(seen, c.Key())
		m.Failure(c, now, errX)
	}
	if seen[0] != "germany/tcp" || seen[1] != "germany/quic" || seen[3] != "netherlands/tcp" {
		t.Fatalf("chain %v", seen)
	}
	c, wait := m.Pick(now)
	if c != nil || wait <= 0 || wait > 2*time.Second {
		t.Fatalf("expected backoff wait, got %v %v", c, wait)
	}
	// repeated failures back off exponentially up to the max
	first := m.Ranked()[0]
	for i := 0; i < 10; i++ {
		m.Failure(first, now, errX)
	}
	if d := first.NextAttempt.Sub(now); d < 48*time.Second || d > 72*time.Second {
		t.Fatalf("backoff not capped near 60s: %v", d)
	}
}

func TestRetryLimitParksCandidate(t *testing.T) {
	p := DefaultPolicy()
	p.MaxRetries = 2
	m := New(p, cands()[:1])
	now := time.Unix(1000, 0)
	c, _ := m.Pick(now)
	m.Failure(c, now, errX)
	m.Failure(c, now, errX)
	if !c.Parked || c.NextAttempt.Sub(now) != p.Cooldown {
		t.Fatal("not parked for cooldown")
	}
	if got, _ := m.Pick(now.Add(p.Cooldown + time.Second)); got != c || c.Parked {
		t.Fatal("not released after cooldown")
	}
}

func TestPreemptWithHysteresis(t *testing.T) {
	m := New(DefaultPolicy(), cands())
	now := time.Unix(1000, 0)
	tcp, _ := m.Pick(now)
	m.Failure(tcp, now, errX)
	quic, _ := m.Pick(now)
	m.Connected(quic, now, 50*time.Millisecond)
	// tcp recovers after one probe -> RECOVERING, no switch yet
	m.ProbeResult(tcp, true, 10*time.Millisecond, now.Add(40*time.Second), nil)
	if tcp.State != health.Recovering {
		t.Fatalf("state %s", tcp.State)
	}
	if c, _ := m.Decide(now.Add(40 * time.Second)); c != nil {
		t.Fatal("switched on a single probe")
	}
	m.ProbeResult(tcp, true, 10*time.Millisecond, now.Add(41*time.Second), nil)
	m.ProbeResult(tcp, true, 10*time.Millisecond, now.Add(42*time.Second), nil)
	if c, _ := m.Decide(now.Add(10 * time.Second)); c != nil {
		t.Fatal("switched within MinHold")
	}
	if c, why := m.Decide(now.Add(43 * time.Second)); c != tcp {
		t.Fatalf("no preempt: %v %s", c, why)
	}
}

func TestSwitchOnDegraded(t *testing.T) {
	p := DefaultPolicy()
	p.Preempt = false
	m := New(p, cands())
	now := time.Unix(1000, 0)
	tcp, _ := m.Pick(now)
	m.Connected(tcp, now, 0)
	if len(m.ProbeTargets(now)) != 0 {
		t.Fatal("probing without reason")
	}
	m.ActiveState(health.Degraded, 0, now.Add(time.Minute))
	targets := m.ProbeTargets(now.Add(time.Minute))
	if len(targets) != 5 {
		t.Fatalf("targets %d", len(targets))
	}
	for i := 0; i < 3; i++ {
		m.ProbeResult(targets[1], true, time.Millisecond, now.Add(time.Minute), nil)
	}
	if c, _ := m.Decide(now.Add(time.Minute)); c != targets[1] {
		t.Fatal("did not leave degraded link")
	}
}

func TestLatencySelection(t *testing.T) {
	p := DefaultPolicy()
	p.EndpointSelection = "latency"
	cs := cands()
	cs[0].LastRTT = 200 * time.Millisecond // germany
	cs[3].LastRTT = 20 * time.Millisecond  // netherlands
	if r := New(p, cs).Ranked(); r[0].Endpoint != "netherlands" {
		t.Fatalf("latency policy picked %s", r[0].Key())
	}
}

func TestEligible(t *testing.T) {
	m := New(DefaultPolicy(), cands())
	now := time.Unix(1000, 0)
	all, _ := m.Eligible(now)
	if len(all) != 6 {
		t.Fatal(len(all))
	}
	m.Failure(all[0], now, errX)
	rest, _ := m.Eligible(now)
	if len(rest) != 5 || rest[0].Key() != "germany/quic" {
		t.Fatalf("%d %s", len(rest), rest[0].Key())
	}
}

func TestSwitchesCountFailureDrivenSwitch(t *testing.T) {
	cs := cands()
	m := New(DefaultPolicy(), cs)
	now := time.Now()
	m.Connected(cs[0], now, 0)
	m.Failure(cs[0], now, errX)
	m.Connected(cs[1], now, 0)
	m.Connected(cs[1], now, 0)
	if got := m.Snapshot().Switches; got != 1 {
		t.Fatalf("switches = %d, want 1", got)
	}
	if got := m.Snapshot().FailureSwitches; got != 1 {
		t.Fatalf("failure switches = %d, want 1", got)
	}
	// a preemptive switch back is a switch but not a failure switch
	m.Connected(cs[0], now, 0)
	if s := m.Snapshot(); s.Switches != 2 || s.FailureSwitches != 1 {
		t.Fatalf("after preempt: switches=%d failure=%d", s.Switches, s.FailureSwitches)
	}
}

// Anti-flap: a candidate left while DEGRADED is held down (no preemption back
// to it) for DegradeHoldoff, doubling on repeated degrade departures.
func TestDegradeHoldoffDampsFlapping(t *testing.T) {
	p := DefaultPolicy()
	p.MinHold, p.DegradeHoldoff, p.RecoverySuccesses = time.Second, time.Minute, 1
	a := &Candidate{Endpoint: "e", Carrier: "udp", CarrierRank: 0}
	b := &Candidate{Endpoint: "e", Carrier: "quic", CarrierRank: 1}
	m := New(p, []*Candidate{a, b})
	now := time.Unix(1000, 0)
	m.Connected(a, now, time.Millisecond)
	now = now.Add(2 * time.Second)
	m.ActiveState(health.Degraded, time.Millisecond, now)
	m.ProbeResult(b, true, time.Millisecond, now, nil)
	if c, why := m.Decide(now); c != b {
		t.Fatalf("want degraded switch to b, got %v %q", c, why)
	}
	m.Connected(b, now, time.Millisecond) // left a while DEGRADED
	if !a.HoldUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("hold not set: %v", a.HoldUntil)
	}
	m.ProbeResult(a, true, time.Millisecond, now.Add(5*time.Second), nil)
	if c, _ := m.Decide(now.Add(10 * time.Second)); c != nil {
		t.Fatalf("preempted back to held candidate %s", c.Key())
	}
	if c, _ := m.Decide(now.Add(61 * time.Second)); c != a {
		t.Fatalf("preempt after hold expected, got %v", c)
	}
	// second degrade departure within the streak window doubles the hold
	now = now.Add(61 * time.Second)
	m.Connected(a, now, time.Millisecond)
	now = now.Add(2 * time.Second)
	m.ActiveState(health.Degraded, time.Millisecond, now)
	m.ProbeResult(b, true, time.Millisecond, now, nil)
	m.Connected(b, now, time.Millisecond)
	if got := a.HoldUntil.Sub(now); got != 2*time.Minute {
		t.Fatalf("hold should double to 2m, got %v", got)
	}
	// a hard failure of the active candidate is never damped
	m.Failure(b, now.Add(time.Second), nil)
	if c, _ := m.Pick(now.Add(time.Second)); c == nil {
		t.Fatal("no candidate after failure")
	}
}

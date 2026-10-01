// Package health measures link quality from authenticated in-tunnel pings and
// classifies it with a configurable state machine.
package health

import (
	"sync"
	"time"
)

type State string

const (
	Unknown    State = "UNKNOWN"
	Available  State = "AVAILABLE"
	Degraded   State = "DEGRADED"
	Failed     State = "FAILED"
	Recovering State = "RECOVERING"
)

// Thresholds are all configurable; the defaults are documented in
// ARCHITECTURE.md and chosen to be conservative, not tuned to any network.
type Thresholds struct {
	Window         int           // number of recent probes used for loss/jitter
	PingTimeout    time.Duration // a ping without pong after this is lost
	DegradedLoss   float64       // percent
	FailedLoss     float64       // percent
	DegradedRTT    time.Duration // 0 disables
	DegradedJitter time.Duration // 0 disables
	FailedMissed   int           // consecutive lost probes => FAILED
	// ClearRatio is the hysteresis factor: a DEGRADED link returns to
	// AVAILABLE only when every metric is below threshold*ClearRatio.
	ClearRatio float64
	MinSamples int // samples required before loss-based decisions
}

func DefaultThresholds() Thresholds {
	return Thresholds{Window: 20, PingTimeout: 2 * time.Second, DegradedLoss: 5, FailedLoss: 50,
		FailedMissed: 4, ClearRatio: 0.5, MinSamples: 5}
}

type sample struct {
	lost bool
	rtt  time.Duration
}

// Metrics is a point-in-time snapshot.
type Metrics struct {
	State      State         `json:"state"`
	RTT        time.Duration `json:"rtt_ns"`
	AvgRTT     time.Duration `json:"avg_rtt_ns"`
	Jitter     time.Duration `json:"jitter_ns"`
	LossPct    float64       `json:"loss_pct"`
	Samples    int           `json:"samples"`
	Sent       uint64        `json:"probes_sent"`
	Received   uint64        `json:"probes_received"`
	StateSince time.Time     `json:"state_since"`
}

// Tracker is safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	th        Thresholds
	pending   map[uint64]time.Time
	samples   []sample
	next      int
	full      bool
	lastRTT   time.Duration
	jitter    float64 // RFC 3550 smoothed, nanoseconds
	haveRTT   bool
	missed    int
	sent, got uint64
	state     State
	since     time.Time
}

func NewTracker(th Thresholds) *Tracker {
	if th.Window <= 0 {
		th.Window = 20
	}
	return &Tracker{th: th, pending: make(map[uint64]time.Time), samples: make([]sample, th.Window), state: Unknown, since: time.Now()}
}

func (t *Tracker) OnSent(id uint64, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) > 4*t.th.Window { // bound memory if the peer never answers
		t.expireLocked(at.Add(t.th.PingTimeout + time.Hour))
	}
	t.pending[id] = at
	t.sent++
}

// OnPong records a reply and returns the measured RTT.
func (t *Tracker) OnPong(id uint64, at time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sent, ok := t.pending[id]
	if !ok {
		return 0, false
	}
	delete(t.pending, id)
	rtt := at.Sub(sent)
	if rtt < 0 {
		rtt = 0
	}
	t.got++
	if t.haveRTT {
		d := float64(rtt - t.lastRTT)
		if d < 0 {
			d = -d
		}
		t.jitter += (d - t.jitter) / 16
	}
	t.lastRTT, t.haveRTT = rtt, true
	t.missed = 0
	t.push(sample{rtt: rtt})
	return rtt, true
}

// Expire marks pings older than PingTimeout as lost.
func (t *Tracker) Expire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked(now)
}

func (t *Tracker) expireLocked(now time.Time) {
	for id, at := range t.pending {
		if now.Sub(at) >= t.th.PingTimeout {
			delete(t.pending, id)
			t.missed++
			t.push(sample{lost: true})
		}
	}
}

func (t *Tracker) push(s sample) {
	t.samples[t.next] = s
	t.next = (t.next + 1) % len(t.samples)
	if t.next == 0 {
		t.full = true
	}
}

// ForceFailed marks the link failed (for example after a carrier error).
func (t *Tracker) ForceFailed(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(Failed, now)
}

func (t *Tracker) setLocked(s State, now time.Time) {
	if s != t.state {
		t.state, t.since = s, now
	}
}

func (t *Tracker) statsLocked() (loss float64, avg time.Duration, n int) {
	count := t.next
	if t.full {
		count = len(t.samples)
	}
	var lost, ok int
	var sum time.Duration
	for i := 0; i < count; i++ {
		s := t.samples[i]
		if s.lost {
			lost++
		} else {
			ok++
			sum += s.rtt
		}
	}
	if count > 0 {
		loss = float64(lost) * 100 / float64(count)
	}
	if ok > 0 {
		avg = sum / time.Duration(ok)
	}
	return loss, avg, count
}

// Evaluate applies the state machine and returns the new state.
func (t *Tracker) Evaluate(now time.Time) State {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == Failed {
		return Failed // failure is terminal for a link; the manager reconnects
	}
	loss, avg, n := t.statsLocked()
	jitter := time.Duration(t.jitter)
	enough := n >= t.th.MinSamples
	if t.th.FailedMissed > 0 && t.missed >= t.th.FailedMissed || enough && loss >= t.th.FailedLoss {
		t.setLocked(Failed, now)
		return t.state
	}
	// Statistical floor: a single lost probe in the window is noise at low
	// loss rates (1 of 20 = 5%) and made the tunnel flap between carriers
	// with the same environmental loss (tests/lab/soak.py). Entering
	// DEGRADED by loss needs >= 2 lost probes; staying needs >= 1.
	lost := int(loss*float64(n)/100 + 0.5)
	bad := func(r float64) bool {
		minLost := 1
		if r >= 1 {
			minLost = 2
		}
		return enough && loss >= t.th.DegradedLoss*r && lost >= minLost ||
			t.th.DegradedRTT > 0 && t.haveRTT && float64(avg) >= float64(t.th.DegradedRTT)*r ||
			t.th.DegradedJitter > 0 && float64(jitter) >= float64(t.th.DegradedJitter)*r
	}
	switch {
	case bad(1):
		t.setLocked(Degraded, now)
	case t.state == Degraded && bad(t.th.ClearRatio):
		// hysteresis: stay degraded until clearly healthy
	case t.got > 0 || t.state == Unknown && t.sent == 0:
		if t.got > 0 {
			t.setLocked(Available, now)
		}
	}
	return t.state
}

func (t *Tracker) Snapshot() Metrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	loss, avg, n := t.statsLocked()
	return Metrics{State: t.state, RTT: t.lastRTT, AvgRTT: avg, Jitter: time.Duration(t.jitter), LossPct: loss,
		Samples: n, Sent: t.sent, Received: t.got, StateSince: t.since}
}

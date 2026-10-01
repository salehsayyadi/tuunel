package health

import (
	"testing"
	"time"
)

func feed(tr *Tracker, start time.Time, n int, rtt time.Duration, lossEvery int) time.Time {
	now := start
	for i := 0; i < n; i++ {
		id := uint64(now.UnixNano())
		tr.OnSent(id, now)
		if lossEvery == 0 || i%lossEvery != 0 {
			tr.OnPong(id, now.Add(rtt))
		}
		now = now.Add(time.Second)
		tr.Expire(now.Add(3 * time.Second))
		tr.Evaluate(now)
	}
	return now
}

func TestAvailable(t *testing.T) {
	tr := NewTracker(DefaultThresholds())
	feed(tr, time.Unix(1000, 0), 10, 20*time.Millisecond, 0)
	m := tr.Snapshot()
	if m.State != Available || m.RTT != 20*time.Millisecond || m.LossPct != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestDegradedByLossAndHysteresis(t *testing.T) {
	th := DefaultThresholds()
	tr := NewTracker(th)
	now := feed(tr, time.Unix(1000, 0), 20, 10*time.Millisecond, 5) // 20% loss
	if s := tr.Snapshot().State; s != Degraded {
		t.Fatalf("want DEGRADED got %s", s)
	}
	// 3 clean samples: loss still above 5%*0.5 => stays degraded
	now = feed(tr, now, 3, 10*time.Millisecond, 0)
	if s := tr.Snapshot().State; s != Degraded {
		t.Fatalf("hysteresis violated: %s", s)
	}
	feed(tr, now, 20, 10*time.Millisecond, 0)
	if s := tr.Snapshot().State; s != Available {
		t.Fatalf("did not recover: %s", s)
	}
}

func TestFailedByConsecutiveMisses(t *testing.T) {
	tr := NewTracker(DefaultThresholds())
	now := feed(tr, time.Unix(1000, 0), 5, 10*time.Millisecond, 0)
	feed(tr, now, 4, 0, 1)
	if s := tr.Snapshot().State; s != Failed {
		t.Fatalf("want FAILED got %s", s)
	}
}

func TestDegradedByRTTAndJitter(t *testing.T) {
	th := DefaultThresholds()
	th.DegradedRTT = 100 * time.Millisecond
	tr := NewTracker(th)
	feed(tr, time.Unix(1000, 0), 6, 150*time.Millisecond, 0)
	if s := tr.Snapshot().State; s != Degraded {
		t.Fatalf("rtt: %s", s)
	}
	th = DefaultThresholds()
	th.DegradedJitter = 5 * time.Millisecond
	tr = NewTracker(th)
	now := time.Unix(1000, 0)
	for i := 0; i < 30; i++ {
		rtt := 10 * time.Millisecond
		if i%2 == 0 {
			rtt = 60 * time.Millisecond
		}
		tr.OnSent(uint64(i), now)
		tr.OnPong(uint64(i), now.Add(rtt))
		now = now.Add(time.Second)
		tr.Evaluate(now)
	}
	if m := tr.Snapshot(); m.State != Degraded || m.Jitter < 5*time.Millisecond {
		t.Fatalf("jitter: %+v", m)
	}
}

func TestPendingBounded(t *testing.T) {
	tr := NewTracker(DefaultThresholds())
	now := time.Unix(1000, 0)
	for i := 0; i < 10000; i++ {
		tr.OnSent(uint64(i), now)
	}
	if len(tr.pending) > 4*20+1 {
		t.Fatalf("pending unbounded: %d", len(tr.pending))
	}
}

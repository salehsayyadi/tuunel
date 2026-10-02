package session

import "testing"

func TestReplayWindow(t *testing.T) {
	var w ReplayWindow
	for _, n := range []uint64{0, 1, 2, 5, 4, 3} {
		if !w.Update(n) {
			t.Fatalf("fresh %d rejected", n)
		}
	}
	for _, n := range []uint64{0, 3, 5} {
		if w.Update(n) {
			t.Fatalf("duplicate %d accepted", n)
		}
	}
	if !w.Update(100000) {
		t.Fatal("jump rejected")
	}
	if w.Update(100000 - windowSize) {
		t.Fatal("too-old counter accepted")
	}
	if !w.Update(100000 - windowSize + 1) {
		t.Fatal("in-window counter rejected")
	}
	if !w.Update(99999) || w.Update(99999) {
		t.Fatal("window bookkeeping wrong")
	}
}

func TestReplayWindowLargeJumpClearsBits(t *testing.T) {
	var w ReplayWindow
	for n := uint64(0); n < 3000; n++ {
		if !w.Update(n) {
			t.Fatalf("%d", n)
		}
	}
	if !w.Update(1_000_000) {
		t.Fatal("jump")
	}
	// counters just below the new top must be accepted once
	for n := uint64(1_000_000 - 100); n < 1_000_000; n++ {
		if !w.Update(n) {
			t.Fatalf("stale bit for %d", n)
		}
	}
}

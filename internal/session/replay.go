package session

// ReplayWindow implements a sliding-window anti-replay filter (RFC 6479
// style) over 64-bit message counters. It is not safe for concurrent use;
// Session serialises access.
type ReplayWindow struct {
	top    uint64
	bitmap [windowWords]uint64
	seen   bool
}

const (
	windowWords = 32                  // 2048-bit ring
	windowSize  = windowWords*64 - 64 // usable window, one word kept as slack
)

// Check reports whether counter n is acceptable (not replayed and not too old)
// without recording it. Callers must only Update after authentication.
func (w *ReplayWindow) Check(n uint64) bool {
	if !w.seen || n > w.top {
		return true
	}
	if w.top-n >= windowSize {
		return false
	}
	idx := n / 64 % windowWords
	return w.bitmap[idx]&(1<<(n%64)) == 0
}

// Update records counter n. It returns false if n is a replay or too old.
func (w *ReplayWindow) Update(n uint64) bool {
	if !w.Check(n) {
		return false
	}
	if !w.seen || n > w.top {
		cur := w.top / 64
		next := n / 64
		if !w.seen {
			for i := range w.bitmap {
				w.bitmap[i] = 0
			}
		} else {
			diff := next - cur
			if diff > windowWords {
				diff = windowWords
			}
			for i := uint64(1); i <= diff; i++ {
				w.bitmap[(cur+i)%windowWords] = 0
			}
		}
		w.top = n
		w.seen = true
	}
	w.bitmap[n/64%windowWords] |= 1 << (n % 64)
	return true
}

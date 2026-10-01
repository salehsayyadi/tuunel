package carrier

import (
	"net"
	"sync"
	"time"
)

// Limiter is a per-source token bucket used by listeners to resist
// connection floods. It is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter allows `burst` immediate events and `rate` events/second per key.
func NewLimiter(rate, burst float64) *Limiter {
	return &Limiter{rate: rate, burst: burst, buckets: make(map[string]*bucket)}
}

// Allow reports whether an event from addr is permitted now.
func (l *Limiter) Allow(addr net.Addr, now time.Time) bool {
	key := hostOf(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute { // bounded memory: drop idle buckets
		for k, b := range l.buckets {
			if now.Sub(b.seen) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	if len(l.buckets) > 65536 {
		return false
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

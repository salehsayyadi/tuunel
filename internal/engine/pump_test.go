package engine

import (
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

// floodConn returns a message on every read until closed, like an
// unauthenticated sender that keeps writing after its handshake was rejected.
type floodConn struct{ closed atomic.Bool }

func (f *floodConn) ReadMessage(b []byte) (int, error) {
	if f.closed.Load() {
		return 0, net.ErrClosed
	}
	b[0] = 0xff
	return 1, nil
}
func (f *floodConn) WriteMessage([]byte) error { return nil }
func (f *floodConn) Close() error              { f.closed.Store(true); return nil }
func (f *floodConn) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (f *floodConn) RemoteAddr() net.Addr      { return &net.UDPAddr{} }

var _ carrier.Conn = (*floodConn)(nil)

// Regression: a pump whose consumer went away (rejected handshake, closed
// link) must not stay blocked on its full channel forever.
func TestPumpStopReleasesReaderWhenChannelFull(t *testing.T) {
	base := runtime.NumGoroutine()
	var pumps []*pump
	for i := 0; i < 50; i++ {
		p := startPump(&floodConn{})
		pumps = append(pumps, p)
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, p := range pumps {
		for len(p.ch) < cap(p.ch) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	for _, p := range pumps {
		p.stop()
	}
	end := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(end) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Fatalf("pump goroutines leaked: before=%d after=%d", base, n)
	}
}

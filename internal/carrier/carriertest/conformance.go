// Package carriertest provides a conformance suite that every carrier must pass.
package carriertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

// Run exercises Listen/Dial/ReadMessage/WriteMessage/Close on c.
// sizes lists message sizes that must round-trip (respecting MaxMessage).
func Run(t *testing.T, c carrier.Carrier, listenAddr string, sizes []int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Check(ctx); err != nil {
		t.Skipf("carrier %s unavailable: %v", c.Name(), err)
	}
	ln, err := c.Listen(ctx, listenAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan carrier.Conn, 1)
	go func() {
		conn, err := ln.Accept(ctx)
		if err == nil {
			accepted <- conn
		}
	}()
	cl, err := c.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()
	// Datagram listeners learn about peers from their first message.
	if err := cl.WriteMessage([]byte("hello")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var srv carrier.Conn
	select {
	case srv = <-accepted:
	case <-ctx.Done():
		t.Fatal("accept timed out")
	}
	defer srv.Close()
	buf := make([]byte, carrier.MaxMessage)
	n, err := srv.ReadMessage(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("first read: %q %v", buf[:n], err)
	}
	for _, size := range sizes {
		msg := make([]byte, size)
		_, _ = rand.Read(msg)
		for _, dir := range []struct {
			name     string
			from, to carrier.Conn
		}{{"c2s", cl, srv}, {"s2c", srv, cl}} {
			if err := dir.from.WriteMessage(msg); err != nil {
				t.Fatalf("%s write %d: %v", dir.name, size, err)
			}
			n, err := dir.to.ReadMessage(buf)
			if err != nil {
				t.Fatalf("%s read %d: %v", dir.name, size, err)
			}
			if !bytes.Equal(buf[:n], msg) {
				t.Fatalf("%s size %d: message corrupted (got %d bytes)", dir.name, size, n)
			}
		}
	}
	caps := c.Capabilities()
	if caps.MaxMessage > 0 {
		if err := cl.WriteMessage(make([]byte, caps.MaxMessage+1)); err == nil {
			t.Fatalf("oversized message accepted")
		}
	}
	if err := cl.Close(); err != nil {
		t.Logf("close: %v", err)
	}
}

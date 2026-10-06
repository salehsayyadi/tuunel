//go:build linux

package udp

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

// TestBatchRoundTrip sends runs of equally sized messages (coalesced with
// UDP GSO when available) both ways and checks order and content.
func TestBatchRoundTrip(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprintf("gsoOff=%v", off), func(t *testing.T) {
			prev := gsoOff.Load()
			gsoOff.Store(off)
			defer gsoOff.Store(prev)
			c := New(carrier.Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ln, err := c.Listen(ctx, "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			a, err := c.Dial(ctx, ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			var msgs [][]byte
			for i := 0; i < 100; i++ {
				size := 1200
				if i%37 == 36 {
					size = 300 // shorter message ends a GSO run
				}
				msgs = append(msgs, bytes.Repeat([]byte{byte(i)}, size))
			}
			n, err := a.(carrier.BatchWriter).WriteMessages(msgs)
			if err != nil || n != len(msgs) {
				t.Fatalf("client write: %d %v", n, err)
			}
			b, err := ln.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			check := func(r carrier.BatchReader) {
				got := 0
				for got < len(msgs) {
					if err := r.ReadMessages(func(m []byte) {
						if !bytes.Equal(m, msgs[got]) {
							t.Fatalf("message %d: got %d bytes of %d", got, len(m), m[0])
						}
						got++
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			check(b.(carrier.BatchReader))
			n, err = b.(carrier.BatchWriter).WriteMessages(msgs)
			if err != nil || n != len(msgs) {
				t.Fatalf("server write: %d %v", n, err)
			}
			check(a.(carrier.BatchReader))
		})
	}
}

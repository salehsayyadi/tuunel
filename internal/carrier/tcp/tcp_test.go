package tcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformance(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}

func TestConformanceMultiStream(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{Streams: 4}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}

// Multi-stream dialers and classic dialers share one listener; flow-tagged
// data messages spread over members and all arrive.
func TestMultiStreamFlows(t *testing.T) {
	ctx := context.Background()
	ln, err := New(carrier.Options{}).Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, streams := range []int{1, 4} {
		c, err := New(carrier.Options{Streams: streams}).Dial(ctx, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		const n = 4000 // the listener classifies a connection by its first bytes
		go func() {
			for i := 0; i < n; i++ {
				msg := []byte(fmt.Sprintf("m%05d", i))
				for c.(carrier.FlowWriter).WriteMessageFlow(msg, uint32(i)) == carrier.ErrBusy {
					time.Sleep(time.Millisecond)
				}
			}
		}()
		s, err := ln.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := streamsOf(s); got != streams {
			t.Fatalf("listener saw %d streams, want %d", got, streams)
		}
		seen := map[string]bool{}
		buf := make([]byte, carrier.MaxMessage)
		for len(seen) < n {
			k, err := s.ReadMessage(buf)
			if err != nil {
				t.Fatal(err)
			}
			seen[string(buf[:k])] = true
		}
		// reverse direction, control path
		if err := s.WriteMessage([]byte("pong")); err != nil {
			t.Fatal(err)
		}
		k, err := c.ReadMessage(buf)
		if err != nil || string(buf[:k]) != "pong" {
			t.Fatalf("reverse: %q %v", buf[:k], err)
		}
		c.Close()
		if _, err := s.ReadMessage(buf); err == nil {
			t.Fatal("expected error after peer close")
		}
		s.Close()
	}
}

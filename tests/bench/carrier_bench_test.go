// Package bench measures raw carrier message throughput over loopback.
// Run: go test -run x -bench . -benchmem ./tests/bench
package bench

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/quic"
	"github.com/salehsayyadi/tuunel/internal/carrier/tcp"
	"github.com/salehsayyadi/tuunel/internal/carrier/udp"
	"github.com/salehsayyadi/tuunel/internal/carrier/websocket"
)

func pair(b *testing.B, c carrier.Carrier) (carrier.Conn, carrier.Conn, func()) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	ln, err := c.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	ch := make(chan carrier.Conn, 1)
	go func() {
		s, err := ln.Accept(ctx)
		if err == nil {
			ch <- s
		}
	}()
	cl, err := c.Dial(ctx, ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	_ = cl.WriteMessage([]byte("hi"))
	s := <-ch
	buf := make([]byte, 100)
	_, _ = s.ReadMessage(buf)
	return cl, s, func() { cl.Close(); s.Close(); ln.Close(); cancel() }
}

func run(b *testing.B, c carrier.Carrier, size int) {
	cl, s, done := pair(b, c)
	defer done()
	msg := make([]byte, size)
	var recv atomic.Int64
	reliable := c.Capabilities().Reliable
	go func() {
		buf := make([]byte, carrier.MaxMessage)
		for {
			if _, err := s.ReadMessage(buf); err != nil {
				return
			}
			recv.Add(1)
		}
	}()
	b.SetBytes(int64(size))
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if err := cl.WriteMessage(msg); err != nil {
			b.Fatal(err)
		}
		if !reliable && i%64 == 63 {
			time.Sleep(20 * time.Microsecond) // pace datagrams to avoid socket buffer overrun
		}
	}
	// wait until everything arrived or the receiver has been idle for 200ms
	last, lastChange := recv.Load(), time.Now()
	for recv.Load() < int64(b.N) && time.Since(lastChange) < 200*time.Millisecond {
		time.Sleep(time.Millisecond)
		if v := recv.Load(); v != last {
			last, lastChange = v, time.Now()
		}
	}
	n := recv.Load()
	el := lastChange.Sub(start)
	b.StopTimer()
	b.ReportMetric(float64(n)/el.Seconds(), "msgs/s")
	b.ReportMetric(100*float64(int64(b.N)-n)/float64(b.N), "loss%")
}

func BenchmarkTCP1400(b *testing.B)  { run(b, tcp.New(carrier.Options{}), 1400) }
func BenchmarkUDP1400(b *testing.B)  { run(b, udp.New(carrier.Options{}), 1400) }
func BenchmarkQUIC1400(b *testing.B) { run(b, quic.New(carrier.Options{}), 1400) }
func BenchmarkQUICDatagram1100(b *testing.B) {
	run(b, quic.New(carrier.Options{Datagrams: true}), 1100)
}
func BenchmarkWSS1400(b *testing.B) { run(b, websocket.New(carrier.Options{}), 1400) }

package quic

import (
	"context"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformanceStream(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 9000, carrier.MaxMessage})
}

func TestConformanceDatagram(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{Datagrams: true}), "127.0.0.1:0", []int{1, 100, 1000, DatagramMax})
}

// Regression (tests/lab/failover.py): a WriteMessage blocked on a stream whose
// peer stopped reading (black-holed path, full flow-control window) must
// return promptly when the conn is closed. Previously Stream.Close() before
// CloseWithError left the Write blocked forever and wedged the engine sender.
func TestCloseUnblocksBlockedWrite(t *testing.T) {
	old := carrier.StreamWriteTimeout
	carrier.StreamWriteTimeout = time.Minute // isolate the Close path from the deadline
	defer func() { carrier.StreamWriteTimeout = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := New(carrier.Options{})
	ln, err := c.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan carrier.Conn, 1)
	go func() {
		s, err := ln.Accept(ctx)
		if err == nil {
			acc <- s // never read from: the client's window fills up
		}
	}()
	cl, err := c.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, 1400)
	if err := cl.WriteMessage(msg); err != nil { // makes the server accept the stream
		t.Fatal(err)
	}
	srv := <-acc
	defer srv.Close()
	blocked := make(chan error, 1)
	go func() {
		for {
			if err := cl.WriteMessage(msg); err != nil {
				blocked <- err
				return
			}
		}
	}()
	time.Sleep(1500 * time.Millisecond) // window exhausted -> Write blocks
	select {
	case err := <-blocked:
		t.Fatalf("write failed before close: %v", err)
	default:
	}
	_ = cl.Close()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("WriteMessage still blocked 3s after Close")
	}
}

func TestStreamWriteDeadline(t *testing.T) {
	old := carrier.StreamWriteTimeout
	carrier.StreamWriteTimeout = 500 * time.Millisecond
	defer func() { carrier.StreamWriteTimeout = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := New(carrier.Options{})
	ln, err := c.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan carrier.Conn, 1)
	go func() {
		if s, err := ln.Accept(ctx); err == nil {
			acc <- s
		}
	}()
	cl, err := c.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	msg := make([]byte, 1400)
	_ = cl.WriteMessage(msg)
	srv := <-acc
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		for {
			if err := cl.WriteMessage(msg); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		t.Logf("blocked write ended by deadline: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("stream write not bounded by StreamWriteTimeout")
	}
}

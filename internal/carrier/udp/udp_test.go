package udp

import (
	"context"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/carriertest"
)

func TestConformance(t *testing.T) {
	carriertest.Run(t, New(carrier.Options{}), "127.0.0.1:0", []int{1, 100, 1400, 8000, 60000})
}

func TestSessionLimit(t *testing.T) {
	c := New(carrier.Options{MaxSessions: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ln, err := c.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, _ := c.Dial(ctx, ln.Addr().String())
	b, _ := c.Dial(ctx, ln.Addr().String())
	defer a.Close()
	defer b.Close()
	_ = a.WriteMessage([]byte("a"))
	if _, err := ln.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	_ = b.WriteMessage([]byte("b"))
	short, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	if _, err := ln.Accept(short); err == nil {
		t.Fatal("session limit not enforced")
	}
}

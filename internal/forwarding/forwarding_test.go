package forwarding

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func echoTCP(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func echoUDP(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 65535)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], a)
		}
	}()
	return pc.LocalAddr().String()
}

func TestTCPAndUDPMultipleRulesAndReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(nil)
	defer m.Close()
	r1 := Rule{Proto: "tcp", Listen: "127.0.0.1:0", Target: echoTCP(t)}
	r2 := Rule{Proto: "tcp", Listen: "127.0.0.2:0", Target: echoTCP(t)}
	u1 := Rule{Proto: "udp", Listen: "127.0.0.1:0", Target: echoUDP(t)}
	if err := m.Apply(ctx, []Rule{r1, r2, u1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Rule{r1, r2} {
		c, err := net.Dial("tcp", m.Addr(r).String())
		if err != nil {
			t.Fatal(err)
		}
		msg := bytes.Repeat([]byte("x"), 100000)
		go c.Write(msg)
		got := make([]byte, len(msg))
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("tcp forward: %v", err)
		}
		c.Close()
	}
	uc, _ := net.Dial("udp", m.Addr(u1).String())
	for i := 0; i < 3; i++ {
		uc.Write([]byte("ping"))
		b := make([]byte, 10)
		uc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := uc.Read(b); err != nil || string(b[:n]) != "ping" {
			t.Fatalf("udp forward: %v", err)
		}
	}
	uc.Close()
	// reload: drop r2, keep r1 running
	a1 := m.Addr(r1).String()
	if err := m.Apply(ctx, []Rule{r1, u1}); err != nil {
		t.Fatal(err)
	}
	if m.Addr(r2) != nil || m.Addr(r1).String() != a1 {
		t.Fatal("reload did not reconcile")
	}
	if len(m.Status()) != 2 {
		t.Fatal("status")
	}
}

func TestConnectionLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(nil)
	defer m.Close()
	r := Rule{Proto: "tcp", Listen: "127.0.0.1:0", Target: echoTCP(t), MaxConnections: 1}
	m.Apply(ctx, []Rule{r})
	c1, _ := net.Dial("tcp", m.Addr(r).String())
	defer c1.Close()
	c1.Write([]byte("a"))
	b := make([]byte, 1)
	io.ReadFull(c1, b)
	c2, _ := net.Dial("tcp", m.Addr(r).String())
	defer c2.Close()
	c2.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c2.Read(b); err == nil {
		t.Fatal("limit not enforced")
	}
}

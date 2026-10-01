package carrier

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

type rwc struct{ bytes.Buffer }

func (*rwc) Close() error { return nil }

func TestStreamFraming(t *testing.T) {
	var b rwc
	c := NewStreamConn(&b, nil, nil)
	for _, m := range [][]byte{[]byte("a"), bytes.Repeat([]byte{7}, 1500)} {
		if err := c.WriteMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 2000)
	n, err := c.ReadMessage(buf)
	if err != nil || string(buf[:n]) != "a" {
		t.Fatalf("got %q %v", buf[:n], err)
	}
	n, err = c.ReadMessage(buf)
	if err != nil || n != 1500 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestStreamRejectsOversizeAndEmpty(t *testing.T) {
	var b rwc
	c := NewStreamConn(&b, nil, nil)
	if err := c.WriteMessage(nil); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("empty accepted: %v", err)
	}
	b.Write([]byte{0x10, 0x00}) // 4096-byte frame
	b.Write(make([]byte, 4096))
	if _, err := c.ReadMessage(make([]byte, 100)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize frame accepted: %v", err)
	}
	var z rwc
	z.Write([]byte{0, 0})
	if _, err := NewStreamConn(&z, nil, nil).ReadMessage(make([]byte, 10)); err == nil {
		t.Fatal("zero frame accepted")
	}
}

func TestPipe(t *testing.T) {
	a, b := Pipe()
	if err := a.WriteMessage([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if n, err := b.ReadMessage(buf); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	a.Close()
	if _, err := b.ReadMessage(buf); err == nil {
		t.Fatal("read after close")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(1, 2)
	addr := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}
	now := time.Now()
	for i := 0; i < 2; i++ {
		if !l.Allow(addr, now) {
			t.Fatalf("burst token %d denied", i)
		}
	}
	if l.Allow(addr, now) {
		t.Fatal("exceeded burst")
	}
	if !l.Allow(addr, now.Add(1100*time.Millisecond)) {
		t.Fatal("refill failed")
	}
	other := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 1}
	if !l.Allow(other, now) {
		t.Fatal("independent source denied")
	}
}

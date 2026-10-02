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
	x, y := net.Pipe()
	w, r := NewStreamConn(x, nil, nil), NewStreamConn(y, nil, nil)
	defer w.Close()
	defer r.Close()
	msgs := [][]byte{[]byte("a"), bytes.Repeat([]byte{7}, 1500)}
	for i := 0; i < 2000; i++ { // many frames: exercises write coalescing and buffered reads
		msgs = append(msgs, bytes.Repeat([]byte{byte(i)}, 1+i%1400))
	}
	go func() {
		for _, m := range msgs {
			if err := w.WriteMessage(m); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	buf := make([]byte, 2000)
	for i, m := range msgs {
		r.rw.(net.Conn).SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := r.ReadMessage(buf)
		if err != nil || !bytes.Equal(buf[:n], m) {
			t.Fatalf("frame %d: got %d bytes %v", i, n, err)
		}
	}
}

func TestStreamCloseUnblocksFullWriter(t *testing.T) {
	x, y := net.Pipe() // nobody reads y: the writer blocks once pending is full
	defer y.Close()
	w := NewStreamConn(x, nil, nil)
	done := make(chan error, 1)
	go func() {
		for {
			if err := w.WriteMessage(make([]byte, 1400)); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	w.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WriteMessage still blocked after Close")
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

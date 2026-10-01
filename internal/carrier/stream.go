package carrier

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// StreamWriteTimeout bounds one framed write on a stream carrier. A stream
// that cannot accept a single frame for this long is dead (the health check
// declares failure much earlier); without a bound, a write blocked on a
// black-holed path would wedge the caller indefinitely. A timed-out or
// failed write leaves the framing undefined, so the connection is closed.
var StreamWriteTimeout = 5 * time.Second

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

// StreamConn adapts a reliable byte stream to message semantics using a
// 16-bit big-endian length prefix. Zero-length messages are rejected.
type StreamConn struct {
	rw         io.ReadWriteCloser
	local      net.Addr
	remote     net.Addr
	wmu        sync.Mutex
	wbuf       []byte
	closeOnce  sync.Once
	closeError error
}

// NewStreamConn wraps rw. local/remote may be nil.
func NewStreamConn(rw io.ReadWriteCloser, local, remote net.Addr) *StreamConn {
	return &StreamConn{rw: rw, local: local, remote: remote, wbuf: make([]byte, 2+MaxMessage)}
}

func (c *StreamConn) ReadMessage(b []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.rw, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return 0, fmt.Errorf("carrier: zero-length frame")
	}
	if n > len(b) {
		// Drain is unsafe on an attacker-controlled stream; fail the connection.
		return 0, fmt.Errorf("%w: frame %d > buffer %d", ErrMessageTooLarge, n, len(b))
	}
	if _, err := io.ReadFull(c.rw, b[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

func (c *StreamConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > MaxMessage {
		return fmt.Errorf("%w: %d", ErrMessageTooLarge, len(b))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	binary.BigEndian.PutUint16(c.wbuf[:2], uint16(len(b)))
	copy(c.wbuf[2:], b)
	buf := c.wbuf[:2+len(b)]
	if d, ok := c.rw.(writeDeadliner); ok && StreamWriteTimeout > 0 {
		_ = d.SetWriteDeadline(time.Now().Add(StreamWriteTimeout))
	}
	for len(buf) > 0 {
		n, err := c.rw.Write(buf)
		if err != nil {
			// partial frame possible: the stream can no longer be framed
			_ = c.Close()
			return fmt.Errorf("carrier: stream write: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

func (c *StreamConn) Close() error {
	c.closeOnce.Do(func() { c.closeError = c.rw.Close() })
	return c.closeError
}

func (c *StreamConn) LocalAddr() net.Addr  { return c.local }
func (c *StreamConn) RemoteAddr() net.Addr { return c.remote }

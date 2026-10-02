package carrier

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// StreamWriteTimeout bounds how long a stream carrier may fail to make
// progress: one batched write, or a WriteMessage waiting for buffer space. A
// stream that cannot accept data for this long is dead (the health check
// declares failure much earlier); without a bound, a write blocked on a
// black-holed path would wedge the caller indefinitely. A timed-out or failed
// write leaves the framing undefined, so the connection is closed.
var StreamWriteTimeout = 5 * time.Second

// streamPendingMax bounds frames queued for the background writer.
const streamPendingMax = 256 << 10

// streamFlowMax bounds queued bytes accepted by WriteMessageFlow. Beyond it
// data packets are dropped rather than queued: together with the kernel's
// TCP_NOTSENT_LOWAT this caps the tunnel's own queueing delay (bufferbloat)
// to a few tens of milliseconds instead of seconds.
const streamFlowMax = 192 << 10

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

// StreamConn adapts a reliable byte stream to message semantics using a
// 16-bit big-endian length prefix. Zero-length messages are rejected.
//
// Performance: reads are buffered (one syscall for many frames) and writes
// are coalesced by a background writer, so a burst of packets costs one
// write syscall instead of one per packet. WriteMessage only copies the frame
// and returns; it blocks (bounded by StreamWriteTimeout) only when
// streamPendingMax bytes are already queued. A write error closes the
// connection and is returned by the next WriteMessage.
type StreamConn struct {
	rw     io.ReadWriteCloser
	br     *bufio.Reader
	local  net.Addr
	remote net.Addr

	mu      sync.Mutex
	pending []byte
	spare   []byte
	werr    error
	busy    time.Time // first failed WriteMessageFlow since the last progress
	kick    chan struct{}
	space   chan struct{}
	done    chan struct{}

	closeOnce  sync.Once
	closeError error
}

// NewStreamConn wraps rw. local/remote may be nil.
func NewStreamConn(rw io.ReadWriteCloser, local, remote net.Addr) *StreamConn {
	return NewStreamConnReader(rw, bufio.NewReaderSize(rw, 64<<10), local, remote)
}

// NewStreamConnReader is NewStreamConn with an existing reader for rw (for
// example one that already peeked at a connection preamble).
func NewStreamConnReader(rw io.ReadWriteCloser, br *bufio.Reader, local, remote net.Addr) *StreamConn {
	c := &StreamConn{rw: rw, br: br, local: local, remote: remote,
		pending: make([]byte, 0, 64<<10), spare: make([]byte, 0, 64<<10),
		kick: make(chan struct{}, 1), space: make(chan struct{}, 1), done: make(chan struct{})}
	go c.writer()
	return c
}

func (c *StreamConn) ReadMessage(b []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
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
	if _, err := io.ReadFull(c.br, b[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

var errStreamClosed = errors.New("carrier: stream closed")

func (c *StreamConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > MaxMessage {
		return fmt.Errorf("%w: %d", ErrMessageTooLarge, len(b))
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		c.mu.Lock()
		if c.werr != nil {
			err := c.werr
			c.mu.Unlock()
			return err
		}
		if len(c.pending) == 0 || len(c.pending)+2+len(b) <= streamPendingMax {
			var hdr [2]byte
			binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
			c.pending = append(append(c.pending, hdr[:]...), b...)
			c.mu.Unlock()
			select {
			case c.kick <- struct{}{}:
			default:
			}
			return nil
		}
		c.mu.Unlock()
		if timer == nil {
			d := StreamWriteTimeout
			if d <= 0 {
				d = time.Hour
			}
			timer = time.NewTimer(d)
		}
		select {
		case <-c.space:
		case <-c.done:
			return errStreamClosed
		case <-timer.C:
			_ = c.fail(fmt.Errorf("carrier: stream write: no progress for %v", StreamWriteTimeout))
			return c.werr
		}
	}
}

// WriteMessageFlow queues a data message without blocking. When the queue is
// full the message is dropped with ErrBusy; a stream that stays full without
// any progress for StreamWriteTimeout is declared dead and closed.
func (c *StreamConn) WriteMessageFlow(b []byte, _ uint32) error {
	if len(b) == 0 || len(b) > MaxMessage {
		return fmt.Errorf("%w: %d", ErrMessageTooLarge, len(b))
	}
	c.mu.Lock()
	if c.werr != nil {
		err := c.werr
		c.mu.Unlock()
		return err
	}
	if len(c.pending) != 0 && len(c.pending)+2+len(b) > streamFlowMax {
		now := time.Now()
		if c.busy.IsZero() {
			c.busy = now
		}
		stuck := StreamWriteTimeout > 0 && now.Sub(c.busy) > StreamWriteTimeout
		c.mu.Unlock()
		if stuck {
			_ = c.fail(fmt.Errorf("carrier: stream write: no progress for %v", StreamWriteTimeout))
		}
		return ErrBusy
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	c.pending = append(append(c.pending, hdr[:]...), b...)
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return nil
}

func (c *StreamConn) fail(err error) error {
	c.mu.Lock()
	if c.werr == nil {
		c.werr = err
	}
	c.mu.Unlock()
	return c.Close()
}

func (c *StreamConn) writer() {
	d, hasDeadline := c.rw.(writeDeadliner)
	for {
		select {
		case <-c.kick:
		case <-c.done:
			return
		}
		for {
			c.mu.Lock()
			if len(c.pending) == 0 || c.werr != nil {
				c.mu.Unlock()
				break
			}
			orig := c.pending
			buf := orig
			c.pending = c.spare[:0]
			c.busy = time.Time{}
			c.mu.Unlock()
			if hasDeadline && StreamWriteTimeout > 0 {
				_ = d.SetWriteDeadline(time.Now().Add(StreamWriteTimeout))
			}
			for len(buf) > 0 {
				n, err := c.rw.Write(buf)
				if err == nil && n == 0 {
					err = io.ErrShortWrite
				}
				if err != nil {
					// partial frame possible: the stream can no longer be framed
					_ = c.fail(fmt.Errorf("carrier: stream write: %w", err))
					return
				}
				buf = buf[n:]
			}
			c.mu.Lock()
			if cap(orig) <= 4*streamPendingMax {
				c.spare = orig[:0]
			}
			c.mu.Unlock()
			select {
			case c.space <- struct{}{}:
			default:
			}
		}
	}
}

func (c *StreamConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.closeError = c.rw.Close()
	})
	return c.closeError
}

func (c *StreamConn) LocalAddr() net.Addr  { return c.local }
func (c *StreamConn) RemoteAddr() net.Addr { return c.remote }

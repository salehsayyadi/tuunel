package carrier

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Pipe returns two connected in-memory message Conns. It is used by tests
// and by the abstraction's conformance suite.
func Pipe() (Conn, Conn) {
	ab := make(chan []byte, 256)
	ba := make(chan []byte, 256)
	done := make(chan struct{})
	var once sync.Once
	closeFn := func() { once.Do(func() { close(done) }) }
	return &pipeConn{in: ba, out: ab, done: done, close: closeFn}, &pipeConn{in: ab, out: ba, done: done, close: closeFn}
}

type pipeConn struct {
	in, out chan []byte
	done    chan struct{}
	close   func()
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func (p *pipeConn) ReadMessage(b []byte) (int, error) {
	select {
	case m := <-p.in:
		if len(m) > len(b) {
			return 0, ErrMessageTooLarge
		}
		return copy(b, m), nil
	case <-p.done:
		return 0, net.ErrClosed
	}
}

func (p *pipeConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > MaxMessage {
		return ErrMessageTooLarge
	}
	m := append([]byte(nil), b...)
	select {
	case p.out <- m:
		return nil
	case <-p.done:
		return net.ErrClosed
	}
}

func (p *pipeConn) Close() error         { p.close(); return nil }
func (p *pipeConn) LocalAddr() net.Addr  { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr { return pipeAddr{} }

// ErrClosed is returned by listeners after Close.
var ErrClosed = errors.New("carrier: listener closed")

// AcceptQueue is a helper for listeners that demultiplex a shared socket.
type AcceptQueue struct {
	ch   chan Conn
	done chan struct{}
	once sync.Once
}

func NewAcceptQueue(n int) *AcceptQueue {
	return &AcceptQueue{ch: make(chan Conn, n), done: make(chan struct{})}
}

// Push offers a new conn; it returns false when the queue is full or closed.
func (q *AcceptQueue) Push(c Conn) bool {
	select {
	case <-q.done:
		return false
	default:
	}
	select {
	case q.ch <- c:
		return true
	default:
		return false
	}
}

func (q *AcceptQueue) Accept(ctx context.Context) (Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *AcceptQueue) Close()                { q.once.Do(func() { close(q.done) }) }
func (q *AcceptQueue) Done() <-chan struct{} { return q.done }

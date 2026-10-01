package engine

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"

	"github.com/salehsayyadi/tuunel/internal/packet"
)

// memDev is an in-memory Device: inject() simulates packets written by local
// applications, out receives packets the engine delivers to the OS.
type memDev struct {
	in   chan []byte
	out  chan []byte
	once sync.Once
	done chan struct{}
}

func newMemDev() *memDev {
	return &memDev{in: make(chan []byte, 1024), out: make(chan []byte, 4096), done: make(chan struct{})}
}

func (d *memDev) Read(b []byte) (int, error) {
	select {
	case p := <-d.in:
		return copy(b, p), nil
	case <-d.done:
		return 0, errors.New("closed")
	}
}

func (d *memDev) Write(b []byte) (int, error) {
	select {
	case d.out <- append([]byte(nil), b...):
	default:
	}
	return len(b), nil
}

func (d *memDev) Close() error { d.once.Do(func() { close(d.done) }); return nil }

func udp4(src, dst string, payload []byte, df bool) []byte {
	p := make([]byte, 28+len(payload))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	if df {
		p[6] = 0x40
	}
	p[8], p[9] = 64, 17
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	binary.BigEndian.PutUint16(p[10:12], packet.Checksum(p[:20], 0))
	binary.BigEndian.PutUint16(p[20:22], 1000)
	binary.BigEndian.PutUint16(p[22:24], 2000)
	binary.BigEndian.PutUint16(p[24:26], uint16(8+len(payload)))
	copy(p[28:], payload)
	return p
}

func udp6(src, dst string, payload []byte) []byte {
	p := make([]byte, 48+len(payload))
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(8+len(payload)))
	p[6], p[7] = 17, 64
	s, d := netip.MustParseAddr(src).As16(), netip.MustParseAddr(dst).As16()
	copy(p[8:24], s[:])
	copy(p[24:40], d[:])
	copy(p[48:], payload)
	return p
}

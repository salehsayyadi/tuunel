// Package framing defines the bounded packet framing used inside authenticated
// carrier connections. Confidentiality and peer authentication are provided by
// the carrier's TLS 1.3 session, not by this package.
package framing

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const headerSize = 10 // uint64 sequence + uint16 packet length

var ErrSequence = errors.New("unexpected packet sequence")

// Encoder writes strictly ordered, length-prefixed IP packets.
type Encoder struct{ next uint64 }

func NewEncoder() *Encoder { return &Encoder{next: 1} }

func (e *Encoder) WritePacket(w io.Writer, packet []byte, maxPacket int) error {
	if err := ValidateIPPacket(packet, maxPacket); err != nil {
		return err
	}
	if e.next == 0 { // sequence wrapped; require a fresh TLS session
		return errors.New("packet sequence exhausted")
	}
	var header [headerSize]byte
	binary.BigEndian.PutUint64(header[:8], e.next)
	binary.BigEndian.PutUint16(header[8:], uint16(len(packet)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	if err := writeAll(w, packet); err != nil {
		return err
	}
	e.next++
	return nil
}

// Decoder rejects duplicates, gaps, reordering, truncated frames and oversized
// packets. Each direction has an independent sequence starting at one.
type Decoder struct{ next uint64 }

func NewDecoder() *Decoder { return &Decoder{next: 1} }

func (d *Decoder) ReadPacket(r io.Reader, maxPacket int) ([]byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	seq := binary.BigEndian.Uint64(header[:8])
	if d.next == 0 {
		return nil, errors.New("packet sequence exhausted")
	}
	if seq != d.next {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrSequence, seq, d.next)
	}
	n := int(binary.BigEndian.Uint16(header[8:]))
	if n == 0 || n > maxPacket {
		return nil, fmt.Errorf("invalid packet length %d (limit %d)", n, maxPacket)
	}
	packet := make([]byte, n)
	if _, err := io.ReadFull(r, packet); err != nil {
		return nil, err
	}
	if err := ValidateIPPacket(packet, maxPacket); err != nil {
		return nil, err
	}
	d.next++
	return packet, nil
}

func ValidateIPPacket(p []byte, limit int) error {
	if len(p) == 0 || len(p) > limit || len(p) > 65535 {
		return fmt.Errorf("invalid IP packet size %d (limit %d)", len(p), limit)
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 {
			return errors.New("truncated IPv4 packet")
		}
		hlen := int(p[0]&0x0f) * 4
		if hlen < 20 || hlen > len(p) {
			return errors.New("invalid IPv4 header length")
		}
		if int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
			return errors.New("IPv4 total length mismatch")
		}
	case 6:
		if len(p) < 40 {
			return errors.New("truncated IPv6 packet")
		}
		// IPv6 jumbograms are intentionally unsupported in this MVP.
		if int(binary.BigEndian.Uint16(p[4:6]))+40 != len(p) {
			return errors.New("IPv6 payload length mismatch or jumbogram")
		}
	default:
		return errors.New("unsupported IP version")
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

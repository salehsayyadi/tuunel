// Package packet contains IP packet validation, address extraction, IPv4
// fragmentation and ICMP/ICMPv6 "packet too big" generation used by the
// tunnel engine. It never allocates based on attacker-controlled lengths
// without first bounding them.
package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	IPv4HeaderMin = 20
	IPv6Header    = 40
	MinIPv4MTU    = 576
	MinIPv6MTU    = 1280
)

var ErrInvalid = errors.New("packet: invalid IP packet")

// Validate checks that p is a well-formed IPv4 or IPv6 packet no larger than limit.
func Validate(p []byte, limit int) error {
	if len(p) == 0 || len(p) > limit || len(p) > 65535 {
		return fmt.Errorf("%w: size %d (limit %d)", ErrInvalid, len(p), limit)
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < IPv4HeaderMin {
			return fmt.Errorf("%w: truncated IPv4", ErrInvalid)
		}
		hl := int(p[0]&0x0f) * 4
		if hl < IPv4HeaderMin || hl > len(p) {
			return fmt.Errorf("%w: IPv4 header length", ErrInvalid)
		}
		if int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
			return fmt.Errorf("%w: IPv4 total length mismatch", ErrInvalid)
		}
	case 6:
		if len(p) < IPv6Header {
			return fmt.Errorf("%w: truncated IPv6", ErrInvalid)
		}
		if int(binary.BigEndian.Uint16(p[4:6]))+IPv6Header != len(p) {
			return fmt.Errorf("%w: IPv6 payload length mismatch or jumbogram", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: version %d", ErrInvalid, p[0]>>4)
	}
	return nil
}

// Addrs returns the source and destination of a validated packet.
func Addrs(p []byte) (src, dst netip.Addr) {
	switch p[0] >> 4 {
	case 4:
		src = netip.AddrFrom4([4]byte(p[12:16]))
		dst = netip.AddrFrom4([4]byte(p[16:20]))
	case 6:
		src = netip.AddrFrom16([16]byte(p[8:24]))
		dst = netip.AddrFrom16([16]byte(p[24:40]))
	}
	return
}

// DontFragment reports whether an IPv4 packet has DF set. IPv6 packets are
// never fragmented by routers and therefore always report true.
func DontFragment(p []byte) bool {
	if p[0]>>4 == 6 {
		return true
	}
	return binary.BigEndian.Uint16(p[6:8])&0x4000 != 0
}

// Checksum computes the Internet checksum (RFC 1071) with an initial sum.
func Checksum(b []byte, initial uint32) uint16 {
	sum := initial
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// FragmentIPv4 splits an IPv4 packet without DF into fragments whose total
// length is <= mtu. Options are copied only into the first fragment's
// header layout if they are all "copy" options; to stay simple and safe,
// packets with IP options are rejected.
func FragmentIPv4(p []byte, mtu int) ([][]byte, error) {
	if p[0]>>4 != 4 {
		return nil, errors.New("packet: not IPv4")
	}
	hl := int(p[0]&0x0f) * 4
	if hl != IPv4HeaderMin {
		return nil, errors.New("packet: refusing to fragment IPv4 with options")
	}
	if DontFragment(p) {
		return nil, errors.New("packet: DF set")
	}
	if len(p) <= mtu {
		return [][]byte{p}, nil
	}
	maxData := (mtu - hl) &^ 7
	if maxData < 8 {
		return nil, errors.New("packet: MTU too small to fragment")
	}
	flagsOff := binary.BigEndian.Uint16(p[6:8])
	baseOff := int(flagsOff&0x1fff) * 8
	origMF := flagsOff&0x2000 != 0
	data := p[hl:]
	var out [][]byte
	for off := 0; off < len(data); off += maxData {
		end := off + maxData
		last := end >= len(data)
		if last {
			end = len(data)
		}
		f := make([]byte, hl+end-off)
		copy(f, p[:hl])
		copy(f[hl:], data[off:end])
		binary.BigEndian.PutUint16(f[2:4], uint16(len(f)))
		fo := uint16((baseOff + off) / 8)
		if !last || origMF {
			fo |= 0x2000
		}
		binary.BigEndian.PutUint16(f[6:8], fo)
		f[10], f[11] = 0, 0
		binary.BigEndian.PutUint16(f[10:12], Checksum(f[:hl], 0))
		out = append(out, f)
	}
	return out, nil
}

// TooBig builds an ICMPv4 "fragmentation needed" (type 3 code 4) or ICMPv6
// "packet too big" (type 2) message from `from` to the original sender.
// It returns nil when no error should be generated (e.g. the original is
// itself an ICMP error, or the source is unspecified/multicast).
func TooBig(orig []byte, mtu int, from netip.Addr) []byte {
	src, _ := Addrs(orig)
	if !src.IsValid() || src.IsUnspecified() || src.IsMulticast() || !from.IsValid() {
		return nil
	}
	switch orig[0] >> 4 {
	case 4:
		if !from.Is4() {
			return nil
		}
		hl := int(orig[0]&0x0f) * 4
		if orig[9] == 1 && len(orig) > hl && isICMPv4Error(orig[hl]) {
			return nil
		}
		quote := orig
		if len(quote) > hl+8 {
			quote = quote[:hl+8]
		}
		icmpLen := 8 + len(quote)
		b := make([]byte, 20+icmpLen)
		b[0] = 0x45
		binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
		b[8] = 64
		b[9] = 1
		f4 := from.As4()
		s4 := src.As4()
		copy(b[12:16], f4[:])
		copy(b[16:20], s4[:])
		binary.BigEndian.PutUint16(b[10:12], Checksum(b[:20], 0))
		ic := b[20:]
		ic[0], ic[1] = 3, 4
		binary.BigEndian.PutUint16(ic[6:8], uint16(mtu))
		copy(ic[8:], quote)
		binary.BigEndian.PutUint16(ic[2:4], Checksum(ic, 0))
		return b
	case 6:
		if !from.Is6() || from.Is4In6() {
			return nil
		}
		if orig[6] == 58 && len(orig) > 40 && orig[40] < 128 { // ICMPv6 error
			return nil
		}
		quote := orig
		if max := MinIPv6MTU - 40 - 8; len(quote) > max {
			quote = quote[:max]
		}
		icmpLen := 8 + len(quote)
		b := make([]byte, 40+icmpLen)
		b[0] = 0x60
		binary.BigEndian.PutUint16(b[4:6], uint16(icmpLen))
		b[6] = 58
		b[7] = 64
		f16 := from.As16()
		s16 := src.As16()
		copy(b[8:24], f16[:])
		copy(b[24:40], s16[:])
		ic := b[40:]
		ic[0] = 2
		binary.BigEndian.PutUint32(ic[4:8], uint32(mtu))
		copy(ic[8:], quote)
		binary.BigEndian.PutUint16(ic[2:4], Checksum(ic, pseudo6(b[8:24], b[24:40], icmpLen, 58)))
		return b
	}
	return nil
}

func isICMPv4Error(t byte) bool { return t == 3 || t == 4 || t == 5 || t == 11 || t == 12 }

func pseudo6(src, dst []byte, length int, next byte) uint32 {
	var sum uint32
	for i := 0; i < 16; i += 2 {
		sum += uint32(src[i])<<8 | uint32(src[i+1])
		sum += uint32(dst[i])<<8 | uint32(dst[i+1])
	}
	sum += uint32(length>>16) + uint32(length&0xffff)
	sum += uint32(next)
	return sum
}

package packet

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

func v4(size int, df bool) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	if df {
		p[6] = 0x40
	}
	p[8] = 64
	p[9] = 17
	copy(p[12:16], []byte{10, 200, 0, 1})
	copy(p[16:20], []byte{10, 200, 0, 2})
	for i := 20; i < size; i++ {
		p[i] = byte(i)
	}
	binary.BigEndian.PutUint16(p[10:12], Checksum(p[:20], 0))
	return p
}

func v6(size int) []byte {
	p := make([]byte, size)
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(size-40))
	p[6] = 17
	a := netip.MustParseAddr("fd00::1").As16()
	b := netip.MustParseAddr("fd00::2").As16()
	copy(p[8:24], a[:])
	copy(p[24:40], b[:])
	return p
}

func TestValidate(t *testing.T) {
	if err := Validate(v4(60, false), 1500); err != nil {
		t.Fatal(err)
	}
	if err := Validate(v6(80), 1500); err != nil {
		t.Fatal(err)
	}
	bad := v4(60, false)
	bad[3] = 61
	for _, p := range [][]byte{nil, {0x45}, bad, v4(1600, false), {0x70, 0, 0, 0}} {
		if Validate(p, 1500) == nil {
			t.Fatalf("accepted %x", p[:min(len(p), 8)])
		}
	}
	src, dst := Addrs(v6(40))
	if src.String() != "fd00::1" || dst.String() != "fd00::2" {
		t.Fatal(src, dst)
	}
}

func TestFragmentReassemblesToOriginal(t *testing.T) {
	orig := v4(3000, false)
	frags, err := FragmentIPv4(orig, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 4 {
		t.Fatalf("fragments: %d", len(frags))
	}
	var data []byte
	for i, f := range frags {
		if len(f) > 1000 || Validate(f, 1000) != nil {
			t.Fatalf("frag %d invalid", i)
		}
		if Checksum(f[:20], 0) != 0 {
			t.Fatalf("frag %d bad checksum", i)
		}
		off := int(binary.BigEndian.Uint16(f[6:8])&0x1fff) * 8
		mf := f[6]&0x20 != 0
		if off != len(data) || mf == (i == len(frags)-1) {
			t.Fatalf("frag %d offset/MF wrong", i)
		}
		data = append(data, f[20:]...)
	}
	if !bytes.Equal(data, orig[20:]) {
		t.Fatal("reassembled payload differs")
	}
	if _, err := FragmentIPv4(v4(3000, true), 1000); err == nil {
		t.Fatal("fragmented DF packet")
	}
}

func TestTooBig(t *testing.T) {
	from4 := netip.MustParseAddr("10.200.0.1")
	m := TooBig(v4(1500, true), 1200, from4)
	if Validate(m, 1500) != nil || m[9] != 1 || m[20] != 3 || m[21] != 4 {
		t.Fatalf("bad ICMP: %x", m[:24])
	}
	if binary.BigEndian.Uint16(m[26:28]) != 1200 || Checksum(m[20:], 0) != 0 {
		t.Fatal("MTU or checksum wrong")
	}
	if TooBig(m, 1200, from4) != nil {
		t.Fatal("ICMP error generated for ICMP error")
	}
	m6 := TooBig(v6(1400), 1280, netip.MustParseAddr("fd00::1"))
	if Validate(m6, 1500) != nil || m6[40] != 2 || binary.BigEndian.Uint32(m6[44:48]) != 1280 {
		t.Fatal("bad ICMPv6 PTB")
	}
	if Checksum(m6[40:], pseudo6(m6[8:24], m6[24:40], len(m6)-40, 58)) != 0 {
		t.Fatal("ICMPv6 checksum")
	}
	if len(m6) > MinIPv6MTU {
		t.Fatal("PTB larger than IPv6 minimum MTU")
	}
}

func FuzzValidate(f *testing.F) {
	f.Add(v4(40, false))
	f.Add(v6(60))
	f.Fuzz(func(t *testing.T, p []byte) {
		if Validate(p, 65535) == nil {
			Addrs(p)
			_ = DontFragment(p)
			if p[0]>>4 == 4 && !DontFragment(p) {
				_, _ = FragmentIPv4(p, 576)
			}
			_ = TooBig(p, 1280, netip.MustParseAddr("10.0.0.1"))
		}
	})
}

func TestFragmentIPv6ReassemblesToOriginal(t *testing.T) {
	payload := make([]byte, 1240) // 1280-byte packet
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	p := make([]byte, IPv6Header+len(payload))
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(len(payload)))
	p[6], p[7] = 17, 64
	p[8], p[23] = 0xfd, 1
	p[24], p[39] = 0xfd, 2
	copy(p[IPv6Header:], payload)
	frags, err := FragmentIPv6(p, 1120, 0xabcdef01)
	if err != nil || len(frags) != 2 {
		t.Fatalf("frags=%d err=%v", len(frags), err)
	}
	var got []byte
	for i, f := range frags {
		if len(f) > 1120 {
			t.Fatalf("fragment %d is %d bytes", i, len(f))
		}
		if err := Validate(f, 1500); err != nil {
			t.Fatalf("fragment %d invalid: %v", i, err)
		}
		if f[6] != 44 || f[IPv6Header] != 17 || binary.BigEndian.Uint32(f[IPv6Header+4:]) != 0xabcdef01 {
			t.Fatalf("fragment %d header wrong: % x", i, f[:IPv6Header+8])
		}
		fo := binary.BigEndian.Uint16(f[IPv6Header+2:])
		if int(fo>>3)*8 != len(got) {
			t.Fatalf("fragment %d offset %d, want %d", i, int(fo>>3)*8, len(got))
		}
		if more := fo&1 == 1; more != (i < len(frags)-1) {
			t.Fatalf("fragment %d M flag %v", i, more)
		}
		got = append(got, f[IPv6Header+8:]...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("reassembled payload differs")
	}
	p[6] = 0 // hop-by-hop options: refused
	if _, err := FragmentIPv6(p, 1120, 1); err == nil {
		t.Fatal("fragmented a packet with extension headers")
	}
}

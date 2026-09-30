package framing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func ipv4() []byte {
	p := make([]byte, 20)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	return p
}

func TestRoundTripAndSequence(t *testing.T) {
	var wire bytes.Buffer
	enc, dec := NewEncoder(), NewDecoder()
	want := ipv4()
	if err := enc.WritePacket(&wire, want, 1400); err != nil { t.Fatal(err) }
	got, err := dec.ReadPacket(&wire, 1400)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(got, want) { t.Fatal("packet changed") }
}

func TestRejectReplayOrGap(t *testing.T) {
	var wire bytes.Buffer
	if err := NewEncoder().WritePacket(&wire, ipv4(), 1400); err != nil { t.Fatal(err) }
	data := append([]byte(nil), wire.Bytes()...)
	binary.BigEndian.PutUint64(data[:8], 2)
	_, err := NewDecoder().ReadPacket(bytes.NewReader(data), 1400)
	if !errors.Is(err, ErrSequence) { t.Fatalf("want ErrSequence, got %v", err) }
}

func TestRejectMalformedIP(t *testing.T) {
	p := ipv4()
	p[3] = 21
	if err := ValidateIPPacket(p, 1400); err == nil { t.Fatal("accepted malformed packet") }
}

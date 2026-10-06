package session

import (
	"bytes"
	"errors"
	"testing"
)

func pair(t *testing.T, psk []byte) (*Session, *Session) {
	t.Helper()
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, psk, func(p []byte) bool { return KeysEqual(p, a.Public) })
	in, m1, err := Initiate(a, b.Public, psk, "node-a", false)
	if err != nil {
		t.Fatal(err)
	}
	sb, m2, err := resp.Respond(m1)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := in.Finish(m2)
	if err != nil {
		t.Fatal(err)
	}
	if sb.PeerNodeID != "node-a" || !KeysEqual(sb.PeerKey, a.Public) {
		t.Fatal("identity not propagated")
	}
	return sa, sb
}

func TestHandshakeAndData(t *testing.T) {
	for _, psk := range [][]byte{nil, bytes.Repeat([]byte{9}, 32)} {
		sa, sb := pair(t, psk)
		msg, err := sa.Seal(nil, InnerIP, []byte("payload"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(msg, []byte("payload")) {
			t.Fatal("plaintext visible on the wire")
		}
		typ, body, err := sb.Open(msg)
		if err != nil || typ != InnerIP || string(body) != "payload" {
			t.Fatalf("open: %v %d %q", err, typ, body)
		}
		back, _ := sb.Seal(nil, InnerPong, []byte("x"))
		if _, _, err := sa.Open(back); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplayRejected(t *testing.T) {
	sa, sb := pair(t, nil)
	msg, _ := sa.Seal(nil, InnerIP, []byte("x"))
	if _, _, err := sb.Open(msg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sb.Open(msg); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay accepted: %v", err)
	}
}

func TestTamperRejected(t *testing.T) {
	sa, sb := pair(t, nil)
	msg, _ := sa.Seal(nil, InnerIP, []byte("abc"))
	for i := range msg {
		m := append([]byte(nil), msg...)
		m[i] ^= 0x40
		if _, _, err := sb.Open(m); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	if _, _, err := sb.Open(msg); err != nil {
		t.Fatalf("original rejected after tamper attempts: %v", err)
	}
}

func TestOutOfOrderWithinWindow(t *testing.T) {
	sa, sb := pair(t, nil)
	var msgs [][]byte
	for i := 0; i < 100; i++ {
		m, _ := sa.Seal(nil, InnerIP, []byte{byte(i)})
		msgs = append(msgs, m)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, _, err := sb.Open(msgs[i]); err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
	}
}

func TestUnauthorizedPeerRejected(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, nil, func([]byte) bool { return false })
	_, m1, _ := Initiate(a, b.Public, nil, "x", false)
	if _, _, err := resp.Respond(m1); !errors.Is(err, ErrAuth) {
		t.Fatalf("unauthorized accepted: %v", err)
	}
}

func TestWrongResponderKeyFails(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	imposter, _ := GenerateKeyPair()
	resp := NewResponder(imposter, nil, func([]byte) bool { return true })
	_, m1, _ := Initiate(a, b.Public, nil, "x", false)
	if _, _, err := resp.Respond(m1); err == nil {
		t.Fatal("imposter decrypted initiation")
	}
}

func TestPSKMismatchFails(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, bytes.Repeat([]byte{1}, 32), func([]byte) bool { return true })
	in, m1, _ := Initiate(a, b.Public, bytes.Repeat([]byte{2}, 32), "x", false)
	// psk2 is mixed after message 1, so the mismatch is detected when the
	// initiator authenticates the response; no session is established.
	_, m2, err := resp.Respond(m1)
	if err != nil {
		return
	}
	if _, err := in.Finish(m2); err == nil {
		t.Fatal("psk mismatch accepted")
	}
}

func TestHandshakeReplayRejected(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, nil, func([]byte) bool { return true })
	_, m1, _ := Initiate(a, b.Public, nil, "x", true)
	s, _, err := resp.Respond(m1)
	if err != nil || !s.Probe {
		t.Fatalf("first: %v", err)
	}
	if _, _, err := resp.Respond(m1); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed initiation accepted: %v", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	_, sb := pair(t, nil)
	for _, m := range [][]byte{nil, {TypeData}, make([]byte, 20), append([]byte{TypeData}, make([]byte, 40)...)} {
		if _, _, err := sb.Open(m); err == nil {
			t.Fatalf("accepted %x", m)
		}
	}
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, nil, func([]byte) bool { return true })
	for _, m := range [][]byte{nil, {TypeHandshakeInit}, make([]byte, 100), append([]byte{TypeHandshakeInit}, make([]byte, 600)...)} {
		if _, _, err := resp.Respond(m); err == nil {
			t.Fatal("malformed handshake accepted")
		}
	}
}

func TestPublicFromPrivate(t *testing.T) {
	k, _ := GenerateKeyPair()
	pub, err := PublicFromPrivate(k.Private)
	if err != nil || !bytes.Equal(pub, k.Public) {
		t.Fatal("public key derivation mismatch")
	}
}

func FuzzOpen(f *testing.F) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, nil, func([]byte) bool { return true })
	in, m1, _ := Initiate(a, b.Public, nil, "f", false)
	sb, m2, _ := resp.Respond(m1)
	sa, _ := in.Finish(m2)
	seed, _ := sa.Seal(nil, InnerIP, []byte("seed"))
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) { _, _, _ = sb.Open(data) })
}

func TestHandshakeOutOfOrderAccepted(t *testing.T) {
	a, _ := GenerateKeyPair()
	b, _ := GenerateKeyPair()
	resp := NewResponder(b, nil, func([]byte) bool { return true })
	_, m1, _ := Initiate(a, b.Public, nil, "x", false)
	_, m2, _ := Initiate(a, b.Public, nil, "x", false)
	if _, _, err := resp.Respond(m2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resp.Respond(m1); err != nil {
		t.Fatalf("older in-window initiation rejected: %v", err)
	}
	if _, _, err := resp.Respond(m1); err == nil {
		t.Fatal("replay accepted")
	}
}

func TestSealInPlace(t *testing.T) {
	sa, sb := pair(t, nil)
	body := []byte("an inner ip packet")
	buf := make([]byte, SealHeadroom+len(body), SealHeadroom+len(body)+16)
	copy(buf[SealHeadroom:], body)
	msg, err := sa.SealInPlace(buf, InnerIP)
	if err != nil {
		t.Fatal(err)
	}
	if &msg[0] != &buf[0] {
		t.Fatal("SealInPlace reallocated despite enough capacity")
	}
	ref, _ := sa.Seal(nil, InnerIP, body) // the formats must be identical
	if len(ref) != len(msg) {
		t.Fatalf("length %d != %d", len(msg), len(ref))
	}
	typ, got, err := sb.Open(msg)
	if err != nil || typ != InnerIP || string(got) != string(body) {
		t.Fatalf("open: %v %d %q", err, typ, got)
	}
	if _, _, err := sb.Open(ref); err != nil {
		t.Fatalf("open reference: %v", err)
	}
}

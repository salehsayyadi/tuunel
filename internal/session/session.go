// Package session implements the carrier-independent secure session layer:
// a Noise IK handshake (github.com/flynn/noise, Curve25519, ChaCha20-Poly1305,
// BLAKE2s) followed by explicit-nonce AEAD data messages with 64-bit counters
// and a sliding replay window. See PROTOCOL.md.
package session

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
)

// Message types (first byte of every session message).
const (
	TypeHandshakeInit byte = 1
	TypeHandshakeResp byte = 2
	TypeData          byte = 3
)

// Inner (encrypted) payload types.
const (
	InnerIP    byte = 1
	InnerPing  byte = 2
	InnerPong  byte = 3
	InnerClose byte = 4
)

const (
	// DataHeaderLen is type(1) + receiver index(4) + counter(8).
	DataHeaderLen = 13
	// Overhead is the total per-packet session overhead for an IP packet:
	// header + inner type + Poly1305 tag.
	Overhead = DataHeaderLen + 1 + 16
	// RekeyAfterMessages triggers a new handshake well before counter limits.
	RekeyAfterMessages = 1 << 48
	// RejectAfterMessages is a hard limit on counter use for one key.
	RejectAfterMessages = 1 << 60
)

var (
	ErrReplay     = errors.New("session: replayed or stale counter")
	ErrAuth       = errors.New("session: authentication failed")
	ErrMalformed  = errors.New("session: malformed message")
	ErrExhausted  = errors.New("session: counter exhausted, rekey required")
	ErrWrongIndex = errors.New("session: unknown receiver index")
	ErrExpired    = errors.New("session: key expired")
)

// Session holds the transport keys of one completed handshake.
type Session struct {
	LocalIndex  uint32
	RemoteIndex uint32
	PeerKey     []byte
	PeerNodeID  string
	Probe       bool
	Initiator   bool
	Created     time.Time

	send, recv noise.Cipher
	counter    atomic.Uint64
	mu         sync.Mutex
	replay     ReplayWindow
}

// Seal encrypts an inner payload into a data message appended to dst.
func (s *Session) Seal(dst []byte, inner byte, body []byte) ([]byte, error) {
	n := s.counter.Add(1) - 1
	if n >= RejectAfterMessages {
		return nil, ErrExhausted
	}
	var hdr [DataHeaderLen]byte
	hdr[0] = TypeData
	binary.BigEndian.PutUint32(hdr[1:5], s.RemoteIndex)
	binary.BigEndian.PutUint64(hdr[5:13], n)
	dst = append(dst, hdr[:]...)
	pt := make([]byte, 1+len(body))
	pt[0] = inner
	copy(pt[1:], body)
	return s.send.Encrypt(dst, n, hdr[:], pt), nil
}

// SealHeadroom is the space SealInPlace needs in front of the body.
const SealHeadroom = DataHeaderLen + 1

// SealInPlace encrypts buf[SealHeadroom:] (an inner payload of type inner)
// in place, writing the data header into buf[:SealHeadroom]. With
// cap(buf) >= len(buf)+16 no allocation or copy takes place. The returned
// message aliases buf.
func (s *Session) SealInPlace(buf []byte, inner byte) ([]byte, error) {
	if len(buf) < SealHeadroom {
		return nil, ErrMalformed
	}
	n := s.counter.Add(1) - 1
	if n >= RejectAfterMessages {
		return nil, ErrExhausted
	}
	buf[0] = TypeData
	binary.BigEndian.PutUint32(buf[1:5], s.RemoteIndex)
	binary.BigEndian.PutUint64(buf[5:13], n)
	buf[13] = inner
	return s.send.Encrypt(buf[:DataHeaderLen], n, buf[:DataHeaderLen], buf[DataHeaderLen:]), nil
}

// Open authenticates and decrypts a data message, enforcing replay protection.
// Decryption happens in place: msg is overwritten and the returned body
// aliases msg[SealHeadroom:].
func (s *Session) Open(msg []byte) (byte, []byte, error) {
	if len(msg) < DataHeaderLen+1+16 || msg[0] != TypeData {
		return 0, nil, ErrMalformed
	}
	if binary.BigEndian.Uint32(msg[1:5]) != s.LocalIndex {
		return 0, nil, ErrWrongIndex
	}
	n := binary.BigEndian.Uint64(msg[5:13])
	if n >= RejectAfterMessages {
		return 0, nil, ErrReplay
	}
	s.mu.Lock()
	ok := s.replay.Check(n)
	s.mu.Unlock()
	if !ok {
		return 0, nil, ErrReplay
	}
	pt, err := s.recv.Decrypt(msg[DataHeaderLen:DataHeaderLen], n, msg[:DataHeaderLen], msg[DataHeaderLen:])
	if err != nil {
		return 0, nil, ErrAuth
	}
	s.mu.Lock()
	ok = s.replay.Update(n)
	s.mu.Unlock()
	if !ok { // concurrent duplicate raced us
		return 0, nil, ErrReplay
	}
	if len(pt) < 1 {
		return 0, nil, ErrMalformed
	}
	return pt[0], pt[1:], nil
}

// SentMessages returns the number of messages sealed so far.
func (s *Session) SentMessages() uint64 { return s.counter.Load() }

// ReceiverIndex extracts the receiver index from a data message.
func ReceiverIndex(msg []byte) (uint32, bool) {
	if len(msg) < DataHeaderLen || msg[0] != TypeData {
		return 0, false
	}
	return binary.BigEndian.Uint32(msg[1:5]), true
}

func randomIndex() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("session: random index: %w", err)
	}
	v := binary.BigEndian.Uint32(b[:])
	if v == 0 {
		v = 1
	}
	return v, nil
}

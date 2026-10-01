package session

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/flynn/noise"
)

// ProtocolVersion is carried inside the encrypted handshake payload.
const ProtocolVersion = 1

var prologue = []byte("tuunel/1 Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s")

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// Flags carried in the handshake payload.
const (
	FlagProbe byte = 1 << 0 // measurement session; must not replace the active link
)

// KeyPair is a Curve25519 static identity.
type KeyPair struct {
	Private []byte
	Public  []byte
}

// GenerateKeyPair creates a new static identity from crypto/rand.
func GenerateKeyPair() (KeyPair, error) {
	k, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: k.Private, Public: k.Public}, nil
}

// PublicFromPrivate derives the public key from a 32-byte private key.
func PublicFromPrivate(priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("session: private key must be 32 bytes")
	}
	k, err := suite.GenerateKeypair(bytes.NewReader(priv))
	if err != nil {
		return nil, err
	}
	return k.Public, nil
}

func newState(local KeyPair, initiator bool, peer []byte, psk []byte) (*noise.HandshakeState, error) {
	cfg := noise.Config{
		CipherSuite:   suite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		Prologue:      prologue,
		StaticKeypair: noise.DHKey{Private: local.Private, Public: local.Public},
		PeerStatic:    peer,
		// psk2 mixes an optional pre-shared key; an all-zero key is used when
		// none is configured (per the Noise spec this is still well-defined).
		PresharedKey:          make([]byte, 32),
		PresharedKeyPlacement: 2,
	}
	if len(psk) == 32 {
		cfg.PresharedKey = psk
	} else if len(psk) != 0 {
		return nil, errors.New("session: pre-shared key must be 32 bytes")
	}
	return noise.NewHandshakeState(cfg)
}

// Initiation is an in-flight initiator handshake.
type Initiation struct {
	hs      *noise.HandshakeState
	index   uint32
	peerKey []byte
	probe   bool
	Started time.Time
}

// Initiate builds a handshake-init message for the responder's static key.
func Initiate(local KeyPair, peerPublic, psk []byte, nodeID string, probe bool) (*Initiation, []byte, error) {
	if len(nodeID) > 64 {
		return nil, nil, errors.New("session: node id too long")
	}
	hs, err := newState(local, true, peerPublic, psk)
	if err != nil {
		return nil, nil, err
	}
	idx, err := randomIndex()
	if err != nil {
		return nil, nil, err
	}
	var flags byte
	if probe {
		flags |= FlagProbe
	}
	payload := make([]byte, 0, 15+len(nodeID))
	payload = append(payload, ProtocolVersion, flags)
	payload = binary.BigEndian.AppendUint32(payload, idx)
	payload = binary.BigEndian.AppendUint64(payload, uint64(time.Now().UnixNano()))
	payload = append(payload, byte(len(nodeID)))
	payload = append(payload, nodeID...)
	msg, _, _, err := hs.WriteMessage([]byte{TypeHandshakeInit}, payload)
	if err != nil {
		return nil, nil, err
	}
	return &Initiation{hs: hs, index: idx, peerKey: peerPublic, probe: probe, Started: time.Now()}, msg, nil
}

// Finish consumes the responder's reply and returns the established session.
func (in *Initiation) Finish(msg []byte) (*Session, error) {
	if len(msg) < 2 || msg[0] != TypeHandshakeResp {
		return nil, ErrMalformed
	}
	payload, csA, csB, err := in.hs.ReadMessage(nil, msg[1:])
	if err != nil {
		return nil, ErrAuth
	}
	if len(payload) != 6 || payload[0] != ProtocolVersion || csA == nil || csB == nil {
		return nil, ErrMalformed
	}
	return &Session{
		LocalIndex:  in.index,
		RemoteIndex: binary.BigEndian.Uint32(payload[2:6]),
		PeerKey:     in.peerKey,
		Probe:       in.probe,
		Initiator:   true,
		Created:     time.Now(),
		send:        csA.Cipher(),
		recv:        csB.Cipher(),
	}, nil
}

// Authorizer decides whether an initiator's static key is permitted.
type Authorizer func(peerPublic []byte) bool

// TimestampWindow bounds how far behind the newest accepted initiation an
// initiation timestamp may be. Initiations inside the window are accepted at
// most once; older ones are rejected as replays. This allows concurrent
// initiations over different carriers to arrive out of order.
const TimestampWindow = 60 * time.Second

type tsWindow struct {
	max  uint64
	seen map[uint64]struct{}
}

// Responder handles handshake-init messages and rejects replayed initiations.
type Responder struct {
	local     KeyPair
	psk       []byte
	authorize Authorizer
	mu        sync.Mutex
	ts        map[string]*tsWindow
}

func NewResponder(local KeyPair, psk []byte, auth Authorizer) *Responder {
	return &Responder{local: local, psk: psk, authorize: auth, ts: make(map[string]*tsWindow)}
}

// acceptTimestamp records ts for peer and reports whether it is fresh.
func (r *Responder) acceptTimestamp(peer string, ts uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.ts[peer]
	if w == nil {
		if len(r.ts) > 4096 { // bounded by the number of authorized peers in practice
			return false
		}
		w = &tsWindow{seen: make(map[uint64]struct{})}
		r.ts[peer] = w
	}
	win := uint64(TimestampWindow)
	if w.max > win && ts < w.max-win {
		return false
	}
	if _, dup := w.seen[ts]; dup {
		return false
	}
	w.seen[ts] = struct{}{}
	if ts > w.max {
		w.max = ts
	}
	if len(w.seen) > 1024 {
		for k := range w.seen {
			if w.max > win && k < w.max-win || len(w.seen) > 1024 {
				delete(w.seen, k)
			}
		}
	}
	return true
}

// Respond validates msg and returns the session plus the reply to send.
func (r *Responder) Respond(msg []byte) (*Session, []byte, error) {
	if len(msg) < 2 || msg[0] != TypeHandshakeInit || len(msg) > 512 {
		return nil, nil, ErrMalformed
	}
	hs, err := newState(r.local, false, nil, r.psk)
	if err != nil {
		return nil, nil, err
	}
	payload, _, _, err := hs.ReadMessage(nil, msg[1:])
	if err != nil {
		return nil, nil, ErrAuth
	}
	peer := hs.PeerStatic()
	if len(peer) != 32 || !r.authorize(peer) {
		return nil, nil, fmt.Errorf("%w: unauthorized peer key", ErrAuth)
	}
	if len(payload) < 15 || payload[0] != ProtocolVersion {
		return nil, nil, ErrMalformed
	}
	flags := payload[1]
	remoteIdx := binary.BigEndian.Uint32(payload[2:6])
	ts := binary.BigEndian.Uint64(payload[6:14])
	idLen := int(payload[14])
	if len(payload) != 15+idLen || remoteIdx == 0 {
		return nil, nil, ErrMalformed
	}
	nodeID := string(payload[15:])
	if !r.acceptTimestamp(string(peer), ts) {
		return nil, nil, fmt.Errorf("%w: replayed handshake timestamp", ErrReplay)
	}
	idx, err := randomIndex()
	if err != nil {
		return nil, nil, err
	}
	reply := []byte{ProtocolVersion, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(reply[2:6], idx)
	out, csA, csB, err := hs.WriteMessage([]byte{TypeHandshakeResp}, reply)
	if err != nil || csA == nil || csB == nil {
		return nil, nil, fmt.Errorf("session: responder write: %v", err)
	}
	return &Session{
		LocalIndex:  idx,
		RemoteIndex: remoteIdx,
		PeerKey:     append([]byte(nil), peer...),
		PeerNodeID:  sanitize(nodeID),
		Probe:       flags&FlagProbe != 0,
		Created:     time.Now(),
		send:        csB.Cipher(),
		recv:        csA.Cipher(),
	}, out, nil
}

// KeysEqual compares keys in constant time.
func KeysEqual(a, b []byte) bool { return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1 }

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

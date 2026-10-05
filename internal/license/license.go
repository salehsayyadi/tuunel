// Package license implements optional activation licensing.
//
// A build is "licensed" when a license server's Ed25519 public key is
// embedded in the binary (the license server patches the placeholder below
// into the binaries it distributes; plain source builds stay unlicensed and
// unrestricted). A licensed daemon only runs while a valid license file
// signed by that key exists for this machine:
//
//	TUL1.<base64url(payload JSON)>.<base64url(Ed25519 signature)>
//
// The signature covers "TUL1." + the encoded payload. The daemon verifies the
// file offline (signature, machine, expiry); "tuunel license sync" (root,
// systemd timer) refreshes it online, which also delivers revocation,
// extensions, the server's clock and the peer's public key.
package license

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// pubMarker holds the embedded public key: the 22-byte tag followed by 44
// base64 characters. Distribution servers overwrite the 44 '*' characters in
// the binary with their key, so the length must never change.
var pubMarker = "TUUNEL-LICENSE-PUBKEY:" + "********************************************"

const tag = "TUUNEL-LICENSE-PUBKEY:"

// Paths used on installed systems.
var (
	File = "/etc/tuunel/license"
	// MachineFiles are tried in order; the installer creates the last one on
	// hosts without a systemd machine id. TUUNEL_MACHINE_ID_FILE overrides
	// (tests, containers).
	MachineFiles = []string{"/etc/machine-id", "/var/lib/dbus/machine-id", "/etc/tuunel/machine-id"}
	ClockFile    = "/var/lib/tuunel/license.clock" // newest server time seen (anti clock rollback)
)

// Payload is the signed license content.
type Payload struct {
	ID      int64  `json:"lid"`  // license (code) id
	Machine string `json:"m"`    // machine fingerprint
	Role    string `json:"r"`    // edge | remote
	Issued  int64  `json:"iat"`  // server time at issue (unix)
	Expires int64  `json:"exp"`  // unix
	Server  string `json:"srv"`  // license server base URL
	Note    string `json:"note"` // shown in status
}

// PublicKey returns the embedded key, or nil for unlicensed builds.
func PublicKey() ed25519.PublicKey {
	s := pubMarker[len(tag):]
	if strings.HasPrefix(s, "*") {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(b)
}

// Required reports whether this binary enforces licensing. A binary whose
// marker was damaged (neither placeholder nor a valid key) also requires a
// license, which then can never verify: tampering fails closed.
func Required() bool {
	return !strings.HasPrefix(pubMarker[len(tag):], "*")
}

// Machine returns this host's fingerprint: sha256("tuunel:"+machine-id),
// first 32 hex characters (the installer computes the same in shell).
func Machine() (string, error) {
	files := MachineFiles
	if f := os.Getenv("TUUNEL_MACHINE_ID_FILE"); f != "" {
		files = []string{f}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(b)); id != "" {
			h := sha256.Sum256([]byte("tuunel:" + id))
			return hex.EncodeToString(h[:])[:32], nil
		}
	}
	return "", errors.New("license: no machine id (" + strings.Join(files, ", ") + ")")
}

// Parse verifies a license token against key and returns its payload. It
// does not check expiry or machine.
func Parse(token string, key ed25519.PublicKey) (*Payload, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "TUL1" {
		return nil, errors.New("license: malformed license")
	}
	if key == nil {
		return nil, errors.New("license: this build has no valid license key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("license: invalid signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("license: malformed payload")
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("license: malformed payload")
	}
	return &p, nil
}

// Status is the result of a local license check.
type Status struct {
	Required bool
	Valid    bool
	Reason   string
	Payload  *Payload
	Left     time.Duration
}

// Now returns the effective time: the local clock, but never earlier than
// the newest server time recorded (a rolled-back clock cannot extend a
// license).
func Now() time.Time {
	now := time.Now()
	if b, err := os.ReadFile(ClockFile); err == nil {
		var ts int64
		if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &ts); err == nil {
			if t := time.Unix(ts, 0); t.After(now) {
				return t
			}
		}
	}
	return now
}

// Check verifies the installed license for this machine.
func Check() Status {
	st := Status{Required: Required()}
	if !st.Required {
		st.Valid = true
		return st
	}
	b, err := os.ReadFile(File)
	if err != nil {
		st.Reason = "no license installed (" + File + ")"
		return st
	}
	p, err := Parse(firstLine(string(b)), PublicKey())
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	st.Payload = p
	m, err := Machine()
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	if p.Machine != m {
		st.Reason = "license belongs to another server"
		return st
	}
	now := Now()
	if iat := time.Unix(p.Issued, 0); iat.After(now) {
		now = iat
	}
	st.Left = time.Unix(p.Expires, 0).Sub(now)
	if st.Left <= 0 {
		st.Reason = "license expired at " + time.Unix(p.Expires, 0).UTC().Format(time.RFC3339)
		return st
	}
	st.Valid = true
	return st
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

// RecordServerTime stores a server timestamp if it is newer than the stored one.
func RecordServerTime(ts int64) {
	if ts <= 0 {
		return
	}
	if b, err := os.ReadFile(ClockFile); err == nil {
		var old int64
		if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &old); err == nil && old >= ts {
			return
		}
	}
	_ = os.WriteFile(ClockFile, []byte(fmt.Sprintf("%d\n", ts)), 0o644)
}

package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/salehsayyadi/tuunel/internal/session"
)

// LoadPrivateKey reads a base64 Curve25519 private key and warns about
// permissive file modes via the returned warning string.
func LoadPrivateKey(path string) (session.KeyPair, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return session.KeyPair{}, "", err
	}
	warn := ""
	if st.Mode().Perm()&0o077 != 0 {
		warn = fmt.Sprintf("private key %s has permissive mode %o; use chmod 600", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return session.KeyPair{}, warn, err
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(priv) != 32 {
		return session.KeyPair{}, warn, fmt.Errorf("private key %s: must be 32 bytes base64", path)
	}
	pub, err := session.PublicFromPrivate(priv)
	if err != nil {
		return session.KeyPair{}, warn, err
	}
	return session.KeyPair{Private: priv, Public: pub}, warn, nil
}

func LoadPSK(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("pre-shared key %s: must be 32 bytes base64", path)
	}
	return k, nil
}

func GenKey() (priv, pub string, err error) {
	k, err := session.GenerateKeyPair()
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.Private), base64.StdEncoding.EncodeToString(k.Public), nil
}

func GenPSK() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

package carrier

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// ServerTLS returns the outer TLS configuration for QUIC/WSS listeners. When
// no certificate is configured an ephemeral self-signed certificate is used.
// Outer TLS is not relied upon for tunnel security: the inner Noise session
// authenticates both nodes and encrypts every payload.
func ServerTLS(o Options, alpn []string) (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	if o.TLSCertFile != "" || o.TLSKeyFile != "" {
		cert, err = tls.LoadX509KeyPair(o.TLSCertFile, o.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("carrier: load TLS certificate: %w", err)
		}
	} else {
		cert, err = ephemeralCert()
		if err != nil {
			return nil, err
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: alpn}, nil
}

// ClientTLS returns the outer TLS client configuration. If a CA file is
// configured the server chain and name are verified; otherwise verification
// is skipped and the inner session's mutual authentication is authoritative.
func ClientTLS(o Options, alpn []string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: alpn, ServerName: o.TLSServerName}
	if o.TLSCAFile != "" && !o.TLSInsecure {
		pem, err := os.ReadFile(o.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("carrier: read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("carrier: TLS CA file has no certificates")
		}
		cfg.RootCAs = pool
		return cfg, nil
	}
	cfg.InsecureSkipVerify = true //nolint:gosec // inner Noise IK session authenticates the peer
	return cfg, nil
}

func ephemeralCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tuunel"},
		DNSNames:     []string{"tuunel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

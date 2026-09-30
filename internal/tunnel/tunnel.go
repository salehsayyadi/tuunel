package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/salehsayyadi/tuunel/internal/framing"
)

const queueDepth = 128

type Config struct {
	Tun *os.File
	MTU int
}

// RunServer accepts one authenticated peer at a time and reaccepts after drops.
func RunServer(ctx context.Context, listen string, tlsConfig *tls.Config, cfg Config) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	packets := make(chan []byte, queueDepth)
	startTunReader(ctx, cfg, packets)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if err := servePeer(ctx, tls.Server(conn, tlsConfig), cfg, packets); err != nil && ctx.Err() == nil {
			slog.Warn("peer session ended", "error", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// RunClient reconnects with bounded exponential backoff, retaining the same TUN.
func RunClient(ctx context.Context, endpoint string, tlsConfig *tls.Config, cfg Config) error {
	packets := make(chan []byte, queueDepth)
	startTunReader(ctx, cfg, packets)
	backoff := time.Second
	for ctx.Err() == nil {
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", endpoint)
		if err == nil {
			err = servePeer(ctx, tls.Client(conn, tlsConfig), cfg, packets)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("tunnel session ended; reconnecting", "error", err, "backoff", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return nil
}

func TLSConfig(certFile, keyFile, caFile, peerName string, server bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load node certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file contains no valid certificates")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: peerName}
	if server {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = pool
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("client certificate is required")
			}
			if err := state.PeerCertificates[0].VerifyHostname(peerName); err != nil {
				return fmt.Errorf("client identity mismatch: %w", err)
			}
			return nil
		}
	}
	return cfg, nil
}

func startTunReader(ctx context.Context, cfg Config, packets chan<- []byte) {
	go func() {
		defer close(packets)
		buf := make([]byte, cfg.MTU+1)
		for {
			n, err := cfg.Tun.Read(buf)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("TUN read failed", "error", err)
				}
				return
			}
			p := append([]byte(nil), buf[:n]...)
			if err := framing.ValidateIPPacket(p, cfg.MTU); err != nil {
				continue
			}
			select {
			case packets <- p:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func servePeer(ctx context.Context, conn *tls.Conn, cfg Config, packets <-chan []byte) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS mutual-auth handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writeDone := make(chan error, 1)
	go func() {
		enc := framing.NewEncoder()
		for {
			select {
			case <-ctx.Done():
				writeDone <- ctx.Err()
				return
			case p, ok := <-packets:
				if !ok {
					writeDone <- errors.New("TUN reader stopped")
					return
				}
				if err := enc.WritePacket(conn, p, cfg.MTU); err != nil {
					writeDone <- err
					return
				}
			}
		}
	}()
	readDone := make(chan error, 1)
	go func() {
		dec := framing.NewDecoder()
		for {
			p, err := dec.ReadPacket(conn, cfg.MTU)
			if err != nil {
				readDone <- err
				return
			}
			if _, err := cfg.Tun.Write(p); err != nil {
				readDone <- err
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		_ = conn.Close()
		return nil
	case err := <-writeDone:
		_ = conn.Close()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case err := <-readDone:
		_ = conn.Close()
		return err
	}
}

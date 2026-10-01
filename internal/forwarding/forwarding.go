// Package forwarding implements TCP and UDP port forwarding. Rules are
// reconciled by Apply so they can be changed at runtime (SIGHUP) without
// disturbing unchanged rules. Targets are normally tunnel addresses, which
// makes forwarding work in both directions, including reverse-tunnel mode.
package forwarding

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Rule struct {
	Proto          string // "tcp" or "udp"
	Listen         string
	Target         string
	MaxConnections int
}

func (r Rule) key() string { return r.Proto + "|" + r.Listen + "|" + r.Target }

type RuleStatus struct {
	Proto    string `json:"proto"`
	Listen   string `json:"listen"`
	Target   string `json:"target"`
	Active   int64  `json:"active"`
	Total    uint64 `json:"total"`
	Rejected uint64 `json:"rejected"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
	Error    string `json:"error,omitempty"`
}

// UDPIdleTimeout ends UDP flows without traffic.
var UDPIdleTimeout = 60 * time.Second

type fwd struct {
	rule                     Rule
	cancel                   context.CancelFunc
	done                     chan struct{}
	addr                     net.Addr
	active                   atomic.Int64
	total, rejected, in, out atomic.Uint64
	err                      string
}

type Manager struct {
	log   *slog.Logger
	mu    sync.Mutex
	rules map[string]*fwd
}

func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, rules: map[string]*fwd{}}
}

// Apply reconciles running forwarders with rules and returns the first error.
func (m *Manager) Apply(ctx context.Context, rules []Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]Rule{}
	for _, r := range rules {
		want[r.key()] = r
	}
	for k, f := range m.rules {
		if _, ok := want[k]; !ok {
			f.cancel()
			<-f.done
			delete(m.rules, k)
			m.log.Info("forwarding removed", "proto", f.rule.Proto, "listen", f.rule.Listen, "target", f.rule.Target)
		}
	}
	var firstErr error
	for k, r := range want {
		if _, ok := m.rules[k]; ok {
			continue
		}
		f, err := start(ctx, r, m.log)
		if err != nil {
			m.log.Error("forwarding failed", "proto", r.Proto, "listen", r.Listen, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.rules[k] = f
		m.log.Info("forwarding added", "proto", r.Proto, "listen", f.addr.String(), "target", r.Target)
	}
	return firstErr
}

func (m *Manager) Close() { _ = m.Apply(context.Background(), nil) }

func (m *Manager) Status() []RuleStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RuleStatus
	for _, f := range m.rules {
		out = append(out, RuleStatus{Proto: f.rule.Proto, Listen: f.addr.String(), Target: f.rule.Target, Active: f.active.Load(),
			Total: f.total.Load(), Rejected: f.rejected.Load(), BytesIn: f.in.Load(), BytesOut: f.out.Load(), Error: f.err})
	}
	return out
}

// Addr returns the bound address of a rule (useful with port 0).
func (m *Manager) Addr(r Rule) net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.rules[r.key()]; ok {
		return f.addr
	}
	return nil
}

func start(parent context.Context, r Rule, log *slog.Logger) (*fwd, error) {
	ctx, cancel := context.WithCancel(parent)
	f := &fwd{rule: r, cancel: cancel, done: make(chan struct{})}
	if r.MaxConnections <= 0 {
		f.rule.MaxConnections = 1024
	}
	switch r.Proto {
	case "tcp":
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", r.Listen)
		if err != nil {
			cancel()
			return nil, err
		}
		f.addr = ln.Addr()
		go f.serveTCP(ctx, ln, log)
	case "udp":
		var lc net.ListenConfig
		pc, err := lc.ListenPacket(ctx, "udp", r.Listen)
		if err != nil {
			cancel()
			return nil, err
		}
		f.addr = pc.LocalAddr()
		go f.serveUDP(ctx, pc.(*net.UDPConn), log)
	default:
		cancel()
		return nil, errors.New("forwarding: unknown protocol " + r.Proto)
	}
	return f, nil
}

func (f *fwd) serveTCP(ctx context.Context, ln net.Listener, log *slog.Logger) {
	defer close(f.done)
	var wg sync.WaitGroup
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if f.active.Load() >= int64(f.rule.MaxConnections) {
			f.rejected.Add(1)
			c.Close()
			continue
		}
		f.active.Add(1)
		f.total.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer f.active.Add(-1)
			f.proxyTCP(ctx, c, log)
		}()
	}
}

func (f *fwd) proxyTCP(ctx context.Context, c net.Conn, log *slog.Logger) {
	defer c.Close()
	d := net.Dialer{Timeout: 10 * time.Second}
	up, err := d.DialContext(ctx, "tcp", f.rule.Target)
	if err != nil {
		log.Debug("forward dial failed", "target", f.rule.Target, "error", err)
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { c.Close(); up.Close() })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn, ctr *atomic.Uint64) {
		defer wg.Done()
		n, _ := io.Copy(dst, src)
		ctr.Add(uint64(n))
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go pipe(up, c, &f.in)
	go pipe(c, up, &f.out)
	wg.Wait()
}

type udpFlow struct {
	up   *net.UDPConn
	last atomic.Int64
}

func (f *fwd) serveUDP(ctx context.Context, pc *net.UDPConn, log *slog.Logger) {
	defer close(f.done)
	var mu sync.Mutex
	flows := map[string]*udpFlow{}
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		pc.Close()
		mu.Lock()
		for _, fl := range flows {
			fl.up.Close()
		}
		mu.Unlock()
	}()
	target, terr := net.ResolveUDPAddr("udp", f.rule.Target)
	buf := make([]byte, 65535)
	for {
		n, client, err := pc.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return
			}
			continue
		}
		if terr != nil {
			target, terr = net.ResolveUDPAddr("udp", f.rule.Target)
			if terr != nil {
				continue
			}
		}
		key := client.String()
		mu.Lock()
		fl := flows[key]
		if fl == nil {
			if len(flows) >= f.rule.MaxConnections {
				mu.Unlock()
				f.rejected.Add(1)
				continue
			}
			up, err := net.DialUDP("udp", nil, target)
			if err != nil {
				mu.Unlock()
				continue
			}
			fl = &udpFlow{up: up}
			flows[key] = fl
			f.total.Add(1)
			f.active.Add(1)
			wg.Add(1)
			go func(client *net.UDPAddr, fl *udpFlow) {
				defer wg.Done()
				defer f.active.Add(-1)
				rb := make([]byte, 65535)
				for {
					_ = fl.up.SetReadDeadline(time.Now().Add(UDPIdleTimeout))
					n, err := fl.up.Read(rb)
					if err != nil {
						var ne net.Error
						idle := errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, fl.last.Load())) < UDPIdleTimeout
						if idle {
							continue
						}
						break
					}
					fl.last.Store(time.Now().UnixNano())
					f.out.Add(uint64(n))
					_, _ = pc.WriteToUDP(rb[:n], client)
				}
				mu.Lock()
				if flows[client.String()] == fl {
					delete(flows, client.String())
				}
				mu.Unlock()
				fl.up.Close()
			}(client, fl)
		}
		mu.Unlock()
		fl.last.Store(time.Now().UnixNano())
		f.in.Add(uint64(n))
		_, _ = fl.up.Write(buf[:n])
	}
}

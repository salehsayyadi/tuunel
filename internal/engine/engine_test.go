package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/faulty"
	"github.com/salehsayyadi/tuunel/internal/carrier/quic"
	"github.com/salehsayyadi/tuunel/internal/carrier/tcp"
	"github.com/salehsayyadi/tuunel/internal/carrier/udp"
	"github.com/salehsayyadi/tuunel/internal/carrier/websocket"
	"github.com/salehsayyadi/tuunel/internal/failover"
	"github.com/salehsayyadi/tuunel/internal/health"
	"github.com/salehsayyadi/tuunel/internal/session"
)

func logger() *slog.Logger {
	if os.Getenv("TUUNEL_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freePort(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type node struct {
	e   *Engine
	dev *memDev
}

func fastHealth(c *Config) {
	c.HealthInterval = 100 * time.Millisecond
	c.Health = health.Thresholds{Window: 10, PingTimeout: 300 * time.Millisecond, DegradedLoss: 20, FailedLoss: 60, FailedMissed: 3, ClearRatio: 0.5, MinSamples: 5}
	c.IdleTimeout = 2 * time.Second
	c.HandshakeTimeout = time.Second
	c.ProbeInterval = 300 * time.Millisecond
	c.FailoverEnabled = true
	p := failover.DefaultPolicy()
	p.BackoffInitial = 100 * time.Millisecond
	p.BackoffMax = 500 * time.Millisecond
	p.MinHold = 500 * time.Millisecond
	p.RecoverySuccesses = 2
	c.Failover = p
}

// startPair builds node A (initiator, 10.200.0.1) and node B (responder, 10.200.0.2).
func startPair(t *testing.T, carriers []carrier.Carrier, addrs []string, tweak func(a, b *Config)) (*node, *node) {
	t.Helper()
	ka, _ := session.GenerateKeyPair()
	kb, _ := session.GenerateKeyPair()
	devA, devB := newMemDev(), newMemDev()
	cb := Config{NodeID: "node-b", Key: kb, Device: devB, MTU: 1400, LocalAddrs: []netip.Addr{netip.MustParseAddr("10.200.0.2"), netip.MustParseAddr("fd00::2")}, Logger: logger(),
		Peers: []PeerConfig{{Name: "a", PublicKey: ka.Public, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.200.0.1/32"), netip.MustParsePrefix("fd00::1/128")}}}}
	ca := Config{NodeID: "node-a", Key: ka, Device: devA, MTU: 1400, LocalAddrs: []netip.Addr{netip.MustParseAddr("10.200.0.1"), netip.MustParseAddr("fd00::1")}, Logger: logger()}
	pa := PeerConfig{Name: "b", PublicKey: kb.Public, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.200.0.0/30"), netip.MustParsePrefix("fd00::/64")}}
	for i, c := range carriers {
		cb.Listeners = append(cb.Listeners, ListenerConfig{Carrier: c, Address: addrs[i]})
		pa.Candidates = append(pa.Candidates, CandidateConfig{Endpoint: "ep", Carrier: c, CarrierRank: i, Address: addrs[i]})
	}
	ca.Peers = []PeerConfig{pa}
	fastHealth(&ca)
	fastHealth(&cb)
	if tweak != nil {
		tweak(&ca, &cb)
	}
	eb, err := New(cb)
	if err != nil {
		t.Fatal(err)
	}
	ea, err := New(ca)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { _ = eb.Run(ctx); close(doneB) }()
	time.Sleep(100 * time.Millisecond)
	go func() { _ = ea.Run(ctx); close(doneA) }()
	t.Cleanup(func() {
		cancel()
		devA.Close()
		devB.Close()
		<-doneA
		<-doneB
	})
	return &node{ea, devA}, &node{eb, devB}
}

func waitUp(t *testing.T, n *node, carrierName string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		st := n.e.Status()
		if len(st.Peers) > 0 && st.Peers[0].Up && (carrierName == "" || st.Peers[0].Carrier == carrierName) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tunnel not up on %q within %v: %+v", carrierName, d, n.e.Status().Peers)
}

// exchange sends a packet A->B and B->A and verifies delivery.
func exchange(t *testing.T, a, b *node, tag string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	check := func(from, to *node, pkt []byte) {
		for time.Now().Before(deadline) {
			from.dev.in <- pkt
			timer := time.NewTimer(300 * time.Millisecond)
			for {
				select {
				case got := <-to.dev.out:
					if bytes.Equal(got, pkt) {
						timer.Stop()
						return
					}
					continue
				case <-timer.C:
				}
				break
			}
		}
		t.Fatalf("%s: packet not delivered", tag)
	}
	check(a, b, udp4("10.200.0.1", "10.200.0.2", []byte("a->b "+tag), true))
	check(b, a, udp4("10.200.0.2", "10.200.0.1", []byte("b->a "+tag), true))
}

func TestL3OverEachCarrier(t *testing.T) {
	cases := []struct {
		c   carrier.Carrier
		net string
	}{
		{tcp.New(carrier.Options{}), "tcp"},
		{udp.New(carrier.Options{}), "udp"},
		{quic.New(carrier.Options{}), "udp"},
		{quic.New(carrier.Options{Datagrams: true}), "udp"},
		{websocket.New(carrier.Options{}), "tcp"},
	}
	for _, tc := range cases {
		name := tc.c.Name()
		if tc.c.Capabilities().Datagram && name == "quic" {
			name = "quic-dgram"
		}
		t.Run(name, func(t *testing.T) {
			a, b := startPair(t, []carrier.Carrier{tc.c}, []string{freePort(t, tc.net)}, nil)
			waitUp(t, a, "", 5*time.Second)
			exchange(t, a, b, name)
			// IPv6 inner traffic
			p6 := udp6("fd00::1", "fd00::2", []byte("v6"))
			a.dev.in <- p6
			select {
			case got := <-b.dev.out:
				if !bytes.Equal(got, p6) {
					t.Fatal("ipv6 corrupted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ipv6 not delivered")
			}
			st := a.e.Status().Peers[0]
			if st.TxPackets == 0 || st.RxPackets == 0 || st.Health.Received == 0 {
				t.Fatalf("stats not tracked: %+v", st)
			}
		})
	}
}

func TestSpoofedSourceDropped(t *testing.T) {
	a, b := startPair(t, []carrier.Carrier{tcp.New(carrier.Options{})}, []string{freePort(t, "tcp")}, nil)
	waitUp(t, a, "", 5*time.Second)
	exchange(t, a, b, "warmup")
	a.dev.in <- udp4("10.9.9.9", "10.200.0.2", []byte("spoof"), true)
	select {
	case <-b.dev.out:
		t.Fatal("spoofed source delivered")
	case <-time.After(500 * time.Millisecond):
	}
	if b.e.Status().Drops["spoofed_source"] == 0 {
		t.Fatal("spoof not counted")
	}
}

func TestUnauthorizedPeerRejected(t *testing.T) {
	addr := freePort(t, "tcp")
	c := tcp.New(carrier.Options{})
	a, b := startPair(t, []carrier.Carrier{c}, []string{addr}, func(ca, cb *Config) {
		other, _ := session.GenerateKeyPair()
		cb.Peers[0].PublicKey = other.Public // B does not know A's key
	})
	time.Sleep(1500 * time.Millisecond)
	if a.e.Status().Peers[0].Up {
		t.Fatal("unauthorized peer connected")
	}
	if b.e.Status().Drops["auth_failures"] == 0 {
		t.Fatal("auth failure not counted")
	}
}

func TestCarrierFailoverAndRecovery(t *testing.T) {
	tcpC := faulty.Wrap(tcp.New(carrier.Options{}), "tcp")
	quicC := faulty.Wrap(quic.New(carrier.Options{}), "quic")
	wssC := faulty.Wrap(websocket.New(carrier.Options{}), "wss")
	a, b := startPair(t, []carrier.Carrier{tcpC, quicC, wssC},
		[]string{freePort(t, "tcp"), freePort(t, "udp"), freePort(t, "tcp")}, nil)
	waitUp(t, a, "tcp", 5*time.Second)
	exchange(t, a, b, "tcp")

	tcpC.SetDown(true)
	quicC.SetDown(true)
	start := time.Now()
	waitUp(t, a, "wss", 10*time.Second)
	t.Logf("failover tcp->wss in %v", time.Since(start))
	exchange(t, a, b, "after-failover")
	if a.e.Status().Peers[0].Reconnects == 0 {
		t.Fatal("reconnect not counted")
	}

	// restore tcp: manager must probe it and preempt back after hysteresis
	tcpC.SetDown(false)
	start = time.Now()
	waitUp(t, a, "tcp", 15*time.Second)
	t.Logf("recovered to tcp in %v", time.Since(start))
	exchange(t, a, b, "after-recovery")
}

func TestDegradedCarrierSwitch(t *testing.T) {
	udpC := faulty.Wrap(udp.New(carrier.Options{}), "udp")
	tcpC := faulty.Wrap(tcp.New(carrier.Options{}), "tcp")
	a, b := startPair(t, []carrier.Carrier{udpC, tcpC}, []string{freePort(t, "udp"), freePort(t, "tcp")}, func(ca, cb *Config) {
		ca.Failover.Preempt = false
	})
	waitUp(t, a, "udp", 5*time.Second)
	udpC.SetLoss(35) // above DegradedLoss (20) but below FailedLoss (60)
	waitUp(t, a, "tcp", 15*time.Second)
	exchange(t, a, b, "after-degraded-switch")
}

func TestEndpointFailover(t *testing.T) {
	// Two endpoints (two listeners on B) with the same carrier type.
	ep1 := faulty.Wrap(tcp.New(carrier.Options{}), "tcp")
	ep2 := faulty.Wrap(tcp.New(carrier.Options{}), "tcp")
	addr1, addr2 := freePort(t, "tcp"), freePort(t, "tcp")
	a, b := startPair(t, []carrier.Carrier{ep1, ep2}, []string{addr1, addr2}, func(ca, cb *Config) {
		ca.Peers[0].Candidates[0].Endpoint = "germany"
		ca.Peers[0].Candidates[1].Endpoint = "netherlands"
		ca.Peers[0].Candidates[1].EndpointRank = 1
		ca.Peers[0].Candidates[1].CarrierRank = 0
	})
	waitUp(t, a, "tcp", 5*time.Second)
	if ep := a.e.Status().Peers[0].Endpoint; ep != "germany" {
		t.Fatalf("endpoint %s", ep)
	}
	ep1.SetDown(true)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := a.e.Status().Peers[0]; st.Up && st.Endpoint == "netherlands" {
			exchange(t, a, b, "endpoint-failover")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no endpoint failover: %+v", a.e.Status().Peers[0])
}

func TestNoUsablePathReported(t *testing.T) {
	c := faulty.Wrap(tcp.New(carrier.Options{}), "tcp")
	c.SetDown(true)
	a, _ := startPair(t, []carrier.Carrier{c}, []string{freePort(t, "tcp")}, nil)
	time.Sleep(700 * time.Millisecond)
	st := a.e.Status().Peers[0]
	if st.Up || st.Candidates[0].State != health.Failed || st.Candidates[0].LastError == "" {
		t.Fatalf("failure not reported: %+v", st)
	}
	rep := a.e.TestCarriers(context.Background(), 0)
	if len(rep) != 1 || rep[0].OK || rep[0].Error == "" {
		t.Fatalf("test-carriers wrong: %+v", rep)
	}
}

func TestReconnectAndRekey(t *testing.T) {
	a, b := startPair(t, []carrier.Carrier{tcp.New(carrier.Options{})}, []string{freePort(t, "tcp")}, func(ca, cb *Config) {
		ca.RekeyInterval = 300 * time.Millisecond
	})
	waitUp(t, a, "", 5*time.Second)
	l := a.e.peers[0].activeLink()
	l.mu.Lock()
	first := l.cur.LocalIndex
	l.mu.Unlock()
	time.Sleep(time.Second)
	l.mu.Lock()
	rotated := l.cur.LocalIndex != first
	l.mu.Unlock()
	if !rotated {
		t.Fatal("session keys not rotated")
	}
	exchange(t, a, b, "after-rekey")
	a.e.Reconnect("")
	time.Sleep(200 * time.Millisecond)
	waitUp(t, a, "", 5*time.Second)
	exchange(t, a, b, "after-reconnect")
}

func TestOversizePackets(t *testing.T) {
	// QUIC datagram mode has a small per-link limit: IPv4 without DF must be
	// fragmented, DF packets must produce ICMP fragmentation-needed locally.
	a, b := startPair(t, []carrier.Carrier{quic.New(carrier.Options{Datagrams: true})}, []string{freePort(t, "udp")}, nil)
	waitUp(t, a, "", 5*time.Second)
	big := udp4("10.200.0.1", "10.200.0.2", bytes.Repeat([]byte{1}, 1300), false)
	a.dev.in <- big
	var frags int
	timeout := time.After(3 * time.Second)
	for frags < 2 {
		select {
		case p := <-b.dev.out:
			if p[9] == 17 {
				frags++
			}
		case <-timeout:
			t.Fatalf("fragments not delivered (%d)", frags)
		}
	}
	a.dev.in <- udp4("10.200.0.1", "10.200.0.2", bytes.Repeat([]byte{1}, 1300), true)
	timeout = time.After(3 * time.Second)
	for {
		select {
		case p := <-a.dev.out:
			if p[9] == 1 && p[20] == 3 && p[21] == 4 {
				return
			}
		case <-timeout:
			t.Fatal("no ICMP fragmentation-needed generated")
		}
	}
}

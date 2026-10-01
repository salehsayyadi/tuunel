package engine

import (
	"net"
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/tcp"
	"github.com/salehsayyadi/tuunel/internal/carrier/udp"
	"github.com/salehsayyadi/tuunel/internal/mtu"
)

type addrConn struct {
	floodConn
	remote net.Addr
}

func (a *addrConn) RemoteAddr() net.Addr { return a.remote }

// A listener's plan assumes a 1500-byte path; the per-link limit must follow
// the kernel's path MTU towards the actual peer for datagram carriers, or
// oversize DF datagrams are silently lost (regression found by the MTU lab).
func TestLinkLimitUsesDetectedPathMTU(t *testing.T) {
	var asked string
	e := &Engine{cfg: Config{PathMTU: 1500, DetectPathMTU: func(a string) (int, error) { asked = a; return 1450, nil }}}
	conn := &addrConn{remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000}}
	u := udp.New(carrier.Options{})
	l := newLink(e, &peer{}, conn, nil, u, "", nil, false, false)
	if want := mtu.LinkLimit(1450, u.Capabilities(), false); l.limit != want {
		t.Fatalf("udp limit = %d, want %d", l.limit, want)
	}
	if asked != "192.0.2.1:40000" {
		t.Fatalf("detector asked %q", asked)
	}
	// Segmenting carriers are not limited by the path and are not probed.
	asked = ""
	tc := tcp.New(carrier.Options{})
	l = newLink(e, &peer{}, conn, nil, tc, "", nil, false, false)
	if want := mtu.LinkLimit(1500, tc.Capabilities(), false); l.limit != want || asked != "" {
		t.Fatalf("tcp limit = %d (want %d), asked %q", l.limit, want, asked)
	}
	// A larger detected value never raises the configured path MTU.
	e.cfg.DetectPathMTU = func(string) (int, error) { return 9000, nil }
	l = newLink(e, &peer{}, conn, nil, u, "", nil, false, false)
	if want := mtu.LinkLimit(1500, u.Capabilities(), false); l.limit != want {
		t.Fatalf("udp limit with jumbo path = %d, want %d", l.limit, want)
	}
}

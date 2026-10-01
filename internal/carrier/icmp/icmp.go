// Package icmp implements the EXPERIMENTAL ICMP echo carrier (IPv4 only).
//
// Requirements and limitations:
//   - Must be enabled explicitly (experimental.icmp: true); otherwise Check
//     returns carrier.ErrDisabled.
//   - Needs a raw ICMP socket: root or CAP_NET_RAW on both nodes.
//   - The client sends Echo Requests (identifier = random session id) and the
//     server answers with Echo Replies carrying the same identifier, so the
//     traffic resembles ordinary ping. Networks that block, rate-limit or
//     rewrite ICMP will make this carrier fail or perform poorly; the engine
//     detects this through handshake timeouts and health probes.
//   - The server can only send when the client has recently sent a request;
//     the engine's periodic pings keep that state alive.
//   - Payloads are the engine's already-encrypted session messages.
//   - Nothing here spoofs addresses or bypasses operating-system controls.
package icmp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

const (
	// MaxPayload keeps echo packets within a 1500-byte path:
	// 1500 - 20 (IPv4) - 8 (ICMP) - 4 (magic) = 1468, rounded down.
	MaxPayload = 1400
	protoICMP  = 1
)

var (
	magicReq = [4]byte{'T', 'U', 'N', 'Q'}
	magicRep = [4]byte{'T', 'U', 'N', 'R'}
)

type Carrier struct{ opts carrier.Options }

func New(opts carrier.Options) *Carrier { return &Carrier{opts: opts} }

func (*Carrier) Name() string { return "icmp" }

func (*Carrier) Capabilities() carrier.Capabilities {
	return carrier.Capabilities{Datagram: true, MaxMessage: MaxPayload, Overhead: 12, Privileged: true, Experimental: true}
}

// Check verifies enablement and that a raw ICMP socket can be opened.
func (c *Carrier) Check(context.Context) error {
	if !c.opts.Experimental {
		return fmt.Errorf("%w: set experimental.icmp: true to enable the ICMP carrier", carrier.ErrDisabled)
	}
	pc, err := xicmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return classify(err)
	}
	pc.Close()
	return nil
}

func classify(err error) error {
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: raw ICMP socket requires root or CAP_NET_RAW (%v)", carrier.ErrPermission, err)
	}
	return fmt.Errorf("%w: %v", carrier.ErrUnavailable, err)
}

type ipAddr struct{ ip net.IP }

func (a ipAddr) Network() string { return "icmp" }
func (a ipAddr) String() string  { return a.ip.String() }

func marshal(typ ipv4.ICMPType, id, seq int, magic [4]byte, b []byte) ([]byte, error) {
	data := make([]byte, 4+len(b))
	copy(data, magic[:])
	copy(data[4:], b)
	m := xicmp.Message{Type: typ, Body: &xicmp.Echo{ID: id, Seq: seq, Data: data}}
	return m.Marshal(nil)
}

func parse(b []byte) (ipv4.ICMPType, *xicmp.Echo, bool) {
	m, err := xicmp.ParseMessage(protoICMP, b)
	if err != nil {
		return 0, nil, false
	}
	e, ok := m.Body.(*xicmp.Echo)
	if !ok || len(e.Data) < 5 {
		return 0, nil, false
	}
	t, _ := m.Type.(ipv4.ICMPType)
	return t, e, true
}

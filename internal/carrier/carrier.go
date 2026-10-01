// Package carrier defines the transport abstraction used by the tunnel engine.
//
// A carrier moves opaque, already-encrypted session messages between two
// nodes. Carriers never see plaintext tunnel payloads and the tunnel engine
// never contains carrier-specific logic: it only uses the interfaces below.
package carrier

import (
	"context"
	"errors"
	"net"
)

// MaxMessage is the largest session message any carrier must transport.
const MaxMessage = 65535

var (
	// ErrMessageTooLarge is returned when a message exceeds a carrier limit.
	ErrMessageTooLarge = errors.New("carrier: message too large")
	// ErrPermission indicates missing privileges (for example CAP_NET_RAW).
	ErrPermission = errors.New("carrier: insufficient privileges")
	// ErrUnavailable indicates the carrier cannot operate on this host.
	ErrUnavailable = errors.New("carrier: unavailable on this host")
	// ErrDisabled indicates an experimental carrier that is not enabled.
	ErrDisabled = errors.New("carrier: disabled by configuration")
)

// Capabilities describes what a carrier provides. The MTU engine uses
// Overhead and Segmenting to compute a safe inner MTU.
type Capabilities struct {
	Reliable     bool // lost messages are retransmitted by the carrier
	Ordered      bool // messages arrive in order
	Datagram     bool // message boundaries map to individual network packets
	Segmenting   bool // messages larger than the path MTU are segmented safely
	MaxMessage   int  // largest accepted message (bytes)
	Overhead     int  // carrier bytes per message, excluding the outer IP header
	Privileged   bool // requires elevated privileges
	Experimental bool // must be explicitly enabled by an administrator
}

// Conn is a bidirectional, message-oriented connection between two nodes.
// ReadMessage returns exactly one message. WriteMessage must be safe for
// concurrent use by multiple goroutines.
type Conn interface {
	ReadMessage(b []byte) (int, error)
	WriteMessage(b []byte) error
	Close() error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// Listener accepts inbound carrier connections.
type Listener interface {
	Accept(ctx context.Context) (Conn, error)
	Close() error
	Addr() net.Addr
}

// Carrier is a pluggable transport implementation.
type Carrier interface {
	Name() string
	Capabilities() Capabilities
	Dial(ctx context.Context, address string) (Conn, error)
	Listen(ctx context.Context, address string) (Listener, error)
	// Check verifies local preconditions (privileges, kernel support) without
	// contacting a remote node.
	Check(ctx context.Context) error
}

// Options holds carrier-specific configuration. Each carrier reads only the
// fields it understands.
type Options struct {
	TLSCertFile   string // server certificate for QUIC/WSS (optional: ephemeral)
	TLSKeyFile    string
	TLSCAFile     string // verify server certificate chain when set
	TLSServerName string // SNI / verification name
	TLSInsecure   bool   // skip outer TLS verification (inner session still authenticates)
	Path          string // WebSocket path
	Host          string // WebSocket Host header override
	Plain         bool   // WebSocket without TLS (behind a TLS-terminating proxy)
	Datagrams     bool   // QUIC: use DATAGRAM frames instead of a stream
	MaxSessions   int    // listener-side limit on concurrent remote sessions
	Experimental  bool   // administrator enablement for experimental carriers
}

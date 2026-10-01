// Package all is the carrier registry: it maps configuration types to
// carrier implementations. It is the only place that knows all carriers.
package all

import (
	"fmt"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/icmp"
	"github.com/salehsayyadi/tuunel/internal/carrier/quic"
	"github.com/salehsayyadi/tuunel/internal/carrier/tcp"
	"github.com/salehsayyadi/tuunel/internal/carrier/udp"
	"github.com/salehsayyadi/tuunel/internal/carrier/websocket"
)

// New constructs a carrier by type name.
func New(typ string, o carrier.Options) (carrier.Carrier, error) {
	switch typ {
	case "tcp":
		return tcp.New(o), nil
	case "udp":
		return udp.New(o), nil
	case "quic":
		return quic.New(o), nil
	case "wss":
		o.Plain = false
		return websocket.New(o), nil
	case "ws":
		o.Plain = true
		return websocket.New(o), nil
	case "icmp":
		return icmp.New(o), nil
	}
	return nil, fmt.Errorf("unknown carrier type %q", typ)
}

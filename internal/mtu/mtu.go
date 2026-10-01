// Package mtu computes safe inner (TUN) MTUs from carrier overheads and the
// detected or configured path MTU, and explains unsafe situations.
package mtu

import (
	"fmt"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/packet"
	"github.com/salehsayyadi/tuunel/internal/session"
)

const DefaultPathMTU = 1500

// Carrier describes one enabled carrier for planning.
type Carrier struct {
	Name string
	Caps carrier.Capabilities
}

// Inner returns the largest inner IP packet carrier c can move without
// outer fragmentation at the given path MTU. For segmenting carriers it is
// the size that fits one outer segment; HardLimit gives the absolute maximum.
func Inner(pathMTU int, c carrier.Capabilities, outerIPv6 bool) int {
	ip := packet.IPv4HeaderMin
	if outerIPv6 {
		ip = packet.IPv6Header
	}
	n := pathMTU - ip - c.Overhead - session.Overhead
	if h := HardLimit(c); n > h {
		n = h
	}
	return n
}

// HardLimit is the largest inner packet the carrier can move at all.
func HardLimit(c carrier.Capabilities) int {
	max := c.MaxMessage
	if max <= 0 || max > carrier.MaxMessage {
		max = carrier.MaxMessage
	}
	return max - session.Overhead
}

// LinkLimit is the per-link maximum inner packet size enforced by the
// engine: datagram carriers must not exceed the path, segmenting carriers
// only their hard limit.
func LinkLimit(pathMTU int, c carrier.Capabilities, outerIPv6 bool) int {
	if c.Segmenting {
		return HardLimit(c)
	}
	return Inner(pathMTU, c, outerIPv6)
}

type Plan struct {
	TunMTU       int            `json:"tun_mtu"`
	Mode         string         `json:"mode"`
	PathMTU      int            `json:"path_mtu"`
	PerCarrier   map[string]int `json:"per_carrier"`
	Warnings     []string       `json:"warnings,omitempty"`
	Pathological bool           `json:"pathological"`
}

// Compute plans the TUN MTU. manual == 0 means automatic. minMTU is the
// administrator's minimum safe MTU (at least 576; 1280 when IPv6 is used
// inside the tunnel).
func Compute(manual, minMTU, pathMTU int, carriers []Carrier, outerIPv6, innerIPv6 bool) (Plan, error) {
	if pathMTU <= 0 {
		pathMTU = DefaultPathMTU
	}
	floor := packet.MinIPv4MTU
	if innerIPv6 {
		floor = packet.MinIPv6MTU
	}
	if minMTU < floor {
		minMTU = floor
	}
	p := Plan{PathMTU: pathMTU, PerCarrier: map[string]int{}, Mode: "auto"}
	if len(carriers) == 0 {
		return p, fmt.Errorf("mtu: no carriers enabled")
	}
	best := -1
	for _, c := range carriers {
		in := Inner(pathMTU, c.Caps, outerIPv6)
		p.PerCarrier[c.Name] = in
		if best < 0 || in < best {
			best = in
		}
	}
	if manual > 0 {
		p.Mode = "manual"
		if manual < floor {
			return p, fmt.Errorf("mtu: configured MTU %d below protocol minimum %d", manual, floor)
		}
		if manual > 65535-session.Overhead {
			return p, fmt.Errorf("mtu: configured MTU %d too large", manual)
		}
		p.TunMTU = manual
	} else {
		p.TunMTU = best
		if p.TunMTU < minMTU {
			p.TunMTU = minMTU
		}
	}
	for _, c := range carriers {
		in := p.PerCarrier[c.Name]
		if p.TunMTU <= in {
			continue
		}
		if p.TunMTU > HardLimit(c.Caps) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("carrier %s cannot carry %d-byte packets (limit %d): oversize IPv4 packets are fragmented by the engine, DF/IPv6 packets receive ICMP packet-too-big", c.Name, p.TunMTU, HardLimit(c.Caps)))
		} else if c.Caps.Segmenting {
			p.Warnings = append(p.Warnings, fmt.Sprintf("carrier %s: %d-byte packets span multiple outer segments at path MTU %d (works, less efficient)", c.Name, p.TunMTU, pathMTU))
			continue
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("carrier %s: packets above %d bytes exceed path MTU %d and are fragmented by the engine or answered with packet-too-big", c.Name, in, pathMTU))
		}
		if in < floor {
			p.Pathological = true
			p.Warnings = append(p.Warnings, fmt.Sprintf("carrier %s: usable inner MTU %d is below the protocol minimum %d; expect heavy fragmentation", c.Name, in, floor))
		}
	}
	return p, nil
}

// Package routing plans which prefixes are routed into the tunnel and
// detects configurations that would create routing loops (carrier traffic to
// an endpoint being routed back into the tunnel).
package routing

import (
	"fmt"
	"net/netip"
	"sort"
)

type Plan struct {
	Routes []netip.Prefix // prefixes to route via the tunnel interface
	Errors []string
}

// Build returns routes for peer allowed IPs and extra routes, skipping
// prefixes already covered by an interface address (connected routes), and
// rejecting prefixes that contain a carrier endpoint address.
func Build(ifacePrefixes []netip.Prefix, allowed []netip.Prefix, extra []netip.Prefix, endpoints []netip.Addr) Plan {
	var p Plan
	seen := map[netip.Prefix]bool{}
	for _, r := range append(append([]netip.Prefix{}, allowed...), extra...) {
		r = r.Masked()
		if seen[r] {
			continue
		}
		seen[r] = true
		connected := false
		for _, ip := range ifacePrefixes {
			ipm := ip.Masked()
			if ipm.Addr().Is4() == r.Addr().Is4() && ipm.Bits() <= r.Bits() && ipm.Contains(r.Addr()) {
				connected = true
			}
		}
		if connected {
			continue
		}
		loop := false
		for _, ep := range endpoints {
			if r.Contains(ep) {
				p.Errors = append(p.Errors, fmt.Sprintf("route %s contains carrier endpoint %s: this would route tunnel traffic into itself (add a more specific route for the endpoint via the physical gateway, or narrow the prefix)", r, ep))
				loop = true
			}
		}
		if !loop {
			p.Routes = append(p.Routes, r)
		}
	}
	sort.Slice(p.Routes, func(i, j int) bool { return p.Routes[i].String() < p.Routes[j].String() })
	return p
}

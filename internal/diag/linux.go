//go:build linux

package diag

import (
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"

	"github.com/salehsayyadi/tuunel/internal/api"
	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/icmp"
	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/daemon"
)

func carrierICMP() (carrier.Carrier, error) {
	return icmp.New(carrier.Options{Experimental: true}), nil
}

func ifaceChecks(r *Report, st *api.FullStatus) {
	l, err := netlink.LinkByName(st.Interface)
	if err != nil {
		r.add("interface", Fail, "%s not found: %v", st.Interface, err)
		return
	}
	a := l.Attrs()
	if a.Flags&net.FlagUp == 0 {
		r.add("interface", Fail, "%s is down", a.Name)
	} else {
		r.add("interface", OK, "%s up, MTU %d", a.Name, a.MTU)
	}
	if a.MTU != st.Engine.MTU {
		r.add("interface", Warn, "%s MTU %d differs from engine MTU %d", a.Name, a.MTU, st.Engine.MTU)
	}
	addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
	if len(addrs) == 0 {
		r.add("interface", Fail, "%s has no addresses", a.Name)
	}
}

// routeChecks verifies that tunnel prefixes route via the TUN interface and
// that carrier endpoints do not (which would be a loop).
func routeChecks(r *Report, cfg *config.Config, plan *daemon.Plan) {
	link, err := netlink.LinkByName(cfg.Interface.Name)
	if err != nil {
		return // reported by ifaceChecks when the daemon is running
	}
	idx := link.Attrs().Index
	for _, p := range cfg.Peers {
		for _, a := range p.AllowedIPs {
			pf, _ := netip.ParsePrefix(a)
			rs, err := netlink.RouteGet(net.IP(pf.Addr().AsSlice()))
			if err != nil || len(rs) == 0 {
				r.add("routing", Fail, "no route to %s (%v)", pf.Addr(), err)
				continue
			}
			if rs[0].LinkIndex != idx {
				r.add("routing", Fail, "%s (peer %s) is routed via ifindex %d, not %s: check conflicting routes", pf.Addr(), p.Name, rs[0].LinkIndex, cfg.Interface.Name)
			} else {
				r.add("routing", OK, "%s routes via %s", pf.Addr(), cfg.Interface.Name)
			}
		}
	}
	for _, ep := range plan.Endpoints {
		rs, err := netlink.RouteGet(net.IP(ep.AsSlice()))
		if err != nil || len(rs) == 0 {
			r.add("routing", Fail, "no route to endpoint %s: %v", ep, err)
			continue
		}
		if rs[0].LinkIndex == idx {
			r.add("routing", Fail, "endpoint %s is routed into the tunnel itself (routing loop)", ep)
		}
	}
}

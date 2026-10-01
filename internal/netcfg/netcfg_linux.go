//go:build linux

// Package netcfg configures the TUN interface (addresses, MTU, routes, link
// state) through netlink. All operations are reversible via Teardown.
package netcfg

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// Configurator applies and reverts interface configuration.
type Configurator struct {
	link  netlink.Link
	name  string
	added []netlink.Route
	addrs []netlink.Addr
}

func New(name string) (*Configurator, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("netcfg: find %s: %w", name, err)
	}
	return &Configurator{link: l, name: name}, nil
}

func (c *Configurator) SetMTU(mtu int) error {
	if err := netlink.LinkSetMTU(c.link, mtu); err != nil {
		return fmt.Errorf("netcfg: set mtu %d: %w", mtu, err)
	}
	return nil
}

func (c *Configurator) Up() error { return netlink.LinkSetUp(c.link) }

func (c *Configurator) AddAddr(p netip.Prefix) error {
	addr := &netlink.Addr{IPNet: toIPNet(p)}
	if err := netlink.AddrReplace(c.link, addr); err != nil {
		return fmt.Errorf("netcfg: add address %s: %w", p, err)
	}
	c.addrs = append(c.addrs, *addr)
	return nil
}

// AddRoute routes a prefix through the interface.
func (c *Configurator) AddRoute(p netip.Prefix) error {
	r := netlink.Route{LinkIndex: c.link.Attrs().Index, Dst: toIPNet(p)}
	if err := netlink.RouteReplace(&r); err != nil {
		return fmt.Errorf("netcfg: add route %s: %w", p, err)
	}
	c.added = append(c.added, r)
	return nil
}

// Teardown removes routes and addresses added by this configurator and brings
// the link down. Errors are accumulated but do not stop cleanup.
func (c *Configurator) Teardown() error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for i := len(c.added) - 1; i >= 0; i-- {
		note(netlink.RouteDel(&c.added[i]))
	}
	for i := range c.addrs {
		note(netlink.AddrDel(c.link, &c.addrs[i]))
	}
	note(netlink.LinkSetDown(c.link))
	return firstErr
}

func toIPNet(p netip.Prefix) *net.IPNet {
	ip := net.IP(p.Addr().AsSlice())
	bits := 32
	if p.Addr().Is6() {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(p.Bits(), bits)}
}

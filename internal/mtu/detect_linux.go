//go:build linux

package mtu

import (
	"fmt"
	"net"
	"syscall"
)

// DetectPathMTU asks the kernel for the currently known path MTU towards
// address (host:port) using a connected UDP socket with IP_MTU. This reflects
// the route/interface MTU and any cached PMTU information; it sends no traffic.
func DetectPathMTU(address string) (int, error) {
	c, err := net.Dial("udp", address)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.(*net.UDPConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	v6 := c.RemoteAddr().(*net.UDPAddr).IP.To4() == nil
	var mtu int
	var serr error
	err = raw.Control(func(fd uintptr) {
		if v6 {
			mtu, serr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_MTU)
		} else {
			mtu, serr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MTU)
		}
	})
	if err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("mtu: getsockopt: %w", serr)
	}
	return mtu, nil
}

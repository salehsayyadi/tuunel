//go:build linux

package tun

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	ifNameSize = 16
	iocSetIFF  = 0x400454ca
	iffTUN     = 0x0001
	iffNoPI    = 0x1000
)

// Open attaches to an existing Linux TUN interface or creates it when absent.
// Addressing and routes are deliberately configured by the local administrator.
func Open(name string) (*os.File, error) {
	if name == "" || len(name) >= ifNameSize || strings.ContainsAny(name, "/\x00") {
		return nil, fmt.Errorf("invalid TUN interface name %q", name)
	}
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil { return nil, fmt.Errorf("open /dev/net/tun: %w", err) }
	var ifr [40]byte // struct ifreq on supported Linux architectures
	copy(ifr[:ifNameSize], name)
	*(*uint16)(unsafe.Pointer(&ifr[ifNameSize])) = iffTUN | iffNoPI
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), iocSetIFF, uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 { f.Close(); return nil, fmt.Errorf("TUNSETIFF %q: %w", name, errno) }
	return f, nil
}

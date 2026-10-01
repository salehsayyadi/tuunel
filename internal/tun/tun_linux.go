//go:build linux

// Package tun manages the Linux TUN device lifecycle.
package tun

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	ifNameSize = 16
	iocSetIFF  = 0x400454ca
	iffTUN     = 0x0001
	iffNoPI    = 0x1000
)

// Device is an open TUN interface.
type Device struct {
	f    *os.File
	name string
	once sync.Once
}

func validName(name string) bool {
	return name != "" && len(name) < ifNameSize && !strings.ContainsAny(name, "/\x00 \t\r\n")
}

// OpenDevice attaches to (creating if absent) a TUN interface with IFF_NO_PI.
func OpenDevice(name string) (*Device, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid TUN interface name %q", name)
	}
	// Open with raw syscalls and only hand the descriptor to the Go runtime
	// poller *after* TUNSETIFF: an unattached TUN fd reports EPOLLERR, and
	// registering it early (os.OpenFile + Fd()) yields "not pollable" reads on
	// newer Go/kernel combinations. A pollable non-blocking fd also lets
	// Close() unblock a pending Read for clean shutdown.
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte
	copy(ifr[:ifNameSize], name)
	*(*uint16)(unsafe.Pointer(&ifr[ifNameSize])) = iffTUN | iffNoPI
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocSetIFF, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		syscall.Close(fd)
		if errno == syscall.EPERM {
			return nil, fmt.Errorf("TUNSETIFF %q: permission denied (need CAP_NET_ADMIN): %w", name, errno)
		}
		return nil, fmt.Errorf("TUNSETIFF %q: %w", name, errno)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set non-blocking on TUN fd: %w", err)
	}
	f := os.NewFile(uintptr(fd), "/dev/net/tun")
	got := string(ifr[:ifNameSize])
	if i := strings.IndexByte(got, 0); i >= 0 {
		got = got[:i]
	}
	return &Device{f: f, name: got}, nil
}

// Open attaches to a TUN interface and returns the underlying file (compat).
func Open(name string) (*os.File, error) {
	d, err := OpenDevice(name)
	if err != nil {
		return nil, err
	}
	return d.f, nil
}

func (d *Device) File() *os.File { return d.f }
func (d *Device) Name() string   { return d.name }

func (d *Device) Read(b []byte) (int, error)  { return d.f.Read(b) }
func (d *Device) Write(b []byte) (int, error) { return d.f.Write(b) }

func (d *Device) Close() error {
	var err error
	d.once.Do(func() { err = d.f.Close() })
	return err
}

//go:build linux

// Package tun manages the Linux TUN device lifecycle.
//
// OpenDevice enables the kernel's TUN offloads (IFF_VNET_HDR with TSO/USO)
// when available: one read then returns a whole TCP super-segment that is
// split into MTU-sized packets in userspace, and writes coalesce consecutive
// TCP/UDP segments (GRO) into one large packet. This cuts the number of
// system calls and kernel stack traversals per byte by an order of magnitude
// (the same technique wireguard-go uses; its tun package does the work).
// Set TUUNEL_TUN_OFFLOAD=0 to disable.
package tun

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

const (
	ifNameSize = 16
	iocSetIFF  = 0x400454ca
	iffTUN     = 0x0001
	iffNoPI    = 0x1000
	iffVnetHdr = 0x4000

	// WriteHeadroom is the minimum offset WritePackets needs in front of
	// every packet (room for the virtio-net header).
	WriteHeadroom = 16
)

// Device is an open TUN interface.
type Device struct {
	f    *os.File
	wg   wgtun.Device // nil without offloads
	name string
	once sync.Once

	rmu   sync.Mutex
	rbufs [][]byte
	rsize []int
	rq    [][]byte // packets split from the last read, not yet returned
	wmu   sync.Mutex
	wbuf  []byte
}

func validName(name string) bool {
	return name != "" && len(name) < ifNameSize && !strings.ContainsAny(name, "/\x00 \t\r\n")
}

func openFD(name string, flags uint16) (int, string, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte
	copy(ifr[:ifNameSize], name)
	*(*uint16)(unsafe.Pointer(&ifr[ifNameSize])) = flags
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocSetIFF, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		syscall.Close(fd)
		if errno == syscall.EPERM {
			return -1, "", fmt.Errorf("TUNSETIFF %q: permission denied (need CAP_NET_ADMIN): %w", name, errno)
		}
		return -1, "", fmt.Errorf("TUNSETIFF %q: %w", name, errno)
	}
	got := string(ifr[:ifNameSize])
	if i := strings.IndexByte(got, 0); i >= 0 {
		got = got[:i]
	}
	return fd, got, nil
}

// OpenDevice attaches to (creating if absent) a TUN interface with IFF_NO_PI,
// with kernel offloads when the kernel supports them.
func OpenDevice(name string) (*Device, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid TUN interface name %q", name)
	}
	if os.Getenv("TUUNEL_TUN_OFFLOAD") != "0" {
		if fd, got, err := openFD(name, iffTUN|iffNoPI|iffVnetHdr); err == nil {
			if wd, _, err := wgtun.CreateUnmonitoredTUNFromFD(fd); err == nil {
				if wd.BatchSize() > 1 {
					return &Device{f: wd.File(), wg: wd, name: got}, nil
				}
				_ = wd.Close()
			} else {
				syscall.Close(fd)
			}
		}
	}
	return openPlain(name)
}

func openPlain(name string) (*Device, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid TUN interface name %q", name)
	}
	fd, got, err := openFD(name, iffTUN|iffNoPI)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("set non-blocking on TUN fd: %w", err)
	}
	return &Device{f: os.NewFile(uintptr(fd), "/dev/net/tun"), name: got}, nil
}

// Open is the legacy helper returning the raw file (no offloads).
func Open(name string) (*os.File, error) {
	d, err := openPlain(name)
	if err != nil {
		return nil, err
	}
	return d.f, nil
}

func (d *Device) File() *os.File { return d.f }
func (d *Device) Name() string   { return d.name }

// Offload reports whether kernel offloads (TSO/GRO) are active.
func (d *Device) Offload() bool { return d.wg != nil }

// BatchSize is the largest number of packets ReadPackets returns at once.
func (d *Device) BatchSize() int {
	if d.wg == nil {
		return 1
	}
	return d.wg.BatchSize()
}

// ReadPackets reads one or more packets into bufs[i][offset:], setting sizes.
func (d *Device) ReadPackets(bufs [][]byte, sizes []int, offset int) (int, error) {
	if d.wg == nil {
		n, err := d.f.Read(bufs[0][offset:])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		return 1, nil
	}
	n, err := d.wg.Read(bufs, sizes, offset)
	if errors.Is(err, wgtun.ErrTooManySegments) { // the rest of the super-segment is dropped; TCP retransmits
		if n < 0 {
			n = 0
		}
		return n, nil
	}
	return n, err
}

// WritePackets writes bufs[i][offset:]; offset must be >= WriteHeadroom.
// With offloads, buffers may be modified (coalesced) and should have spare
// capacity for coalescing to take effect.
func (d *Device) WritePackets(bufs [][]byte, offset int) (int, error) {
	if d.wg == nil {
		var n int
		var errs error
		for _, b := range bufs {
			if _, err := d.f.Write(b[offset:]); err != nil {
				errs = errors.Join(errs, err)
			} else {
				n++
			}
		}
		return n, errs
	}
	return d.wg.Write(bufs, offset)
}

// Read returns one packet (compatibility path for callers without batching).
func (d *Device) Read(b []byte) (int, error) {
	if d.wg == nil {
		return d.f.Read(b)
	}
	d.rmu.Lock()
	defer d.rmu.Unlock()
	for len(d.rq) == 0 {
		if d.rbufs == nil {
			bs := d.wg.BatchSize()
			d.rbufs, d.rsize = make([][]byte, bs), make([]int, bs)
			for i := range d.rbufs {
				d.rbufs[i] = make([]byte, 65535)
			}
		}
		n, err := d.ReadPackets(d.rbufs, d.rsize, 0)
		if err != nil {
			return 0, err
		}
		for i := 0; i < n; i++ {
			d.rq = append(d.rq, d.rbufs[i][:d.rsize[i]])
		}
	}
	p := d.rq[0]
	d.rq = d.rq[1:]
	return copy(b, p), nil
}

// Write writes one packet.
func (d *Device) Write(b []byte) (int, error) {
	if d.wg == nil {
		return d.f.Write(b)
	}
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if cap(d.wbuf) < WriteHeadroom+len(b) {
		d.wbuf = make([]byte, WriteHeadroom+len(b))
	}
	buf := d.wbuf[:WriteHeadroom+len(b)]
	copy(buf[WriteHeadroom:], b)
	if _, err := d.wg.Write([][]byte{buf}, WriteHeadroom); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (d *Device) Close() error {
	var err error
	d.once.Do(func() {
		if d.wg != nil {
			err = d.wg.Close()
		} else {
			err = d.f.Close()
		}
	})
	return err
}

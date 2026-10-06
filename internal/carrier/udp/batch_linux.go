//go:build linux

package udp

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// Batched UDP I/O: sendmmsg/recvmmsg plus UDP GSO (UDP_SEGMENT, Linux 4.18+)
// and UDP GRO (Linux 5.0+). With GSO a run of equally sized datagrams to the
// same destination is handed to the kernel as one buffer and segmented as
// late as possible (often by the NIC), with GRO the kernel delivers several
// datagrams of one flow in one receive. Both cut per-packet costs, which
// dominate on small virtual servers.

const (
	maxGSOSegments = 64    // UDP_MAX_SEGMENTS of older kernels
	maxGSOBytes    = 65000 // stay below the 64 KiB IP limit
	gsoBatch       = 16    // super-datagrams per sendmmsg
	groBufs        = 8     // receive buffers per recvmmsg
)

var gsoOff atomic.Bool // set once the kernel refuses UDP_SEGMENT

func init() {
	if v := getenv("TUUNEL_UDP_OFFLOAD"); v == "0" {
		gsoOff.Store(true)
		groOff = true
	}
}

var groOff bool

// enableGRO asks the kernel to coalesce received datagrams (best effort).
func enableGRO(c *net.UDPConn) bool {
	if groOff {
		return false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	_ = raw.Control(func(fd uintptr) {
		ok = unix.SetsockoptInt(int(fd), unix.IPPROTO_UDP, unix.UDP_GRO, 1) == nil
	})
	return ok
}

func gsoSize(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_UDP && m.Header.Type == unix.UDP_GRO && len(m.Data) >= 2 {
			if len(m.Data) >= 4 {
				return int(*(*int32)(ptr(m.Data)))
			}
			return int(*(*uint16)(ptr(m.Data)))
		}
	}
	return 0
}

func putGSO(oob []byte, size uint16) []byte {
	l := unix.CmsgSpace(2)
	if cap(oob) < l {
		oob = make([]byte, l)
	}
	oob = oob[:l]
	for i := range oob {
		oob[i] = 0
	}
	h := (*unix.Cmsghdr)(ptr(oob))
	h.Level, h.Type = unix.SOL_UDP, unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(2))
	*(*uint16)(ptr(oob[unix.CmsgLen(0):])) = size
	return oob
}

// batchIO does batched reads and writes on one UDP socket.
type batchIO struct {
	pc  *ipv6.PacketConn // works for both families (plain sendmmsg/recvmmsg)
	gro bool

	rmu   sync.Mutex
	rmsgs []ipv6.Message
	rbufs [][]byte
	rq    []segment // received datagrams, rq[rh:] not yet returned
	rh    int

	wmu   sync.Mutex
	wmsgs []ipv6.Message
	wbufs [][]byte
}

type segment struct {
	b    []byte
	addr net.Addr
}

func newBatchIO(c *net.UDPConn) *batchIO {
	b := &batchIO{pc: ipv6.NewPacketConn(c), gro: enableGRO(c), rq: make([]segment, 0, 512)}
	b.rmsgs, b.rbufs = make([]ipv6.Message, groBufs), make([][]byte, groBufs)
	for i := range b.rmsgs {
		b.rbufs[i] = make([]byte, 65535)
		b.rmsgs[i].Buffers = [][]byte{b.rbufs[i]}
		b.rmsgs[i].OOB = make([]byte, unix.CmsgSpace(4))
	}
	b.wmsgs, b.wbufs = make([]ipv6.Message, gsoBatch), make([][]byte, gsoBatch)
	for i := range b.wbufs {
		b.wbufs[i] = make([]byte, 0, 65535)
	}
	return b
}

// read returns received datagrams one by one, copying each into dst via fn.
// It blocks until at least one datagram is available and returns at most
// max datagrams per call; the remainder is kept for the next call.
func (b *batchIO) read(max int, fn func(p []byte, addr net.Addr) bool) (int, error) {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	if b.rh >= len(b.rq) {
		b.rq, b.rh = b.rq[:0], 0
	}
	for len(b.rq) == 0 {
		for i := range b.rmsgs {
			b.rmsgs[i].Buffers[0] = b.rbufs[i]
			b.rmsgs[i].OOB = b.rmsgs[i].OOB[:cap(b.rmsgs[i].OOB)]
			b.rmsgs[i].N, b.rmsgs[i].NN, b.rmsgs[i].Addr = 0, 0, nil
		}
		n, err := b.pc.ReadBatch(b.rmsgs, 0)
		if err != nil {
			return 0, err
		}
		for i := 0; i < n; i++ {
			m := &b.rmsgs[i]
			data := b.rbufs[i][:m.N]
			seg := 0
			if b.gro && m.NN > 0 {
				seg = gsoSize(m.OOB[:m.NN])
			}
			if seg <= 0 || seg >= len(data) {
				if len(data) > 0 {
					b.rq = append(b.rq, segment{data, m.Addr})
				}
				continue
			}
			for off := 0; off < len(data); off += seg {
				end := off + seg
				if end > len(data) {
					end = len(data)
				}
				b.rq = append(b.rq, segment{data[off:end], m.Addr})
			}
		}
	}
	k := 0
	for b.rh < len(b.rq) && k < max {
		s := b.rq[b.rh]
		b.rq[b.rh] = segment{}
		b.rh++
		k++
		if !fn(s.b, s.addr) {
			break
		}
	}
	return k, nil
}

// write sends msgs to addr (nil on connected sockets), coalescing runs of
// equal-sized messages with UDP GSO. It returns the number sent.
func (b *batchIO) write(msgs [][]byte, addr net.Addr) (int, error) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	sent := 0
	for sent < len(msgs) {
		k, cnt := 0, 0 // super-datagrams built, messages covered
		gso := !gsoOff.Load()
		for k < len(b.wmsgs) && sent+cnt < len(msgs) {
			i := sent + cnt
			size := len(msgs[i])
			buf := append(b.wbufs[k][:0], msgs[i]...)
			n := 1
			if gso {
				for i+n < len(msgs) && n < maxGSOSegments && len(buf)+len(msgs[i+n]) <= maxGSOBytes && len(msgs[i+n]) <= size {
					buf = append(buf, msgs[i+n]...)
					n++
					if len(msgs[i+n-1]) < size { // a shorter datagram ends the run
						break
					}
				}
			}
			b.wbufs[k] = buf
			m := &b.wmsgs[k]
			m.Buffers = [][]byte{buf}
			m.Addr = addr
			m.OOB = m.OOB[:0]
			if n > 1 {
				m.OOB = putGSO(m.OOB, uint16(size))
			}
			k++
			cnt += n
		}
		w, err := b.pc.WriteBatch(b.wmsgs[:k], 0)
		if err != nil {
			if gso && (errors.Is(err, unix.EIO) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.EOPNOTSUPP)) {
				gsoOff.Store(true) // kernel/NIC cannot do UDP GSO: retry without
				continue
			}
			return sent, err
		}
		// count messages in the super-datagrams actually written
		covered := 0
		for j := 0; j < w; j++ {
			covered += segmentsIn(b.wmsgs[j], len(msgs[sent+covered]))
		}
		sent += covered
		if w < k {
			return sent, nil
		}
	}
	return sent, nil
}

func segmentsIn(m ipv6.Message, first int) int {
	l := len(m.Buffers[0])
	if len(m.OOB) == 0 || first <= 0 {
		return 1
	}
	return (l + first - 1) / first
}

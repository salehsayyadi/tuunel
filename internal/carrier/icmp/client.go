package icmp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"

	xicmp "golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/salehsayyadi/tuunel/internal/carrier"
)

type clientConn struct {
	pc     *xicmp.PacketConn
	remote *net.IPAddr
	id     int
	seq    atomic.Uint32
	once   sync.Once
	buf    []byte
}

func (c *Carrier) Dial(ctx context.Context, address string) (carrier.Conn, error) {
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	ra, err := net.ResolveIPAddr("ip4", address)
	if err != nil {
		return nil, err
	}
	pc, err := xicmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, classify(err)
	}
	var idb [2]byte
	_, _ = rand.Read(idb[:])
	return &clientConn{pc: pc, remote: ra, id: int(binary.BigEndian.Uint16(idb[:])), buf: make([]byte, 65535)}, nil
}

func (c *clientConn) ReadMessage(b []byte) (int, error) {
	for {
		n, from, err := c.pc.ReadFrom(c.buf)
		if err != nil {
			return 0, err
		}
		ia, ok := from.(*net.IPAddr)
		if !ok || !ia.IP.Equal(c.remote.IP) {
			continue
		}
		t, e, ok := parse(c.buf[:n])
		if !ok || t != ipv4.ICMPTypeEchoReply || e.ID != c.id || [4]byte(e.Data[:4]) != magicRep {
			continue // kernel echo replies to our own requests carry magicReq
		}
		p := e.Data[4:]
		if len(p) > len(b) {
			continue
		}
		return copy(b, p), nil
	}
}

func (c *clientConn) WriteMessage(b []byte) error {
	if len(b) == 0 || len(b) > MaxPayload {
		return carrier.ErrMessageTooLarge
	}
	seq := int(uint16(c.seq.Add(1)))
	m, err := marshal(ipv4.ICMPTypeEcho, c.id, seq, magicReq, b)
	if err != nil {
		return err
	}
	_, err = c.pc.WriteTo(m, c.remote)
	return err
}

func (c *clientConn) Close() error {
	var err error
	c.once.Do(func() { err = c.pc.Close() })
	return err
}

func (c *clientConn) LocalAddr() net.Addr  { return c.pc.LocalAddr() }
func (c *clientConn) RemoteAddr() net.Addr { return ipAddr{c.remote.IP} }

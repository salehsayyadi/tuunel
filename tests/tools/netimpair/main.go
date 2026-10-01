//go:build linux

// netimpair is a userspace network impairment bridge used by the lab when the
// kernel lacks sch_netem. It bridges Ethernet frames between two interfaces
// with AF_PACKET sockets and applies random loss, delay and jitter to IP
// frames (ARP passes unimpaired so the impairment hits only data/carriers).
//
//	netimpair -a ma -b mb -ctl /tmp/ctl -stats /tmp/stats.json
//
// The control file holds "loss_pct delay_ms jitter_ms" and is re-read every
// 200 ms, so tests can change conditions without restarting the bridge.
// Frames keep their order (delay is monotonic per direction), like netem
// without reordering. Counters are written as JSON on SIGTERM/SIGINT and
// every second.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type params struct {
	loss          float64
	delay, jitter time.Duration
}

var cur atomic.Pointer[params]

type dirStats struct {
	RxFrames   atomic.Uint64 `json:"-"`
	IPFrames   atomic.Uint64 `json:"-"`
	Dropped    atomic.Uint64 `json:"-"`
	Forwarded  atomic.Uint64 `json:"-"`
	SendErrors atomic.Uint64 `json:"-"`
	QueueFull  atomic.Uint64 `json:"-"`
}

func (d *dirStats) snap() map[string]uint64 {
	return map[string]uint64{"rx_frames": d.RxFrames.Load(), "ip_frames": d.IPFrames.Load(), "dropped": d.Dropped.Load(),
		"forwarded": d.Forwarded.Load(), "send_errors": d.SendErrors.Load(), "queue_full": d.QueueFull.Load()}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func open(ifname string) (int, int) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		log.Fatalf("interface %s: %v", ifname, err)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		log.Fatalf("socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_ALL), Ifindex: ifi.Index}); err != nil {
		log.Fatalf("bind %s: %v", ifname, err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 8<<20)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 8<<20)
	// The lab puts the interface in promiscuous mode (ip link set IF promisc on).
	return fd, ifi.Index
}

type item struct {
	b   []byte
	due time.Time
}

func pump(name string, in, out, outIdx int, st *dirStats) {
	q := make(chan item, 20000)
	go func() { // sender: releases frames at their due time, in order
		to := &syscall.SockaddrLinklayer{Ifindex: outIdx}
		for it := range q {
			if d := time.Until(it.due); d > 0 {
				time.Sleep(d)
			}
			if err := syscall.Sendto(out, it.b, 0, to); err != nil {
				st.SendErrors.Add(1)
				continue
			}
			st.Forwarded.Add(1)
		}
	}()
	var last time.Time
	buf := make([]byte, 65536)
	for {
		n, from, err := syscall.Recvfrom(in, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			log.Fatalf("%s recv: %v", name, err)
		}
		if ll, ok := from.(*syscall.SockaddrLinklayer); ok && ll.Pkttype == syscall.PACKET_OUTGOING {
			continue // our own transmissions on this interface
		}
		st.RxFrames.Add(1)
		if n < 14 {
			continue
		}
		p := cur.Load()
		due := time.Now()
		et := binary.BigEndian.Uint16(buf[12:14])
		if et == 0x0800 || et == 0x86DD {
			st.IPFrames.Add(1)
			if p.loss > 0 && rand.Float64()*100 < p.loss {
				st.Dropped.Add(1)
				continue
			}
			d := p.delay
			if p.jitter > 0 {
				d += time.Duration((rand.Float64()*2 - 1) * float64(p.jitter))
			}
			if d > 0 {
				due = due.Add(d)
			}
			if due.Before(last) { // keep order
				due = last
			}
			last = due
		}
		b := make([]byte, n)
		copy(b, buf[:n])
		select {
		case q <- item{b, due}:
		default:
			st.QueueFull.Add(1)
		}
	}
}

func readCtl(path string) {
	var prev string
	for {
		if b, err := os.ReadFile(path); err == nil && string(b) != prev {
			prev = string(b)
			var l, d, j float64
			if _, err := fmt.Sscan(strings.TrimSpace(prev), &l, &d, &j); err == nil {
				cur.Store(&params{loss: l, delay: time.Duration(d * float64(time.Millisecond)), jitter: time.Duration(j * float64(time.Millisecond))})
				log.Printf("impairment: loss=%.1f%% delay=%.1fms jitter=%.1fms", l, d, j)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func main() {
	a := flag.String("a", "", "interface A")
	b := flag.String("b", "", "interface B")
	ctl := flag.String("ctl", "", "control file: \"loss_pct delay_ms jitter_ms\"")
	stats := flag.String("stats", "", "JSON stats output file")
	flag.Parse()
	if *a == "" || *b == "" {
		log.Fatal("need -a and -b")
	}
	cur.Store(&params{})
	if *ctl != "" {
		go readCtl(*ctl)
	}
	fa, ia := open(*a)
	fb, ib := open(*b)
	var ab, ba dirStats
	go pump("a->b", fa, fb, ib, &ab)
	go pump("b->a", fb, fa, ia, &ba)
	var mu sync.Mutex
	write := func() {
		if *stats == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		p := cur.Load()
		j, _ := json.Marshal(map[string]any{"a_to_b": ab.snap(), "b_to_a": ba.snap(),
			"loss_pct": p.loss, "delay_ms": p.delay.Seconds() * 1000, "jitter_ms": p.jitter.Seconds() * 1000})
		_ = os.WriteFile(*stats+".tmp", j, 0o644)
		_ = os.Rename(*stats+".tmp", *stats)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	t := time.NewTicker(time.Second)
	for {
		select {
		case <-t.C:
			write()
		case <-sig:
			write()
			return
		}
	}
}

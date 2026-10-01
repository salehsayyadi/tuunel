package mtu

import (
	"strings"
	"testing"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/quic"
	"github.com/salehsayyadi/tuunel/internal/carrier/tcp"
	"github.com/salehsayyadi/tuunel/internal/carrier/udp"
	"github.com/salehsayyadi/tuunel/internal/carrier/websocket"
	"github.com/salehsayyadi/tuunel/internal/session"
)

func caps(c carrier.Carrier) Carrier { return Carrier{Name: c.Name(), Caps: c.Capabilities()} }

func TestPerCarrierOverhead(t *testing.T) {
	o := carrier.Options{}
	cases := []struct {
		c    carrier.Carrier
		want int
	}{
		{tcp.New(o), 1500 - 20 - 34 - session.Overhead},
		{udp.New(o), 1500 - 20 - 8 - session.Overhead},
		{quic.New(o), 1500 - 20 - 52 - session.Overhead},
		{quic.New(carrier.Options{Datagrams: true}), quic.DatagramMax - session.Overhead},
		{websocket.New(o), 1500 - 20 - 70 - session.Overhead},
	}
	for _, tc := range cases {
		if got := Inner(1500, tc.c.Capabilities(), false); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.c.Name(), got, tc.want)
		}
	}
	if Inner(1500, udp.New(o).Capabilities(), true) != 1500-40-8-session.Overhead {
		t.Error("IPv6 outer header not accounted")
	}
}

func TestAutoPicksMinimum(t *testing.T) {
	o := carrier.Options{}
	p, err := Compute(0, 0, 1500, []Carrier{caps(tcp.New(o)), caps(udp.New(o)), caps(websocket.New(o))}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.TunMTU != 1500-20-70-session.Overhead || len(p.Warnings) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestAutoRespectsMinimumAndWarns(t *testing.T) {
	p, err := Compute(0, 1280, 1300, []Carrier{caps(udp.New(carrier.Options{}))}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.TunMTU != 1280 || len(p.Warnings) == 0 {
		t.Fatalf("%+v", p)
	}
}

func TestManualValidation(t *testing.T) {
	c := []Carrier{caps(tcp.New(carrier.Options{}))}
	if _, err := Compute(500, 0, 1500, c, false, false); err == nil {
		t.Fatal("accepted MTU below 576")
	}
	if _, err := Compute(1000, 0, 1500, c, false, true); err == nil {
		t.Fatal("accepted MTU below 1280 with IPv6")
	}
	p, err := Compute(9000, 0, 1500, c, false, false)
	if err != nil || p.TunMTU != 9000 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "segments") {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPathological(t *testing.T) {
	p, _ := Compute(1400, 0, 600, []Carrier{caps(udp.New(carrier.Options{}))}, false, false)
	if !p.Pathological {
		t.Fatalf("not flagged: %+v", p)
	}
}

func TestLinkLimit(t *testing.T) {
	if LinkLimit(1500, tcp.New(carrier.Options{}).Capabilities(), false) != carrier.MaxMessage-session.Overhead {
		t.Fatal("tcp link limit")
	}
	if LinkLimit(1500, udp.New(carrier.Options{}).Capabilities(), false) != 1500-28-session.Overhead {
		t.Fatal("udp link limit")
	}
}

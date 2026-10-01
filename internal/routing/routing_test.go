package routing

import (
	"net/netip"
	"testing"
)

func pf(s ...string) []netip.Prefix {
	var o []netip.Prefix
	for _, x := range s {
		o = append(o, netip.MustParsePrefix(x))
	}
	return o
}

func TestConnectedSkippedAndExtraAdded(t *testing.T) {
	p := Build(pf("10.200.0.1/30"), pf("10.200.0.2/32", "192.168.50.0/24"), pf("fd10::/64", "192.168.50.0/24"), nil)
	if len(p.Errors) != 0 || len(p.Routes) != 2 || p.Routes[0].String() != "192.168.50.0/24" || p.Routes[1].String() != "fd10::/64" {
		t.Fatalf("%+v", p)
	}
}

func TestLoopDetected(t *testing.T) {
	p := Build(pf("10.200.0.1/30"), pf("0.0.0.0/0"), nil, []netip.Addr{netip.MustParseAddr("203.0.113.10")})
	if len(p.Errors) != 1 || len(p.Routes) != 0 {
		t.Fatalf("%+v", p)
	}
}

package exitnode

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/salehsayyadi/tuunel/internal/config"
)

const key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func cfg(t *testing.T, y string) *config.Config {
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const edgeY = `
node: {id: e}
interface: {name: tun0, addresses: ["10.200.0.1/30"]}
security: {private_key_file: /k}
listen: [{carrier: tcp, address: "0.0.0.0:443"}]
peers: [{name: remote, public_key: "` + key + `", allowed_ips: ["10.200.0.2/32"]}]
exit: {mode: client, exclude: ["198.51.100.0/24"]}
`

type rec struct{ cmds []string }

func (r *rec) run(stdin, name string, args ...string) (string, error) {
	c := name + " " + strings.Join(args, " ")
	r.cmds = append(r.cmds, c)
	if strings.HasPrefix(c, "ip -4 -o addr show") {
		return "1: lo    inet 127.0.0.1/8 scope host lo\n2: eth0    inet 94.184.37.77/24 brd 94.184.37.255 scope global eth0\n5: tun0    inet 10.200.0.1/30 scope global tun0", nil
	}
	if strings.HasPrefix(c, "ip -6 -o addr show") {
		return "2: eth0    inet6 2001:db8::5/64 scope global\n2: eth0    inet6 fe80::1/64 scope link", nil
	}
	if strings.Contains(c, " del ") || strings.Contains(c, " -D ") {
		return "", errFake
	}
	return "", nil
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake" }

var errFake = fakeErr{}

func TestClientPlan(t *testing.T) {
	c := cfg(t, edgeY)
	r := &rec{}
	a := New(c, []netip.Addr{netip.MustParseAddr("203.0.113.7")})
	a.Run, a.Log = r.run, func(string, ...any) {}
	a.Sysctl = func(string, string) error { return nil }
	a.ReadFile = func(string) ([]byte, error) { return []byte("2"), nil }
	if err := a.Up(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(r.cmds, "\n")
	for _, want := range []string{
		"ip -4 route replace default dev tun0 src 10.200.0.1 table 7120",
		"ip -4 rule add fwmark 0x100000/0x100000 lookup main priority 5200",
		"ip -4 rule add to 10.0.0.0/8 lookup main priority 5210",
		"ip -4 rule add from 94.184.37.77 lookup main priority 5205",
		"ip -4 rule add ipproto tcp sport 443 lookup main priority 5206",
		"ip -6 rule add from 2001:db8::5 lookup main priority 5205",
		"ip -4 rule add to 203.0.113.7/32 lookup main priority 5210",
		"ip -4 rule add to 198.51.100.0/24 lookup main priority 5210",
		"ip -4 rule add lookup 7120 priority 5290",
		"ip -6 route replace unreachable default table 7120",
		"ip -6 rule add lookup 7120 priority 5290",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	// the catch-all rule must come after the mark and exclude rules
	if strings.Index(all, "lookup 7120 priority 5290") < strings.Index(all, "fwmark") {
		t.Error("tunnel rule before mark rule")
	}
	if strings.Contains(all, "from 10.200.0.1 ") || strings.Contains(all, "from 127.0.0.1") || strings.Contains(all, "from fe80::1") {
		t.Error("tunnel/loopback/link-local address used in a from rule")
	}
}

func TestClientRulesetMarksRepliesOnly(t *testing.T) {
	rs := ClientRuleset("ip", "tun0")
	if !strings.Contains(rs, "ct direction reply meta mark set meta mark or 0x100000") || !strings.Contains(ClientRuleset("ip6", "tun0"), "table ip6 tuunel_exit") {
		t.Fatal(rs)
	}
	if !strings.Contains(rs, `iifname != "tun0"`) {
		t.Fatal("tunnel ingress must not be marked")
	}
}

func TestServerSources(t *testing.T) {
	c := cfg(t, `
node: {id: r}
interface: {name: tun0, addresses: ["10.200.0.2/30"]}
security: {private_key_file: /k}
peers: [{name: edge, public_key: "`+key+`", allowed_ips: ["10.200.0.1/32", "fd00::1/128"], endpoints: [{name: e, address: 192.0.2.2}], carriers: [{type: tcp, port: 443}]}]
exit: {mode: server}
`)
	a := New(c, nil)
	if got := strings.Join(a.ServerSources(), ","); got != "10.200.0.1/32" {
		t.Fatalf("sources %s", got)
	}
	if !strings.Contains(ServerNAT("tun0", a.ServerSources()), `oifname != "tun0" ip saddr { 10.200.0.1/32 } masquerade`) {
		t.Fatal("nat rule")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, y := range map[string]string{
		"bad mode":  strings.Replace(edgeY, "mode: client", "mode: bogus", 1),
		"bad peer":  strings.Replace(edgeY, "mode: client", "mode: client, peer: nope", 1),
		"bad table": strings.Replace(edgeY, "mode: client", "mode: client, table: 5", 1),
		"bad ipv6":  strings.Replace(edgeY, "mode: client", "mode: client, ipv6: maybe", 1),
		"bad excl":  strings.Replace(edgeY, "198.51.100.0/24", "nope", 1),
	} {
		if _, err := config.Parse([]byte(y)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

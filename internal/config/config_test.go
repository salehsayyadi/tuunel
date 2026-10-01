package config

import (
	"strings"
	"testing"
)

const key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func base() string {
	return `
node: {id: node-a}
interface:
  name: tun0
  addresses: ["10.200.0.1/30"]
security: {private_key_file: /etc/tuunel/node.key}
peers:
  - name: node-b
    public_key: "` + key + `"
    allowed_ips: ["10.200.0.2/32"]
    endpoints:
      - {name: germany, address: 203.0.113.10}
      - {name: netherlands, address: 203.0.113.20, priority: 1}
    carriers:
      - {type: quic, port: 443}
      - {type: tcp, port: 443}
      - {type: wss, port: 443, path: /t}
`
}

func TestValidConfigAndDefaults(t *testing.T) {
	c, err := Parse([]byte(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.TunMTU() != 0 || c.Health.Interval.Seconds() != 2 || !*c.Failover.Enabled || c.API.Socket == "" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":     strings.Replace(base(), "node: {id: node-a}", "node: {id: node-a, bogus: 1}", 1),
		"bad cidr":        strings.Replace(base(), "10.200.0.1/30", "10.200.0.1", 1),
		"icmp w/o flag":   strings.Replace(base(), "{type: tcp, port: 443}", "{type: icmp}", 1),
		"bad key":         strings.Replace(base(), key, "short", 1),
		"bad endpoint":    strings.Replace(base(), "203.0.113.10", "203.0.113.10:443", 1),
		"shell injection": strings.Replace(base(), "name: tun0", "name: \"tun0;rm -rf /\"", 1),
		"bad mtu":         strings.Replace(base(), "name: tun0", "name: tun0\n  mtu: \"100\"", 1),
		"bad carrier":     strings.Replace(base(), "type: quic", "type: carrier-pigeon", 1),
		"remote api":      base() + "api: {listen: \"0.0.0.0:9000\"}\n",
		"bad forward":     base() + "forwarding: {tcp: [{listen: \"0.0.0.0\", target: \"x:1\"}]}\n",
		"bad thresholds":  base() + "health: {degraded_loss_pct: 60, failed_loss_pct: 50}\n",
		"bad wss path":    strings.Replace(base(), "path: /t", "path: \"/t x\"", 1),
		"duplicate peer":  base() + strings.SplitN(base(), "peers:\n", 2)[1],
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestICMPAllowedWithFlag(t *testing.T) {
	y := strings.Replace(base(), "{type: tcp, port: 443}", "{type: icmp}", 1) + "experimental: {icmp: true}\n"
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatal(err)
	}
}

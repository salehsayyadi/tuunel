// Package exitnode applies "route all" internet-exit networking (see
// config.Exit) with ip(8) and nft(8). It runs as root from the systemd unit
// (ExecStartPost=+tuunel exit-up / ExecStopPost=+tuunel exit-down) because the
// daemon itself is sandboxed (ProtectKernelTunables, read-only /run).
//
// Client (edge) design — keep every inbound service reachable:
//   - packets that belong to connections initiated FROM OUTSIDE (conntrack
//     direction "reply": SSH, panels, proxy users, the tunnel carriers
//     themselves) get fwmark 0x100000 and use the main table;
//   - private, loopback, link-local, multicast, tunnel and endpoint
//     destinations use the main table;
//   - everything else (connections this host or its containers initiate)
//     uses table 7120: default dev tun0 src <tunnel IPv4>;
//   - IPv6 is answered "unreachable" for new outbound connections (no leak;
//     applications fall back to IPv4) unless exit.ipv6 is "direct".
//
// When the tunnel interface disappears (daemon stopped/crashed) the kernel
// drops the table 7120 route and traffic falls back to the main table.
// Server (remote): ip_forward=1, masquerade of the peers' tunnel addresses,
// MSS clamping, and FORWARD accept rules for hosts whose iptables FORWARD
// policy is DROP (Docker/ufw).
package exitnode

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/salehsayyadi/tuunel/internal/config"
)

const (
	Mark          = 0x100000
	DefaultTable  = 7120
	prioMark      = 5200
	prioLocal     = 5205 // replies sourced from this host's own (non-tunnel) addresses
	prioPorts     = 5206 // replies from the tunnel's own listener ports
	prioExclude   = 5210
	prioTunnel    = 5290
	clientTable   = "tuunel_exit"
	serverTable   = "tuunel_exit_srv"
	natTable      = "tuunel_exit_nat"
	iptComment    = "tuunel-exit"
	commandTimout = 15 * time.Second
)

// Runner executes one command; replaceable in tests.
type Runner func(stdin string, name string, args ...string) (string, error)

func execRunner(stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type Applier struct {
	Cfg       *config.Config
	Endpoints []netip.Addr // carrier endpoints (client: must bypass the tunnel)
	Run       Runner
	Log       func(format string, a ...any)
	Sysctl    func(path, value string) error
	ReadFile  func(path string) ([]byte, error)
	Wait      time.Duration // wait for the tunnel interface (client)
}

func New(cfg *config.Config, endpoints []netip.Addr) *Applier {
	return &Applier{Cfg: cfg, Endpoints: endpoints, Run: execRunner, Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		Sysctl: func(p, v string) error { return os.WriteFile(p, []byte(v), 0o644) }, ReadFile: os.ReadFile, Wait: 30 * time.Second}
}

func (a *Applier) table() int {
	if a.Cfg.Exit.Table > 0 {
		return a.Cfg.Exit.Table
	}
	return DefaultTable
}

func (a *Applier) tunIPv4() (netip.Addr, bool) {
	for _, p := range a.Cfg.Prefixes() {
		if p.Addr().Is4() {
			return p.Addr(), true
		}
	}
	return netip.Addr{}, false
}

// Up applies the configured mode (no-op without exit.mode).
func (a *Applier) Up() error {
	switch a.Cfg.Exit.Mode {
	case "client":
		a.Down()
		return a.client()
	case "server":
		a.Down()
		return a.server()
	}
	return nil
}

// Down removes everything Up may have created (safe to call any time).
func (a *Applier) Down() {
	q := func(name string, args ...string) { _, _ = a.Run("", name, args...) }
	q("nft", "delete", "table", "ip", clientTable)
	q("nft", "delete", "table", "ip6", clientTable)
	q("nft", "delete", "table", "ip", serverTable)
	q("nft", "delete", "table", "ip", natTable)
	for _, fam := range []string{"-4", "-6"} {
		for _, p := range []int{prioMark, prioLocal, prioPorts, prioExclude, prioTunnel} {
			for i := 0; i < 64; i++ { // several rules may share a priority
				if _, err := a.Run("", "ip", fam, "rule", "del", "priority", fmt.Sprint(p)); err != nil {
					break
				}
			}
		}
		q("ip", fam, "route", "flush", "table", fmt.Sprint(a.table()))
	}
	{
		for _, r := range iptRules(a.Cfg.Interface.Name) {
			for i := 0; i < 16; i++ {
				if _, err := a.Run("", "iptables", append([]string{"-w", "5", "-D", "FORWARD"}, r...)...); err != nil {
					break
				}
			}
		}
	}
}

func iptRules(dev string) [][]string {
	return [][]string{
		{"-i", dev, "-m", "comment", "--comment", iptComment, "-j", "ACCEPT"},
		{"-o", dev, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-m", "comment", "--comment", iptComment, "-j", "ACCEPT"},
	}
}

func (a *Applier) waitInterface(dev string) error {
	deadline := time.Now().Add(a.Wait)
	for {
		out, err := a.Run("", "ip", "-4", "-o", "addr", "show", "dev", dev)
		if err == nil && strings.Contains(out, "inet ") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("interface %s has no IPv4 address after %v", dev, a.Wait)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ClientExcludes returns destinations that keep using the main table.
func (a *Applier) ClientExcludes() (v4, v6 []string) {
	v4 = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.168.0.0/16", "224.0.0.0/4", "255.255.255.255/32"}
	v6 = []string{"::1/128", "fc00::/7", "fe80::/10", "ff00::/8"}
	add := func(p netip.Prefix) {
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	for _, p := range a.Cfg.Prefixes() {
		add(p.Masked())
	}
	for _, e := range a.Endpoints {
		add(netip.PrefixFrom(e, e.BitLen()))
	}
	for _, x := range a.Cfg.Exit.Exclude {
		add(netip.MustParsePrefix(x).Masked())
	}
	return dedup(v4), dedup(v6)
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ClientRuleset is the nftables filter ruleset of the client side for one
// family ("ip" or "ip6"; the inet family is not available on every kernel).
func ClientRuleset(family, dev string) string {
	clamp := ""
	if family == "ip" {
		clamp = fmt.Sprintf("\tchain clamp {\n\t\ttype filter hook forward priority -150; policy accept;\n"+
			"\t\toifname \"%s\" tcp flags syn tcp option maxseg size set rt mtu\n\t}\n", dev)
	}
	return fmt.Sprintf(`table %[1]s %[2]s {
	chain mark_in {
		type filter hook prerouting priority -150; policy accept;
		iifname != "%[3]s" ct direction reply meta mark set meta mark or 0x%[4]x
	}
	chain mark_out {
		type route hook output priority -150; policy accept;
		ct direction reply meta mark set meta mark or 0x%[4]x
	}
%[5]s}
`, family, clientTable, dev, Mark, clamp)
}

// ClientNAT masquerades traffic that enters the tunnel with a non-tunnel
// source (containers, sockets bound to the public address).
func ClientNAT(dev string, tunIP netip.Addr) string {
	return fmt.Sprintf("table ip %s {\n\tchain nat_out {\n\t\ttype nat hook postrouting priority 100; policy accept;\n"+
		"\t\toifname \"%s\" ip saddr != %s masquerade\n\t}\n}\n", natTable, dev, tunIP)
}

// localAddrs lists this host's addresses (family "-4"/"-6") except the
// tunnel interface, loopback and link-local ones.
func (a *Applier) localAddrs(fam string) []string {
	out, err := a.Run("", "ip", fam, "-o", "addr", "show")
	if err != nil {
		return nil
	}
	var res []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] == a.Cfg.Interface.Name {
			continue
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" || f[i] == "inet6" {
				if p, err := netip.ParsePrefix(f[i+1]); err == nil {
					ad := p.Addr()
					if !ad.IsLoopback() && !ad.IsLinkLocalUnicast() {
						res = append(res, ad.String())
					}
				}
			}
		}
	}
	return dedup(res)
}

// listenerPorts returns "proto port" pairs of the tunnel's own listeners
// (carriers and the proxy): their replies must never enter the tunnel, even
// for flows conntrack picked up mid-stream.
func (a *Applier) listenerPorts() [][2]string {
	var out [][2]string
	add := func(proto, addr string) {
		if _, port, err := splitPort(addr); err == nil {
			out = append(out, [2]string{proto, port})
		}
	}
	for _, l := range a.Cfg.Listen {
		switch l.Carrier {
		case "tcp", "wss", "ws":
			add("tcp", l.Address)
		case "udp", "quic":
			add("udp", l.Address)
		}
	}
	if a.Cfg.Proxy.Listen != "" && a.Cfg.Proxy.Upstream != "" {
		add("tcp", a.Cfg.Proxy.Listen)
	}
	return out
}

func splitPort(hp string) (string, string, error) {
	i := strings.LastIndex(hp, ":")
	if i < 0 || i == len(hp)-1 {
		return "", "", errors.New("no port")
	}
	return hp[:i], hp[i+1:], nil
}

func (a *Applier) client() error {
	dev := a.Cfg.Interface.Name
	tip, ok := a.tunIPv4()
	if !ok {
		return errors.New("exit client: no IPv4 tunnel address")
	}
	if err := a.waitInterface(dev); err != nil {
		return err
	}
	// reverse-path filtering: replies arrive on the tunnel interface
	_ = a.Sysctl("/proc/sys/net/ipv4/conf/"+dev+"/rp_filter", "0")
	if b, err := a.ReadFile("/proc/sys/net/ipv4/conf/all/rp_filter"); err == nil && strings.TrimSpace(string(b)) == "1" {
		_ = a.Sysctl("/proc/sys/net/ipv4/conf/all/rp_filter", "2")
		a.Log("exit: net.ipv4.conf.all.rp_filter was strict (1), set to loose (2)")
	}
	if _, err := a.Run(ClientRuleset("ip", dev), "nft", "-f", "-"); err != nil {
		return err
	}
	block6 := a.Cfg.Exit.IPv6 != "direct"
	if block6 {
		if _, err := a.Run(ClientRuleset("ip6", dev), "nft", "-f", "-"); err != nil {
			// inbound IPv6 services stay reachable through the from-address rules
			a.Log("exit: IPv6 connection marking unavailable (%v); using address rules only", err)
		}
	}
	if _, err := a.Run(ClientNAT(dev, tip), "nft", "-f", "-"); err != nil {
		a.Log("exit: NAT unavailable on this kernel (%v): containers/bound sockets will not be masqueraded", err)
	}
	t := fmt.Sprint(a.table())
	steps := [][]string{
		{"ip", "-4", "route", "replace", "default", "dev", dev, "src", tip.String(), "table", t},
		{"ip", "-4", "rule", "add", "fwmark", fmt.Sprintf("0x%x/0x%x", Mark, Mark), "lookup", "main", "priority", fmt.Sprint(prioMark)},
	}
	v4, v6 := a.ClientExcludes()
	for _, ad := range a.localAddrs("-4") {
		steps = append(steps, []string{"ip", "-4", "rule", "add", "from", ad, "lookup", "main", "priority", fmt.Sprint(prioLocal)})
	}
	for _, lp := range a.listenerPorts() {
		steps = append(steps, []string{"ip", "-4", "rule", "add", "ipproto", lp[0], "sport", lp[1], "lookup", "main", "priority", fmt.Sprint(prioPorts)})
	}
	for _, p := range v4 {
		steps = append(steps, []string{"ip", "-4", "rule", "add", "to", p, "lookup", "main", "priority", fmt.Sprint(prioExclude)})
	}
	steps = append(steps, []string{"ip", "-4", "rule", "add", "lookup", t, "priority", fmt.Sprint(prioTunnel)})
	if block6 {
		steps = append(steps,
			[]string{"ip", "-6", "route", "replace", "unreachable", "default", "table", t},
			[]string{"ip", "-6", "rule", "add", "fwmark", fmt.Sprintf("0x%x/0x%x", Mark, Mark), "lookup", "main", "priority", fmt.Sprint(prioMark)})
		for _, ad := range a.localAddrs("-6") {
			steps = append(steps, []string{"ip", "-6", "rule", "add", "from", ad, "lookup", "main", "priority", fmt.Sprint(prioLocal)})
		}
		for _, lp := range a.listenerPorts() {
			steps = append(steps, []string{"ip", "-6", "rule", "add", "ipproto", lp[0], "sport", lp[1], "lookup", "main", "priority", fmt.Sprint(prioPorts)})
		}
		for _, p := range v6 {
			steps = append(steps, []string{"ip", "-6", "rule", "add", "to", p, "lookup", "main", "priority", fmt.Sprint(prioExclude)})
		}
		steps = append(steps, []string{"ip", "-6", "rule", "add", "lookup", t, "priority", fmt.Sprint(prioTunnel)})
	}
	for _, s := range steps {
		if _, err := a.Run("", s[0], s[1:]...); err != nil {
			if strings.Contains(strings.Join(s, " "), " sport ") {
				a.Log("exit: port rule skipped (old iproute2/kernel?): %v", err)
				continue
			}
			if strings.HasPrefix(strings.Join(s, " "), "ip -6") {
				a.Log("exit: IPv6 step skipped: %v", err) // host without IPv6
				continue
			}
			a.Down()
			return err
		}
	}
	a.Log("exit client: outbound traffic now leaves through %s (table %s, src %s); inbound services keep the normal route", dev, t, tip)
	return nil
}

// ServerSources are the source prefixes NATed by the server side.
func (a *Applier) ServerSources() []string {
	var src []string
	if len(a.Cfg.Exit.Source) > 0 {
		src = append(src, a.Cfg.Exit.Source...)
	} else {
		for _, p := range a.Cfg.Peers {
			src = append(src, p.AllowedIPs...)
		}
	}
	var v4 []string
	for _, s := range src {
		if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
			v4 = append(v4, p.Masked().String())
		}
	}
	return dedup(v4)
}

func ServerRuleset(dev string) string {
	return fmt.Sprintf("table ip %[1]s {\n\tchain clamp {\n\t\ttype filter hook forward priority -150; policy accept;\n"+
		"\t\tiifname \"%[2]s\" tcp flags syn tcp option maxseg size set rt mtu\n"+
		"\t\toifname \"%[2]s\" tcp flags syn tcp option maxseg size set rt mtu\n\t}\n}\n", serverTable, dev)
}

func ServerNAT(dev string, sources []string) string {
	return fmt.Sprintf("table ip %s {\n\tchain nat_out {\n\t\ttype nat hook postrouting priority 100; policy accept;\n"+
		"\t\toifname != \"%s\" ip saddr { %s } masquerade\n\t}\n}\n", natTable, dev, strings.Join(sources, ", "))
}

func (a *Applier) server() error {
	dev := a.Cfg.Interface.Name
	src := a.ServerSources()
	if len(src) == 0 {
		return errors.New("exit server: no IPv4 peer addresses to NAT")
	}
	if err := a.Sysctl("/proc/sys/net/ipv4/ip_forward", "1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	if _, err := a.Run(ServerRuleset(dev), "nft", "-f", "-"); err != nil {
		return err
	}
	if _, err := a.Run(ServerNAT(dev, src), "nft", "-f", "-"); err != nil {
		a.Log("exit: ERROR: NAT (masquerade) unavailable: %v; the edge's traffic can only work if the network routes %s back to this host", err, strings.Join(src, ","))
	}
	if _, err := exec.LookPath("iptables"); err == nil {
		for _, r := range iptRules(dev) {
			if _, err := a.Run("", "iptables", append([]string{"-w", "5", "-I", "FORWARD", "1"}, r...)...); err != nil {
				a.Log("exit: iptables FORWARD rule not added (%v); if FORWARD policy is DROP, allow %s manually", err, dev)
			}
		}
	}
	a.Log("exit server: forwarding and NAT enabled for %s from %s", strings.Join(src, ","), dev)
	return nil
}

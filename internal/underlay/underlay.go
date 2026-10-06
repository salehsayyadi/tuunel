// Package underlay applies the optional kernel GRE tunnel (config.Underlay)
// that tuunel carriers can run inside. It is executed as root from the
// systemd unit (ExecStartPre=+tuunel underlay-up / ExecStopPost=+underlay-down).
package underlay

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/salehsayyadi/tuunel/internal/config"
)

// Runner executes a command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

func execRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func defaults(g config.GRE) config.GRE {
	if g.Name == "" {
		g.Name = "tgre0"
	}
	if g.MTU == 0 {
		g.MTU = 1476
	}
	return g
}

// SourceFor returns the local IPv4 the kernel uses towards dst ("" if unknown).
func SourceFor(run Runner, dst string) string {
	if run == nil {
		run = execRun
	}
	out, err := run("ip", "-4", "route", "get", dst)
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "src" {
			return f[i+1]
		}
	}
	return ""
}

// Up creates (or recreates) the GRE device. A missing remote means "not
// paired yet" and only removes a stale device.
func Up(cfg *config.Config, run Runner, logf func(string, ...any)) error {
	if run == nil {
		run = execRun
	}
	g := defaults(cfg.Underlay.GRE)
	if cfg.Underlay.GRE == (config.GRE{}) {
		return nil
	}
	Down(cfg, run)
	if !g.Active() {
		logf("underlay: gre %s waiting for the peer address", g.Name)
		return nil
	}
	local := g.Local
	if local == "" {
		local = SourceFor(run, g.Remote)
	}
	if local == "" {
		return fmt.Errorf("underlay: no local address towards %s", g.Remote)
	}
	_, _ = run("modprobe", "ip_gre")
	add := func() (string, error) {
		return run("ip", "tunnel", "add", g.Name, "mode", "gre", "remote", g.Remote, "local", local, "ttl", "255")
	}
	if out, err := add(); err != nil {
		// another GRE device with the same endpoints blocks ours: remove it once
		if list, lerr := run("ip", "-o", "tunnel", "show"); lerr == nil {
			for _, line := range strings.Split(list, "\n") {
				f := strings.Fields(line)
				if len(f) > 0 && strings.Contains(line, "gre/ip") && strings.Contains(line, "remote "+g.Remote+" ") {
					dev := strings.TrimSuffix(f[0], ":")
					if dev != g.Name && dev != "gre0" {
						logf("underlay: removing conflicting GRE device %s", dev)
						_, _ = run("ip", "link", "del", dev)
					}
				}
			}
		}
		if out2, err2 := add(); err2 != nil {
			return fmt.Errorf("underlay: ip tunnel add: %v %s / %s", err, out, out2)
		}
	}
	steps := [][]string{
		{"ip", "addr", "replace", g.Address, "dev", g.Name},
		{"ip", "link", "set", g.Name, "mtu", fmt.Sprint(g.MTU), "up"},
	}
	for _, s := range steps {
		if out, err := run(s[0], s[1:]...); err != nil {
			return fmt.Errorf("underlay: %s: %v %s", strings.Join(s, " "), err, out)
		}
	}
	// hosts with a restrictive INPUT policy (ufw, firewalld, Docker hosts):
	// accept GRE from the peer and the traffic inside this point-to-point link
	for _, r := range firewallRules(g) {
		if _, err := run("iptables", append([]string{"-C"}, r...)...); err != nil {
			if out, err := run("iptables", append([]string{"-I"}, r...)...); err != nil {
				logf("underlay: firewall rule skipped (%v %s)", err, out)
			}
		}
	}
	logf("underlay: gre %s up (%s -> %s, %s, mtu %d)", g.Name, local, g.Remote, g.Address, g.MTU)
	return nil
}

func firewallRules(g config.GRE) [][]string {
	return [][]string{
		{"INPUT", "-p", "gre", "-s", g.Remote, "-m", "comment", "--comment", "tuunel-underlay", "-j", "ACCEPT"},
		{"INPUT", "-i", g.Name, "-m", "comment", "--comment", "tuunel-underlay", "-j", "ACCEPT"},
	}
}

// Down removes the GRE device and its firewall rules (safe to call any time).
func Down(cfg *config.Config, run Runner) {
	if run == nil {
		run = execRun
	}
	if cfg.Underlay.GRE == (config.GRE{}) {
		return
	}
	g := defaults(cfg.Underlay.GRE)
	_, _ = run("ip", "link", "del", g.Name)
	if g.Active() {
		for _, r := range firewallRules(g) {
			for i := 0; i < 4; i++ {
				if _, err := run("iptables", append([]string{"-D"}, r...)...); err != nil {
					break
				}
			}
		}
	}
}

// Package diag implements `tunnelctl doctor` checks. Each check reports what
// it observed; nothing is assumed to work without evidence.
package diag

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/salehsayyadi/tuunel/internal/api"
	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/daemon"
	"github.com/salehsayyadi/tuunel/internal/engine"
	"github.com/salehsayyadi/tuunel/internal/mtu"
)

type Level string

const (
	OK   Level = "OK"
	Info Level = "INFO"
	Warn Level = "WARN"
	Fail Level = "FAIL"
)

type Result struct {
	Area   string
	Level  Level
	Detail string
}

type Report struct{ Results []Result }

func (r *Report) add(area string, l Level, f string, a ...any) {
	r.Results = append(r.Results, Result{area, l, fmt.Sprintf(f, a...)})
}

func (r *Report) Worst() Level {
	w := OK
	for _, x := range r.Results {
		if x.Level == Fail {
			return Fail
		}
		if x.Level == Warn {
			w = Warn
		}
	}
	return w
}

// Inputs are gathered by tunnelctl; Status is nil when the daemon is not reachable.
type Inputs struct {
	ConfigPath string
	Status     *api.FullStatus
	StatusErr  error
	Probes     []engine.ProbeReport
}

// ClassifyError maps a dial/handshake error string to a likely cause.
func ClassifyError(s string) string {
	l := strings.ToLower(s)
	switch {
	case s == "":
		return ""
	case strings.Contains(l, "refused"):
		return "connection refused: nothing listening on that port or a firewall REJECT rule"
	case strings.Contains(l, "no such host") || strings.Contains(l, "lookup"):
		return "DNS resolution failed for the endpoint"
	case strings.Contains(l, "network is unreachable") || strings.Contains(l, "no route"):
		return "no route to the endpoint from this host"
	case strings.Contains(l, "permission") || strings.Contains(l, "cap_net_raw"):
		return "insufficient privileges for this carrier"
	case strings.Contains(l, "disabled"):
		return "carrier disabled by configuration"
	case strings.Contains(l, "timeout") || strings.Contains(l, "no valid response") || strings.Contains(l, "deadline"):
		return "no response: packets are likely dropped by a firewall/ACL, the carrier is blocked on the path, or the peer key is wrong"
	case strings.Contains(l, "unreliable") || strings.Contains(l, "no echo"):
		return "handshake succeeded but the path is unreliable"
	}
	return s
}

// Run executes all checks.
func Run(ctx context.Context, in Inputs) *Report {
	r := &Report{}
	cfg, err := config.Load(in.ConfigPath)
	if err != nil {
		r.add("config", Fail, "%v", err)
	} else {
		r.add("config", OK, "%s is valid (node %s)", in.ConfigPath, cfg.Node.ID)
	}
	system(r)
	if cfg != nil {
		keys(r, cfg)
		dns(ctx, r, cfg)
		if plan, err := daemon.BuildPlan(cfg, mtu.DetectPathMTU); err != nil {
			r.add("mtu", Fail, "%v", err)
		} else {
			mtuChecks(r, plan)
			for _, e := range plan.Routes.Errors {
				r.add("routing", Fail, "%s", e)
			}
			routeChecks(r, cfg, plan)
		}
		if cfg.Experimental.ICMP {
			icmpCheck(ctx, r)
		}
	}
	if in.Status == nil {
		r.add("daemon", Fail, "management API unreachable (%v): is the service running? try: systemctl status tuunel", in.StatusErr)
		return r
	}
	st := in.Status
	r.add("daemon", OK, "running version %s, interface %s, MTU %d", st.Version, st.Interface, st.Engine.MTU)
	ifaceChecks(r, st)
	for _, l := range st.Engine.Listeners {
		if l.Listening {
			r.add("listener", OK, "%s listening on %s (remote peers must be allowed through the firewall to this port)", l.Carrier, l.Address)
		} else {
			r.add("listener", Fail, "%s on %s: %s", l.Carrier, l.Address, ClassifyError(l.Error)+" ("+l.Error+")")
		}
	}
	for _, p := range st.Engine.Peers {
		if p.Up {
			r.add("tunnel", OK, "peer %s UP via %s/%s rtt=%v loss=%.1f%%", p.Name, p.Endpoint, p.Carrier, p.Health.AvgRTT.Round(time.Millisecond), p.Health.LossPct)
		} else if p.Role == "responder" {
			r.add("tunnel", Warn, "peer %s has not connected (this node waits for the peer to dial in)", p.Name)
		} else {
			r.add("tunnel", Fail, "peer %s DOWN: no usable carrier/endpoint", p.Name)
		}
		for _, c := range p.Candidates {
			if c.LastError != "" && !c.Active {
				r.add("carrier", Warn, "%s %s/%s (%s): %s", p.Name, c.Endpoint, c.Carrier, c.Address, ClassifyError(c.LastError))
			}
		}
	}
	up := map[string]bool{}
	for _, p := range st.Engine.Peers {
		up[p.Name] = p.Up
	}
	for _, pr := range in.Probes {
		lvl := OK
		detail := fmt.Sprintf("handshake %v, rtt %v, loss %.0f%%", pr.HandshakeRTT.Round(time.Millisecond), pr.RTT.Round(time.Millisecond), pr.LossPct)
		switch {
		case !pr.OK && up[pr.Peer]:
			// the tunnel works over another carrier: this one is only a missing backup
			lvl, detail = Warn, ClassifyError(pr.Error)+" (tunnel is UP via another carrier; this carrier is only unavailable as a backup)"
		case !pr.OK:
			lvl, detail = Fail, ClassifyError(pr.Error)
		case pr.LossPct > 20:
			lvl, detail = Warn, detail+" (unreliable)"
		}
		r.add("reachability", lvl, "%s %s/%s %s: %s", pr.Peer, pr.Endpoint, pr.Carrier, pr.Address, detail)
	}
	for k, v := range st.Engine.Drops {
		if v > 0 && (k == "auth_failures" || k == "spoofed_source") {
			r.add("security", Info, "%d packets dropped (%s)", v, k)
		}
	}
	return r
}

func system(r *Report) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		r.add("kernel", Fail, "/dev/net/tun missing: load the tun module (modprobe tun) or enable TUN for this container/VPS")
	} else {
		r.add("kernel", OK, "/dev/net/tun present")
	}
	caps := effectiveCaps()
	if os.Geteuid() == 0 {
		r.add("permissions", OK, "running as root")
	} else if caps&(1<<12) != 0 {
		r.add("permissions", OK, "CAP_NET_ADMIN present")
	} else {
		r.add("permissions", Info, "tunnelctl is not privileged; the daemon needs CAP_NET_ADMIN (and CAP_NET_RAW for ICMP), granted by the systemd unit")
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) == "0" {
		r.add("kernel", Info, "net.ipv4.ip_forward=0: fine for host-to-host tunnels; enable it only if this node routes other subnets")
	}
	for _, t := range []string{"/usr/sbin/nft", "/usr/sbin/iptables", "/sbin/iptables"} {
		if _, err := os.Stat(t); err == nil {
			r.add("firewall", Info, "%s present: ensure listener ports are allowed and that forwarding/NAT rules match your routes", t)
			break
		}
	}
}

func effectiveCaps() uint64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			var v uint64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), "%x", &v)
			return v
		}
	}
	return 0
}

func keys(r *Report, cfg *config.Config) {
	k, warn, err := daemon.LoadPrivateKey(cfg.Security.PrivateKeyFile)
	switch {
	case errors.Is(err, os.ErrPermission):
		r.add("keys", Info, "cannot read %s as this user (expected when not root)", cfg.Security.PrivateKeyFile)
	case err != nil:
		r.add("keys", Fail, "private key: %v (generate with: tuunel genkey > %s)", err, cfg.Security.PrivateKeyFile)
	default:
		r.add("keys", OK, "private key loaded; public key %s", b64(k.Public))
		if warn != "" {
			r.add("keys", Warn, "%s", warn)
		}
		for _, p := range cfg.Peers {
			pk, _ := config.ParseKey(p.PublicKey)
			if string(pk) == string(k.Public) {
				r.add("keys", Fail, "peer %s is configured with this node's own public key", p.Name)
			}
		}
	}
	if _, err := daemon.LoadPSK(cfg.Security.PresharedKeyFile); err != nil && !errors.Is(err, os.ErrPermission) {
		r.add("keys", Fail, "pre-shared key: %v", err)
	}
	for _, l := range cfg.Listen {
		if l.TLSCertFile != "" {
			if _, err := os.Stat(l.TLSCertFile); err != nil {
				r.add("certificates", Fail, "%s listener certificate: %v", l.Carrier, err)
			}
		}
	}
}

func dns(ctx context.Context, r *Report, cfg *config.Config) {
	for _, p := range cfg.Peers {
		for _, e := range p.Endpoints {
			if net.ParseIP(e.Address) != nil {
				continue
			}
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			ips, err := net.DefaultResolver.LookupHost(c, e.Address)
			cancel()
			if err != nil {
				r.add("dns", Fail, "endpoint %s (%s): %v", e.Name, e.Address, err)
			} else {
				r.add("dns", OK, "endpoint %s resolves to %s", e.Name, strings.Join(ips, ","))
			}
		}
	}
}

func mtuChecks(r *Report, plan *daemon.Plan) {
	r.add("mtu", OK, "planned TUN MTU %d (%s), path MTU %d", plan.MTU.TunMTU, plan.MTU.Mode, plan.MTU.PathMTU)
	for _, w := range plan.MTU.Warnings {
		r.add("mtu", Warn, "%s", w)
	}
	if plan.MTU.Pathological {
		r.add("mtu", Fail, "pathological fragmentation: the path MTU is too small for the configured carriers")
	}
}

func icmpCheck(ctx context.Context, r *Report) {
	c, _ := carrierICMP()
	if err := c.Check(ctx); err != nil {
		lvl := Warn
		if errors.Is(err, carrier.ErrPermission) {
			lvl = Info
		}
		r.add("icmp", lvl, "%v (the daemon's own capabilities are what matter)", err)
	} else {
		r.add("icmp", OK, "raw ICMP socket available")
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

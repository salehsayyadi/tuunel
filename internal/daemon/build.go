// Package daemon wires configuration into the engine, TUN device, network
// configuration, forwarding and management API.
package daemon

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/salehsayyadi/tuunel/internal/carrier"
	"github.com/salehsayyadi/tuunel/internal/carrier/all"
	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/engine"
	"github.com/salehsayyadi/tuunel/internal/failover"
	"github.com/salehsayyadi/tuunel/internal/forwarding"
	"github.com/salehsayyadi/tuunel/internal/health"
	"github.com/salehsayyadi/tuunel/internal/mtu"
	"github.com/salehsayyadi/tuunel/internal/routing"
)

// Plan is everything derived from a configuration before touching the system.
type Plan struct {
	Engine    engine.Config
	MTU       mtu.Plan
	Routes    routing.Plan
	Endpoints []netip.Addr
	Carriers  []mtu.Carrier
}

func carrierFor(cfg *config.Config, c config.Carrier) (carrier.Carrier, error) {
	return all.New(c.Type, carrier.Options{Path: c.Path, Host: c.Host, TLSServerName: c.TLSServerName, TLSCAFile: c.TLSCAFile,
		Datagrams: c.Datagrams, Experimental: cfg.Experimental.ICMP})
}

// BuildPlan converts configuration into an engine configuration. It performs
// DNS resolution of endpoints only for loop detection and MTU detection.
func BuildPlan(cfg *config.Config, detectMTU func(string) (int, error)) (*Plan, error) {
	p := &Plan{}
	ec := &p.Engine
	ec.NodeID = cfg.Node.ID
	ec.HealthInterval = cfg.Health.Interval.Duration
	ec.IdleTimeout = cfg.Health.IdleTimeout.Duration
	ec.HandshakeTimeout = cfg.Security.HandshakeTimeout.Duration
	ec.RekeyInterval = cfg.Security.RekeyInterval.Duration
	ec.ProbeInterval = cfg.Failover.ProbeInterval.Duration
	ec.FailoverEnabled = *cfg.Failover.Enabled
	ec.Health = health.Thresholds{Window: cfg.Health.Window, PingTimeout: cfg.Health.PingTimeout.Duration, DegradedLoss: cfg.Health.DegradedLoss,
		FailedLoss: cfg.Health.FailedLoss, DegradedRTT: cfg.Health.DegradedRTT.Duration, DegradedJitter: cfg.Health.DegradedJitter.Duration,
		FailedMissed: cfg.Health.FailedMissed, ClearRatio: cfg.Health.ClearRatio, MinSamples: 5}
	f := cfg.Failover
	ec.Failover = failover.Policy{Order: f.Order, EndpointSelection: f.EndpointSelection, BackoffInitial: f.BackoffInitial.Duration,
		BackoffMax: f.BackoffMax.Duration, MaxRetries: f.MaxRetries, Cooldown: f.Cooldown.Duration, MinHold: f.MinHold.Duration,
		RecoverySuccesses: f.RecoverySuccesses, Preempt: *f.Preempt, SwitchOnDegraded: *f.SwitchOnDegraded,
		DegradeHoldoff: f.DegradeHoldoff.Duration}
	for _, pf := range cfg.Prefixes() {
		ec.LocalAddrs = append(ec.LocalAddrs, pf.Addr())
	}
	seen := map[string]bool{}
	addCaps := func(c carrier.Carrier) {
		if !seen[c.Name()] {
			seen[c.Name()] = true
			p.Carriers = append(p.Carriers, mtu.Carrier{Name: c.Name(), Caps: c.Capabilities()})
		}
	}
	for _, l := range cfg.Listen {
		c, err := all.New(l.Carrier, carrier.Options{TLSCertFile: l.TLSCertFile, TLSKeyFile: l.TLSKeyFile, Path: l.Path,
			Datagrams: l.Datagrams, MaxSessions: l.MaxSessions, Experimental: cfg.Experimental.ICMP, ReplyFilter: cfg.Experimental.ICMPReplyFilter, Plain: l.Carrier == "ws"})
		if err != nil {
			return nil, err
		}
		ec.Listeners = append(ec.Listeners, engine.ListenerConfig{Carrier: c, Address: l.Address})
		addCaps(c)
	}
	var allowed []netip.Prefix
	firstDial := ""
	for _, pc := range cfg.Peers {
		key, _ := config.ParseKey(pc.PublicKey)
		ep := engine.PeerConfig{Name: pc.Name, PublicKey: key}
		for _, a := range pc.AllowedIPs {
			pf := netip.MustParsePrefix(a)
			ep.AllowedIPs = append(ep.AllowedIPs, pf)
			allowed = append(allowed, pf)
		}
		for ei, e := range pc.Endpoints {
			rank := ei
			if e.Priority != 0 {
				rank = e.Priority
			}
			if ip, err := netip.ParseAddr(e.Address); err == nil {
				p.Endpoints = append(p.Endpoints, ip)
			} else if ips, err := net.LookupIP(e.Address); err == nil {
				for _, ip := range ips {
					if a, ok := netip.AddrFromSlice(ip); ok {
						p.Endpoints = append(p.Endpoints, a.Unmap())
					}
				}
			}
			for ci, cc := range pc.Carriers {
				if !*cc.Enabled {
					continue
				}
				c, err := carrierFor(cfg, cc)
				if err != nil {
					return nil, err
				}
				addr := e.Address
				if cc.Type != "icmp" {
					addr = net.JoinHostPort(e.Address, strconv.Itoa(cc.Port))
				}
				if firstDial == "" {
					// ICMP has no port; path MTU detection only needs the host
					// (connected UDP socket + IP_MTU, nothing is sent).
					firstDial = net.JoinHostPort(e.Address, strconv.Itoa(max(cc.Port, 9)))
				}
				ep.Candidates = append(ep.Candidates, engine.CandidateConfig{Endpoint: e.Name, EndpointRank: rank, Carrier: c, CarrierRank: ci, Address: addr})
				addCaps(c)
			}
		}
		ec.Peers = append(ec.Peers, ep)
	}
	var extra []netip.Prefix
	for _, r := range cfg.Interface.Routes {
		extra = append(extra, netip.MustParsePrefix(r))
	}
	p.Routes = routing.Build(cfg.Prefixes(), allowed, extra, p.Endpoints)
	pathMTU := cfg.Interface.PathMTU
	if pathMTU == 0 && firstDial != "" && detectMTU != nil {
		if m, err := detectMTU(firstDial); err == nil {
			pathMTU = m
		}
	}
	if pathMTU == 0 {
		pathMTU = mtu.DefaultPathMTU
	}
	innerV6 := false
	for _, a := range ec.LocalAddrs {
		innerV6 = innerV6 || a.Is6()
	}
	outerV6 := false
	for _, a := range p.Endpoints {
		outerV6 = outerV6 || a.Is6() && !a.Is4In6()
	}
	plan, err := mtu.Compute(cfg.TunMTU(), cfg.Interface.MinMTU, pathMTU, p.Carriers, outerV6, innerV6)
	if err != nil {
		return nil, err
	}
	p.MTU = plan
	ec.MTU = plan.TunMTU
	ec.PathMTU = pathMTU
	ec.DetectPathMTU = detectMTU
	return p, nil
}

// ForwardRules converts configuration forwarding rules.
func ForwardRules(cfg *config.Config) []forwarding.Rule {
	var out []forwarding.Rule
	for _, r := range cfg.Forwarding.TCP {
		out = append(out, forwarding.Rule{Proto: "tcp", Listen: r.Listen, Target: r.Target, MaxConnections: r.MaxConnections})
	}
	for _, r := range cfg.Forwarding.UDP {
		out = append(out, forwarding.Rule{Proto: "udp", Listen: r.Listen, Target: r.Target, MaxConnections: r.MaxConnections})
	}
	return out
}

func describe(p *Plan) string {
	return fmt.Sprintf("mtu=%d (%s, path %d) carriers=%d peers=%d", p.MTU.TunMTU, p.MTU.Mode, p.MTU.PathMTU, len(p.Carriers), len(p.Engine.Peers))
}

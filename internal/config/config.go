// Package config loads and strictly validates the YAML configuration. Unknown
// keys are rejected so typos cannot silently disable security settings.
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML-friendly time.Duration ("5s", "2m").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	if v < 0 {
		return fmt.Errorf("line %d: negative duration", n.Line)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

type Config struct {
	Node         Node         `yaml:"node"`
	Interface    Interface    `yaml:"interface"`
	Security     Security     `yaml:"security"`
	Listen       []Listen     `yaml:"listen"`
	Peers        []Peer       `yaml:"peers"`
	Health       Health       `yaml:"health"`
	Failover     Failover     `yaml:"failover"`
	Forwarding   Forwarding   `yaml:"forwarding"`
	Proxy        Proxy        `yaml:"proxy"`
	API          API          `yaml:"api"`
	Experimental Experimental `yaml:"experimental"`
	Log          Log          `yaml:"log"`
}

type Node struct {
	ID string `yaml:"id"`
}

type Interface struct {
	Name      string   `yaml:"name"`
	Addresses []string `yaml:"addresses"`
	MTU       string   `yaml:"mtu"`     // "auto" or an integer
	MinMTU    int      `yaml:"min_mtu"` // minimum safe MTU for auto mode
	PathMTU   int      `yaml:"path_mtu"`
	Manage    *bool    `yaml:"manage"` // configure addresses/MTU/routes via netlink (default true)
	Routes    []string `yaml:"routes"` // extra prefixes routed into the tunnel
}

type Security struct {
	PrivateKeyFile   string   `yaml:"private_key_file"`
	PresharedKeyFile string   `yaml:"preshared_key_file"`
	RekeyInterval    Duration `yaml:"rekey_interval"`
	HandshakeTimeout Duration `yaml:"handshake_timeout"`
}

type Listen struct {
	Carrier     string `yaml:"carrier"`
	Address     string `yaml:"address"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	Path        string `yaml:"path"`
	Datagrams   bool   `yaml:"datagrams"`
	MaxSessions int    `yaml:"max_sessions"`
}

type Peer struct {
	Name       string     `yaml:"name"`
	PublicKey  string     `yaml:"public_key"`
	AllowedIPs []string   `yaml:"allowed_ips"`
	Endpoints  []Endpoint `yaml:"endpoints"`
	Carriers   []Carrier  `yaml:"carriers"`
}

type Endpoint struct {
	Name     string `yaml:"name"`
	Address  string `yaml:"address"` // host or IP (no port)
	Priority int    `yaml:"priority"`
}

type Carrier struct {
	Type          string `yaml:"type"`
	Port          int    `yaml:"port"`
	Enabled       *bool  `yaml:"enabled"`
	Path          string `yaml:"path"`
	Host          string `yaml:"host"`
	TLSServerName string `yaml:"tls_server_name"`
	TLSCAFile     string `yaml:"tls_ca_file"`
	Datagrams     bool   `yaml:"datagrams"`
}

type Health struct {
	Interval       Duration `yaml:"interval"`
	PingTimeout    Duration `yaml:"ping_timeout"`
	Window         int      `yaml:"window"`
	DegradedLoss   float64  `yaml:"degraded_loss_pct"`
	FailedLoss     float64  `yaml:"failed_loss_pct"`
	DegradedRTT    Duration `yaml:"degraded_rtt"`
	DegradedJitter Duration `yaml:"degraded_jitter"`
	FailedMissed   int      `yaml:"failed_after_missed"`
	ClearRatio     float64  `yaml:"clear_ratio"`
	IdleTimeout    Duration `yaml:"idle_timeout"`
}

type Failover struct {
	Enabled           *bool    `yaml:"enabled"`
	Order             string   `yaml:"order"`
	EndpointSelection string   `yaml:"endpoint_selection"`
	BackoffInitial    Duration `yaml:"backoff_initial"`
	BackoffMax        Duration `yaml:"backoff_max"`
	MaxRetries        int      `yaml:"max_retries"`
	Cooldown          Duration `yaml:"cooldown"`
	MinHold           Duration `yaml:"min_hold"`
	ProbeInterval     Duration `yaml:"probe_interval"`
	RecoverySuccesses int      `yaml:"recovery_successes"`
	Preempt           *bool    `yaml:"preempt"`
	SwitchOnDegraded  *bool    `yaml:"switch_on_degraded"`
	DegradeHoldoff    Duration `yaml:"degrade_holdoff"` // anti-flap hold for a candidate left while DEGRADED (default 1m)
}

type Forwarding struct {
	TCP []Rule `yaml:"tcp"`
	UDP []Rule `yaml:"udp"`
}

type Rule struct {
	Listen         string `yaml:"listen"`
	Target         string `yaml:"target"`
	MaxConnections int    `yaml:"max_connections"`
}

// Proxy is the built-in SOCKS5/HTTP exit proxy (see internal/proxy).
// Edge: public listen + users + upstream (the remote's backend through the
// tunnel). Remote: listen on its tunnel address, no users, no upstream.
type Proxy struct {
	Listen         string      `yaml:"listen"`
	Upstream       string      `yaml:"upstream"`
	Users          []ProxyUser `yaml:"users"`
	AllowPrivate   bool        `yaml:"allow_private"`
	MaxConnections int         `yaml:"max_connections"`
}

type ProxyUser struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type API struct {
	Socket    string `yaml:"socket"`     // unix socket (default /run/tuunel/tuunel.sock)
	Listen    string `yaml:"listen"`     // optional TCP address
	TokenFile string `yaml:"token_file"` // required when Listen is not loopback
}

type Experimental struct {
	ICMP bool `yaml:"icmp"`
	// ICMPReplyFilter controls how an ICMP listener stops the kernel from
	// echoing tunnel requests back: "auto" (default) installs a narrow
	// nftables rule that drops only kernel echo replies carrying the tunnel
	// request magic, leaving normal ping intact; "off" installs nothing (the
	// tunnel still works, but every request is reflected by the kernel).
	ICMPReplyFilter string `yaml:"icmp_reply_filter"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // text|json
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$`)
	// KnownCarriers lists carrier types accepted in configuration.
	ICMPReplyFilters = map[string]bool{"": true, "auto": true, "off": true}
	KnownCarriers    = map[string]bool{"tcp": true, "udp": true, "quic": true, "wss": true, "ws": true, "icmp": true}
)

// Load reads, parses, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("config: file larger than 1 MiB")
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func setD(d *Duration, v time.Duration) {
	if d.Duration == 0 {
		d.Duration = v
	}
}

func (c *Config) defaults() {
	if c.Interface.Name == "" {
		c.Interface.Name = "tun0"
	}
	if c.Interface.MTU == "" {
		c.Interface.MTU = "auto"
	}
	if c.Interface.Manage == nil {
		t := true
		c.Interface.Manage = &t
	}
	setD(&c.Security.RekeyInterval, 10*time.Minute)
	setD(&c.Security.HandshakeTimeout, 5*time.Second)
	h := &c.Health
	setD(&h.Interval, 2*time.Second)
	setD(&h.PingTimeout, 2*time.Second)
	setD(&h.IdleTimeout, 30*time.Second)
	if h.Window == 0 {
		h.Window = 20
	}
	if h.DegradedLoss == 0 {
		h.DegradedLoss = 5
	}
	if h.FailedLoss == 0 {
		h.FailedLoss = 50
	}
	if h.FailedMissed == 0 {
		h.FailedMissed = 4
	}
	if h.ClearRatio == 0 {
		h.ClearRatio = 0.5
	}
	f := &c.Failover
	t, fl := true, false
	if f.Enabled == nil {
		f.Enabled = &t
	}
	if f.Preempt == nil {
		f.Preempt = &t
	}
	if f.SwitchOnDegraded == nil {
		f.SwitchOnDegraded = &t
	}
	if f.DegradeHoldoff.Duration == 0 {
		f.DegradeHoldoff.Duration = time.Minute
	}
	_ = fl
	if f.Order == "" {
		f.Order = "endpoint"
	}
	if f.EndpointSelection == "" {
		f.EndpointSelection = "priority"
	}
	setD(&f.BackoffInitial, time.Second)
	setD(&f.BackoffMax, time.Minute)
	setD(&f.Cooldown, 5*time.Minute)
	setD(&f.MinHold, 30*time.Second)
	setD(&f.ProbeInterval, 10*time.Second)
	if f.RecoverySuccesses == 0 {
		f.RecoverySuccesses = 3
	}
	if c.API.Socket == "" {
		c.API.Socket = "/run/tuunel/tuunel.sock"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	for i := range c.Peers {
		for j := range c.Peers[i].Carriers {
			cr := &c.Peers[i].Carriers[j]
			if cr.Enabled == nil {
				cr.Enabled = &t
			}
		}
	}
}

// TunMTU returns the manual MTU or 0 for automatic.
func (c *Config) TunMTU() int {
	if c.Interface.MTU == "auto" {
		return 0
	}
	n, _ := strconv.Atoi(c.Interface.MTU)
	return n
}

// Prefixes returns the interface address prefixes.
func (c *Config) Prefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, a := range c.Interface.Addresses {
		if p, err := netip.ParsePrefix(a); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ParseKey decodes a base64 Curve25519 key.
func ParseKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, errors.New("key must be 32 bytes, base64 encoded")
	}
	return b, nil
}

// Validate performs strict semantic validation.
func (c *Config) Validate() error {
	if !ICMPReplyFilters[c.Experimental.ICMPReplyFilter] {
		return fmt.Errorf("config: experimental.icmp_reply_filter: %q must be auto or off", c.Experimental.ICMPReplyFilter)
	}
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if !nameRe.MatchString(c.Node.ID) {
		add("node.id must match %s", nameRe)
	}
	if n := c.Interface.Name; n == "" || len(n) > 15 || !nameRe.MatchString(n) {
		add("interface.name %q invalid (max 15 chars, [A-Za-z0-9._-])", n)
	}
	if len(c.Interface.Addresses) == 0 {
		add("interface.addresses: at least one address is required")
	}
	innerV6 := false
	for _, a := range c.Interface.Addresses {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			add("interface.addresses: %q is not a CIDR prefix", a)
			continue
		}
		if p.Addr().Is6() {
			innerV6 = true
		}
	}
	for _, r := range c.Interface.Routes {
		if _, err := netip.ParsePrefix(r); err != nil {
			add("interface.routes: %q is not a CIDR prefix", r)
		}
	}
	if c.Interface.MTU != "auto" {
		n, err := strconv.Atoi(c.Interface.MTU)
		min := 576
		if innerV6 {
			min = 1280
		}
		if err != nil || n < min || n > 65000 {
			add("interface.mtu must be \"auto\" or an integer in [%d, 65000]", min)
		}
	}
	if c.Interface.MinMTU != 0 && (c.Interface.MinMTU < 576 || c.Interface.MinMTU > 65000) {
		add("interface.min_mtu out of range")
	}
	if c.Interface.PathMTU != 0 && (c.Interface.PathMTU < 576 || c.Interface.PathMTU > 65535) {
		add("interface.path_mtu out of range")
	}
	if c.Security.PrivateKeyFile == "" {
		add("security.private_key_file is required")
	}
	if c.Security.RekeyInterval.Duration < time.Minute {
		add("security.rekey_interval must be >= 1m")
	}
	if len(c.Listen) == 0 && len(c.Peers) == 0 {
		add("configure at least one listen entry or peer")
	}
	for i, l := range c.Listen {
		if !KnownCarriers[l.Carrier] {
			add("listen[%d].carrier %q unknown", i, l.Carrier)
		}
		if l.Carrier == "icmp" {
			if !c.Experimental.ICMP {
				add("listen[%d]: icmp carrier requires experimental.icmp: true", i)
			}
			if net.ParseIP(l.Address) == nil {
				add("listen[%d].address: icmp listens on an IP address without port", i)
			}
			continue
		}
		if _, _, err := net.SplitHostPort(l.Address); err != nil {
			add("listen[%d].address %q must be host:port", i, l.Address)
		}
		if (l.TLSCertFile == "") != (l.TLSKeyFile == "") {
			add("listen[%d]: tls_cert_file and tls_key_file must be set together", i)
		}
	}
	names := map[string]bool{}
	for i, p := range c.Peers {
		if !nameRe.MatchString(p.Name) || names[p.Name] {
			add("peers[%d].name %q invalid or duplicate", i, p.Name)
		}
		names[p.Name] = true
		if _, err := ParseKey(p.PublicKey); err != nil {
			add("peers[%d].public_key: %v", i, err)
		}
		if len(p.AllowedIPs) == 0 {
			add("peers[%d].allowed_ips: at least one prefix is required", i)
		}
		for _, a := range p.AllowedIPs {
			if _, err := netip.ParsePrefix(a); err != nil {
				add("peers[%d].allowed_ips: %q invalid", i, a)
			}
		}
		if len(p.Endpoints) > 0 && len(p.Carriers) == 0 {
			add("peers[%d]: endpoints configured but no carriers", i)
		}
		epn := map[string]bool{}
		for j, e := range p.Endpoints {
			if !nameRe.MatchString(e.Name) || epn[e.Name] {
				add("peers[%d].endpoints[%d].name invalid or duplicate", i, j)
			}
			epn[e.Name] = true
			if net.ParseIP(e.Address) == nil && !hostRe.MatchString(e.Address) {
				add("peers[%d].endpoints[%d].address %q must be an IP or hostname without port", i, j, e.Address)
			}
		}
		for j, cr := range p.Carriers {
			if !KnownCarriers[cr.Type] {
				add("peers[%d].carriers[%d].type %q unknown", i, j, cr.Type)
			}
			if cr.Type == "icmp" && *cr.Enabled && !c.Experimental.ICMP {
				add("peers[%d].carriers[%d]: icmp requires experimental.icmp: true", i, j)
			}
			if cr.Type != "icmp" && (cr.Port < 1 || cr.Port > 65535) {
				add("peers[%d].carriers[%d].port must be 1-65535", i, j)
			}
			if cr.Path != "" && (!strings.HasPrefix(cr.Path, "/") || strings.ContainsAny(cr.Path, " \r\n?#")) {
				add("peers[%d].carriers[%d].path invalid", i, j)
			}
		}
	}
	h := c.Health
	if h.Interval.Duration < 100*time.Millisecond || h.PingTimeout.Duration < 100*time.Millisecond {
		add("health.interval and health.ping_timeout must be >= 100ms")
	}
	if h.Window < 3 || h.Window > 1000 {
		add("health.window must be 3-1000")
	}
	if h.DegradedLoss <= 0 || h.FailedLoss > 100 || h.DegradedLoss >= h.FailedLoss {
		add("health: require 0 < degraded_loss_pct < failed_loss_pct <= 100")
	}
	if h.ClearRatio <= 0 || h.ClearRatio > 1 {
		add("health.clear_ratio must be in (0,1]")
	}
	f := c.Failover
	if f.Order != "endpoint" && f.Order != "carrier" {
		add("failover.order must be endpoint or carrier")
	}
	if f.EndpointSelection != "priority" && f.EndpointSelection != "latency" {
		add("failover.endpoint_selection must be priority or latency")
	}
	if f.BackoffInitial.Duration <= 0 || f.BackoffMax.Duration < f.BackoffInitial.Duration {
		add("failover: backoff_max must be >= backoff_initial > 0")
	}
	if f.DegradeHoldoff.Duration < 0 || f.DegradeHoldoff.Duration > time.Hour {
		add("failover.degrade_holdoff must be between 0 and 1h")
	}
	for i, r := range append(append([]Rule{}, c.Forwarding.TCP...), c.Forwarding.UDP...) {
		if _, _, err := net.SplitHostPort(r.Listen); err != nil {
			add("forwarding rule %d: listen %q must be host:port", i, r.Listen)
		}
		if _, _, err := net.SplitHostPort(r.Target); err != nil {
			add("forwarding rule %d: target %q must be host:port", i, r.Target)
		}
	}
	if px := c.Proxy; px.Listen != "" || px.Upstream != "" || len(px.Users) > 0 {
		host, _, err := net.SplitHostPort(px.Listen)
		if err != nil {
			add("proxy.listen %q must be host:port", px.Listen)
		}
		if px.Upstream != "" {
			if _, _, err := net.SplitHostPort(px.Upstream); err != nil {
				add("proxy.upstream %q must be host:port", px.Upstream)
			}
		}
		for i, u := range px.Users {
			if u.Username == "" || u.Password == "" || len(u.Username) > 255 || len(u.Password) > 255 || strings.Contains(u.Username, ":") {
				add("proxy.users[%d]: username/password must be 1-255 characters (no ':' in username)", i)
			}
		}
		if len(px.Users) == 0 && err == nil {
			// without authentication the proxy must only be reachable through the tunnel
			ip, perr := netip.ParseAddr(host)
			ok := perr == nil && ip.IsLoopback()
			for _, pf := range c.Prefixes() {
				if perr == nil && pf.Addr() == ip {
					ok = true
				}
			}
			if !ok {
				add("proxy without users must listen on this node's tunnel address or loopback (got %q)", px.Listen)
			}
		}
	}
	if c.API.Listen != "" {
		host, _, err := net.SplitHostPort(c.API.Listen)
		if err != nil {
			add("api.listen must be host:port")
		} else if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && c.API.TokenFile == "" {
			add("api.listen on a non-loopback address requires api.token_file")
		}
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

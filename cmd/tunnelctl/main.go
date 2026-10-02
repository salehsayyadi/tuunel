// Command tunnelctl administers a running tuunel daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/salehsayyadi/tuunel/internal/api"
	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/diag"
	"github.com/salehsayyadi/tuunel/internal/engine"
	"github.com/salehsayyadi/tuunel/internal/forwarding"
	"github.com/salehsayyadi/tuunel/internal/proxy"
)

var (
	socket  = flag.String("socket", "/run/tuunel/tuunel.sock", "management socket")
	apiURL  = flag.String("api", "", "management API base URL (instead of the socket), e.g. http://127.0.0.1:9900")
	cfgPath = flag.String("config", "/etc/tuunel/config.yaml", "configuration file (doctor)")
	asJSON  = flag.Bool("json", false, "print raw JSON")
	peerF   = flag.String("peer", "", "peer name (default: all/first)")
)

func client() (*http.Client, string) {
	if *apiURL != "" {
		return &http.Client{Timeout: 3 * time.Minute}, strings.TrimRight(*apiURL, "/")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", *socket)
	}}
	return &http.Client{Transport: tr, Timeout: 3 * time.Minute}, "http://tuunel"
}

func call(method, path string, out any) error {
	c, base := client()
	req, _ := http.NewRequest(method, base+path, nil)
	if t := os.Getenv("TUNNELCTL_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if *asJSON {
		os.Stdout.Write(b)
		return errPrinted
	}
	if s, ok := out.(*string); ok {
		*s = string(b)
		return nil
	}
	return json.Unmarshal(b, out)
}

var errPrinted = fmt.Errorf("printed")

func main() {
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "status":
		err = status()
	case "carriers":
		err = carriers()
	case "endpoints":
		err = endpoints()
	case "test-carriers":
		err = testCarriers(200)
	case "test":
		err = test()
	case "ping":
		err = ping(4)
	case "routes":
		err = routes()
	case "metrics":
		var s string
		if err = call("GET", "/api/metrics", &s); err == nil {
			fmt.Print(s)
		}
	case "forwarding":
		err = forwardingCmd()
	case "proxy":
		err = proxyCmd()
	case "reconnect":
		var r map[string]int
		if err = call("POST", "/api/tunnels/reconnect?peer="+*peerF, &r); err == nil {
			fmt.Printf("reconnect requested for %d peer(s)\n", r["reconnected"])
		}
	case "doctor":
		os.Exit(doctor())
	case "logs":
		cmd := exec.Command("journalctl", "-u", "tuunel", "-n", "200", "--no-pager")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && err != errPrinted {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: tunnelctl [-socket PATH | -api URL] [-json] [-peer NAME] <command>

  status          tunnel state, carrier, endpoint, RTT, loss, jitter, traffic
  carriers        listeners and per-carrier candidate health
  endpoints       per-endpoint health summary
  test            in-tunnel ping plus carrier tests
  test-carriers   actively probe every configured endpoint/carrier
  ping            authenticated in-tunnel echo over the active link
  routes          routes installed into the tunnel interface
  metrics         Prometheus metrics
  forwarding      port forwarding rules and counters
  proxy           built-in SOCKS5/HTTP exit proxy: counters and client connection links
  reconnect       force re-establishment of the tunnel
  doctor          diagnose kernel, permissions, config, keys, DNS, MTU, routing, reachability
  logs            recent service logs (journalctl -u tuunel)
`)
}

func human(n uint64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1024 || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return ""
}

func dur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	d = d.Round(time.Second)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, int(d.Seconds())%60)
	}
	return d.String()
}

func ms(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func status() error {
	var st api.FullStatus
	if err := call("GET", "/api/status", &st); err != nil {
		return err
	}
	fmt.Printf("Node: %s   Interface: %s   MTU: %d (%s)   Daemon uptime: %s\n", st.Engine.NodeID, st.Interface, st.Engine.MTU, st.MTU.Mode, dur(st.Engine.Uptime))
	for _, p := range st.Engine.Peers {
		if *peerF != "" && p.Name != *peerF {
			continue
		}
		state := "DOWN"
		if p.Up {
			state = "UP"
		}
		fmt.Printf("\nPeer: %s (%s)\n", p.Name, p.Role)
		fmt.Printf("Tunnel: %s\n", state)
		if p.Up {
			ep := p.Endpoint
			if ep == "" {
				ep = p.Remote + " (inbound)"
			}
			fmt.Printf("Endpoint: %s\nCarrier: %s\nHealth: %s\nRTT: %s\nLoss: %.1f%%\nJitter: %s\n",
				ep, strings.ToUpper(p.Carrier), p.Health.State, ms(p.Health.AvgRTT), p.Health.LossPct, ms(p.Health.Jitter))
		}
		fmt.Printf("TX: %s (%d pkts)\nRX: %s (%d pkts)\nDrops: %d\nReconnects: %d\nUptime: %s\n",
			human(p.TxBytes), p.TxPackets, human(p.RxBytes), p.RxPackets, p.Drops, p.Reconnects, dur(p.Uptime))
		if !p.Up && p.Role == "initiator" {
			fmt.Println("No usable path: every configured endpoint/carrier is failing (see `tunnelctl carriers`).")
		}
	}
	return nil
}

func carriers() error {
	var st api.FullStatus
	if err := call("GET", "/api/status", &st); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(st.Engine.Listeners) > 0 {
		fmt.Fprintln(w, "LISTENER\tADDRESS\tSTATE")
		for _, l := range st.Engine.Listeners {
			s := "listening"
			if !l.Listening {
				s = "FAILED: " + l.Error
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", l.Carrier, l.Address, s)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "PEER\tENDPOINT\tCARRIER\tADDRESS\tSTATE\tACTIVE\tRTT\tFAILS\tRETRY\tLAST ERROR")
	for _, p := range st.Engine.Peers {
		for _, c := range p.Candidates {
			act := ""
			if c.Active {
				act = "*"
			}
			st := string(c.State)
			if c.Parked {
				st += " (cooldown)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", p.Name, c.Endpoint, c.Carrier, c.Address, st, act, ms(c.LastRTT), c.Failures, dur(c.RetryIn), diag.ClassifyError(c.LastError))
		}
	}
	return w.Flush()
}

func endpoints() error {
	var st api.FullStatus
	if err := call("GET", "/api/status", &st); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PEER\tENDPOINT\tSTATE\tBEST RTT\tCARRIERS OK")
	for _, p := range st.Engine.Peers {
		type agg struct {
			state   string
			rtt     time.Duration
			ok, all int
		}
		order := []string{}
		m := map[string]*agg{}
		for _, c := range p.Candidates {
			a := m[c.Endpoint]
			if a == nil {
				a = &agg{state: "FAILED"}
				m[c.Endpoint] = a
				order = append(order, c.Endpoint)
			}
			a.all++
			if c.State == "AVAILABLE" || c.State == "DEGRADED" || c.State == "RECOVERING" {
				a.ok++
				if a.state != "ACTIVE" {
					a.state = string(c.State)
				}
				if c.LastRTT > 0 && (a.rtt == 0 || c.LastRTT < a.rtt) {
					a.rtt = c.LastRTT
				}
			} else if c.State == "UNKNOWN" && a.state == "FAILED" {
				a.state = "UNKNOWN"
			}
			if c.Active {
				a.state = "ACTIVE"
			}
		}
		for _, e := range order {
			a := m[e]
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\n", p.Name, e, a.state, ms(a.rtt), a.ok, a.all)
		}
	}
	return w.Flush()
}

func testCarriers(bulk int) error {
	var rs []engine.ProbeReport
	if err := call("POST", fmt.Sprintf("/api/carriers/test?bulk=%d", bulk), &rs); err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Println("No dialable carriers configured on this node (it only accepts inbound peers). Run test-carriers on the initiating node.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PEER\tENDPOINT\tCARRIER\tRESULT\tHANDSHAKE\tRTT\tJITTER\tLOSS\tECHO THROUGHPUT\tDETAIL")
	for _, r := range rs {
		res := "OK"
		switch {
		case !r.OK && r.Handshake:
			res = "UNRELIABLE"
		case !r.OK && strings.Contains(strings.ToLower(r.Error), "permission"):
			res = "PERMISSION DENIED"
		case !r.OK && strings.Contains(strings.ToLower(r.Error), "disabled"):
			res = "DISABLED"
		case !r.OK:
			res = "FAILED"
		case r.LossPct > 20:
			res = "DEGRADED"
		}
		tp := "-"
		if r.ThroughputBps > 0 {
			tp = fmt.Sprintf("%.1f Mbit/s", r.ThroughputBps/1e6)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\n", r.Peer, r.Endpoint, r.Carrier, res, ms(r.HandshakeRTT), ms(r.RTT), ms(r.Jitter), r.LossPct, tp, diag.ClassifyError(r.Error))
	}
	return w.Flush()
}

func ping(n int) error {
	var rs []engine.PingResult
	if err := call("POST", fmt.Sprintf("/api/ping?count=%d&peer=%s", n, *peerF), &rs); err != nil {
		return err
	}
	ok := 0
	for _, r := range rs {
		if r.Error != "" {
			fmt.Printf("seq=%d %s\n", r.Seq, r.Error)
			continue
		}
		ok++
		fmt.Printf("seq=%d carrier=%s rtt=%s\n", r.Seq, r.Carrier, ms(r.RTT))
	}
	fmt.Printf("%d/%d replies\n", ok, len(rs))
	if ok == 0 {
		return fmt.Errorf("no replies")
	}
	return nil
}

func test() error {
	fmt.Println("== in-tunnel ping ==")
	perr := ping(4)
	fmt.Println("\n== carriers ==")
	if err := testCarriers(0); err != nil {
		return err
	}
	return perr
}

func routes() error {
	var r struct {
		Interface string   `json:"interface"`
		Routes    []string `json:"routes"`
		Errors    []string `json:"errors"`
	}
	if err := call("GET", "/api/routes", &r); err != nil {
		return err
	}
	var st api.FullStatus
	if err := call("GET", "/api/status", &st); err == nil {
		for _, p := range st.Engine.Peers {
			fmt.Printf("peer %s allowed IPs: %s\n", p.Name, strings.Join(p.AllowedIPs, ", "))
		}
	}
	fmt.Printf("routes via %s:\n", r.Interface)
	if len(r.Routes) == 0 {
		fmt.Println("  (none beyond the connected interface subnet)")
	}
	for _, x := range r.Routes {
		fmt.Println("  " + x)
	}
	for _, e := range r.Errors {
		fmt.Println("ERROR:", e)
	}
	return nil
}

func forwardingCmd() error {
	var rs []forwarding.RuleStatus
	if err := call("GET", "/api/forwarding", &rs); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROTO\tLISTEN\tTARGET\tACTIVE\tTOTAL\tREJECTED\tIN\tOUT")
	for _, r := range rs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", r.Proto, r.Listen, r.Target, r.Active, r.Total, r.Rejected, human(r.BytesIn), human(r.BytesOut))
	}
	return w.Flush()
}

func proxyCmd() error {
	var r struct {
		Enabled bool         `json:"enabled"`
		Stats   *proxy.Stats `json:"stats"`
	}
	if err := call("GET", "/api/proxy", &r); err != nil {
		return err
	}
	if !r.Enabled || r.Stats == nil {
		fmt.Println("Proxy: disabled (no proxy section in the configuration)")
		return nil
	}
	st := r.Stats
	fmt.Printf("Proxy: %s  (%s)\nActive: %d   Total: %d   Rejected: %d   Auth failures: %d\nIn: %s   Out: %s\n",
		st.Listen, st.Mode, st.Active, st.Total, st.Rejected, st.AuthFail, human(st.BytesIn), human(st.BytesOut))
	cfg, err := config.Load(*cfgPath)
	if err != nil || len(cfg.Proxy.Users) == 0 {
		if err == nil && cfg.Proxy.Upstream == "" {
			fmt.Println("This is the backend (exit) side: clients connect to the edge node's proxy port.")
		}
		return nil
	}
	_, port, _ := net.SplitHostPort(cfg.Proxy.Listen)
	ip := publicIP()
	u := cfg.Proxy.Users[0]
	fmt.Printf("\nClient settings (SOCKS5 or HTTP proxy on the same port):\n  server:   %s\n  port:     %s\n  username: %s\n  password: %s\n", ip, port, u.Username, u.Password)
	fmt.Printf("  socks5://%s:%s@%s:%s\n  http://%s:%s@%s:%s\n  Telegram: tg://socks?server=%s&port=%s&user=%s&pass=%s\n",
		u.Username, u.Password, ip, port, u.Username, u.Password, ip, port, ip, port, u.Username, u.Password)
	return nil
}

// publicIP returns the source address of the default route (no packet is
// sent) or a placeholder when it is not a public address.
func publicIP() string {
	if v := os.Getenv("TUUNEL_PUBLIC_IP"); v != "" {
		return v
	}
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err == nil {
		defer c.Close()
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsPrivate() && !a.IP.IsLoopback() {
			return a.IP.String()
		}
	}
	return "SERVER_PUBLIC_IP"
}

func doctor() int {
	in := diag.Inputs{ConfigPath: *cfgPath}
	save := *asJSON
	*asJSON = false
	var st api.FullStatus
	if err := call("GET", "/api/status", &st); err != nil {
		in.StatusErr = err
	} else {
		in.Status = &st
		var rs []engine.ProbeReport
		if err := call("POST", "/api/carriers/test?bulk=0", &rs); err == nil {
			in.Probes = rs
		}
	}
	*asJSON = save
	rep := diag.Run(context.Background(), in)
	for _, r := range rep.Results {
		fmt.Printf("[%-4s] %-13s %s\n", r.Level, r.Area, r.Detail)
	}
	fmt.Printf("\nOverall: %s\n", rep.Worst())
	if rep.Worst() == diag.Fail {
		return 1
	}
	return 0
}

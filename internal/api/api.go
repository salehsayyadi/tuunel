// Package api implements the local management API. It is served on a Unix
// socket by default (filesystem permissions control access). An optional TCP
// listener requires a bearer token unless bound to loopback.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/salehsayyadi/tuunel/internal/engine"
	"github.com/salehsayyadi/tuunel/internal/forwarding"
	"github.com/salehsayyadi/tuunel/internal/mtu"
	"github.com/salehsayyadi/tuunel/internal/proxy"
)

// Backend is what the API exposes; *engine.Engine plus daemon extras.
type Backend struct {
	Engine     *engine.Engine
	Forwarding *forwarding.Manager
	MTU        mtu.Plan
	Interface  string
	Routes     func() []string
	RouteErrs  []string
	Version    string
	Proxy      *proxy.Server
}

type Server struct {
	b     Backend
	token []byte
	srvs  []*http.Server
}

func New(b Backend, token []byte) *Server { return &Server{b: b, token: token} }

func (s *Server) handler(requireToken bool) http.Handler {
	mux := http.NewServeMux()
	get := func(path string, f func(r *http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.reply(w, r, f)
		})
	}
	post := func(path string, f func(r *http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.reply(w, r, f)
		})
	}
	get("/api/status", func(*http.Request) (any, error) { return s.status(), nil })
	get("/api/tunnels", func(*http.Request) (any, error) { return s.b.Engine.Status().Peers, nil })
	get("/api/carriers", func(*http.Request) (any, error) {
		st := s.b.Engine.Status()
		type out struct {
			Listeners []engine.ListenerStatus             `json:"listeners"`
			Peers     map[string][]engine.CandidateStatus `json:"peers"`
		}
		o := out{Listeners: st.Listeners, Peers: map[string][]engine.CandidateStatus{}}
		for _, p := range st.Peers {
			o.Peers[p.Name] = p.Candidates
		}
		return o, nil
	})
	get("/api/routes", func(*http.Request) (any, error) {
		var r []string
		if s.b.Routes != nil {
			r = s.b.Routes()
		}
		return map[string]any{"interface": s.b.Interface, "routes": r, "errors": s.b.RouteErrs}, nil
	})
	get("/api/forwarding", func(*http.Request) (any, error) {
		if s.b.Forwarding == nil {
			return []forwarding.RuleStatus{}, nil
		}
		return s.b.Forwarding.Status(), nil
	})
	get("/api/proxy", func(*http.Request) (any, error) {
		if s.b.Proxy == nil {
			return map[string]any{"enabled": false}, nil
		}
		return map[string]any{"enabled": true, "stats": s.b.Proxy.Stats()}, nil
	})
	get("/api/mtu", func(*http.Request) (any, error) { return s.b.MTU, nil })
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(Prometheus(s.b.Engine.Status())))
	})
	post("/api/tunnels/reconnect", func(r *http.Request) (any, error) {
		return map[string]int{"reconnected": s.b.Engine.Reconnect(r.URL.Query().Get("peer"))}, nil
	})
	post("/api/carriers/test", func(r *http.Request) (any, error) {
		bulk, _ := strconv.Atoi(r.URL.Query().Get("bulk"))
		if bulk < 0 || bulk > 2000 {
			return nil, errors.New("bulk must be 0-2000")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		return s.b.Engine.TestCarriers(ctx, bulk), nil
	})
	post("/api/ping", func(r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("count"))
		return s.b.Engine.Ping(r.URL.Query().Get("peer"), n, 500*time.Millisecond)
	})
	var h http.Handler = mux
	if requireToken {
		h = s.auth(h)
	}
	return limitBody(h)
}

func limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		h.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(s.token) == 0 || subtle.ConstantTimeCompare([]byte(got), s.token) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, f func(*http.Request) (any, error)) {
	v, err := f(r)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// FullStatus is returned by /api/status.
type FullStatus struct {
	Version   string        `json:"version"`
	Interface string        `json:"interface"`
	MTU       mtu.Plan      `json:"mtu"`
	Engine    engine.Status `json:"engine"`
}

func (s *Server) status() FullStatus {
	return FullStatus{Version: s.b.Version, Interface: s.b.Interface, MTU: s.b.MTU, Engine: s.b.Engine.Status()}
}

// ServeUnix listens on a Unix socket with mode 0660.
func (s *Server) ServeUnix(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.handler(false), ReadHeaderTimeout: 5 * time.Second}
	s.srvs = append(s.srvs, srv)
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// ServeTCP listens on addr; a token is mandatory for non-loopback addresses.
func (s *Server) ServeTCP(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	loop := ip != nil && ip.IsLoopback()
	if !loop && len(s.token) == 0 {
		return fmt.Errorf("api: refusing to listen on %s without a token", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(len(s.token) > 0), ReadHeaderTimeout: 5 * time.Second}
	s.srvs = append(s.srvs, srv)
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (s *Server) Close() {
	for _, srv := range s.srvs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
}

// BuildLabels (version, commit, Go version) is set by the daemon at start.
var BuildLabels [3]string

// Prometheus renders engine metrics in the Prometheus text format.
func Prometheus(st engine.Status) string {
	var b strings.Builder
	w := func(name, help, typ string, lines ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	lbl := func(p engine.PeerStatus) string {
		return fmt.Sprintf(`{peer=%q,carrier=%q,endpoint=%q}`, p.Name, p.Carrier, p.Endpoint)
	}
	var up, rtt, loss, jit, tx, rx, txp, rxp, rc, upt, drops, sw, esw, fsw, act []string
	for _, p := range st.Peers {
		l := lbl(p)
		v := 0
		if p.Up {
			v = 1
		}
		up = append(up, fmt.Sprintf("tuunel_tunnel_up%s %d", l, v))
		rtt = append(rtt, fmt.Sprintf("tuunel_rtt_seconds%s %g", l, p.Health.AvgRTT.Seconds()))
		loss = append(loss, fmt.Sprintf("tuunel_packet_loss_ratio%s %g", l, p.Health.LossPct/100))
		jit = append(jit, fmt.Sprintf("tuunel_jitter_seconds%s %g", l, p.Health.Jitter.Seconds()))
		tx = append(tx, fmt.Sprintf("tuunel_tx_bytes_total{peer=%q} %d", p.Name, p.TxBytes))
		rx = append(rx, fmt.Sprintf("tuunel_rx_bytes_total{peer=%q} %d", p.Name, p.RxBytes))
		txp = append(txp, fmt.Sprintf("tuunel_tx_packets_total{peer=%q} %d", p.Name, p.TxPackets))
		rxp = append(rxp, fmt.Sprintf("tuunel_rx_packets_total{peer=%q} %d", p.Name, p.RxPackets))
		rc = append(rc, fmt.Sprintf("tuunel_reconnects_total{peer=%q} %d", p.Name, p.Reconnects))
		upt = append(upt, fmt.Sprintf("tuunel_tunnel_uptime_seconds{peer=%q} %g", p.Name, p.Uptime.Seconds()))
		sw = append(sw, fmt.Sprintf("tuunel_carrier_switches_total{peer=%q} %d", p.Name, p.CarrierSwitch))
		esw = append(esw, fmt.Sprintf("tuunel_endpoint_switches_total{peer=%q} %d", p.Name, p.EndpointSw))
		fsw = append(fsw, fmt.Sprintf("tuunel_failure_switches_total{peer=%q} %d", p.Name, p.FailureSw))
		if p.Up {
			act = append(act, fmt.Sprintf("tuunel_active_carrier_info%s 1", l))
		}
	}
	keys := make([]string, 0, len(st.Drops))
	for k := range st.Drops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		drops = append(drops, fmt.Sprintf("tuunel_dropped_packets_total{reason=%q} %d", k, st.Drops[k]))
	}
	w("tuunel_tunnel_up", "Whether the tunnel to the peer is up.", "gauge", up...)
	w("tuunel_rtt_seconds", "Average in-tunnel RTT.", "gauge", rtt...)
	w("tuunel_packet_loss_ratio", "Probe loss ratio over the health window.", "gauge", loss...)
	w("tuunel_jitter_seconds", "Smoothed RTT jitter.", "gauge", jit...)
	w("tuunel_tx_bytes_total", "Inner IP bytes sent.", "counter", tx...)
	w("tuunel_rx_bytes_total", "Inner IP bytes received.", "counter", rx...)
	w("tuunel_tx_packets_total", "Inner IP packets sent.", "counter", txp...)
	w("tuunel_rx_packets_total", "Inner IP packets received.", "counter", rxp...)
	w("tuunel_reconnects_total", "Link re-establishments.", "counter", rc...)
	w("tuunel_tunnel_uptime_seconds", "Current link uptime.", "gauge", upt...)
	w("tuunel_dropped_packets_total", "Dropped packets by reason.", "counter", drops...)
	w("tuunel_carrier_switches_total", "Changes of the active endpoint/carrier candidate.", "counter", sw...)
	w("tuunel_endpoint_switches_total", "Switches that changed the endpoint.", "counter", esw...)
	w("tuunel_failure_switches_total", "Switches caused by failure of the active candidate.", "counter", fsw...)
	w("tuunel_active_carrier_info", "Active carrier and endpoint per peer (value 1).", "gauge", act...)
	if b := BuildLabels; b[0] != "" {
		w("tuunel_build_info", "Build information (value 1).", "gauge", fmt.Sprintf("tuunel_build_info{version=%q,commit=%q,goversion=%q} 1", b[0], b[1], b[2]))
	}
	// Process gauges used by the long-run resource test to detect leaks.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	w("tuunel_goroutines", "Number of goroutines in the daemon.", "gauge", fmt.Sprintf("tuunel_goroutines %d", runtime.NumGoroutine()))
	w("tuunel_heap_inuse_bytes", "Go heap bytes in use.", "gauge", fmt.Sprintf("tuunel_heap_inuse_bytes %d", ms.HeapInuse))
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		w("tuunel_open_fds", "Open file descriptors.", "gauge", fmt.Sprintf("tuunel_open_fds %d", len(ents)))
	}
	return b.String()
}

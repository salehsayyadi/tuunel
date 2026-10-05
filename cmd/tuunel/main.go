// Command tuunel is the tunnel daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/daemon"
	"github.com/salehsayyadi/tuunel/internal/exitnode"
	"github.com/salehsayyadi/tuunel/internal/license"
	"github.com/salehsayyadi/tuunel/internal/mtu"
	"github.com/salehsayyadi/tuunel/internal/tun"
	"github.com/salehsayyadi/tuunel/internal/tunnel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfg := fs.String("config", "/etc/tuunel/config.yaml", "configuration file")
		_ = fs.Parse(os.Args[2:])
		if pf := os.Getenv("TUUNEL_CPUPROFILE"); pf != "" { // development: CPU profile of the daemon
			if f, err := os.Create(pf); err == nil {
				_ = pprof.StartCPUProfile(f)
				defer pprof.StopCPUProfile()
			}
		}
		if err := daemon.Run(*cfg); err != nil {
			fatal(err.Error())
		}
	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/tuunel/config.yaml", "configuration file")
		_ = fs.Parse(os.Args[2:])
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fatal(err.Error())
		}
		if _, warn, err := daemon.LoadPrivateKey(cfg.Security.PrivateKeyFile); err != nil {
			fatal(err.Error())
		} else if warn != "" {
			fmt.Println("WARN:", warn)
		}
		plan, err := daemon.BuildPlan(cfg, mtu.DetectPathMTU)
		if err != nil {
			fatal(err.Error())
		}
		for _, w := range plan.MTU.Warnings {
			fmt.Println("WARN: mtu:", w)
		}
		for _, e := range plan.Routes.Errors {
			fmt.Println("ERROR: routing:", e)
		}
		if len(plan.Routes.Errors) > 0 {
			os.Exit(1)
		}
		fmt.Printf("configuration OK: node=%s mtu=%d (%s) peers=%d listeners=%d\n", cfg.Node.ID, plan.MTU.TunMTU, plan.MTU.Mode, len(cfg.Peers), len(cfg.Listen))
	case "exit-up", "exit-down":
		// run as root by systemd (ExecStartPost=+ / ExecStopPost=+); never fails the unit
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/tuunel/config.yaml", "configuration file")
		_ = fs.Parse(os.Args[2:])
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "exit:", err)
			return
		}
		var eps []netip.Addr
		if plan, err := daemon.BuildPlan(cfg, nil); err == nil {
			eps = plan.Endpoints
		}
		a := exitnode.New(cfg, eps)
		if os.Args[1] == "exit-down" {
			if cfg.Exit.Mode != "" {
				a.Down()
				fmt.Fprintln(os.Stderr, "exit: rules removed")
			}
			return
		}
		if err := a.Up(); err != nil {
			fmt.Fprintln(os.Stderr, "exit: ERROR:", err)
		}
	case "license":
		licenseCmd(os.Args[2:])
	case "genkey":
		priv, pub, err := daemon.GenKey()
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(priv)
		fmt.Fprintln(os.Stderr, "public key:", pub)
	case "pubkey":
		fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
		path := fs.String("key", "/etc/tuunel/node.key", "private key file")
		_ = fs.Parse(os.Args[2:])
		k, _, err := daemon.LoadPrivateKey(*path)
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(b64(k.Public))
	case "genpsk":
		k, err := daemon.GenPSK()
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(k)
	case "version", "-version", "--version":
		fmt.Println(daemon.VersionString())
	case "server", "client":
		legacy(os.Args[1])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: tuunel <command> [flags]

Commands:
  run     -config FILE   run the tunnel daemon
  exit-up / exit-down -config FILE
                         apply/remove "route all" exit networking (run by systemd as root)
  check   -config FILE   validate configuration, keys, MTU and routing plan
  genkey                 print a new private key (public key on stderr)
  pubkey  -key FILE      print the public key for a private key file
  genpsk                 print a new optional pre-shared key
  version                print version
  license status         show the activation license of this server
  license sync -config FILE
                         refresh the license online and pair with the peer (root; systemd timer)
  server|client          legacy MVP mode: single TUN over TCP+TLS (see README)
`)
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}

// legacy keeps the original MVP (TUN over TCP with TLS 1.3 mutual auth).
func legacy(mode string) {
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	iface := fs.String("tun", "tun0", "Linux TUN interface (created if absent)")
	mtuV := fs.Int("mtu", 1300, "maximum inner IP packet size (576..65535)")
	listen := fs.String("listen", ":9443", "TCP listen address (server)")
	endpoint := fs.String("endpoint", "", "remote host:port (client)")
	cert := fs.String("cert", "", "node certificate PEM")
	key := fs.String("key", "", "node private key PEM")
	ca := fs.String("ca", "", "trusted CA PEM")
	serverName := fs.String("server-name", "", "expected server DNS name (client)")
	peerName := fs.String("peer-name", "", "expected client certificate DNS name (server)")
	_ = fs.Parse(os.Args[2:])
	if *mtuV < 576 || *mtuV > 65535 {
		fatal("MTU must be between 576 and 65535")
	}
	if *cert == "" || *key == "" || *ca == "" {
		fatal("-cert, -key and -ca are required")
	}
	isServer := mode == "server"
	if isServer && (*endpoint != "" || *serverName != "" || *peerName == "") {
		fatal("server requires -peer-name; -endpoint and -server-name are client-only")
	}
	if !isServer && (*endpoint == "" || *serverName == "" || *peerName != "") {
		fatal("client requires -endpoint and -server-name; -peer-name is server-only")
	}
	identity := *serverName
	if isServer {
		identity = *peerName
	}
	tlsConfig, err := tunnel.TLSConfig(*cert, *key, *ca, identity, isServer)
	if err != nil {
		fatal(err.Error())
	}
	device, err := tun.Open(*iface)
	if err != nil {
		fatal(err.Error())
	}
	defer device.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg := tunnel.Config{Tun: device, MTU: *mtuV}
	if isServer {
		err = tunnel.RunServer(ctx, *listen, tlsConfig, cfg)
	} else {
		err = tunnel.RunClient(ctx, *endpoint, tlsConfig, cfg)
	}
	if err != nil {
		fatal(err.Error())
	}
}

func licenseCmd(args []string) {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("license", flag.ExitOnError)
	cfg := fs.String("config", "/etc/tuunel/config.yaml", "configuration file")
	file := fs.String("file", license.File, "license file")
	restart := fs.Bool("restart", true, "sync: restart the tuunel service when the peer key changed")
	_ = fs.Parse(args)
	license.File = *file
	switch sub {
	case "status":
		st := license.Check()
		if !st.Required {
			fmt.Println("license: not required (unlicensed build)")
			return
		}
		if st.Payload != nil {
			p := st.Payload
			fmt.Printf("license: id=%d role=%s server=%s\n", p.ID, p.Role, p.Server)
			fmt.Printf("expires: %s\n", time.Unix(p.Expires, 0).UTC().Format(time.RFC3339))
		}
		if !st.Valid {
			fmt.Println("status:  INVALID:", st.Reason)
			os.Exit(1)
		}
		fmt.Printf("status:  valid, %s left\n", st.Left.Round(time.Second))
	case "machine":
		m, err := license.Machine()
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(m)
	case "sync":
		if !license.Required() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := license.Sync(ctx, *cfg)
		if err != nil {
			fatal(err.Error())
		}
		if res.Message != "" {
			fmt.Println("license server:", res.Message)
		}
		if res.Refreshed {
			fmt.Println("license refreshed")
		}
		if res.PeerUpdated {
			fmt.Println("peer public key installed from the license server")
			if *restart {
				if out, err := exec.Command("systemctl", "restart", "tuunel").CombinedOutput(); err != nil {
					fmt.Fprintln(os.Stderr, "systemctl restart tuunel:", err, string(out))
				}
			}
		}
		if res.Revoked {
			os.Exit(3)
		}
	default:
		fatal("license: unknown subcommand " + sub + " (status, sync, machine)")
	}
}

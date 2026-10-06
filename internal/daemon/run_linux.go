//go:build linux

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/salehsayyadi/tuunel/internal/api"
	"github.com/salehsayyadi/tuunel/internal/config"
	"github.com/salehsayyadi/tuunel/internal/engine"
	"github.com/salehsayyadi/tuunel/internal/forwarding"
	"github.com/salehsayyadi/tuunel/internal/license"
	"github.com/salehsayyadi/tuunel/internal/mtu"
	"github.com/salehsayyadi/tuunel/internal/netcfg"
	"github.com/salehsayyadi/tuunel/internal/proxy"
	"github.com/salehsayyadi/tuunel/internal/tun"
)

func NewLogger(c config.Log) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// Run starts the daemon and blocks until SIGINT/SIGTERM.
func Run(configPath string) error {
	if os.Getenv("GOGC") == "" {
		// The data path allocates one short-lived buffer per packet; a less
		// eager collector trades a few MB of memory for noticeably less CPU.
		debug.SetGCPercent(400)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := NewLogger(cfg.Log)
	slog.SetDefault(log)
	sigCtx, sigStop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer sigStop()
	if license.Required() {
		if err := license.Wait(sigCtx, log); err != nil {
			return nil // stopped while waiting for a license
		}
	}
	key, warn, err := LoadPrivateKey(cfg.Security.PrivateKeyFile)
	if err != nil {
		return err
	}
	if warn != "" {
		log.Warn(warn)
	}
	psk, err := LoadPSK(cfg.Security.PresharedKeyFile)
	if err != nil {
		return err
	}
	plan, err := BuildPlan(cfg, mtu.DetectPathMTU)
	if err != nil {
		return err
	}
	for _, w := range plan.MTU.Warnings {
		log.Warn("mtu", "detail", w)
	}
	if plan.MTU.Pathological {
		log.Error("pathological MTU configuration: expect heavy fragmentation; see tunnelctl doctor")
	}
	for _, e := range plan.Routes.Errors {
		log.Error("routing", "detail", e)
	}
	log.Info("configuration loaded", "node", cfg.Node.ID, "summary", describe(plan))

	dev, err := tun.OpenDevice(cfg.Interface.Name)
	if err != nil {
		return err
	}
	defer dev.Close()
	log.Info("tun device opened", "name", dev.Name(), "offload", dev.Offload(), "batch", dev.BatchSize())
	var nc *netcfg.Configurator
	var applied []string
	if *cfg.Interface.Manage {
		nc, err = netcfg.New(dev.Name())
		if err != nil {
			return err
		}
		defer func() {
			if err := nc.Teardown(); err != nil {
				log.Warn("interface teardown", "error", err)
			}
		}()
		if err := nc.SetMTU(plan.MTU.TunMTU); err != nil {
			return err
		}
		for _, pf := range cfg.Prefixes() {
			if err := nc.AddAddr(pf); err != nil {
				return err
			}
		}
		if err := nc.Up(); err != nil {
			return err
		}
		for _, r := range plan.Routes.Routes {
			if err := nc.AddRoute(r); err != nil {
				log.Error("route", "prefix", r.String(), "error", err)
				continue
			}
			applied = append(applied, r.String())
		}
		log.Info("interface configured", "name", dev.Name(), "addresses", strings.Join(cfg.Interface.Addresses, ","), "mtu", plan.MTU.TunMTU, "routes", len(applied))
	}

	ec := plan.Engine
	ec.Key, ec.PSK, ec.Device, ec.Logger = key, psk, dev, log
	eng, err := engine.New(ec)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(sigCtx)
	defer stop()
	var licErr error
	var licMu sync.Mutex
	if license.Required() {
		go license.Watch(ctx, log, func(e error) {
			licMu.Lock()
			licErr = e
			licMu.Unlock()
			stop()
		})
	}

	fw := forwarding.New(log)
	defer fw.Close()
	if err := fw.Apply(ctx, ForwardRules(cfg)); err != nil {
		log.Error("forwarding", "error", err)
	}

	var token []byte
	if cfg.API.TokenFile != "" {
		b, err := os.ReadFile(cfg.API.TokenFile)
		if err != nil {
			return fmt.Errorf("api token: %w", err)
		}
		token = []byte(strings.TrimSpace(string(b)))
		if len(token) < 16 {
			return fmt.Errorf("api token must be at least 16 characters")
		}
	}
	var px *proxy.Server
	if cfg.Proxy.Listen != "" {
		px = proxy.New(ProxyConfig(cfg), log)
		go func() {
			if err := px.Listen(ctx, 30*time.Second); err != nil {
				log.Error("proxy listen failed", "address", cfg.Proxy.Listen, "error", err)
				return
			}
			log.Info("proxy listening", "address", px.Addr().String(), "mode", px.Mode())
			px.Serve(ctx)
		}()
		defer px.Close()
	}
	v, c, g := BuildInfo()
	api.BuildLabels = [3]string{v, c, g}
	srv := api.New(api.Backend{Engine: eng, Forwarding: fw, MTU: plan.MTU, Interface: dev.Name(), Version: Version,
		Routes: func() []string { return applied }, RouteErrs: plan.Routes.Errors, Proxy: px}, token)
	if err := srv.ServeUnix(cfg.API.Socket); err != nil {
		log.Warn("management socket unavailable", "path", cfg.API.Socket, "error", err)
	}
	if cfg.API.Listen != "" {
		if err := srv.ServeTCP(cfg.API.Listen); err != nil {
			return err
		}
	}
	defer srv.Close()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			nc, err := config.Load(configPath)
			if err != nil {
				log.Error("reload rejected", "error", err)
				continue
			}
			if err := fw.Apply(ctx, ForwardRules(nc)); err != nil {
				log.Error("reload forwarding", "error", err)
			}
			log.Info("configuration reloaded (forwarding rules); other changes require restart")
		}
	}()
	err = eng.Run(ctx)
	log.Info("shutting down")
	licMu.Lock()
	defer licMu.Unlock()
	if licErr != nil {
		return licErr // non-zero exit: systemd restarts the unit, which then waits for a license
	}
	return err
}

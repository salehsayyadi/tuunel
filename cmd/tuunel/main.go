package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/salehsayyadi/tuunel/internal/tun"
	"github.com/salehsayyadi/tuunel/internal/tunnel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	iface := fs.String("tun", "tun0", "Linux TUN interface (created if absent)")
	mtu := fs.Int("mtu", 1300, "maximum inner IP packet size (576..65535)")
	listen := fs.String("listen", ":9443", "TCP listen address (server)")
	endpoint := fs.String("endpoint", "", "remote host:port (client)")
	cert := fs.String("cert", "", "node certificate PEM")
	key := fs.String("key", "", "node private key PEM")
	ca := fs.String("ca", "", "trusted CA PEM")
	serverName := fs.String("server-name", "", "expected server DNS name (client)")
	peerName := fs.String("peer-name", "", "expected client certificate DNS name (server)")
	_ = fs.Parse(os.Args[2:])
	if mode != "server" && mode != "client" {
		usage()
		os.Exit(2)
	}
	if *mtu < 576 || *mtu > 65535 {
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
	cfg := tunnel.Config{Tun: device, MTU: *mtu}
	if isServer {
		slog.Info("listening", "address", *listen, "tun", *iface, "mtu", *mtu)
		err = tunnel.RunServer(ctx, *listen, tlsConfig, cfg)
	} else {
		slog.Info("connecting", "endpoint", *endpoint, "tun", *iface, "mtu", *mtu)
		err = tunnel.RunClient(ctx, *endpoint, tlsConfig, cfg)
	}
	if err != nil {
		fatal(err.Error())
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: tuunel {server|client} [flags]\nSee README.md for certificate and TUN setup.")
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}

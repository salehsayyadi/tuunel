# tuunel

`tuunel` is a **Layer 3 tunnel engine for Linux**, written in Go. It moves
whole IPv4/IPv6 packets between two `tun` interfaces over an authenticated,
encrypted session (Noise IKpsk2). That session runs over interchangeable
transports ("carriers"): **TCP, UDP, QUIC (stream or DATAGRAM), WebSocket/WSS**
and an **experimental ICMP** carrier. A health engine measures every path, and
a failover manager moves the tunnel between carriers and endpoints without
restarting the daemon or recreating the interface.

The public architecture of
[BackPack](https://github.com/AminMGMT/BackPack) informed the design. This is an
independent implementation: no code or protocol was copied.

> **Status: pre-1.0, NOT production ready.** See [FINAL_AUDIT.md](FINAL_AUDIT.md)
> for what was verified (real TUN, network-namespace lab) and what was not
> (systemd/installer execution, Docker, real Internet, two physical servers).
> This is a self-review, not an independent security audit.

## Architecture (short)

```
 app ─▶ tun0 ─▶ engine (allowed-IPs routing, anti-spoof, MTU)
                  │
                  ▼
        Noise IKpsk2 session (X25519 / ChaCha20-Poly1305 / BLAKE2s,
        64-bit counters, 2048-packet replay window, rekey)
                  │
                  ▼
        failover manager ◀── health engine (RTT, jitter, loss, states)
                  │
     ┌──────┬─────┼──────┬───────┬────────┐
    tcp    udp   quic  quic-dgram  wss/ws   icmp(exp)
```

Details: [ARCHITECTURE.md](ARCHITECTURE.md) (components),
[PROTOCOL.md](PROTOCOL.md) (wire format), [SECURITY.md](SECURITY.md).

## Supported systems and requirements

- Linux with `/dev/net/tun` (kernel ≥ 4.x; tested on 6.18), amd64 or arm64.
- The installer needs systemd, `bash`, `ip` (iproute2) and optionally `setcap`.
  Intended targets: Ubuntu 22.04/24.04, Debian 12, RHEL/Alma/Rocky 9. The
  installer has **not** been executed on these systems (see FINAL_AUDIT.md).
- Building from source: Go **1.26+** (`go.mod` says `go 1.26.0`; release
  builds and the audit used Go 1.27.1).
- Capabilities: `CAP_NET_ADMIN` (TUN, addresses, routes),
  `CAP_NET_BIND_SERVICE` (ports < 1024), `CAP_NET_RAW` (ICMP carrier only).
  The systemd unit runs as user `tuunel` with only these capabilities.
  Running as non-root with exactly these capabilities was tested in the lab.

## Topology used by the installer (reverse tunnel)

| Role | Typical host | Does | Tunnel IP |
|---|---|---|---|
| `edge` (alias `iran`) | server with a public IP that clients reach | **listens** on tcp/443, quic(udp)/443, wss/8443, udp/51900; can expose services of the remote node with `forwarding:` | 10.200.0.1/30 |
| `remote` (alias `foreign`, `kharej`) | server with unrestricted upstream | **dials** the edge; needs no inbound port | 10.200.0.2/30 |

## Installation

Build a release archive (on any machine with Go 1.26+):

```bash
git clone https://github.com/salehsayyadi/tuunel && cd tuunel
scripts/release.sh v0.9.0           # dist/tuunel-linux-{amd64,arm64}.tar.gz, install.sh, SHA256SUMS
```

Copy the archive for the server's architecture to **both** servers, then:

```bash
# EDGE (Iran) server
tar xzf tuunel-linux-amd64.tar.gz && cd tuunel-v0.9.0
sudo bash install.sh --role=edge
# prints this node's PUBLIC KEY -> give it to the remote node

# REMOTE (foreign) server
tar xzf tuunel-linux-amd64.tar.gz && cd tuunel-v0.9.0
sudo bash install.sh --role=remote --edge-address=EDGE_PUBLIC_IP --peer-key=EDGE_PUBLIC_KEY
# prints this node's PUBLIC KEY

# back on the EDGE: authorize the remote node's key (re-running is safe)
sudo bash install.sh --peer-key=REMOTE_PUBLIC_KEY
```

The installer checks OS, architecture, root, systemd and `/dev/net/tun`. It
creates user `tuunel`, generates `/etc/tuunel/node.key` (0600) and
`/etc/tuunel/config.yaml` (0640), installs `/usr/local/bin/tuunel` and
`tunnelctl` plus the systemd unit, validates the config with `tuunel check`,
then starts the service and confirms that `tun0` exists. Re-running it is
idempotent: an existing key or config is never overwritten (`--force-config`
regenerates the config and keeps a backup). Run
`sudo bash install.sh --help` to see every option (`--ports`, `--local-ip`,
`--peer-ip`, `--binary-dir`, `--url`, `--no-start`).

### One-line install

`install.sh --url=BASE_URL` downloads `BASE_URL/tuunel-linux-ARCH.tar.gz`,
verifies it against `BASE_URL/SHA256SUMS`, and installs it. The one-liner works
only once the files from `dist/` are hosted on an HTTPS location **you**
control. This repository is private, so its release assets cannot be
downloaded anonymously:

```bash
curl -fsSL BASE_URL/install.sh | sudo bash -s -- --url=BASE_URL --role=edge
curl -fsSL BASE_URL/install.sh | sudo bash -s -- --url=BASE_URL --role=remote --edge-address=EDGE_IP --peer-key=EDGE_KEY
```

No hosting URL is provided by this project, and the one-line path has not been
executed.

### Manual install (no installer)

```bash
make build
sudo install -m 0755 bin/tuunel bin/tunnelctl /usr/local/bin/
sudo install -d -m 0750 /etc/tuunel
sudo sh -c 'umask 077; tuunel genkey > /etc/tuunel/node.key'
tuunel pubkey -key /etc/tuunel/node.key
sudo cp configs/reverse-edge.yaml /etc/tuunel/config.yaml   # or reverse-remote / node-a / node-b
sudo tuunel check -config /etc/tuunel/config.yaml
sudo tuunel run   -config /etc/tuunel/config.yaml
```

## Authentication and keys

Each node has a static X25519 key (`tuunel genkey`, `tuunel pubkey`). A node
accepts a peer only if that peer's public key is listed under `peers:`. Both
sides authenticate each other inside the Noise IK handshake (mutual
authentication), and unknown keys get no response. An optional PSK
(`tuunel genpsk`, `security.preshared_key_file`) adds a symmetric secret. The
outer TLS used by QUIC/WSS is only transport camouflage; see SECURITY.md.

## Configuration

`/etc/tuunel/config.yaml`; unknown keys are rejected. Annotated examples are in
`configs/`. Main sections: `node`, `interface` (name, addresses, `mtu:
auto|N`, routes), `security`, `listen` (edge), `peers` (keys, `allowed_ips`,
`endpoints`, `carriers` in preference order), `failover`, `health`,
`forwarding`, `api`, `log`, `experimental`. `sudo tuunel check -config FILE`
validates a config and prints the MTU and routing plan.

## Operating

```bash
sudo systemctl start|stop|restart tuunel
sudo systemctl reload tuunel                 # SIGHUP: reload forwarding rules without dropping the tunnel
sudo tunnelctl status                        # state, carrier, endpoint, RTT, loss, jitter, traffic
sudo journalctl -u tuunel --no-pager -n 100  # logs (or: sudo tunnelctl logs)
sudo tunnelctl doctor                        # kernel, permissions, config, keys, DNS, MTU, routing, reachability
sudo tunnelctl test-carriers                 # actively probe every endpoint × carrier
sudo tunnelctl metrics                       # Prometheus metrics
ping 10.200.0.1                              # from the remote node, through the tunnel
```

The API socket `/run/tuunel/tuunel.sock` has mode 0660 and belongs to the
service user, so run `tunnelctl` with `sudo` (or as user/group `tuunel`).

## Carrier selection and failover

The remote node tries carriers in the order listed under `peers[].carriers`
(installer default: tcp → quic → wss → udp) on each endpoint. Health probes
measure RTT, jitter and loss. After a path misses `failed_after_missed`
probes, the next candidate takes over. Several candidates are raced, and the
new link is established before the old one is torn down. A preferred carrier
that recovers is preempted back after `min_hold`. Tunnel IPs, routes and
established inner TCP connections survive a switch. Measured in the lab:
failover took 2–4.5 s, with a ping outage of about 2–4 s (see BENCHMARKS.md).

Several servers can be listed under `endpoints:` (`endpoint_selection:
priority|latency`).

## MTU

`mtu: auto` subtracts the worst-case overhead of the configured carriers from
the underlay MTU (default 1380 on a 1500 underlay). Path MTU changes are
detected, and the plan is re-applied with a floor of 1280 for IPv6. Oversized
packets are fragmented by the kernel, or the engine answers them with ICMP
"too big". Datagram carriers also check the kernel path MTU per link. Set
`mtu: N` to force a value. QUIC needs a path MTU of at least 1228 (IPv4);
on smaller paths, failover uses the other carriers.

## Port forwarding

`forwarding.tcp` / `forwarding.udp` rules (`listen`, `target`, optional
`max_connections`) on the edge expose services of the remote node, for
example `0.0.0.0:2222 → 10.200.0.2:22`. Rules can be changed and applied with
`systemctl reload tuunel`.

## Uninstall

```bash
sudo bash install.sh --uninstall           # remove service + binaries, keep /etc/tuunel
sudo bash install.sh --uninstall --purge   # also remove keys, config, state and the tuunel user
```

## Development and tests

```bash
make check                 # gofmt, go vet, go test, go test -race, staticcheck
sudo make lab              # real-TUN two-namespace acceptance lab
sudo make lab-extended     # carriers, failover cycles, endpoints, shutdown, reverse, MTU matrix
sudo make soak             # 6-minute soak with periodic carrier failure + leak sampling
sudo make bench            # per-carrier end-to-end benchmark through tun0
make release VERSION=v0.9.0
```

## Limitations

- Systemd unit, installer, one-line installer and the Docker lab were **not
  executed** in the audit environment (no systemd/Docker daemon).
- Not tested over the real Internet, across two physical servers, or behind
  DPI or censoring networks. No obfuscation or anti-DPI claims.
- Loss/latency injection (`tc netem`) was unavailable; loss-driven failover is
  covered only by unit/engine tests.
- TCP-based carriers (tcp, wss, quic stream) suffer TCP-over-TCP slowdown on
  lossy paths; prefer udp or quic DATAGRAM there.
- The ICMP carrier is experimental, IPv4 only, and needs
  `net.ipv4.icmp_echo_ignore_all=1` on the listening side.
- The legacy `tuunel server|client` (TLS mTLS MVP, `internal/tunnel`) is kept
  for compatibility. It is not used by `tuunel run` and was not re-audited.

## License

See [LICENSE](LICENSE).

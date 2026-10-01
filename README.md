# tuunel

`tuunel` is a **Layer 3 tunnel engine for Linux**, written in Go. It moves
whole IPv4/IPv6 packets between two `tun` interfaces over an authenticated,
encrypted session (Noise IKpsk2). That session runs over interchangeable
transports ("carriers"): **TCP, UDP, QUIC (stream or DATAGRAM), WebSocket/WSS**
and an **experimental ICMP** carrier. A health engine measures every path, and
a failover manager moves the tunnel between carriers and endpoints without
restarting the daemon or recreating the interface.

The public architecture of [BackPack](https://github.com/AminMGMT/BackPack)
informed the design. This is an independent implementation: no code or
protocol was copied.

> **Status: NOT production ready (pre-1.0).** Everything below was validated
> on one Linux VM in network-namespace labs with a real TUN device, a
> userspace loss/latency bridge, real installer runs (`--root` mode) and an
> HTTPS one-line install. **Not** yet executed: the service under a running
> systemd, Docker containers, two real servers over the Internet, and a
> restricted/filtered network. Evidence and the exact procedures for the
> missing tests: [docs/FINAL_VALIDATION.md](docs/FINAL_VALIDATION.md).
> This is a self-review, not an independent security audit.

## Documentation

| Document | Content |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | one-line install, archives, systemd, firewall, Docker, uninstall |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | components, data path, concurrency, metrics |
| [PROTOCOL.md](PROTOCOL.md) | wire format |
| [docs/FAILOVER.md](docs/FAILOVER.md) | failover model, anti-flap, measured switch times |
| [docs/NETWORK-TESTING.md](docs/NETWORK-TESTING.md) | loss/latency/jitter/MTU labs and results |
| [docs/SECURITY.md](docs/SECURITY.md) | threat model, checks run, 19-point review |
| [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) | symptoms → causes → fixes |
| [docs/FINAL_VALIDATION.md](docs/FINAL_VALIDATION.md) | validation matrix with evidence |
| [BENCHMARKS.md](BENCHMARKS.md) | throughput / CPU / memory |

## Architecture

```
 app ─▶ tun0 ─▶ engine (allowed-IPs routing, anti-spoof, MTU, fragmentation)
                  │
                  ▼
        Noise IKpsk2 session (X25519 / ChaCha20-Poly1305 / BLAKE2s,
        64-bit counters, 2048-packet replay window, rekey)
                  │
                  ▼
        failover manager ◀── health engine (RTT, jitter, loss, UP/DEGRADED/FAILED)
                  │
     ┌──────┬─────┼──────┬───────────┬────────┐
    tcp    udp   quic  quic-dgram  wss/ws   icmp (experimental)
```

One active link per peer; candidates are endpoint × carrier. Failover is
make-before-break when possible; `tun0`, addresses, routes and inner TCP
connections survive a switch. Details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Supported transports

| Carrier | Underlay | Properties | Notes |
|---|---|---|---|
| `tcp` | TCP | reliable stream, length-prefixed | works almost everywhere; TCP-over-TCP slows down on lossy paths |
| `udp` | UDP | datagram | best on lossy paths; per-link path MTU |
| `quic` | UDP (QUIC/TLS 1.3) | reliable streams | needs UDP and a path MTU ≥ 1228 (IPv4) / 1248 (IPv6) |
| `quic-dgram` | UDP (QUIC DATAGRAM) | datagram | carries ≤ 1120-byte inner packets; larger IPv4 packets and 1121–1280-byte IPv6 packets are fragmented by the engine |
| `wss` / `ws` | TCP (TLS) + WebSocket | reliable stream | looks like WebSocket over TLS; `ws` is plaintext WebSocket (still Noise-encrypted) |
| `icmp` | ICMP echo (IPv4) | datagram | **experimental**; needs `CAP_NET_RAW`, and `nft` + `CAP_NET_ADMIN` on the listener |

The outer TLS of QUIC/WSS is unverified by default; authentication and
confidentiality come from the inner Noise session ([docs/SECURITY.md](docs/SECURITY.md)).

## Requirements

- Linux with `/dev/net/tun`, amd64 or arm64 (tested: kernel 6.18 amd64;
  arm64 binaries built, not executed).
- Capabilities: `CAP_NET_ADMIN` (TUN, addresses, routes, ICMP reply filter),
  `CAP_NET_BIND_SERVICE` (ports < 1024), `CAP_NET_RAW` (ICMP carrier only).
  Running as a non-root user with exactly these capabilities was tested.
- Service install: systemd, `bash`, `iproute2`, `curl` (downloads), optional
  `setcap`, `ssh-keygen` (signed releases), `nftables` (ICMP listener).
  Intended: Ubuntu 22.04/24.04, Debian 12, RHEL/Alma/Rocky 9.
- Build: Go **1.26+** (`go.mod`: `go 1.26.0`; validated with Go 1.27.1).

## Installation

Full guide: [docs/INSTALL.md](docs/INSTALL.md). Roles:

| Role | Typical host | Does | Tunnel IP | Inbound ports |
|---|---|---|---|---|
| `edge` (alias `iran`) | public server clients reach | **listens**; exposes services of the remote node with `forwarding:` | 10.200.0.1/30 | tcp/443, udp/443, tcp/8443, udp/51900 |
| `remote` (alias `foreign`, `kharej`) | server with unrestricted upstream | **dials** the edge | 10.200.0.2/30 | none |

### One-line install

Published releases (the `dist/` of `scripts/release.sh`) are served from an
HTTPS location **you** control; this repository is private and no public
URL is provided by the project (BLOCKED_EXTERNAL_DEPENDENCY: hosting).

```bash
# maintainer: build + sign + embed the URL, then upload dist/* to $BASE/v1.0.0/
RELEASE_BASE_URL=https://dl.example.com/tuunel SIGNING_KEY=~/.ssh/tuunel_release scripts/release.sh v1.0.0
```

### Edge setup

```bash
curl -fsSL --proto '=https' --tlsv1.2 https://dl.example.com/tuunel/v1.0.0/install.sh | sudo bash -s -- --role=edge
# prints the edge PUBLIC KEY
```

### Remote setup

```bash
curl -fsSL --proto '=https' --tlsv1.2 https://dl.example.com/tuunel/v1.0.0/install.sh \
  | sudo bash -s -- --role=remote --edge-address=EDGE_PUBLIC_IP --peer-key=EDGE_PUBLIC_KEY
# prints the remote PUBLIC KEY; then on the edge:
curl -fsSL --proto '=https' https://dl.example.com/tuunel/v1.0.0/install.sh | sudo bash -s -- --peer-key=REMOTE_PUBLIC_KEY
```

The installer downloads `tuunel-linux-ARCH.tar.gz`, verifies `SHA256SUMS`
(and `SHA256SUMS.sig` with the embedded signer key when present;
`--require-signature` makes it mandatory), creates user `tuunel`,
`/etc/tuunel/node.key` (0600) and `config.yaml` (0640), installs the
binaries and the unit, validates the config, starts the service, waits for
`tun0`, and runs `tunnelctl doctor`. It is idempotent; keys and configs are
never overwritten without `--force-config` (a backup is kept). From an
archive: `tar xzf tuunel-linux-amd64.tar.gz && cd tuunel-v1.0.0 && sudo bash install.sh --role=edge`.
`sudo bash install.sh --help` lists every option.

## systemd

`/etc/systemd/system/tuunel.service` runs `tuunel run -config
/etc/tuunel/config.yaml` as user `tuunel` with only the three capabilities,
`ExecStartPre=tuunel check`, `Restart=on-failure`, SIGTERM with a 15 s stop
timeout, `ExecReload` = SIGHUP (reloads forwarding rules), `ProtectSystem=strict`,
`DevicePolicy=closed` + `/dev/net/tun`, a `@system-service` syscall filter.
`systemd-analyze verify` is clean and `systemd-analyze security` rates it
2.1 (OK); it has **not** been run under a live systemd yet.

```bash
sudo systemctl status tuunel
sudo systemctl reload tuunel        # forwarding changes without dropping the tunnel
sudo journalctl -u tuunel -n 100 --no-pager
```

## Docker

`Dockerfile` (pinned Go builder that runs `go vet` + `go test`, Debian slim
runtime, **uid 10001** with file capabilities, `HEALTHCHECK tunnelctl status`,
SIGTERM) and `docker-compose.yml` (profiles `edge` / `remote`, host network,
`cap_drop: ALL` + NET_ADMIN/NET_RAW/NET_BIND_SERVICE, `/dev/net/tun`,
read-only rootfs, log rotation). See [deploy/docker/README.md](deploy/docker/README.md).

```bash
docker compose --profile edge up -d      # config + key in deploy/docker/edge/
```

Not executed yet (no Docker daemon in the validation VM);
`scripts/test-docker.sh` performs the build/run/failover checks where a
daemon exists.

## Firewall requirements

Only the **edge** needs inbound rules (defaults): tcp/443 (tcp carrier),
udp/443 (QUIC), tcp/8443 (WSS), udp/51900 (udp); ICMP echo-request for the
experimental ICMP carrier. The remote node only needs outbound access to
those ports. Allow inbound traffic on `tun0` for the services you forward.
Examples (ufw, nftables, firewalld): [docs/INSTALL.md](docs/INSTALL.md#5-firewall).

## MTU considerations

`interface.mtu: auto` subtracts the worst-case overhead of the configured
carriers from the detected path MTU (1500 → TUN 1380 with the default
carrier set), never below 1280 when IPv6 runs inside. Each datagram link
also asks the kernel for its own path MTU. Packets above a link's limit are
fragmented by the engine (IPv4 without DF; IPv6 when the link cannot carry
1280 bytes) or answered with ICMP/ICMPv6 "too big". Measured: underlay MTU
1200–1500 × all carriers, IPv4 and IPv6 ([docs/NETWORK-TESTING.md](docs/NETWORK-TESTING.md#mtu-sweep)).
QUIC carriers need an underlay MTU ≥ 1228 (IPv4) / 1248 (IPv6) (RFC 9000);
failover uses the other carriers on smaller paths.

## Failover

Candidates are tried in `peers[].carriers` order on each endpoint
(installer default tcp → quic → wss → udp). After `failed_after_missed`
probes the next candidate takes over; a recovered better candidate is
preempted back after `min_hold`. `failover.degrade_holdoff` (default 1 m)
stops degrade/preempt flapping on lossy paths. Several servers can be listed
under `endpoints:`. Measured switch times 2.5–8.3 s per failed carrier,
endpoint switch 3.7 s, long-lived TCP and UDP forwards survived every switch
([docs/FAILOVER.md](docs/FAILOVER.md)).

## Diagnostics

```bash
sudo tunnelctl status          # state, active carrier/endpoint, RTT, loss, jitter, traffic
sudo tunnelctl doctor          # kernel, permissions, config, keys, DNS, MTU, routing, reachability
sudo tunnelctl test-carriers   # probe every endpoint × carrier
sudo tunnelctl metrics         # Prometheus text (switches, failures, RTT, loss, bytes, build info, …)
tuunel version                 # release version + commit
```

The API socket `/run/tuunel/tuunel.sock` is mode 0660 (`sudo` or group
`tuunel`). Problems: [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

## Configuration

`/etc/tuunel/config.yaml`; unknown keys are rejected; annotated examples in
`configs/`. Sections: `node`, `interface`, `security`, `listen`, `peers`,
`failover`, `health`, `forwarding`, `api`, `log`, `experimental`.
`sudo tuunel check -config FILE` validates and prints the MTU/routing plan.
Port forwarding (`forwarding.tcp` / `forwarding.udp`, e.g. `0.0.0.0:2222 →
10.200.0.2:22`) is reloadable with `systemctl reload tuunel`.

## Security limitations

- Self-review only; no third-party audit.
- No traffic obfuscation or anti-DPI claims: carriers look like their
  protocol, sizes and timing are not shaped.
- Outer TLS (QUIC/WSS) is not verified by default; an on-path attacker can
  terminate it but cannot read or forge tunnel traffic (Noise).
- Handshake replay protection depends on clocks (60 s window); keep NTP running.
- Key rotation requires a restart; no revocation list.
- One-line install integrity relies on HTTPS plus SHA-256; signatures are
  optional unless `--require-signature` is used.

## Known limitations

- Not yet executed: live systemd, Docker, two real servers over the
  Internet, restricted networks ([docs/FINAL_VALIDATION.md](docs/FINAL_VALIDATION.md)).
- Kernel `netem` was unavailable; loss/latency/jitter were injected by a
  userspace AF_PACKET bridge (`tests/tools/netimpair`).
- Stream carriers (tcp, wss, quic) inflate latency under loss
  (TCP-over-TCP); at 20 %/direction loss they reconnect. Prefer udp/quic-dgram.
- ICMP carrier: experimental, IPv4 only; the listener adds one nftables rule.
- The legacy `tuunel server|client` TLS MVP (`internal/tunnel`) is not used
  by `tuunel run` and was not re-audited.

## Development and tests

```bash
make check                                  # gofmt, vet, test, race, staticcheck
sudo scripts/test-network-impairment.sh     # loss/latency/jitter (FULL=1 for all carriers)
sudo scripts/test-failover.sh               # carrier/endpoint failover, reverse (FULL=1)
sudo scripts/test-mtu.sh                    # MTU sweep (FULL=1)
sudo SOAK_SECONDS=1800 scripts/test-soak.sh # soak
sudo scripts/test-install.sh                # installer, release, one-line install
scripts/test-docker.sh                      # needs a Docker daemon
make release VERSION=v1.0.0                 # reproducible linux/amd64 + arm64
```

## License

See [LICENSE](LICENSE).

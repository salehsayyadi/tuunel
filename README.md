# tuunel

`tuunel` is a Linux-first, multi-carrier **Layer 3 tunnel engine** written in Go.
It moves whole IPv4/IPv6 packets between two `tun` interfaces over an
authenticated, encrypted session (Noise IKpsk2) and carries that session over
interchangeable transports ("carriers"): **TCP, UDP, QUIC (stream or DATAGRAM),
WebSocket/WSS** and an **experimental ICMP** carrier. A health engine measures
every path, and a failover manager moves the tunnel between carriers and
endpoints without restarting the daemon or the interface.

The design was informed by reading the public architecture of
[BackPack](https://github.com/AminMGMT/BackPack). This is an independent
implementation; no code or protocol was copied.

> Status: pre-1.0. Everything listed under "Verified" below has a reproducible
> test in this repository. Items under "Not verified here" are implemented but
> were not executed in the development environment. See `BENCHMARKS.md` and
> `SECURITY.md`.

## Features

| Area | What it does | Verified by |
|---|---|---|
| L3 engine | TUN ⇄ encrypted session, IPv4 + IPv6, allowed-IPs routing (LPM), source anti-spoofing | `internal/engine` tests, `tests/lab/netns-acceptance.sh` |
| Carriers | tcp, udp, quic, quic DATAGRAM, ws/wss (+HTTP proxy), icmp (experimental, IPv4) | `carriertest` conformance suite per carrier |
| Session | Noise IKpsk2 (X25519/ChaChaPoly/BLAKE2s), 64-bit counters, 2048-packet replay window, rekey, handshake replay window | `internal/session` tests + fuzz |
| MTU | auto/manual, per-carrier overhead budget, path MTU detection, fragmentation or ICMP "too big" | `internal/mtu`, engine oversize tests, lab MTU step |
| Health | RTT, jitter (RFC 3550), loss, states UNKNOWN/AVAILABLE/DEGRADED/FAILED/RECOVERING with hysteresis | `internal/health` tests |
| Failover | carrier and endpoint failover, parallel candidate race, make-before-break, backoff+jitter, cooldown, min-hold, preempt | engine tests, lab (TCP→QUIC→WSS→TCP) |
| Multi-endpoint | several servers per peer, priority or latency selection | engine endpoint failover test, lab |
| Reverse tunnel | NAT'd node dials out, edge exposes its services | lab reverse step |
| Forwarding | TCP and UDP port forwarding, many rules, connection limits, hot reload (SIGHUP) | `internal/forwarding` tests, lab |
| Operations | `tunnelctl` (status, carriers, endpoints, test, ping, routes, metrics, doctor, …), Unix-socket API, Prometheus metrics | lab status/doctor checks |

## Quick start (two nodes)

```bash
make build                         # bin/tuunel, bin/tunnelctl (Go 1.23+)
sudo install -m 0755 bin/tuunel bin/tunnelctl /usr/local/bin/
sudo install -d -m 0700 /etc/tuunel
sudo sh -c 'umask 077; tuunel genkey > /etc/tuunel/node.key'   # public key printed on stderr
sudo tuunel pubkey -key /etc/tuunel/node.key                   # share with the other node
```

Copy `configs/node-a.yaml` (dialing side) and `configs/node-b.yaml` (listening
side) to `/etc/tuunel/tuunel.yaml`, exchange public keys, then:

```bash
sudo tuunel check -config /etc/tuunel/tuunel.yaml   # strict validation + plan (MTU, routes)
sudo tuunel run   -config /etc/tuunel/tuunel.yaml
tunnelctl status
tunnelctl doctor
```

Or use `scripts/install.sh` + `systemd/tuunel.service`. A full walkthrough is in
[`docs/deployment.md`](docs/deployment.md); reverse mode uses
`configs/reverse-edge.yaml` / `configs/reverse-remote.yaml`.

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) – components and data flow
- [PROTOCOL.md](PROTOCOL.md) – wire format and handshake
- [SECURITY.md](SECURITY.md) – threat model, audit results, limitations
- [BENCHMARKS.md](BENCHMARKS.md) – measured throughput/latency/CPU/RAM
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) – common failures and `doctor` output
- [docs/deployment.md](docs/deployment.md) – two-node and reverse deployment

## Development

```bash
make check          # gofmt, go vet, go test, go test -race, staticcheck
sudo make lab       # real-TUN two-namespace acceptance + failover lab
sudo make bench     # per-carrier end-to-end benchmark through tun0
make microbench     # carrier + crypto micro-benchmarks
docker compose run --rm lab   # same lab in a privileged container
```

## Not verified here

- `scripts/install.sh`, the systemd unit and the Docker lab were written but not
  executed in the development environment (no systemd/Docker available).
- Loss/latency injection with `tc netem` and `iptables -m statistic` was
  skipped: the build kernel lacks `sch_netem`/`xt_statistic`. Loss-driven
  failover is covered by unit/engine tests with a fault-injecting carrier.
- Not tested across real Internet paths, DPI or censoring networks.

## Legacy MVP

`tuunel server|client` (TLS 1.3 mTLS over TCP, `internal/tunnel`) is kept for
compatibility and is not used by `tuunel run`.

## License

See [LICENSE](LICENSE).

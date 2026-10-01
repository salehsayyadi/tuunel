# Final production-readiness audit

> **Historical.** This is the previous audit (published in commit e7fd1d6). It is superseded by
> [docs/FINAL_VALIDATION.md](docs/FINAL_VALIDATION.md), which re-tests every
> item and lists the bugs found afterwards (#10–#15: QUIC stream close wedge,
> carrier flapping, ICMP path-MTU planning, IPv6 over small-MTU links,
> installer `VERSION` clobbering, ICMP host-ping side effect).

- Repository: `salehsayyadi/tuunel`, branch `main`
- Baseline commit: `89a4dd7253dea3f7677b345c01f08d2991bd204f`
- Audit date: 2026-10-01
- Auditor: internal self-review, assisted by an automated agent. **Not an
  independent third-party audit.**

## Overall result: **NOT PRODUCTION READY**

The tunnel engine works in a real-TUN, two-network-namespace lab: carriers,
encryption, mutual authentication, failover, reverse tunnel, forwarding, MTU,
soak, and running non-root with capabilities only. Production readiness
cannot be claimed because the installer, the systemd unit, the Docker lab,
a clean-machine install, real-Internet paths and a two-server deployment were
**not executed**. The audit environment had no systemd and no Docker daemon,
and requests to start them were refused by the environment's safety policy.

## Test environment

| Item | Value |
|---|---|
| OS / kernel | Amazon Linux 2023, Linux 6.18.49, x86_64 |
| CPU / RAM | 2 vCPU (shared cloud), 4 GB |
| Go | 1.27.1 (final build and tests); Go 1.25.1 was used for the baseline checks |
| Privileges | root via sudo; `/dev/net/tun`, netns, veth, iptables available |
| Missing | systemd (PID 1 is a sandbox init), Docker daemon, `sch_netem`, `xt_statistic`, kernel modules |
| Topology | two network namespaces joined by a veth pair (192.0.2.1 ⇄ 192.0.2.2), real `tun0` in each, iptables used to block carriers |

## Status table

| Component | Implemented | Actually Tested | Production Verified | Result | Notes |
|---|---|---|---|---|---|
| TUN | Yes | Yes | No | PASS | Real `tun0` in netns; created/configured/destroyed on SIGTERM; restart ×3. **Bug fixed**: daemon died at start (`not pollable`) |
| L3 | Yes | Yes | No | PASS | IPv4 + IPv6 inside tunnel, allowed-IPs LPM routing, anti-spoof drop; 5 MiB TCP + UDP echo checksummed |
| TCP | Yes | Yes | No | PASS | Up 0.03 s, ≈1 Gbit/s on veth, reconnect after peer restart, failure detection ≈3.4 s |
| UDP | Yes | Yes | No | PASS | ≈1 Gbit/s; **bug fixed**: large datagrams were lost on small-MTU paths (per-link PMTU + ICMP source fix) |
| QUIC | Yes | Yes | No | PASS | Stream and DATAGRAM modes; **fixed**: handshake failed at path MTU 1280/1300 (initial packet 1200). Needs path MTU ≥1228. quic-go upgraded for GO-2025-4017 |
| WSS | Yes | Yes | No | PASS | wss and ws, ≈500 Mbit/s; outer TLS not verified by default (inner Noise authenticates) |
| ICMP | Yes (experimental) | Yes | No | PASS | IPv4 only, needs `CAP_NET_RAW` and `icmp_echo_ignore_all=1` on the listener (which also suppresses normal pings) |
| Encryption | Yes | Yes | No | PASS | Noise IKpsk2: X25519, ChaCha20-Poly1305, BLAKE2s; unit tests and fuzzing; rekey |
| Mutual Authentication | Yes | Yes | No | PASS | Static keys both directions; unknown key gets no response (engine test); wrong key → `doctor` explains |
| Replay Protection | Yes | Yes | No | PASS | 64-bit counters, 2048-message window (unit tests + fuzz); handshake timestamp window 60 s + seen-set. Limitation: peer clock stepping back >60 s blocks it until the responder restarts |
| MTU | Yes | Yes | No | PASS | Sweep 1200–1500 × tcp/udp/quic/wss: DF max passes, DF+1 blocked, TCP and 1400-B UDP delivered; QUIC at 1200 skipped (RFC 9000 minimum) |
| Health Monitoring | Yes | Yes | No | PASS | RTT/jitter/loss/state, TX/RX, reconnects, switches, uptime in status + Prometheus; goroutine/heap/fd gauges added. **Bug fixed**: failure-driven switches were not counted |
| Carrier Failover | Yes | Yes | No | PASS | TCP→QUIC→WSS→TCP ×3 (two full runs): blocked-carrier switches 2.1–4.5 s, ping outage ≈1.9–3.9 s; preempt back to TCP 1.8–8.4 s with 0 loss; inner TCP connection survived 9 switches; reverse-direction traffic OK. Loss-based failover only unit-tested (no netem) |
| Endpoint Failover | Yes | Yes | No | PASS | 3 endpoints: e1→e2 3.4–3.5 s, e2→e3 5.7–7.8 s, preempt back to e1; tunnel IP unchanged; inner TCP survived |
| Reverse Tunnel | Yes | Yes | No | PASS | Remote dials out only (inbound SYN dropped); edge exposes 2 TCP + 2 UDP services; remote restart restores mappings; failover to WSS keeps mappings |
| TCP Forwarding | Yes | Yes | No | PASS | Multiple rules, `max_connections`; fails cleanly while remote is down |
| UDP Forwarding | Yes | Yes | No | PASS | Multiple rules through reverse tunnel |
| CLI | Yes | Yes | No | PASS | `tunnelctl status/test-carriers/doctor/metrics/forwarding` exercised by labs; `tuunel check/genkey/pubkey` used |
| Installer | Yes | Partially | No | NOT VERIFIED | `bash -n` OK. Its generated edge/remote configs pass `tuunel check` and ran against each other (non-root + caps: up, ping, 4/4 carriers, TCP→QUIC failover). The script itself was **not executed** (no systemd; running it on the audit host was refused) |
| Systemd | Yes | No | No | ENVIRONMENT LIMITATION | Unit written and hardened; no systemd in the environment. Non-root operation with exactly its capabilities was tested |
| Docker Lab | Yes | No | No | ENVIRONMENT LIMITATION | Dockerfile updated to Go 1.27; no Docker daemon (starting one was refused) |
| Security | Yes | Yes | No | PASS | gofmt, vet, staticcheck, race, gosec reviewed, govulncheck: 0 reachable after upgrades; DoS goroutine leak fixed. Self-review only |
| Performance | Yes | Yes | No | PASS | veth lab only: tcp 703–1063, udp 737–958, quic 418–646, wss 448–528 Mbit/s; RSS 15–18 MB; see BENCHMARKS.md. Not representative of Internet paths |
| Long Run | Yes | Yes (6 min) | No | PASS | 360 s soak: 2.16 GB inner TCP echoed intact, UDP 3283/3293, 10 forced TCP blocks → 10 switches, longest outage 3.8 s, RSS 12.6–20.7 MB, goroutines/fds back to baseline, no panic. **First run found bug #7** (55 s undetected outage). Multi-hour/day soak not done |
| Real Internet | No | No | No | NOT VERIFIED | No Internet path between two hosts was available |
| Two-Server Test | No | No | No | NOT VERIFIED | Only two namespaces on one kernel |
| One-Line Installer | Yes | No | No | NOT VERIFIED | `--url` download + SHA256 verification implemented; needs a public HTTPS host chosen by the owner (repo is private); never executed |

## Bugs found and fixed

| # | Severity | Component | Bug | Fix | Regression test |
|---|---|---|---|---|---|
| 1 | Critical | TUN | fd registered with the Go poller before `TUNSETIFF` → `read /dev/net/tun: not pollable`, daemon exited at start (lab: 0 tunnels) | `syscall.Open` → `TUNSETIFF` → `SetNonblock` → `os.NewFile` | `netns-acceptance.sh` (all lab tests) |
| 2 | High (DoS) | engine | carrier reader goroutine blocked forever on a full channel after a rejected handshake or closed link → unauthenticated goroutine/memory leak | `pump.stop()` (close conn + `done` chan) on every exit path | `TestPumpStopReleasesReaderWhenChannelFull` |
| 3 | High (security) | deps | quic-go v0.48.2 GO-2025-4017 remote panic; Go 1.25.1 stdlib CVEs; x/net v0.30.0 CVEs | quic-go v0.63.0, x/net v0.59.0, Go 1.27.1 (`go 1.26.0` in go.mod) | govulncheck: 0 reachable |
| 4 | High | MTU/UDP | listener assumed a 1500-byte path, so oversize DF datagrams were silently lost on smaller paths (1400-B UDP 0/20) | per-link path-MTU detection towards each link's remote address | `TestLinkLimitUsesDetectedPathMTU`, MTU sweep |
| 5 | High | MTU | ICMP "too big" injected into TUN with a local source address → kernel dropped it as a martian, so PMTU never adapted | source = original destination | `TestOversizePackets` (source asserted), MTU sweep 19/20 |
| 6 | Medium | QUIC | handshake failed on 1280/1300 paths (quic-go initial packet 1280 B) | `InitialPacketSize: 1200`, `DatagramMax` 1180→1150 | MTU sweep |
| 7 | High | failover | health loop sent pings synchronously: on a black-holed stream carrier (tcp/wss/quic-stream) under load the write blocked behind a full socket buffer, health evaluation stopped and the dead link stayed "UP" (soak: ~55 s undetected outage) | async health-ping and rekey writes; a ping that cannot be queued counts as lost | `TestStalledWriterCarrierFailsOver` (fails on old code after 12 s, passes in <1 s), soak rerun |
| 8 | Low | metrics | `carrier_switches` ignored failure-driven switches | track last connected candidate | `TestSwitchesCountFailureDrivenSwitch`, failover lab |
| 9 | Low | build | Dockerfile pinned Go 1.23 | Go 1.27 | – |

## Commands and results

```text
gofmt -l .                       -> (empty)
go vet ./...                     -> OK
staticcheck ./... (Go 1.27.1)    -> OK
go test ./...                    -> OK (all packages)
go test -race ./...              -> OK
govulncheck ./...                -> 0 vulnerabilities reachable (GO-2026-5932 x/crypto/openpgp: not imported)
gosec ./...                      -> reviewed: G115/G104/G304/G404/G103 only, no exploitable issue
sudo tests/lab/netns-acceptance.sh bin             -> 24 passed, 0 failed, 2 skipped (netem/statistic unavailable)
sudo tests/lab/netns-extended.sh bin carriers      -> 7 passed, 0 failed (tcp, udp, quic, quic-dgram, wss, ws, icmp)
sudo tests/lab/netns-extended.sh bin failover      -> 14 passed, 0 failed
sudo tests/lab/netns-extended.sh bin endpoints     -> 6 passed, 0 failed
sudo tests/lab/netns-extended.sh bin shutdown      -> 6 passed, 0 failed
sudo tests/lab/netns-extended.sh bin reverse       -> 5 passed, 0 failed
sudo tests/lab/netns-extended.sh bin mtu           -> 1 passed (27 combinations), 0 failed, 1 skipped (QUIC at 1200)
sudo tests/lab/netns-soak.sh bin 360               -> PASS (after fix #7; first run exposed a 53 s outage)
sudo tests/lab/netns-bench.sh bin 100              -> see BENCHMARKS.md
installer-generated configs, non-root + caps       -> tunnel up, ping 0% loss, test-carriers 4/4 OK, TCP→QUIC failover
```

## Not verified / environment limitations

- `scripts/install.sh` was not run end to end; systemd unit not run;
  `journalctl` logging not observed. Docker lab not run. Clean-machine install
  on Ubuntu/Debian/RHEL not done.
- No real Internet path, no two physical/virtual servers, no DPI/censoring
  network, no NAT traversal across real NAT devices.
- No packet loss/latency/jitter injection (`sch_netem` and `xt_statistic`
  missing). Loss/degradation-driven failover is only covered by engine tests
  using a fault-injecting carrier.
- Interoperability with builds from before the audit was not tested.

## Blocking issues for production

1. Installer + systemd unit must be executed on a clean Ubuntu/Debian host
   (install, restart, reboot persistence, reload, uninstall).
2. A two-server test over the real Internet (edge + remote), including carrier
   blocking and reboot of either side.
3. Loss/latency testing with netem on a capable kernel.
4. An independent security review of the protocol and implementation.
5. A hosting location for release artifacts if the one-line installer is
   wanted.

## How to reproduce

```bash
make check
make build
sudo make lab
sudo make lab-extended
sudo make soak SOAK_SECONDS=360
sudo make bench
make release VERSION=v0.9.0
```

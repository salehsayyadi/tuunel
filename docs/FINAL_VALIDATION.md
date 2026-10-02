# Final validation

Every row below states what was **executed**. Nothing is marked PASS without
a run and its evidence file. Raw results (JSON/logs) were written to
`/data/results/` on the validation VM; the scripts that produce them are in
`scripts/` and `tests/lab/`.

## Environment

| Item | Value |
|---|---|
| Baseline commit | `e7fd1d6587bf6bba4bc661e3e5602f29013caca8` (validated code = baseline + the changes in this commit) |
| Environment | single sandbox VM, no second machine |
| OS | Amazon Linux 2023.12 |
| Kernel | 6.18.49, x86_64 |
| CPU / RAM | 2 shared vCPUs, 4.2 GB |
| Go | go1.27.1 linux/amd64 (`go.mod`: go 1.26.0) |
| Capabilities | root via sudo (full capability set); non-root runs used uid 10001 with `cap_net_admin,cap_net_raw,cap_net_bind_service` file caps |
| Networking tools | iproute2, tc, nftables (`ip` family only; `inet`/`ip6` families unsupported by this kernel), iptables/ip6tables, iperf3, ethtool, tcpdump, ping, ss, curl, openssl, ssh-keygen, setcap |
| Missing tools | shellcheck, hadolint, gosec (not re-run) |
| systemd | **not running** (PID 1 = `sandbox-init`); `systemd-analyze` 252 available for offline checks |
| Docker | CLI present, **no daemon** (`docker info` fails); starting one was not permitted in this session |
| TUN | `/dev/net/tun` available (real TUN devices used in all labs) |
| netem | **not available** (no `sch_netem` module, no `xt_statistic`) → userspace AF_PACKET impairment bridge `tests/tools/netimpair` |
| Two-server testing | **not possible** (one VM, no second host, no credentials for external servers) |
| Restricted network | **not available** |

## Validation matrix

| Feature | Implemented | Actually Tested | Result | Evidence |
|---|---:|---:|---|---|
| TUN/L3 | Yes | Yes | PASS | real `tun0` on both namespace nodes in every lab run; IPv4+IPv6 addresses, routes, MTU applied via netlink; clean teardown (`b_exit 0`) |
| IPv4 | Yes | Yes | PASS | ping/iperf3/TCP+UDP probes over all carriers (`impair-final.json`, `mtu.json`, `failover.json`); IPv4 and IPv6 underlay |
| IPv6 | Yes | Yes | PASS | inner IPv6 ping at the link limit on every carrier; 1280-byte IPv6 over links < 1280 via new engine IPv6 fragmentation (19/19 cases, `mtu.json`); IPv6 underlay sweep 1280/1400/1500 × 5 carriers |
| TCP | Yes | Yes | PASS | carrier `tcp`: impairment matrix, MTU sweep, failover, compat old↔new, soak |
| UDP | Yes | Yes | PASS | carrier `udp`: same set |
| QUIC stream | Yes | Yes | PASS | carrier `quic`: same set; critical close/write wedge found and fixed (regression tests `TestCloseUnblocksBlockedWrite`, `TestStreamWriteDeadline`); n/a below 1228-byte underlay (RFC 9000) |
| QUIC datagram | Yes | Yes | PASS | carrier `quic-dgram`: impairment, MTU (per-link limit 1120, larger packets fragmented), compat; n/a below 1228 |
| WSS | Yes | Yes | PASS | carrier `wss`: impairment, MTU, failover, reverse transfer finished on wss, compat |
| ICMP | Yes (experimental) | Yes | PASS | carrier `icmp`: impairment matrix, MTU sweep (planning bug fixed), failover target + restore, non-root; reply filter: host ping keeps working, `icmp_echo_ignore_all` stays 0, kernel replies to tunnel requests dropped (85k–207k), table removed on exit (`icmp_check`, `nonroot.out`). Remains EXPERIMENTAL (IPv4 only, raw sockets) |
| CMP | No | No | NOT TESTABLE | No "CMP" carrier exists in the code base; interpreted as a typo for ICMP (row above). If a different protocol was meant, it is not implemented |
| Packet loss | Yes | Yes | PASS | 6 carriers × 0/1/5/10/20 %/dir via userspace bridge (no netem): environment vs tunnel loss separated, RTT, jitter, goodput, CPU, RSS, reconnects (`impair-final.json`) |
| Latency | Yes | Yes | PASS | +20/50/100/200 ms RTT × 6 carriers; RTT added exactly, tunnel overhead < 1 ms (`impair-final.json`) |
| Jitter | Yes | Yes | PASS | +50 ms ±5 ms and +100 ms ±10 ms × 6 carriers; mdev ≈ 4 / 7.5 ms; `tuunel_jitter_seconds` matches ping mdev (3.98 vs 3.97 ms, `metrics.json`) |
| MTU | Yes | Yes | PASS | underlay 1200/1228/1280/1300/1350/1400/1450/1500 × tcp/udp/quic/quic-dgram/wss/icmp (IPv4) + 1280/1400/1500 × 5 carriers (IPv6): DF at limit, DF+1 signalled, no-DF at TUN MTU, 8000-byte, IPv6, iperf3 — 63/63 (`mtu.json`) |
| Carrier failover | Yes | Yes | PASS | real blocking (nftables/ip6tables) of udp → quic → tcp → wss → icmp, all-down, restore, preempt; 3 cycles; long-lived inner TCP + TCP forward 0 integrity errors, same connection survived; UDP flow + UDP forward survive, loss only during outages (`failover.json`) |
| Endpoint failover | Yes | Yes | PASS | e1 unreachable → e2 in 3.7 s ×2, preempt back 2.8/6.1 s, `endpoint_switches` counted (`failover.json`) |
| Reverse tunnel | Yes | Yes | PASS | remote dials out, edge inbound blocked for the remote; 32 MiB SHA-256-identical transfer through a forward, steady (0.9 s) and with the active carrier blocked mid-transfer (8.5 s, finished on wss); UDP forward 200/200 (`failover.json` reverse) |
| TCP forwarding | Yes | Yes | PASS | TCP forward probe through every failover event (0 integrity errors); reverse 32 MiB transfers |
| UDP forwarding | Yes | Yes | PASS | UDP forward probe through every failover event (0 integrity errors); reverse mapping 200/200 |
| Systemd | Yes | Partial | PARTIAL | unit verified offline: `systemd-analyze verify` clean, `systemd-analyze security --offline` exposure 2.1 OK; installer-generated configs run non-root with the unit's 3 capabilities. **BLOCKED — no running systemd (PID 1 is sandbox-init), booting a systemd container was not permitted.** Procedure below |
| Installer | Yes | Yes | PARTIAL | `scripts/test-install.sh`: 34 passed / 0 failed (edge + remote `--root` installs, peer-key fill, idempotent re-run, `--force-config` backup, configs run and fail over in the lab, uninstall/purge). Service start/`systemctl`/`doctor` under systemd not executed (see Systemd) |
| Docker | Yes | No | NOT TESTABLE | Dockerfile (non-root uid 10001, healthcheck, SIGTERM) + compose written; `scripts/test-docker.sh` prints NOT TESTABLE (exit 3). **BLOCKED — no Docker daemon; starting one was not permitted.** Non-root + file-capability runtime model tested outside Docker (`nonroot.out`) |
| One-line install | Yes | Yes (local HTTPS) | BLOCKED_EXTERNAL_DEPENDENCY | `curl -fsSL https://… \| sudo bash -s -- …` executed against a local HTTPS server with a test CA: SHA-256 + SSH signature verified, tampered archive / SHA256SUMS / missing signature / untrusted certificate rejected. **BLOCKED — no public HTTPS host for release artifacts (repository is private; no URL invented)** |
| Real two-server test | Yes (code) | No | BLOCKED_EXTERNAL_DEPENDENCY | **BLOCKED — only one network environment is available.** Required: one edge VPS + one remote VPS with independent public connectivity. Procedure below |
| Restricted network test | Yes (code) | No | BLOCKED_EXTERNAL_DEPENDENCY | **BLOCKED — no authorized restricted/filtered network available.** No restricted-network compatibility is claimed. Procedure below |
| Security checks | Yes | Yes | PASS | gofmt, vet, staticcheck clean; govulncheck 0 reachable; `go mod verify`; `go test -race` pass; fuzz 60 s × 2 targets (≈4.7 M execs, no crash); secret scan 0 hits (tree + history); 19-point review ([SECURITY.md](SECURITY.md)). Self-review, not an independent audit |
| Soak test | Yes | Yes (2 × 1 h) | PARTIAL | fixed build, 1 h: no leaks (RSS 17.7→18.3 MB, goroutines/fds flat), no crash/deadlock, 0 integrity errors (28 173 TCP echoes, 175 666 UDP), 11/11 bursts, 5/5 double-carrier blocks recovered; **script verdict FAIL on flapping**: 32 switches vs bound 20 (pre-fix build: 222 and a permanent traffic wedge). 6 h not run (see [Soak](#soak)) |

### Additional checks

| Check | Result | Evidence |
|---|---|---|
| Metrics reflect reality | PASS | `tests/lab/metrics_check.py`: 21/21 — Prometheus text parses; RTT 51.3 ms vs ping 50.7 ms; jitter 3.98 vs 3.97 ms; loss ratio 0.15 at 10 %/dir and back to 0; tx/rx bytes = exact inner bytes (205 600 for 200 × 1028 B); packets; active_carrier_info follows switch; block → carrier_switches +1, failure_switches +1, reconnects +1; preempt → +1 / +0; endpoint switches counted in failover lab |
| Compatibility old ↔ new | PASS | `tests/lab/compat.py` with the e7fd1d6 build: old dialer → new listener and new dialer → old listener over udp, quic, quic-dgram, tcp, wss: tunnel up, 0 % loss, iperf 490–1037 Mbit/s, udp-blocked failover 3.3 s each. Wire protocol unchanged (no version negotiation needed; ICMP not included) |
| Reproducible release amd64/arm64 | PASS | `scripts/release.sh` twice → identical SHA256SUMS; archives contain correct ELF arch; `tuunel version` prints version + commit (in `test-install.sh`). arm64 binary not executed (no arm64 host/qemu) |
| Non-root runtime | PASS | uid 10001, CapEff 0x3400 only, all carriers 0 % ping loss, ICMP filter via nft with caps (`nonroot.out`) |
| Wrapper scripts (quick mode) | PASS | `test-network-impairment.sh` 86 s, `test-failover.sh` 72 s, `test-mtu.sh` 31 s, `test-soak.sh` 173 s (100 s soak + setup/teardown); all PASS |


## Bugs found and fixed in this round

| # | Severity | Component | Bug (how found) | Fix | Regression evidence |
|---|---|---|---|---|---|
| 10 | **Critical** | QUIC stream carrier / engine | `Stream.Close` did not unblock a `Write` stuck on a black-holed path; after a QUIC failure under load the engine's sender stayed wedged on the dead link → **all tunnel traffic stopped permanently while status showed UP** (failover lab + 1 h soak) | `CancelWrite`+`CancelRead` before `CloseWithError`; 5 s write deadline on every stream-carrier write (`carrier.StreamWriteTimeout`; WebSocket write ctx 15 s → 5 s) | `TestCloseUnblocksBlockedWrite` (fails on old code), `TestStreamWriteDeadline`; failover 3 cycles PASS; soak 2 traffic alive |
| 11 | High | failover / health | udp ↔ quic degrade/preempt flapping at 1 % loss: 222 switches/h (soak) | `failover.degrade_holdoff` (1 m, doubling, cap 32×) + health loss noise floor (≥ 2 lost probes) | `TestDegradeHoldoffDampsFlapping`, `TestSingleLossIsNoise`; soak 2: 32/h |
| 12 | High | ICMP carrier | listener needed `net.ipv4.icmp_echo_ignore_all=1` → host stopped answering all pings | nftables rule drops only kernel replies to `TUNQ` requests; removed on exit | `icmp_check` (host ping OK, sysctl 0, 164k replies dropped, table removed); non-root variant |
| 13 | Medium | MTU planning | ICMP-only configs skipped path-MTU detection (no port) → TUN MTU 1370 on a 1200-byte path; DF packets black-holed (MTU sweep) | detect with a placeholder port for portless endpoints | MTU sweep icmp 1200/1228 PASS |
| 14 | High | engine / IPv6 | IPv6 packets of 1121–1280 bytes black-holed on links that cannot carry 1280 bytes (quic-dgram always; udp/icmp on underlay < ≈1340): ICMPv6 too-big below 1280 is invalid so the engine dropped them (MTU sweep follow-up) | RFC 8200 §5 link fragmentation: engine emits IPv6 fragments, destination reassembles; packets > 1280 get too-big 1280 | `TestFragmentIPv6ReassemblesToOriginal`; 19/19 lab cases |
| 15 | Medium | installer | sourcing `/etc/os-release` overwrote `$VERSION` (release version) | release version in `REL_VERSION`; os-release read in subshells | `test-install.sh` “installed the embedded release version” |
| 16 | Low | QUIC | dial timeout gave no hint | error mentions UDP filtering / path MTU < 1228/1248 | – |
| 17 | Low (test) | lab | ping with `-w` inflated loss; MTU test took the link limit from the log | fixed in `labkit.py` / `mtu.py` (uses status `link_mtu`) | – |

## Exact commands

```bash
export PATH=/data/opt/go/bin:$PATH GOTOOLCHAIN=local
gofmt -l . ; go vet ./... ; staticcheck ./... ; govulncheck ./... ; go mod verify
go test -count=1 ./... ; go test -race -count=1 ./...
go test -run='^$' -fuzz=FuzzValidate -fuzztime=60s ./internal/packet
go test -run='^$' -fuzz=FuzzOpen -fuzztime=60s ./internal/session
systemd-analyze verify --root=R /etc/systemd/system/tuunel.service ; systemd-analyze security --offline=yes --root=R tuunel.service
sudo bash scripts/test-install.sh                                    # 34/0
bash scripts/test-docker.sh                                          # exit 3 NOT TESTABLE
sudo TUUNEL_BIN=BIN python3 tests/lab/impair.py  results/impair-final.json
sudo TUUNEL_BIN=BIN python3 tests/lab/mtu.py     results/mtu.json
sudo TUUNEL_BIN=BIN CYCLES=3 python3 tests/lab/failover.py results/failover.json
sudo TUUNEL_BIN=BIN SOAK_SECONDS=3600 BLOCK_DEPTH=2 python3 tests/lab/soak.py results/soak2-3600.json
sudo TUUNEL_BIN=BIN python3 tests/lab/metrics_check.py results/metrics.json
sudo TUUNEL_BIN=BIN OLD_BIN=OLDBIN python3 tests/lab/compat.py results/compat.json
sudo TUUNEL_BIN=BIN python3 tests/lab/nonroot.py ; sudo TUUNEL_BIN=BIN python3 tests/lab/icmp_check.py
sudo tests/lab/netns-bench.sh BIN 100
sudo bash scripts/test-network-impairment.sh ; sudo bash scripts/test-failover.sh ; sudo bash scripts/test-mtu.sh ; sudo bash scripts/test-soak.sh
```

## Soak

Two 1-hour soaks were run (`SOAK_SECONDS=3600`, `BLOCK_DEPTH=2`: every 10 min
the active carrier **and its successor** blocked for 60 s; 1 %/dir loss,
+10 ms, ±1 ms; carriers udp → quic → tcp → wss; TCP + UDP integrity probes;
iperf3 bursts every 5 min; samples every 30 s).

| | Soak 1 (pre-fix build) | Soak 2 (fixed build) |
|---|---|---|
| Script verdict | PASS at the time — **re-classified FAIL** (the script then lacked the dead-traffic check) | **FAIL** (flapping criterion only) |
| Traffic | **dead from t≈2418 s** after the quic block until the end (≈20 min), status still UP; 4 iperf bursts failed | alive for the whole hour; 11/11 bursts OK (4.3–7.4 Mbit/s at 1 % loss); traffic OK after all 5 blocks |
| Blocks (switch s) | – | 5/5 recovered: 3.2–4.3 s to the next carrier, 3.2–9.8 s to the one after |
| TCP probe | 18 956 echoes, 0 integrity errors (until death) | 28 173/28 173, **0 integrity errors**, 0 reconnects, max gap 15.4 s, 28.8 MB verified |
| UDP probe | – | 175 666/178 666 (1.68 % loss ≈ environment), 0 integrity errors, max gap 9.8 s |
| Carrier switches | **222** (udp↔quic flapping) | **32** (10 failure-driven); bound for 5 induced outages = 20 → flagged |
| RSS A / B | 17.5 → 17.9 MB | 17.7 → 18.3 / 17.5 → 18.1 MB (trend), max 18.6 MB |
| Goroutines A / B | 14→15 / 25 | 15→17 / 25→26 |
| fds A / B | 8 / 11 | 8 / 11 (flat) |
| Leaks / crashes / deadlocks | none / none / **engine wedge** (bug fixed) | none / none / none |

Soak 2 residual: 12 voluntary udp ↔ quic switches outside the block windows
(the udp link really sees ≈2 % probe loss at 1 %/dir with the lab's
aggressive health settings — 500 ms interval). The degrade hold-off grew as
designed (60 s → 126 s …) and the tunnel ended on quic. This is a 7× reduction
from soak 1 but still above the script's strict bound, so the row is
**PARTIAL**, not PASS. A 6-hour soak was not run.

## Performance

Hardware: the VM above (2 shared vCPUs, 4.2 GB, kernel 6.18.49). Network:
veth pair in namespaces, underlay MTU 1500, no impairment, 100 MiB TCP
stream per carrier through `tun0` (`sudo tests/lab/netns-bench.sh BIN 100`),
final build. CPU-bound; ±30 % run-to-run.

| Carrier | Goodput | In-tunnel RTT | CPU A / B (s per 100 MiB) | RSS A / B |
|---|---:|---:|---|---|
| tcp | 929 Mbit/s | 0.30 ms | 0.75 / 0.67 | 17.3 / 17.1 MB |
| udp | 898 Mbit/s | 0.29 ms | 0.82 / 0.82 | 17.5 / 17.7 MB |
| quic | 552 Mbit/s | 0.46 ms | 1.49 / 1.25 | 18.7 / 18.7 MB |
| quic-dgram | 553 Mbit/s | 0.42 ms | 1.35 / 1.31 | 18.6 / 18.5 MB |
| wss | 455 Mbit/s | 0.35 ms | 1.50 / 1.83 | 18.3 / 17.8 MB |
| icmp | 386 Mbit/s (4 s iperf3 in the impairment matrix, loss 0; not in the 100 MiB bench) | 0.44 ms | – | – |

Under impairment and failover see [NETWORK-TESTING.md](NETWORK-TESTING.md)
and [FAILOVER.md](FAILOVER.md): failover 2.5–8.3 s per failed carrier
(UDP-flow outage ≈ switch time), endpoint failover 3.7 s, preempt without loss.

## Blocked items — reasons and exact procedures

### SYSTEMD: BLOCKED

Reason: PID 1 is `sandbox-init`; no systemd; booting a systemd container
needs a Docker daemon (not permitted here).

Procedure (clean Ubuntu 24.04 / Debian 12 VM, or a systemd container):

```bash
# on a VM: copy dist/ from scripts/release.sh
sudo bash install.sh --role=edge                       # installs + starts the unit
systemctl is-active tuunel && systemctl show tuunel -p User,AmbientCapabilities,NRestarts
sudo tunnelctl doctor && ip link show tun0
sudo systemctl reload tuunel && journalctl -u tuunel -n 20 --no-pager | grep -i reload
sudo kill -9 "$(systemctl show -p MainPID --value tuunel)"; sleep 5; systemctl is-active tuunel   # Restart=on-failure
sudo systemctl stop tuunel; ip link show tun0 || echo "tun0 removed"        # graceful stop
sudo reboot   # then: systemctl is-active tuunel (enabled at boot)
sudo bash install.sh --role=edge                       # idempotent re-run
sudo bash install.sh --uninstall --purge
# container alternative (needs Docker):
docker run -d --name sd --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -v "$PWD/dist:/dist:ro" jrei/systemd-ubuntu:24.04
docker exec sd bash -c 'cd /tmp && tar xzf /dist/tuunel-linux-amd64.tar.gz && cd tuunel-* && bash install.sh --role=edge'
```

### DOCKER: BLOCKED

Reason: no Docker daemon in the VM; starting `dockerd` was not permitted.

Procedure (any host with Docker ≥ 24 and `/dev/net/tun`):

```bash
bash scripts/test-docker.sh      # compose config, image build (vet+test), uid 10001, healthcheck,
                            # edge+remote containers on a bridge network, ping through the
                            # tunnel, health=healthy, tcp blocked -> udp failover, SIGTERM stop
docker compose --profile edge up -d && docker compose ps     # host -> container deployment
```

### ONE-LINE INSTALL: BLOCKED_EXTERNAL_DEPENDENCY

Reason: the repository is private; no public HTTPS host was provided, and
none was invented. Everything else (HTTPS-only download, SHA-256, optional
SSH signature, embedded URL/version/signer) is implemented and was executed
against a local HTTPS server.

Required: an HTTPS location you control (object storage, CDN, or a
web server). Procedure:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/tuunel_release -N ''            # once
RELEASE_BASE_URL=https://dl.example.com/tuunel SIGNING_KEY=~/.ssh/tuunel_release scripts/release.sh v1.0.0
# upload dist/* to https://dl.example.com/tuunel/v1.0.0/
curl -fsSL --proto '=https' --tlsv1.2 https://dl.example.com/tuunel/v1.0.0/install.sh | sudo bash -s -- --role=edge --require-signature
```

### REAL_TWO_SERVER_TEST: PARTIAL (first real deployment, v0.9.1)

Edge: Ubuntu 24.04 VPS in Iran; remote: Ubuntu 22.04, Hetzner Helsinki.
Ports 443/8443 were already taken on the edge, so both nodes used
`--ports=tcp=2083,quic=51901,wss=2087,udp=51900` (QUIC also tried on 2083).

| Check | Result |
|---|---|
| one-line install from GitHub Releases, key exchange, service start | PASS (both nodes) |
| tunnel up, `ping` 10.200.0.1 <-> 10.200.0.2 | PASS, ~86-90 ms, 0% loss |
| carriers (`tunnelctl test-carriers` / doctor reachability) | tcp PASS, wss PASS, udp PASS, **quic FAIL** on udp/2083 and udp/51901 while plain UDP on 51900 worked: QUIC is blocked on the path (DPI), not a port problem |
| iperf3 through the tunnel (tcp carrier) | ~190 Mbit/s remote->edge, 100-160 Mbit/s edge->remote |
| failover | PASS: edge service restart killed the udp session; remote marked udp failed after ~10 s and continued on wss, then preempted to tcp (better ranked) |
| carrier preference order | as configured; moving udp first in the remote config made udp the active carrier |

Findings fixed in v0.9.2: `doctor` reported Overall FAIL when only a backup
carrier was unreachable (now WARN while the tunnel is UP); the installer's
firewall hint printed default ports instead of the configured ones; carrier
preference order could not be chosen at install time (now = `--ports` order).
Still open: QUIC through Iranian DPI; restricted-network soak.

v0.9.2 real deployment follow-up: the built-in proxy worked end to end
(Windows curl / Telegram -> edge:54781 -> remote) after moving the remote
backend off port 1080, which Xray already used on that host; v0.9.3 uses
41080 by default and warns when it is taken.

v0.9.3 route-all (`tests/lab/exit_check.py`, installer-generated configs):
edge-originated TCP and ICMP reach an "internet" namespace only through the
tunnel, inbound HTTP to the edge and a TCP connection opened before activation
keep working, IPv6 outbound is blocked, `exit-down` leaves no rules. The lab
kernel has no nftables NAT, so the remote's masquerade itself was not
exercised there (the test routes the tunnel subnet back instead); it must be
confirmed on a real host with `curl -4 ifconfig.me` on the edge.

### RESTRICTED_NETWORK_TEST: BLOCKED

Reason: no authorized restricted/filtered network was available. No claim
about behaviour in such networks is made.

Procedure (only on a network you are authorized to test): install as above
with the remote (or a client) inside the restricted network, then for each
carrier record CONNECTED / FAILED / BLOCKED / UNSTABLE / DEGRADED:

```bash
sudo tunnelctl -json test-carriers > carriers.json    # per endpoint × carrier result
for i in $(seq 1 60); do sudo tunnelctl -json status >> status.jsonl; sleep 60; done   # 1 h stability
sudo tunnelctl metrics | grep -E 'switches|reconnects|loss|rtt'
```

Then remove carriers from the edge `listen:` one by one and verify automatic
fallback in `status.jsonl` and `tuunel_failure_switches_total`.

## Verdict

**NOT PRODUCTION READY.** The tunnel core, carriers, failover, MTU handling,
metrics and installer logic passed every executed lab test, and this round
fixed one critical availability bug. Critical real-world validation is still
missing: live systemd, Docker, a full two-server soak (the first real
Iran <-> Hetzner deployment passed install, tunnel, throughput and failover;
QUIC is blocked on that path), and a restricted-network soak. The soak still shows residual degrade flapping on lossy
paths.

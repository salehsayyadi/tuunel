# Network testing

All results here come from **one VM** (Amazon Linux 2023, kernel 6.18.49,
x86_64, 2 shared vCPUs, 4.2 GB RAM, Go 1.27.1) using Linux network
namespaces and a real TUN device on each node. They are **not** Internet or
two-server results. Throughput is CPU-bound and shared with the impairment
bridge and other lab processes; run-to-run variation was up to ±30 %.

## Lab topology

```text
  <P>a 192.0.2.1 ──veth── <P>m [netimpair bridge] ──veth── <P>b 192.0.2.2 (.3/.4, 2001:db8::2)
   node-a (dialer)              loss / delay / jitter            node-b (listener)
   tun0 10.200.0.1, fd20::1                                      tun0 10.200.0.2, fd20::2
```

The kernel has **no `sch_netem`** (and no `xt_statistic`), so impairment is
applied by `tests/tools/netimpair`: a userspace AF_PACKET bridge in the middle
namespace. Semantics (differs from a single-sided netem qdisc):

| Setting | Effect |
|---|---|
| loss L | L % dropped independently in **each** direction (round trip ≈ 1-(1-L)²) |
| latency D | D/2 ms added per direction → +D ms RTT |
| jitter J | uniform ±J/2 ms per direction on top of the delay (order preserved) |

Carrier blocking (failover tests) uses nftables (IPv4) / ip6tables (IPv6)
rules in node-b's namespace that drop the carrier's port inbound on the
underlay veth only. Probes (`tests/lab/probe.py`): TCP echo stream and UDP
flow with sequence numbers and SHA-256 per message (integrity, gaps, loss).

## Scripts

| Script | Quick (default, ≤ 120 s) | Full |
|---|---|---|
| `scripts/test-network-impairment.sh` | tcp+udp × loss 0/5 %, +50 ms | `FULL=1`: 6 carriers × loss 0/1/5/10/20, +20/50/100/200 ms, jitter (≈ 25 min) |
| `scripts/test-failover.sh` | 1 carrier cycle | `FULL=1`: 3 cycles + endpoints + reverse (≈ 8 min) |
| `scripts/test-mtu.sh` | udp+tcp at 1280/1500 | `FULL=1`: 1200…1500 × 6 carriers IPv4 + 3 MTUs × 5 carriers IPv6 (≈ 20 min) |
| `scripts/test-soak.sh` | 100 s smoke | `SOAK_SECONDS=1800` / `3600` … |
| `tests/lab/metrics_check.py` | – | metrics vs independent measurements (≈ 100 s) |
| `tests/lab/compat.py` | – | old ↔ new build, all carriers (`OLD_BIN=`) |

Each writes JSON to `$RESULTS` (default `/data/results`) and exits 0 PASS,
1 FAIL, 3 NOT TESTABLE. Variables: `TUUNEL_BIN` (directory with `tuunel`,
`tunnelctl`, `netimpair`; built automatically if missing).

## Loss / latency / jitter matrix

Single-carrier lab per row (no failover can hide a problem). 150–300 pings at
10 ms through the underlay ("environment loss") and through the tunnel,
then a 4 s iperf3 TCP stream through the tunnel. Build: final validation
build (health noise floor + degrade hold-off + QUIC close fix).
Verdict rule: for loss ≤ 10 % the tunnel adds at most 3 + L/2 points of
loss beyond the environment, every condition delivers packets, no crash.

| Carrier | Condition | Underlay ping loss | Tunnel ping loss | Tunnel RTT avg / mdev (ms) | TCP goodput (Mbit/s) | Reconnects | CPU A/B % |
|---|---|---:|---:|---|---:|---:|---|
| tcp | loss 0%/dir | 0.0% | 0.0% | 0.348 / 0.064 | 735.8 | 0 | 29.3/26.7 |
| tcp | loss 1%/dir | 2.0% | 0.0% | 0.49 / 1.306 | 537.0 | 0 | 21.3/23.7 |
| tcp | loss 5%/dir | 7.33% | 0.0% | 10.586 / 37.235 | 36.5 | 0 | 1.7/1.9 |
| tcp | loss 10%/dir | 16.33% | 0.0% | 130.085 / 178.506 | 4.9 | 0 | 0.7/0.7 |
| tcp | loss 20%/dir | 35.67% | 0.0% | 710.542 / 502.033 | 0.3 | 1 | 0.4/0.2 |
| tcp | +20 ms RTT | 0.0% | 0.0% | 21.53 / 1.81 | 238.4 | 0 | 12.4/10.8 |
| tcp | +50 ms RTT | 0.0% | 0.0% | 51.122 / 0.161 | 192.5 | 0 | 8.6/8.4 |
| tcp | +100 ms RTT | 0.0% | 0.0% | 101.736 / 0.646 | 95.0 | 0 | 5.9/5.2 |
| tcp | +200 ms RTT | 0.0% | 0.0% | 201.524 / 0.307 | 43.4 | 0 | 2.8/2.6 |
| tcp | +50 ms ±5 ms | 0.0% | 0.0% | 51.784 / 3.909 | 141.2 | 0 | 8.0/7.5 |
| tcp | +100 ms ±10 ms | 0.0% | 0.0% | 103.583 / 7.55 | 29.9 | 0 | 2.8/2.6 |
| udp | loss 0%/dir | 0.0% | 0.0% | 0.34 / 0.146 | 737.0 | 0 | 27.6/27.0 |
| udp | loss 1%/dir | 3.33% | 2.67% | 0.352 / 0.468 | 267.8 | 0 | 15.2/15.6 |
| udp | loss 5%/dir | 10.0% | 8.33% | 0.35 / 0.274 | 36.7 | 0 | 2.3/2.2 |
| udp | loss 10%/dir | 21.33% | 16.33% | 0.316 / 0.328 | 4.7 | 0 | 0.8/0.7 |
| udp | loss 20%/dir | 38.0% | 45.0% | 0.331 / 0.122 | 0.3 | 1 | 0.3/0.2 |
| udp | +20 ms RTT | 0.0% | 0.0% | 21.292 / 1.475 | 205.6 | 0 | 9.8/9.3 |
| udp | +50 ms RTT | 0.0% | 0.0% | 51.021 / 0.195 | 122.9 | 0 | 6.3/6.2 |
| udp | +100 ms RTT | 0.0% | 0.0% | 101.67 / 0.552 | 101.2 | 0 | 6.1/5.9 |
| udp | +200 ms RTT | 0.0% | 0.0% | 201.553 / 0.673 | 38.2 | 0 | 2.1/2.0 |
| udp | +50 ms ±5 ms | 0.0% | 0.0% | 51.872 / 4.073 | 142.5 | 0 | 7.9/7.8 |
| udp | +100 ms ±10 ms | 0.0% | 0.0% | 102.342 / 6.887 | 96.6 | 0 | 5.8/5.7 |
| quic | loss 0%/dir | 0.0% | 0.0% | 0.581 / 0.652 | 382.3 | 0 | 29.3/23.1 |
| quic | loss 1%/dir | 2.67% | 0.0% | 0.591 / 1.888 | 232.7 | 0 | 25.4/21.4 |
| quic | loss 5%/dir | 13.33% | 0.0% | 3.305 / 8.823 | 68.0 | 0 | 7.4/6.5 |
| quic | loss 10%/dir | 16.67% | 0.0% | 8.933 / 19.215 | 16.2 | 0 | 3.0/2.6 |
| quic | loss 20%/dir | 34.33% | 0.0% | 15.769 / 22.414 | 4.1 | 0 | 1.6/1.4 |
| quic | +20 ms RTT | 0.0% | 0.0% | 21.658 / 1.092 | 63.2 | 0 | 8.5/6.7 |
| quic | +50 ms RTT | 0.0% | 0.0% | 51.243 / 0.484 | 51.5 | 0 | 8.4/6.5 |
| quic | +100 ms RTT | 0.0% | 0.0% | 101.854 / 0.683 | 30.1 | 0 | 5.7/4.5 |
| quic | +200 ms RTT | 0.0% | 0.0% | 201.823 / 0.34 | 12.2 | 0 | 4.6/3.9 |
| quic | +50 ms ±5 ms | 0.0% | 0.0% | 51.767 / 4.005 | 59.1 | 0 | 8.5/6.8 |
| quic | +100 ms ±10 ms | 0.0% | 0.0% | 103.963 / 7.641 | 16.8 | 0 | 3.8/3.1 |
| quic-dgram | loss 0%/dir | 0.0% | 0.0% | 0.594 / 0.647 | 316.9 | 0 | 22.1/21.3 |
| quic-dgram | loss 1%/dir | 2.67% | 4.0% | 0.788 / 0.884 | 109.8 | 0 | 13.4/12.8 |
| quic-dgram | loss 5%/dir | 12.0% | 7.67% | 0.862 / 1.161 | 17.8 | 0 | 3.0/2.8 |
| quic-dgram | loss 10%/dir | 23.0% | 22.67% | 0.817 / 0.856 | 1.6 | 0 | 1.0/0.9 |
| quic-dgram | loss 20%/dir | 41.0% | 37.0% | 0.637 / 0.782 | 0.3 | 1 | 0.8/0.7 |
| quic-dgram | +20 ms RTT | 0.0% | 0.0% | 21.991 / 1.182 | 37.2 | 0 | 6.5/5.9 |
| quic-dgram | +50 ms RTT | 0.0% | 0.0% | 51.706 / 0.96 | 29.8 | 0 | 6.1/5.5 |
| quic-dgram | +100 ms RTT | 0.0% | 0.0% | 101.687 / 0.69 | 18.5 | 0 | 4.7/4.2 |
| quic-dgram | +200 ms RTT | 0.0% | 0.0% | 201.617 / 0.581 | 9.2 | 0 | 4.0/3.7 |
| quic-dgram | +50 ms ±5 ms | 0.0% | 0.0% | 52.487 / 3.847 | 42.4 | 0 | 6.9/6.5 |
| quic-dgram | +100 ms ±10 ms | 0.0% | 0.0% | 105.74 / 7.566 | 21.6 | 0 | 5.2/4.4 |
| wss | loss 0%/dir | 0.0% | 0.0% | 0.438 / 0.25 | 337.1 | 0 | 21.7/28.1 |
| wss | loss 1%/dir | 0.67% | 0.0% | 1.712 / 4.369 | 337.2 | 0 | 20.2/29.1 |
| wss | loss 5%/dir | 9.33% | 0.0% | 20.846 / 51.996 | 63.1 | 0 | 3.4/4.9 |
| wss | loss 10%/dir | 16.33% | 0.0% | 17.488 / 43.003 | 5.5 | 0 | 1.0/1.0 |
| wss | loss 20%/dir | 34.0% | 0.0% | 959.996 / 548.371 | 0.2 | 1 | 0.3/0.2 |
| wss | +20 ms RTT | 0.0% | 0.0% | 21.084 / 1.142 | 26.7 | 0 | 2.8/3.9 |
| wss | +50 ms RTT | 0.0% | 0.0% | 51.062 / 0.184 | 71.3 | 0 | 5.1/7.8 |
| wss | +100 ms RTT | 0.0% | 0.0% | 101.689 / 0.491 | 114.2 | 0 | 9.7/12.0 |
| wss | +200 ms RTT | 0.0% | 0.0% | 201.202 / 0.321 | 45.4 | 0 | 4.0/4.8 |
| wss | +50 ms ±5 ms | 0.0% | 0.0% | 52.037 / 4.138 | 128.9 | 0 | 12.5/15.4 |
| wss | +100 ms ±10 ms | 0.0% | 0.0% | 103.09 / 7.497 | 54.7 | 0 | 5.5/6.9 |
| icmp | loss 0%/dir | 0.0% | 0.0% | 0.435 / 0.442 | 386.4 | 0 | 25.1/24.2 |
| icmp | loss 1%/dir | 0.67% | 0.67% | 0.292 / 0.153 | 215.6 | 0 | 16.9/17.1 |
| icmp | loss 5%/dir | 10.0% | 10.33% | 0.353 / 0.071 | 21.8 | 0 | 2.3/2.1 |
| icmp | loss 10%/dir | 15.67% | 20.0% | 0.266 / 0.371 | 2.9 | 0 | 0.6/0.6 |
| icmp | loss 20%/dir | 36.0% | 41.67% | 0.259 / 0.059 | n/a | 0 | 0.4/0.3 |
| icmp | +20 ms RTT | 0.0% | 0.0% | 21.299 / 1.624 | 81.1 | 0 | 6.6/6.1 |
| icmp | +50 ms RTT | 0.0% | 0.0% | 50.936 / 0.253 | 48.6 | 0 | 4.3/4.1 |
| icmp | +100 ms RTT | 0.0% | 0.0% | 101.632 / 0.256 | 25.8 | 0 | 2.6/2.3 |
| icmp | +200 ms RTT | 0.0% | 0.0% | 201.568 / 0.585 | 15.7 | 0 | 1.6/1.4 |
| icmp | +50 ms ±5 ms | 0.0% | 0.0% | 51.557 / 4.023 | 46.3 | 0 | 4.8/4.8 |
| icmp | +100 ms ±10 ms | 0.0% | 0.0% | 102.455 / 7.895 | 17.6 | 0 | 2.4/2.4 |

Verdict: **PASS** (0 failures)

### Interpretation

- **Environment vs tunnel loss.** Datagram carriers (udp, quic-dgram, icmp)
  pass the environment loss through (tunnel loss ≈ underlay loss); up to
  10 %/direction the difference stays within sampling noise (±5 points with
  150–300 pings; it is negative as often as positive). Stream
  carriers (tcp, wss, quic stream) retransmit, so the tunnel shows 0 % ping
  loss but RTT and jitter inflate sharply (TCP-over-TCP head-of-line
  blocking) and goodput collapses at ≥ 10 %.
- **20 %/direction (≈ 36 % round trip)** is beyond what the health engine
  treats as usable: tcp, udp, quic-dgram and wss declared the link failed and
  reconnected once ("Reconnects"); with a single carrier there is nowhere to
  fail over to. quic (stream) survived without reconnect.
- **Latency** is added exactly (+20/50/100/200 ms → RTT +20/50/100/200 ms);
  the tunnel adds < 1 ms. Goodput at high RTT is limited by TCP window/CPU,
  not by the tunnel.
- **Jitter** shows up 1:1 as ping mdev (≈ 4 ms for ±5 ms, ≈ 7.5 ms for ±10 ms).

## MTU sweep

`scripts/test-mtu.sh` with `FULL=1`. Per underlay MTU × carrier, a fresh lab
with that MTU on every underlay veth. Checks: tunnel up; IPv4 ping with DF
at the per-link limit passes; DF one byte above is **signalled** (EMSGSIZE or
ICMP frag-needed, never a silent black hole); no-DF ping at the full TUN MTU
passes (engine fragmentation); 8000-byte ping passes; IPv6 ping at the limit
or, where the link cannot carry 1280 bytes, a 1280-byte IPv6 ping passes via
engine IPv6 fragmentation; iperf3 TCP > 1 Mbit/s.


**IPv4 underlay** — cell: TUN MTU / per-link limit, TCP goodput Mbit/s, ✔ = all checks passed

| Underlay MTU | tcp | udp | quic | quic-dgram | wss | icmp |
|---:|---|---|---|---|---|---|
| 1200 | ✔ 1280/1280, 820.5 | ✔ 1280/1142, 776.6 | n/a (QUIC needs ≥1228) | n/a (QUIC needs ≥1228) | ✔ 1280/1280, 466.5 | ✔ 1280/1138, 501.8 |
| 1228 | ✔ 1280/1280, 885.7 | ✔ 1280/1170, 834.1 | ✔ 1280/1280, 477.3 | ✔ 1280/1120, 431.7 | ✔ 1280/1280, 475.7 | ✔ 1280/1166, 389.5 |
| 1280 | ✔ 1280/1280, 946.1 | ✔ 1280/1222, 851.0 | ✔ 1280/1280, 430.5 | ✔ 1280/1120, 537.7 | ✔ 1280/1280, 454.4 | ✔ 1280/1218, 510.7 |
| 1300 | ✔ 1280/1280, 901.7 | ✔ 1280/1242, 691.3 | ✔ 1280/1280, 439.3 | ✔ 1280/1120, 458.6 | ✔ 1280/1280, 392.7 | ✔ 1280/1238, 470.6 |
| 1350 | ✔ 1280/1280, 742.2 | ✔ 1292/1292, 674.8 | ✔ 1280/1280, 434.1 | ✔ 1280/1120, 359.2 | ✔ 1280/1280, 332.4 | ✔ 1288/1288, 444.2 |
| 1400 | ✔ 1316/1316, 767.5 | ✔ 1342/1342, 782.0 | ✔ 1298/1298, 449.0 | ✔ 1280/1120, 402.2 | ✔ 1280/1280, 445.9 | ✔ 1338/1338, 575.5 |
| 1450 | ✔ 1366/1366, 810.2 | ✔ 1392/1392, 812.6 | ✔ 1348/1348, 545.3 | ✔ 1280/1120, 353.2 | ✔ 1330/1330, 354.2 | ✔ 1370/1370, 493.5 |
| 1500 | ✔ 1416/1416, 742.6 | ✔ 1442/1442, 842.7 | ✔ 1398/1398, 496.0 | ✔ 1280/1120, 402.5 | ✔ 1380/1380, 458.8 | ✔ 1370/1370, 503.6 |

**IPv6 underlay** — cell: TUN MTU / per-link limit, TCP goodput Mbit/s, ✔ = all checks passed

| Underlay MTU | tcp | udp | quic | quic-dgram | wss |
|---:|---|---|---|---|---|
| 1280 | ✔ 1280/1280, 777.6 | ✔ 1280/1202, 755.4 | ✔ 1280/1280, 456.1 | ✔ 1280/1120, 485.5 | ✔ 1280/1280, 366.1 |
| 1400 | ✔ 1296/1296, 836.3 | ✔ 1322/1322, 542.5 | ✔ 1280/1280, 552.1 | ✔ 1280/1120, 378.8 | ✔ 1280/1280, 452.3 |
| 1500 | ✔ 1396/1396, 847.0 | ✔ 1422/1422, 713.6 | ✔ 1378/1378, 495.6 | ✔ 1280/1120, 492.6 | ✔ 1360/1360, 462.7 |

Verdict: **PASS** (0 failures)

Protocol-specific minimums:

| Carrier | Minimum underlay MTU | Reason |
|---|---|---|
| quic, quic-dgram | 1228 (IPv4) / 1248 (IPv6) | RFC 9000 §14.1: QUIC datagrams ≥ 1200 bytes. At 1200 the handshake cannot complete; the dial error says so and failover uses other carriers |
| quic-dgram | – | DATAGRAM payload fixed at 1150 bytes (fits the 1200-byte initial packet size) → per-link limit 1120 at any underlay MTU |
| udp, icmp, quic-dgram | – | below ≈ 1340 (udp) the link cannot carry 1280-byte inner IPv6 packets; the engine fragments them (RFC 8200 §5 link-specific fragmentation) |
| tcp, wss, quic stream | 576 | segmenting carriers; TUN MTU ≥ 1280 always works |

Bugs found by the sweep and fixed: ICMP-only configs planned a 1370-byte TUN
MTU on a 1200-byte path (path MTU detection skipped portless endpoints);
1121–1280-byte IPv6 packets were black-holed on links below 1280 (engine
dropped them because an ICMPv6 too-big below 1280 is not allowed). Earlier
audit fixes (per-link PMTU, non-local ICMP source, QUIC initial size) were
re-verified.

## Failover, endpoints, reverse tunnel

See [FAILOVER.md](FAILOVER.md) for measured switch times and long-lived flow
results.

## Soak

`scripts/test-soak.sh` / `tests/lab/soak.py`: udp → quic → tcp → wss
carriers under 1 %/dir loss, +10 ms, ±1 ms; continuous TCP + UDP integrity
probes; iperf3 bursts every 5 min; every 10 min the active carrier **and its
successor** are blocked for 60 s (`BLOCK_DEPTH=2`); samples every 30 s of
RSS, heap, goroutines, fds, CPU, counters, switches. Fails on leaks (linear
trend), crashes, dead traffic, integrity errors, failed bursts or flapping
(> 12 switches/h outside block windows). Results: [FINAL_VALIDATION.md](FINAL_VALIDATION.md#soak).

## Not covered by this lab

Real Internet paths (reordering, asymmetric routes, NAT rebinding, middleboxes,
DPI), separate machines, kernels with netem, and restricted networks. See
[FINAL_VALIDATION.md](FINAL_VALIDATION.md) for the procedures.

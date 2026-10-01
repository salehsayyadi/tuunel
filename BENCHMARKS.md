# Benchmarks

All numbers come from the audit VM: 2 shared cloud vCPUs, 4 GB, Linux 6.18,
final build with Go 1.27.1. Two network namespaces are joined by a veth pair
(underlay MTU 1500) with no injected loss or latency. They are **not
Internet-path numbers**, and run-to-run variation on this shared VM was large
(up to ±30 %).

## Final validation build (latest)

`sudo tests/lab/netns-bench.sh BIN 100`, final validation build, same VM:

| Carrier | Goodput | In-tunnel RTT | CPU A / B (s) | RSS A / B |
|---|---|---|---|---|
| tcp | 929 Mbit/s | 0.30 ms | 0.75 / 0.67 | 17.3 / 17.1 MB |
| udp | 898 Mbit/s | 0.29 ms | 0.82 / 0.82 | 17.5 / 17.7 MB |
| quic (stream) | 552 Mbit/s | 0.46 ms | 1.49 / 1.25 | 18.7 / 18.7 MB |
| quic (DATAGRAM) | 553 Mbit/s | 0.42 ms | 1.35 / 1.31 | 18.6 / 18.5 MB |
| wss | 455 Mbit/s | 0.35 ms | 1.50 / 1.83 | 18.3 / 17.8 MB |

Loss/latency/jitter, MTU and failover numbers: [docs/NETWORK-TESTING.md](docs/NETWORK-TESTING.md),
[docs/FAILOVER.md](docs/FAILOVER.md), [docs/FINAL_VALIDATION.md](docs/FINAL_VALIDATION.md).
The sections below are from the previous audit.

## End-to-end through tun0 (`sudo tests/lab/netns-bench.sh bin 100`)

100 MiB inner TCP stream per carrier, measuring in-tunnel RTT (ping) and
daemon CPU (user+sys) and RSS on both nodes.

| Carrier | Goodput (final build) | Goodput (earlier run, Go 1.25 build) | In-tunnel RTT | CPU A / B (s) | RSS A / B |
|---|---|---|---|---|---|
| tcp | 703 Mbit/s | 1063 Mbit/s | 0.36 ms | 0.97 / 0.93 | 16.4 / 15.0 MB |
| udp | 737 Mbit/s | 958 Mbit/s | 0.33 ms | 0.91 / 0.90 | 15.2 / 15.3 MB |
| quic (stream) | 418 Mbit/s | 646 Mbit/s | 0.56 ms | 1.80 / 1.59 | 17.7 / 16.7 MB |
| quic (DATAGRAM) | 432 Mbit/s | 638 Mbit/s | 0.52 ms | 1.56 / 1.48 | 16.6 / 16.8 MB |
| wss | 448 Mbit/s | 528 Mbit/s | 0.44 ms | 1.41 / 1.78 | 16.5 / 17.2 MB |

The two runs were taken at different times on the same VM. The difference is
consistent with noisy shared CPUs: both runs are CPU-bound (≈1 s of CPU per
100 MiB on each side). It has not been attributed to the dependency upgrade.

## Carrier lab (`sudo tests/lab/netns-extended.sh bin carriers`)

Each carrier is checked for connect time, in-tunnel RTT, a short TCP transfer,
20/20 UDP echoes, reconnect after a peer restart, and failure detection
after the path is blocked:

| Carrier | Connect | Reconnect after peer restart | Failure detection |
|---|---|---|---|
| tcp, udp, quic, quic-dgram, wss, ws, icmp | 0.03–0.24 s | 0.46–3.5 s | 3.1–3.6 s |

## Failover (`netns-extended.sh failover`, `endpoints`), three full runs

| Scenario | Result |
|---|---|
| TCP blocked → QUIC | 3.1–4.5 s; ping outage ≈2.6–3.9 s (20 pps) |
| QUIC blocked → WSS | 2.1–4.0 s; outage ≈1.9–3.6 s |
| unblock → preempt back to TCP | 1.8–8.4 s, 0 packets lost (make-before-break) |
| long-lived inner TCP connection | survived 9 switches per run |
| endpoint e1 → e2 → e3, back to e1 | 3.4–3.6 s, 5.7–7.8 s, preempt 6.6–6.8 s |

## Soak (`sudo tests/lab/netns-soak.sh bin 360`)

Over 360 s, TCP was blocked once per minute (for roughly 20–35 s) while a continuous inner
TCP echo stream, UDP echo and 5 pps ping ran:

| Metric | Value |
|---|---|
| inner TCP echoed | 2.16 GB, byte-exact |
| UDP echo | 3283/3293 |
| carrier switches | 10 (one per block and unblock) |
| longest ping outage | 3.8 s |
| RSS | 12.6–20.7 MB, no growth |
| goroutines / fds | return to baseline (A 13/8, B 21/11 on TCP) |

The first soak run exposed a 53 s undetected outage (bug #7 in FINAL_AUDIT.md).
The numbers above are from the run after the fix.

## Acceptance lab timings (`sudo tests/lab/netns-acceptance.sh bin`)

24 passed, 0 failed, 2 skipped (loss and latency injection: kernel lacked
`xt_statistic`/`sch_netem`).

| Scenario | Result |
|---|---|
| TCP carrier blocked (iptables DROP) → QUIC | switched in 3.8–4.5 s (two runs), same tunnel IPs kept working |
| QUIC blocked → WSS | switched, ping OK |
| Carriers unblocked → back to TCP | 2 s after hold/recovery criteria met |
| 5 MiB TCP + UDP echo through tunnel | ~650 Mbit/s, checksums match |

Failover time ≈ detection (`failed_after_missed × interval` or idle timeout)
+ candidate race (≤ 250 ms stagger per candidate) + handshake (1 RTT).

## Micro-benchmarks (`make microbench`)

| Benchmark | Result |
|---|---|
| carrier loopback TCP | 1191 MB/s |
| carrier loopback UDP | 145 MB/s (unpaced, ~5% kernel drops) |
| carrier loopback QUIC stream | 195 MB/s |
| carrier loopback QUIC DATAGRAM | 154 MB/s |
| carrier loopback WSS | 172 MB/s |
| session Seal+Open 1400 B | 589 MB/s (2375 ns/op) |
| Noise IKpsk2 handshake | 625 µs |

## Not measured

- Behaviour under real packet loss/jitter (netem unavailable here).
- Real Internet paths; multi-hour soak; many concurrent peers; multi-core scaling.
- TCP-over-TCP penalty under loss: expected to be significant for tcp/wss/quic
  stream carriers; prefer udp or quic DATAGRAM on lossy paths.

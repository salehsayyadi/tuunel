# Benchmarks

All numbers were measured in the development VM (shared cloud vCPUs, Linux,
Go 1.23.4). They are reproducible with the commands shown but **will differ**
on other hardware and are not Internet-path numbers. Two network namespaces
connected by a veth pair, underlay MTU 1500, no injected loss/latency.

## End-to-end through tun0 (`sudo tests/lab/netns-bench.sh bin 50`)

50 MiB inner TCP transfer through the tunnel, in-tunnel RTT via ping,
daemon CPU time (user+sys) and RSS on both nodes.

| Carrier | Goodput | In-tunnel RTT | CPU A / B (s) | RSS A / B |
|---|---|---|---|---|
| tcp | 1005 Mbit/s | 0.286 ms | 0.36 / 0.28 | 15.2 / 13.3 MB |
| udp | 991 Mbit/s | 0.288 ms | 0.35 / 0.33 | 13.5 / 14.0 MB |
| quic (stream) | 666 Mbit/s | 0.435 ms | 0.62 / 0.49 | 14.9 / 15.1 MB |
| quic (DATAGRAM) | 656 Mbit/s | 0.390 ms | 0.57 / 0.53 | 14.8 / 14.9 MB |
| wss | 500 Mbit/s | 0.350 ms | 0.60 / 0.77 | 14.9 / 14.0 MB |

Run-to-run variation on this VM was roughly ±10 %.

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
- Long-duration soak, many concurrent peers, multi-core scaling.
- TCP-over-TCP penalty under loss: expected to be significant for tcp/wss/quic
  stream carriers; prefer udp or quic DATAGRAM on lossy paths.

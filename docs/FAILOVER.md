# Failover

tuunel keeps one **active link** per peer and moves it between candidates
(endpoint × carrier) without recreating `tun0`, its addresses or routes.
Inner TCP connections and UDP flows survive a switch; only packets in flight
on the failed link are lost.

## Model

| Term | Meaning |
|---|---|
| candidate | one `endpoint` × one `carrier` from `peers[].endpoints` / `peers[].carriers` |
| rank | `failover.order: endpoint` – all carriers of the best endpoint first; `carrier` – best carrier on any endpoint. Endpoints by `endpoint_selection: priority` (list order) or `latency` |
| link | carrier connection + Noise session + health tracker |
| health states | `UP` → `DEGRADED` (loss/RTT/jitter above `health.*` thresholds) → `FAILED` (`failed_after_missed` probes missed or idle timeout) |

### Decisions

1. **Hard failure** (`FAILED`, carrier error, write timeout): the manager
   immediately races the next eligible candidates (250 ms stagger), installs
   the first one that completes a handshake and closes the old link
   (*make-before-break* when the old link is still alive, otherwise
   *break-before-make*). Counted in `tuunel_failure_switches_total`.
2. **Degraded** (`switch_on_degraded: true`): switch only if a better
   candidate is measured healthy by a probe session and `min_hold` elapsed.
3. **Preempt** (`preempt: true`): return to a higher-ranked candidate after it
   passed `recovery_successes` consecutive probes and `min_hold` elapsed.
4. **Backoff**: failed candidates are retried with exponential backoff
   (`backoff_initial` … `backoff_max`); `max_retries > 0` parks a candidate
   for `cooldown`.

### Anti-flapping

| Mechanism | Setting | Effect |
|---|---|---|
| minimum hold | `failover.min_hold` (30 s) | no voluntary switch (degrade/preempt) sooner |
| degrade hold-off | `failover.degrade_holdoff` (default 1 m, range 1 ms–1 h; unset/0 = default) | a candidate left because it was `DEGRADED` is not chosen again by preempt or degrade-switch until the hold-off expires; every further degrade departure from the same candidate within 10 min doubles it (cap 32×). Hard failures are not damped |
| loss noise floor | built in | one lost probe in the window never marks a link degraded; ≥2 are needed (≥1 keeps it degraded) |
| recovery hysteresis | `failover.recovery_successes`, `health.clear_ratio` | a degraded/failed link must be clearly good again |

The degrade hold-off and noise floor were added after the 1-hour soak
showed **222 carrier switches per hour** at 1 %/direction loss (udp ↔ quic
degrade/preempt loop).

## Bugs found by the failover lab

| Bug | Symptom | Fix | Regression test |
|---|---|---|---|
| QUIC stream link close did not unblock writers (`Stream.Close` only closes the send direction gracefully; a `Write` blocked on a black-holed path stayed blocked after `CloseWithError`) | after a QUIC carrier failed **under load**, the engine's sender stayed wedged on the dead link: **all tunnel traffic stopped permanently while status showed UP** (reproduced: block udp, then quic, with traffic → 100 % loss on tcp; 1-hour soak with the old build died at t≈2412 s) | `CancelWrite`+`CancelRead` before `CloseWithError`; plus a 5 s write deadline on every stream-carrier message write (`carrier.StreamWriteTimeout`), which closes the connection | `TestCloseUnblocksBlockedWrite`, `TestStreamWriteDeadline` (`internal/carrier/quic`) |
| flapping between udp and quic at 1 % loss | 222 switches/h | degrade hold-off + loss noise floor | `TestDegradeHoldoffDampsFlapping`, `TestSingleLossIsNoise` |

## Measured (netns lab, `scripts/test-failover.sh`, FULL=1, 3 cycles)

Carrier order udp → quic → tcp → wss → icmp, one endpoint, continuous probes:
inner TCP echo stream, inner UDP flow, TCP port forward, UDP port forward
(sequence numbers + SHA-256 per message). Switch time = block → new carrier
active in `tunnelctl status`; outage = longest gap of the UDP flow.

| Event | Switch time (3 cycles) | UDP flow outage | Notes |
|---|---|---|---|
| udp blocked → quic | 3.4 / 3.5 / 3.4 s | 3.4 / 3.5 / 3.4 s | |
| quic blocked → tcp | 5.2 / 4.7 / 18.6 s | 5.2 / 4.7 / 3.1 s | cycle 3 passed through an intermediate candidate (3 switches) before settling; traffic resumed after 3.1 s |
| tcp blocked → wss | 7.9 / 2.5 / 2.5 s | 7.9 / 2.5 / 2.5 s | |
| wss blocked → icmp | 8.2 / 8.3 / 8.2 s | 8.2 s | |
| all carriers blocked | down after 3.4 s | 15–18 s (blocked period) | status DOWN, no crash |
| icmp restored | 0.9 / 4.2 / 4.2 s | 0 | |
| all restored → preempt to udp | 4.4 / 4.0 / 4.0 s | 0 | make-before-break, no loss |
| endpoint e1 unreachable → e2 | 3.7 / 3.7 s | 3.6 / 3.7 s | `endpoint_switches` +1 |
| e1 restored → preempt e1 | 2.8 / 6.1 s | 0 | |

Long-lived flows over the whole run (≈9 switches per cycle):
- inner TCP echo and TCP forward: 0 integrity errors, 0 reconnects, the same
  connection survived all switches; the longest stall (54 s) is TCP
  retransmission back-off after the "all carriers blocked" period.
- inner UDP and UDP forward: 0 integrity errors; loss only during outages.

Reverse tunnel (remote dials out, edge inbound blocked for the remote):
32 MiB TCP transfer edge → remote through a forward, SHA-256 identical both
steady (0.9 s) and with the active carrier blocked mid-transfer (8.5 s,
finished on wss); UDP forward mapping 200/200.

## Tuning

- Faster detection: lower `health.interval` and `failed_after_missed`
  (default ≈3 s detection). Too low → false failures on jittery paths.
- Lossy but working paths: keep `switch_on_degraded: true` and the default
  `degrade_holdoff`; raise `min_hold` if switches are still frequent
  (`tuunel_carrier_switches_total`).
- Prefer datagram carriers (udp, quic-dgram) on lossy paths: stream carriers
  add TCP-over-TCP head-of-line delay (see [NETWORK-TESTING.md](NETWORK-TESTING.md)).

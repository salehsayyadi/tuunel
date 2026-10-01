# Architecture

## Reference review

BackPack's public documentation (https://github.com/AminMGMT/BackPack) was
read for its architecture only: separation of the L3 engine from carriers,
point-to-point TUN model, direct vs. reverse modes, health/failover boundaries
and MTU concerns. tuunel is an independent implementation with its own wire
protocol; no BackPack code was copied.

## Data flow

```text
apps → kernel routing → tun0 → engine.readDevice ─┐
                                                  │ route(dst) – longest-prefix match on peers' allowed_ips
                                                  ▼
                                     peer.active link (atomic pointer)
                                                  │ session.Seal  (Noise transport keys, 64-bit counter)
                                                  ▼
                                    carrier.Conn.WriteMessage   (tcp | udp | quic | quic-dgram | ws/wss | icmp)
                                                  ⋮  underlay network
                                    carrier.Conn.ReadMessage
                                                  │ session.Open + replay window
                                                  │ source anti-spoof (src ∈ peer.allowed_ips)
                                                  ▼
                                       tun0 on the remote node → kernel → apps
```

## Packages

| Package | Responsibility |
|---|---|
| `internal/carrier` | `Carrier`/`Conn`/`Listener` interfaces, `Capabilities` (reliable, datagram, max message, overhead, privileged, experimental), length-prefixed stream framing, per-source admission limiter, TLS helpers |
| `internal/carrier/{tcp,udp,quic,websocket,icmp}` | Transport implementations. Each passes `carriertest` conformance (message boundaries, sizes, close semantics, concurrency) |
| `internal/carrier/faulty` | Test-only wrapper injecting outages and loss |
| `internal/session` | Noise IKpsk2 handshake, transport keys, counters, replay window, rekey limits |
| `internal/packet` | IPv4/IPv6 validation, fragmentation, ICMP/ICMPv6 "too big" generation |
| `internal/mtu` | TUN MTU planning from path MTU and carrier overheads, path-MTU detection |
| `internal/health` | Per-link RTT/jitter/loss tracking and state machine with hysteresis |
| `internal/failover` | Candidate ranking (endpoint × carrier), backoff, cooldown, anti-flap hold, preemption |
| `internal/engine` | Peers, links, routing, dialing/racing, probes, rekey, status, reconnect, ping |
| `internal/forwarding` | TCP/UDP port forwarding with limits and live reload |
| `internal/routing`, `internal/netcfg` | Route planning with loop detection; netlink address/MTU/route management and teardown |
| `internal/config` | Strict YAML (unknown keys rejected), defaults, validation |
| `internal/api` | Unix-socket (0660) JSON API, optional TCP with bearer token, Prometheus `/api/metrics` |
| `internal/daemon` | Wiring: config → plan → TUN → engine → forwarding → API; SIGHUP reload |
| `internal/diag` | `tunnelctl doctor` checks |
| `cmd/tuunel`, `cmd/tunnelctl` | Daemon and control CLI |
| `internal/tunnel`, `internal/framing` | Legacy TLS-over-TCP MVP (kept, not used by `tuunel run`) |

## Links, peers and failover

- A **peer** has a public key, allowed IPs and a list of **candidates**
  (endpoint × carrier). Exactly one **active link** carries data; it is an
  atomic pointer so the data path never blocks on failover logic.
- A **link** = one carrier connection + one Noise session + one health tracker.
- The dialer races all currently eligible candidates in ranked order with a
  250 ms stagger and keeps the first that completes a handshake; losers are
  closed. A responder installs a new link only after the first authenticated
  data/ping from the initiator (confirm-before-install), so a replayed or
  half-open handshake cannot hijack the active link.
- Failover is **make-before-break**: the new link is installed, then the old one
  closed. Inner TCP connections survive the switch (the TUN device and
  addresses do not change); in-flight packets on the failed link are lost and
  retransmitted by the inner transport.
- Health pings run on every link. `DEGRADED` (loss/RTT/jitter above threshold)
  triggers a switch when `switch_on_degraded` is set and a better candidate is
  available; `FAILED` (missed pings / idle timeout) always triggers one.
  Recovery requires `recovery_successes` probes and respects `min_hold`.
- Probe sessions (handshake flag `probe`) measure non-active candidates and are
  never installed as the data link.

## Direct vs reverse

There is no special "reverse" protocol: a node with `listen:` accepts, a node
with `endpoints:` dials. In reverse mode the NAT'd node dials the public edge,
and the edge exposes services through `forwarding:` rules pointing at the
remote node's tunnel address.

## Transport vs transported traffic

The carrier is the *transport* of the tunnel. Anything inside the tunnel
(including WireGuard, OpenVPN, SSH, …) is *transported* traffic. tuunel does
not implement WireGuard; WireGuard can run over a tuunel tunnel like any other
IP traffic (MTU must account for both overheads).

## Concurrency model

One reader goroutine for the TUN device, one reader per link, one health loop
per peer, one accept loop per listener. Per-link writes are serialized by the
carrier connection. All shared counters are atomics; `go test -race ./...` is
clean.

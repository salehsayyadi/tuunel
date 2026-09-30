# Architecture and reconnaissance

## Reference review (Phase 1)

The public BackPack documentation was reviewed for its separation of the L3 engine from carrier implementations, point-to-point TUN model, direct/reverse modes, health/failover boundaries, and operational concerns such as MTU and independent tunnel lifecycles. Useful architectural lessons are: keep the packet engine isolated from carrier code; treat direct L3 and service forwarding as separate capabilities; make peer/config agreement explicit; avoid flapping during failover; and report carrier limitations rather than assuming reachability.

This project is an independent implementation. No BackPack source files or protocol implementation were copied. The reference is architectural only: https://github.com/AminMGMT/BackPack

## Current MVP (Phase 3)

```text
Linux applications -> kernel routes -> tun0 -> bounded IP frame -> TLS 1.3/mTLS over TCP
   -> bounded IP frame -> tun0 -> kernel routes -> remote applications
```

- The TUN device supplies whole IPv4/IPv6 packets; local administrators configure addresses and routes.
- The carrier boundary is the authenticated `net.Conn` stream. The L3 framing package knows nothing about TCP sockets or certificates.
- Mutual peer authentication and AEAD-protected transport are delegated to Go's TLS 1.3 implementation, with server-side client certificate DNS identity allowlisting. Application framing only adds a strict sequence and packet boundary.
- The client reconnects with capped exponential backoff. The listener accepts one peer at a time.
- Queued packets are bounded; malformed and oversized IP packets are rejected.

## Deliberate scope limits

This first commit is a compileable MVP, not a production-ready general tunnel suite. It does not yet configure routes, manage YAML/TOML, perform auto-MTU probing, support multiple carriers/endpoints, reverse forwarding, API/metrics, systemd installation, or live carrier migration. IPv6 jumbograms are not supported. TCP is the requested initial carrier, but TCP carrying arbitrary inner TCP can suffer TCP-over-TCP head-of-line/retransmission collapse under loss. Do not interpret this MVP as a recommendation to put a TCP tunnel on a lossy route.

## Roadmap

1. Harden lifecycle, observability, config validation and TUN integration tests.
2. Add reusable carrier interface and UDP datagram transport; define explicit per-carrier MTU budgets.
3. Add QUIC using a maintained Go implementation, then WSS only if a real deployment requires it.
4. Add health measurement and hysteretic failover between equivalent peers; do not silently claim session migration where carrier semantics cannot provide it.
5. Add endpoint selection, controlled forwarding and operator diagnostics.
6. Consider ICMP only as opt-in experimental functionality after authorization/availability checks and resource limits.

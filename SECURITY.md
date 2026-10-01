# Security

## Threat model

- The underlay network is hostile: it can observe, drop, delay, reorder,
  replay and inject packets, and can probe listeners.
- Peers are authenticated by static X25519 keys configured on both sides.
  A peer is trusted only for the source addresses in its `allowed_ips`.
- The local host (root, config files, key files) is trusted.

## Mechanisms

| Concern | Mechanism | Test |
|---|---|---|
| Confidentiality/integrity | Noise IKpsk2, ChaCha20-Poly1305 transport, optional PSK | `internal/session` |
| Peer authentication | responder authorizes initiator static key before replying; unknown keys get no response | engine `unauthorized peer` test |
| Replay (data) | 64-bit counters, 2048-message sliding window | replay tests + fuzz |
| Replay (handshake) | timestamp window (60 s) + seen-set per key; confirm-before-install on responder | session/engine tests |
| Spoofing inside tunnel | source address must be in sender's `allowed_ips`, otherwise dropped and counted (`spoofed`) | engine spoof test |
| Key exhaustion | rekey on interval or 2^48 messages; hard reject at 2^60 | session tests |
| Admission/DoS | per-source token bucket on new connections/sessions; handshake read deadline; UDP/ICMP session caps and idle reaping; bounded queues; max message 65535 | carrier tests |
| Amplification | handshake responses only to authorized keys; pong size == ping size; ping body ≤ 1200 bytes | code review |
| Malformed input | strict IPv4/IPv6 header validation; parsers fuzzed (`session`, `packet`) | fuzz targets |
| Management API | Unix socket mode 0660; TCP API requires bearer token for non-loopback listen | config validation |
| Secrets | permissive key file modes are warned about on load; keys never logged | code review, `doctor` |
| Config | unknown YAML keys rejected; addresses, names, ports, prefixes validated | `internal/config` tests |
| Privileges | systemd unit keeps only `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`; `ProtectSystem=strict`, `NoNewPrivileges`, `DeviceAllow=/dev/net/tun` | not executed here |

## Audit performed (this repository)

Final internal production-readiness audit: see [FINAL_AUDIT.md](FINAL_AUDIT.md).

- `gofmt -l .` – clean; `go vet ./...` – clean
- `staticcheck ./...` (built with Go 1.27.1) – clean
- `go test ./...` and `go test -race ./...` (Go 1.27.1) – pass, no races
- `gosec ./...` – reviewed. Findings are integer-conversion warnings (G115) on
  bounded values, unchecked `Close` errors (G104), config-path file reads
  (G304), `math/rand` for backoff jitter (G404, not security-relevant) and
  `unsafe` for the TUN ioctl (G103). No exploitable issue was found.
- `govulncheck ./...` – **0 vulnerabilities reachable** after upgrading:
  - Go toolchain 1.25.1 → **1.27.1** (stdlib CVEs in net/http, crypto/tls,
    encoding/*). `go.mod` now requires `go 1.26.0`.
  - `golang.org/x/net` v0.30.0 → **v0.59.0**.
  - `github.com/quic-go/quic-go` v0.48.2 → **v0.63.0**. GO-2025-4017 was a
    remotely triggerable panic, reachable through `quic.DialAddr` and
    `quic.ListenAddr`.
  - Remaining module-level notice GO-2026-5932 (`x/crypto/openpgp` is
    deprecated) does not apply: tuunel never imports that package.
- Fuzz targets: session open/handshake parsing, packet validation.

### Security-relevant bugs fixed in the audit

| Bug | Impact | Fix |
|---|---|---|
| Carrier reader goroutine (`pump`) blocked forever on a full channel after a rejected handshake or a closed link | **unauthenticated memory/goroutine leak** (remote DoS) | `pump.stop()` closes the connection and a `done` channel; it is used on every error path; regression test `TestPumpStopReleasesReaderWhenChannelFull` |
| TUN fd registered with the Go poller before `TUNSETIFF` (`read /dev/net/tun: not pollable`) | daemon exited at start (availability) | open with `syscall.Open`, ioctl, set non-blocking, then `os.NewFile` |
| quic-go GO-2025-4017 | remote panic of the daemon | dependency upgrade |

Functional bugs found by the lab (MTU handling, failover metrics) and their
fixes are listed in FINAL_AUDIT.md.

This is a self-review, **not** an independent third-party audit.

## Outer TLS

QUIC and WSS use TLS for the carrier. By default the server presents an
ephemeral self-signed certificate and the client does not verify it, because
authentication is provided by the inner Noise session; the outer TLS exists for
transport compatibility. Configure `tls_cert_file`/`tls_key_file` (server) and
`tls_ca_file`/`tls_server_name` (client) if you want the outer TLS verified as
well. Do not rely on outer TLS alone.

## Known limitations

- No traffic obfuscation/anti-DPI claims. Carriers look like their protocol
  (e.g. WSS looks like WebSocket over TLS) but timing/size patterns are not
  shaped.
- ICMP carrier is experimental, IPv4 only, needs `CAP_NET_RAW`.
- No key revocation list or hot key rotation without restart (reload covers
  forwarding rules only).
- The legacy `server|client` MVP uses TLS mTLS and has not received the same
  review.
- Handshake replay protection uses a 60 s timestamp window. As with
  WireGuard, if a peer's clock steps back by more than the window, its
  handshakes are rejected until the responder restarts. Keep NTP running.
- The ICMP carrier requires `net.ipv4.icmp_echo_ignore_all=1` on the listener,
  which also stops the host from answering normal pings.
- Outer TLS on QUIC/WSS is not verified by default (see above). An on-path
  attacker can terminate it but cannot read or forge tunnel traffic.
- The systemd hardening (`ProtectSystem=strict`, capability bounding,
  `MemoryDenyWriteExecute`, …) was not executed under systemd in the audit.
  Running non-root with only `CAP_NET_ADMIN`/`CAP_NET_RAW`/
  `CAP_NET_BIND_SERVICE` was tested.

## Reporting

Please report vulnerabilities privately to the repository owner.

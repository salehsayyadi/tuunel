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

- `gofmt -l .` – clean
- `go vet ./...` – clean
- `staticcheck ./...` (2024.1.1) – clean (unused code removed)
- `go test ./...` and `go test -race ./...` – pass, no races
- Fuzz targets: session open/handshake parsing, packet validation

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

## Reporting

Please report vulnerabilities privately to the repository owner.

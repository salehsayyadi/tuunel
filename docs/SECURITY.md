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
| Privileges | systemd unit keeps only `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE`; `ProtectSystem=strict`, `NoNewPrivileges`, `DeviceAllow=/dev/net/tun` | offline `systemd-analyze verify/security`; non-root + caps lab |

## Checks executed (final validation, Go 1.27.1, linux/amd64)

Raw output: `/data/results/sec/*.txt` on the validation VM; summary below.

| Check | Command | Result |
|---|---|---|
| formatting | `gofmt -l .` | clean |
| vet | `go vet ./...` | clean |
| staticcheck | `staticcheck ./...` (2026.x built with Go 1.27.1) | clean |
| vulnerabilities | `govulncheck ./...` | **0 reachable**; 1 module-level notice GO-2026-5932 (`x/crypto/openpgp` deprecated, package not imported, no fix available) |
| module integrity | `go mod verify` | all modules verified |
| dependencies | `go list -m all` | 25 modules; direct: coder/websocket v1.8.12, flynn/noise v1.1.0, quic-go v0.63.0, vishvananda/netlink v1.3.0, x/net v0.59.0, yaml.v3 v3.0.1 |
| unit + race | `go test -race -count=1 ./...` | pass, no races |
| fuzz | `go test -fuzz=FuzzValidate -fuzztime=60s ./internal/packet`; `-fuzz=FuzzOpen ./internal/session` | ≈2.3 M / 2.4 M execs, no crash |
| secret scan | grep for private-key PEM headers, `ghp_`/`github_pat_`, AWS `AKIA`, Slack `xox[bp]-`, `sk-…` in all tracked files and `git log -p` | 0 matches; no `*.key`/`*.pem` tracked (`.gitignore` excludes `deploy/docker/*/node.key`) |
| systemd unit | `systemd-analyze verify` (offline root) and `systemd-analyze security --offline=yes` (systemd 252) | verify clean; exposure **2.1 OK** (remaining: network caps, host network, no PrivateUsers, `/dev/net/tun` ACL, no IPAddressDeny – all inherent to a tunnel) |
| malformed input | unit tests: truncated/oversized/invalid IPv4/IPv6 headers, bad frames, wrong lengths, replayed and reordered counters, bit-flipped ciphertext, unknown keys | pass |
| non-root runtime | lab: uid 10001 with only `cap_net_admin,cap_net_raw,cap_net_bind_service` file caps, all carriers, ICMP filter via `nft` | pass |
| installer tampering | `scripts/test-install.sh`: tampered archive, tampered SHA256SUMS, missing signature with `--require-signature`, untrusted TLS certificate | all rejected |

`gosec` was run in the previous audit (findings: G115 bounded integer
conversions, G104 unchecked Close, G304 config-path reads, G404 math/rand for
backoff jitter, G103 unsafe for the TUN ioctl; none exploitable). It was not
re-run here.

## Focused security review (19 items)

| # | Topic | Finding | Status |
|---|---|---|---|
| 1 | unauthenticated packet acceptance | Every data packet is AEAD-authenticated before use (`session.Open`); responders answer only handshakes from configured static keys; after decryption the inner source must be in the peer's `allowed_ips` (`spoofed_source` drop). Unknown keys get no response | OK |
| 2 | replay | 64-bit counters + 2048-entry sliding window per session; handshake timestamp window (60 s) + per-key seen set; confirm-before-install on the responder | OK (clock-step caveat below) |
| 3 | nonce reuse | Counter is the ChaCha20-Poly1305 nonce, strictly increasing, never reset within a session; rekey every 10 min or 2^48 messages; hard stop at 2^60 | OK |
| 4 | key leakage | Private keys never logged; files written 0600 under `umask 077`; key loader warns about group/world-readable keys; status/doctor/metrics expose only public keys | OK |
| 5 | memory exhaustion | Bounded per-link channels (64), max message 65535, UDP/ICMP session caps + idle reaping, per-source admission token bucket, handshake read deadline. 1 h soak RSS 17–18 MB, flat | OK |
| 6 | fd exhaustion | Session caps per listener; `max_connections` per forward; soak fds flat (8/11) | OK; no global fd cap (relies on `LimitNOFILE`) |
| 7 | goroutine leaks | `pump.stop()` on every exit path (fixed in previous audit); soak goroutines 14→15 / 25 flat | OK |
| 8 | malformed frames | Length-prefixed framing rejects 0/oversized lengths; WebSocket text frames rejected; fuzzed parsers | OK |
| 9 | oversized packets | Packets > link limit are fragmented (IPv4 without DF) or answered with ICMP/ICMPv6 too-big; MTU sweep 1200–1500 verified | OK |
| 10 | invalid sequence numbers | Counters below the window or already seen fail `session.Open` and are dropped (counted under `auth_failures`); counter ≥ 2^60 rejected | OK |
| 11 | handshake abuse | Token bucket per source IP for new sessions; responder keeps no state for unauthenticated initiators beyond the Noise read; handshake response only to authorized keys (no amplification: response ≤ request) | OK; a distributed flood from many IPs is bounded only by CPU |
| 12 | authentication bypass | Noise IK requires the initiator's static key in the first message; PSK (optional) mixed in `psk2`; no unauthenticated control messages | OK |
| 13 | privilege escalation | Runs as user `tuunel` (systemd) / uid 10001 (Docker) with 3 capabilities; `NoNewPrivileges`; the daemon executes only `nft` (absolute lookup in fixed paths, fixed arguments, stdin script built from constants) | OK |
| 14 | unsafe installer behaviour | `set -euo pipefail`; HTTPS-only (`--proto =https --tlsv1.2`), bounded timeouts, SHA-256 verification mandatory for downloads, optional/required SSH signature verification, `tar --no-same-owner`, never overwrites keys/configs without `--force-config` (backup kept), temp dir via `mktemp -d` + trap | OK |
| 15 | unsafe systemd config | See exposure table above. `ReadOnlyPaths=/etc/tuunel`, `ProtectSystem=strict`, `DevicePolicy=closed`, syscall filter `@system-service` minus privileged sets | OK (offline-verified only; not executed under systemd) |
| 16 | Docker privilege escalation | Image runs as uid 10001; compose: `cap_drop: ALL` + NET_ADMIN/NET_RAW/NET_BIND_SERVICE, read-only rootfs, only `/dev/net/tun`. `no-new-privileges` is deliberately **not** set: it would disable the file capabilities the non-root binary needs (use root-in-container + `no-new-privileges` instead if you prefer). `network_mode: host` is required for a host tunnel and gives the container the host's network namespace | OK by review; **not executed** (no Docker daemon) |
| 17 | secret leakage in logs | Logs contain peer names, public keys, addresses; never private keys, PSKs or API tokens (grep of all `log.`/`slog` calls) | OK |
| 18 | insecure temporary files | Installer uses `mktemp -d`; daemon writes no temp files; API socket in `RuntimeDirectory` (0750) with mode 0660 | OK |
| 19 | unsafe download/update | No self-update. One-line install requires HTTPS, verifies SHA256SUMS, optionally an SSH signature (`ssh-keygen -Y verify -n tuunel-release`) with a pinned signer key embedded at release time. Without a signature, integrity relies on the HTTPS host (documented) | OK; signing is optional by design |

### Security-relevant bugs fixed in this round

| Bug | Impact | Fix |
|---|---|---|
| QUIC stream close left blocked writers (see [FAILOVER.md](FAILOVER.md#bugs-found-by-the-failover-lab)) | an on-path attacker that black-holes QUIC while traffic flows could permanently stop the tunnel (availability) | cancel both stream directions before close; 5 s write deadline on all stream carriers |
| ICMP carrier required `net.ipv4.icmp_echo_ignore_all=1` | disabled host ping system-wide | narrow nftables rule drops only kernel replies to tunnel requests (magic `TUNQ`) |

### Earlier audit fixes

| Bug | Impact | Fix |
|---|---|---|
| Carrier reader goroutine (`pump`) blocked forever on a full channel after a rejected handshake or a closed link | **unauthenticated memory/goroutine leak** (remote DoS) | `pump.stop()` closes the connection and a `done` channel; regression test `TestPumpStopReleasesReaderWhenChannelFull` |
| TUN fd registered with the Go poller before `TUNSETIFF` | daemon exited at start | open with `syscall.Open`, ioctl, set non-blocking, then `os.NewFile` |
| quic-go GO-2025-4017 | remote panic | dependency upgrade to v0.63.0 |

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
- The ICMP listener installs nftables table `ip tuunel_icmp` (one rule: drop
  kernel echo replies whose payload starts with `TUNQ`) and removes it on
  exit. If the daemon is killed with SIGKILL the table stays until the next
  start (which flushes it) or `nft delete table ip tuunel_icmp`. It needs
  `nft` and `CAP_NET_ADMIN`; with `experimental.icmp_reply_filter: off` you
  must stop kernel replies yourself. Nothing else on the host is changed
  (no sysctl writes).
- Outer TLS on QUIC/WSS is not verified by default (see above). An on-path
  attacker can terminate it but cannot read or forge tunnel traffic.
- The systemd hardening (`ProtectSystem=strict`, capability bounding,
  `MemoryDenyWriteExecute`, …) was verified offline (`systemd-analyze`) but
  not executed under a running systemd.
  Running non-root with only `CAP_NET_ADMIN`/`CAP_NET_RAW`/
  `CAP_NET_BIND_SERVICE` was tested.

## Reporting

Please report vulnerabilities privately to the repository owner.

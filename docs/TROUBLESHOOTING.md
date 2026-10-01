# Troubleshooting

Start with:

```bash
sudo tunnelctl doctor     # kernel, permissions, config, keys, DNS, MTU, routing, reachability
sudo tunnelctl status     # state, active carrier/endpoint, RTT, loss, jitter, traffic
sudo tunnelctl test-carriers # actively probe every endpoint × carrier
journalctl -u tuunel --no-pager -n 100   # or: tunnelctl logs
systemctl status tuunel
```

| Symptom | Likely cause | Check / fix |
|---|---|---|
| `engine: device read: read /dev/net/tun: not pollable`, daemon exits right after start | builds before the audit fix (TUN fd registered with the poller before `TUNSETIFF`) | upgrade; fixed in `internal/tun/tun_linux.go` |
| installer: `systemd is required` | host without systemd (container, WSL1) | use the manual install / `tuunel run` under another supervisor |
| installer: `validation failed` / config contains `REPLACE_ME` | peer public key not set yet | `sudo bash install.sh --peer-key=OTHER_NODE_KEY` |
| service fails at start: `bind: permission denied` on :443 | missing `CAP_NET_BIND_SERVICE` (unit edited, or run outside systemd without setcap) | keep the unit's `AmbientCapabilities`, or `setcap cap_net_admin,cap_net_raw,cap_net_bind_service+ep /usr/local/bin/tuunel` (note `chown` clears file capabilities) |
| `bind: address already in use` on 443 | web server already uses tcp/443 (or udp/443 for QUIC) | `ss -ltnup 'sport = :443'`; change ports with `--ports=tcp=8444,quic=8444,wss=8443,udp=51900 --force-config` on **both** nodes |
| remote stays `CONNECTING`, edge shows nothing in its log | edge firewall/cloud security group | open tcp/443, udp/443, tcp/8443, udp/51900 inbound on the edge only |
| `open /dev/net/tun: permission denied` | missing `CAP_NET_ADMIN` or device | run as root / systemd unit; `modprobe tun` |
| `handshake timeout` on every carrier | wrong peer public key, firewall, wrong port | `tuunel pubkey` on the other node; `doctor` reachability; firewall on listener |
| `handshake rejected` in server debug log, `tuunel_dropped_packets_total{reason="auth_failures"}` rises | client key not in server `peers` or PSK mismatch | compare keys/PSK on both nodes |
| doctor: "connection refused" | nothing listening on that port | check `listen:` and that the daemon runs |
| doctor: "no response: packets are likely dropped…" | firewall/DPI drops the carrier | try another carrier (quic/wss/udp) |
| state `DEGRADED`, frequent switching (`tuunel_carrier_switches_total` rising) | lossy path | default `failover.degrade_holdoff: 1m` (doubles on repeats) already damps this; raise `min_hold`/`degrade_holdoff`, or prefer udp/quic-dgram. Builds before the final validation flapped ≈220×/h at 1 % loss |
| status `UP` on tcp/wss but **all** traffic stopped after a QUIC failure (fixed) | old builds: a writer blocked on the dead QUIC stream wedged the engine's sender | upgrade (QUIC close cancels both stream directions; 5 s write deadline on stream carriers). Workaround on old builds: `systemctl restart tuunel` |
| `connect failed ... carrier=quic error="dial: context deadline exceeded (no QUIC handshake: UDP blocked or path MTU < 1228/1248?)"` | UDP to the QUIC port filtered, or path MTU below QUIC's minimum | check UDP reachability (`tunnelctl doctor`); on small-MTU paths use tcp/wss/udp |
| QUIC carrier never connects on a small-MTU path | QUIC needs ≥1200-byte UDP datagrams (path MTU ≥1228 IPv4 / ≥1248 IPv6, RFC 9000) | use tcp/wss/udp on such paths; failover skips QUIC automatically |
| large UDP datagrams through the tunnel are lost (fixed in the audit) | old builds: listener assumed a 1500-byte path, and its ICMP "too big" used a local source address that the kernel dropped | upgrade; per-link path MTU detection and a non-local ICMP source fix this. The first oversize DF packet per destination is still dropped while PMTU is learned |
| status says `UP` on tcp/wss but no traffic passes for tens of seconds after the path is blocked (fixed in the audit) | old builds: health pings blocked behind a full TCP send buffer, so failure was never evaluated | upgrade; detection now takes ≈3 s regardless of load |
| ping works, large transfers stall | MTU black hole | `tunnelctl status` MTU plan; set `interface.mtu` lower or `path_mtu`; ensure ICMP "frag needed" is not filtered |
| `routing loop` in `tuunel check`/doctor | endpoint address routed into the tunnel | add a host route for the endpoint via the underlay, or narrow `routes` |
| ICMP carrier `disabled` | `experimental.icmp: false` | enable it and grant `CAP_NET_RAW` |
| ICMP carrier `permission denied` | no raw socket capability | `CAP_NET_RAW` (and `CAP_NET_ADMIN` for the reply filter) |
| listener log `icmp: kernel reply filter not installed`, or ICMP carrier gets no replies | `nft` missing or no `CAP_NET_ADMIN`: the kernel also answers every tunnel echo request (works, but doubles downstream traffic); no replies at all = ICMP filtered by the provider | install `nftables`; check `nft list table ip tuunel_icmp` on the listener (one rule matching payload `TUNQ`). With `experimental.icmp_reply_filter: off` you must drop those kernel replies yourself. Do **not** set `icmp_echo_ignore_all` (breaks ordinary ping) |
| `nft list tables` shows `ip tuunel_icmp` although tuunel is stopped | daemon was killed with SIGKILL | harmless (only tunnel requests are affected); `sudo nft delete table ip tuunel_icmp`, or it is flushed at the next start |
| forwarded port not reachable | rule listens on wrong address or limit hit | `tunnelctl forwarding` counters; `systemctl reload tuunel` after edits |
| dropped packets with reason `spoofed_source` increase | peer sends sources outside `allowed_ips` | fix `allowed_ips` or the peer's routing |
| API `permission denied` | socket `/run/tuunel/tuunel.sock` is 0660, owned by the service user | `sudo tunnelctl …` or add the user to group `tuunel` |

## Reverse mode

The remote node must reach the edge's listener outbound. The edge's forwarding
targets must be the remote node's *tunnel* address (e.g. `10.200.0.2:8080`),
and the service on the remote node must listen on that address or `0.0.0.0`.

## Collecting a report

```bash
tunnelctl -json status > status.json
tunnelctl -json doctor > doctor.json
tunnelctl metrics > metrics.txt
```

These files contain addresses and public keys but no private keys.

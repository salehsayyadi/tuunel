# Troubleshooting

Start with:

```bash
tunnelctl doctor          # kernel, permissions, config, keys, DNS, MTU, routing, reachability
tunnelctl status          # state, active carrier/endpoint, RTT, loss, jitter, traffic
tunnelctl test-carriers   # actively probe every endpoint × carrier
journalctl -u tuunel -e   # or: tunnelctl logs
```

| Symptom | Likely cause | Check / fix |
|---|---|---|
| `open /dev/net/tun: permission denied` | missing `CAP_NET_ADMIN` or device | run as root / systemd unit; `modprobe tun` |
| `handshake timeout` on every carrier | wrong peer public key, firewall, wrong port | `tuunel pubkey` on the other node; `doctor` reachability; firewall on listener |
| `handshake rejected` in server debug log, `tuunel_dropped_packets_total{reason="auth_failures"}` rises | client key not in server `peers` or PSK mismatch | compare keys/PSK on both nodes |
| doctor: "connection refused" | nothing listening on that port | check `listen:` and that the daemon runs |
| doctor: "no response: packets are likely dropped…" | firewall/DPI drops the carrier | try another carrier (quic/wss/udp) |
| state `DEGRADED`, frequent switching | lossy path | raise `min_hold`, `clear_ratio` lower, or prefer udp/quic-dgram |
| ping works, large transfers stall | MTU black hole | `tunnelctl status` MTU plan; set `interface.mtu` lower or `path_mtu`; ensure ICMP "frag needed" is not filtered |
| `routing loop` in `tuunel check`/doctor | endpoint address routed into the tunnel | add a host route for the endpoint via the underlay, or narrow `routes` |
| ICMP carrier `disabled` | `experimental.icmp: false` | enable it and grant `CAP_NET_RAW` |
| ICMP carrier `permission denied` | no raw socket capability | `CAP_NET_RAW` |
| ICMP carrier no replies | server kernel answers echo itself, or ICMP filtered | `sysctl net.ipv4.icmp_echo_ignore_all=1` on server; check provider filtering |
| forwarded port not reachable | rule listens on wrong address or limit hit | `tunnelctl forwarding` counters; `systemctl reload tuunel` after edits |
| dropped packets with reason `spoofed_source` increase | peer sends sources outside `allowed_ips` | fix `allowed_ips` or the peer's routing |
| API `permission denied` | socket is 0660 root:root | run `tunnelctl` as root or a member of the socket group |

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

# Deployment

## Installer roles (recommended)

`scripts/install.sh` (also shipped at the top of every release archive) sets up
the reverse-tunnel topology. The **edge** listens and the **remote** dials out:

| | edge (`--role=edge`, alias `iran`) | remote (`--role=remote`, alias `foreign`/`kharej`) |
|---|---|---|
| listens | tcp/443, quic udp/443, wss tcp/8443, udp/51900 | nothing |
| tunnel IP | 10.200.0.1/30 | 10.200.0.2/30 |
| firewall | allow those ports inbound | outbound only |

```bash
# 1. edge
sudo bash install.sh --role=edge                                   # note printed public key E
# 2. remote
sudo bash install.sh --role=remote --edge-address=EDGE_IP --peer-key=E   # note printed key R
# 3. edge: authorize R (fills REPLACE_ME in config, restarts)
sudo bash install.sh --peer-key=R
# 4. verify (either node)
sudo tunnelctl status ; ping -c3 10.200.0.1   # from remote
```

Re-running the installer upgrades binaries and the unit, and keeps the key and
config. `--ports=tcp=8444,quic=8444,wss=8443,udp=51900` changes ports (use the
same value on both nodes; add `--force-config` if a config already exists).
To expose a service of the remote node, add a rule on the edge such as
`forwarding: {tcp: [{listen: "0.0.0.0:2222", target: "10.200.0.2:22"}]}`
and run `sudo systemctl reload tuunel`.

The installer's generated edge and remote configs were validated with
`tuunel check` and run against each other in the network-namespace lab
(non-root, capabilities only: tunnel up, ping, all 4 carriers OK, TCP→QUIC
failover). The installer itself was executed in `--root` mode and through
an HTTPS one-line install, but **not** on a live systemd host. See
[FINAL_VALIDATION.md](FINAL_VALIDATION.md).

## Manual configuration

### Two-node direct tunnel

Node A (client, may be behind NAT) dials node B (server with public IP).

1. On both nodes:
   ```bash
   sudo bash scripts/install.sh --no-start   # binaries/unit/user only; or: make build && copy binaries
   sudo sh -c 'umask 077; tuunel genkey > /etc/tuunel/node.key'
   sudo tuunel pubkey -key /etc/tuunel/node.key
   ```
2. Node B: copy `configs/node-b.yaml` to `/etc/tuunel/config.yaml`, put node A's
   public key in `peers[0].public_key`. Open the listener ports (tcp/443,
   udp/443 for QUIC, tcp/8443 for WSS, udp/51900) in the firewall.
3. Node A: copy `configs/node-a.yaml`, put node B's public key and B's address(es)
   in `endpoints`.
4. Optional PSK (post-quantum hedge): `tuunel genpsk` once, store the same value
   in `/etc/tuunel/psk` (mode 600) on both nodes, set `security.preshared_key_file`.
5. Validate and start:
   ```bash
   sudo tuunel check -config /etc/tuunel/config.yaml
   sudo systemctl enable --now tuunel
   tunnelctl status
   ping 10.200.0.2
   ```

#### Several servers (endpoints)

Add more entries under `endpoints:`. With `failover.order: endpoint`, all
carriers of the best endpoint are tried before the next endpoint; with
`order: carrier`, the preferred carrier is tried on all endpoints first.
`endpoint_selection: latency` ranks endpoints by measured RTT.

#### Routing a subnet

Add the prefix to the dialing node's `interface.routes` and to the peer's
`allowed_ips` on the other side, and enable forwarding on the exit node:
`sysctl -w net.ipv4.ip_forward=1` (plus NAT if needed). `tuunel check` refuses
routes that would send the endpoint addresses into the tunnel.

## Reverse tunnel (expose services of a NAT'd node)

- Edge (public): `configs/reverse-edge.yaml` – listens, has one peer per remote
  node, and `forwarding:` rules such as `0.0.0.0:80 → 10.200.0.2:8080`.
- Remote (NAT'd): `configs/reverse-remote.yaml` – only dials the edge.
- Edit forwarding rules and apply without dropping the tunnel:
  `systemctl reload tuunel` (SIGHUP).

## WireGuard over tuunel

Run WireGuard *inside* the tunnel by pointing its endpoint at the peer's tunnel
address (e.g. `10.200.0.2:51820`) or by forwarding its UDP port on a reverse
edge. Lower the WireGuard MTU by 80 bytes below the tuunel TUN MTU.

## Upgrades

Replace binaries and `systemctl restart tuunel`. Both nodes must run the same
protocol version (currently 1).

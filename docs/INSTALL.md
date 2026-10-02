# Installing tuunel

Roles used by the installer (reverse-tunnel topology):

| Role | Typical host | Does | Tunnel IP | Inbound ports |
|---|---|---|---|---|
| `edge` (alias `iran`) | server with a public IP | **listens** for carriers; exposes services of the remote node with `forwarding:` | 10.200.0.1/30 | tcp/443, udp/443 (QUIC), tcp/8443 (WSS), udp/51900 |
| `remote` (alias `foreign`, `kharej`) | server with unrestricted upstream | **dials** the edge; needs no inbound port | 10.200.0.2/30 | none |

Requirements: Linux amd64/arm64 with `/dev/net/tun`, systemd (for the service),
`bash`, `curl` (downloads), `iproute2`, optionally `setcap` (libcap) and
`ssh-keygen` (OpenSSH, to verify signed releases). Intended distributions:
Ubuntu 22.04/24.04, Debian 12, RHEL/Alma/Rocky 9. See
[FINAL_VALIDATION.md](FINAL_VALIDATION.md) for what was actually executed.

## 0. Official one-line install (GitHub releases)

```bash
# EDGE (Iran)
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --role=edge
# REMOTE (foreign)
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --role=remote --edge-address=EDGE_IP --peer-key=EDGE_KEY
# EDGE again
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --peer-key=REMOTE_KEY
```

Releases are built by `.github/workflows/release.yml` whenever `VERSION`
changes on `main`. A specific release: `.../releases/download/v0.9.0/install.sh`.

## 1. One-line install (self-hosted release)

A release is the content of `dist/` produced by `scripts/release.sh`, published
at one **public HTTPS** URL per version (`$BASE/$VERSION/…`):

```bash
# maintainer, once per release
RELEASE_BASE_URL=https://dl.example.com/tuunel SIGNING_KEY=~/.ssh/tuunel_release \
  scripts/release.sh v1.0.0        # dist/: tuunel-linux-{amd64,arm64}.tar.gz, install.sh, SHA256SUMS, SHA256SUMS.sig
# upload dist/* to https://dl.example.com/tuunel/v1.0.0/
```

`release.sh` embeds the base URL, the version and the signing public key into
`dist/install.sh`. On the servers:

```bash
# EDGE
curl -fsSL --proto '=https' --tlsv1.2 https://dl.example.com/tuunel/v1.0.0/install.sh | sudo bash -s -- --role=edge
# REMOTE (EDGE_KEY is printed by the edge install)
curl -fsSL --proto '=https' --tlsv1.2 https://dl.example.com/tuunel/v1.0.0/install.sh \
  | sudo bash -s -- --role=remote --edge-address=EDGE_PUBLIC_IP --peer-key=EDGE_KEY
# EDGE again: authorize the remote key (REMOTE_KEY printed by the remote install)
curl -fsSL --proto '=https' https://dl.example.com/tuunel/v1.0.0/install.sh | sudo bash -s -- --peer-key=REMOTE_KEY
```

What the piped installer does before touching the system:

1. downloads `tuunel-linux-$ARCH.tar.gz` and `SHA256SUMS` over HTTPS only
   (`--proto =https`, bounded `--connect-timeout 15 --max-time 300`, 3 retries);
2. if the installer embeds a signer (or `--signing-key=FILE` /
   `--require-signature` is given) downloads `SHA256SUMS.sig` and verifies it
   with `ssh-keygen -Y verify -n tuunel-release`; a missing or invalid
   signature aborts;
3. verifies the archive against `SHA256SUMS`; a mismatch aborts;
4. runs the downloaded binary (`tuunel version`) before installing it.

GitHub Releases work as `RELEASE_BASE_URL=https://github.com/OWNER/REPO/releases/download`
**only for a public repository** (assets of private repositories need
authentication). **Never put a GitHub token (PAT) into a `curl | bash`
command** — it ends up in shell history, process lists and logs. For a
private repository, mirror the release files to a public HTTPS location or
copy the archive to the servers (section 2).

`curl | bash` trusts the HTTPS server for `install.sh` itself. To verify the
installer too, download it first and compare it with `SHA256SUMS` (and its
signature) from a second channel:

```bash
curl -fsSLO --proto '=https' https://dl.example.com/tuunel/v1.0.0/install.sh
curl -fsSLO --proto '=https' https://dl.example.com/tuunel/v1.0.0/SHA256SUMS
grep ' install.sh$' SHA256SUMS | sha256sum -c - && sudo bash install.sh --role=edge
```

Other installer options: `--version=V` (another release under the embedded
base URL), `--url=BASE` (explicit release directory), `--ca-file=FILE`
(private CA for an internal mirror), `--ports=tcp=443,quic=443,wss=8443,udp=51900`,
`--local-ip`, `--peer-ip`, `--no-start`, `--force-config`, `--no-systemd`,
`--uninstall [--purge]`, `--help`.

## 2. Install from an archive or a source checkout

```bash
scripts/release.sh v1.0.0                  # or download the archive
scp dist/tuunel-linux-amd64.tar.gz server:
ssh server 'tar xzf tuunel-linux-amd64.tar.gz && cd tuunel-v1.0.0 && sudo bash install.sh --role=edge'
# from a checkout with Go installed: make build && sudo bash scripts/install.sh --role=edge
```

## 3. What the installer does

| Step | Action | Bounded by |
|---|---|---|
| preflight | Linux, amd64/arm64, root, systemd (unless `--no-systemd`), `/dev/net/tun` (`modprobe tun`), OS check (warning only) | — |
| 1 binaries | `/usr/local/bin/tuunel`, `tunnelctl` (atomic rename) | curl `--max-time 300` |
| 2 user/dirs | system user `tuunel`; `/etc/tuunel` (root:tuunel 0750), `/var/lib/tuunel` | — |
| 3 key | `/etc/tuunel/node.key` (tuunel 0600) generated once; `node.pub` | — |
| 4 config | role template → `/etc/tuunel/config.yaml` (0640) only if absent; `--peer-key` fills `REPLACE_ME` (backup kept) | — |
| 5 unit | `/etc/systemd/system/tuunel.service`; `daemon-reload` | `timeout 90` |
| 6 caps | `setcap` (only for runs outside systemd) | — |
| 7 validate | `tuunel check` as user `tuunel` | `timeout 30` |
| 8 service | enable, restart, wait ≤15 s for active + `tun0`, then **`tunnelctl doctor`** | `timeout 90` / `timeout 25` |

Re-running is idempotent (key and config are kept). Upgrades: run the same
command with a newer release; the running service is restarted.

## 4. systemd service

`systemd/tuunel.service`: runs as `tuunel` with only `CAP_NET_ADMIN`
(TUN, addresses, routes, ICMP reply filter via `nft`), `CAP_NET_RAW` (ICMP
carrier) and `CAP_NET_BIND_SERVICE` (ports < 1024) as ambient capabilities;
`NoNewPrivileges`, `ProtectSystem=strict`, `ReadOnlyPaths=/etc/tuunel`,
`DevicePolicy=closed` + `DeviceAllow=/dev/net/tun`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`,
`SystemCallFilter=@system-service`, `MemoryMax=512M`, `Restart=on-failure`,
`ExecStartPre=tuunel check`, `ExecReload=kill -HUP` (forwarding reload).

```bash
sudo systemctl status tuunel; sudo systemctl reload tuunel; sudo journalctl -u tuunel -f
sudo tunnelctl status; sudo tunnelctl doctor
```

## 5. Firewall

Edge (inbound): the `listen:` ports — default tcp/443, udp/443, tcp/8443,
udp/51900 — and, if used, forwarded service ports. ICMP carrier: allow ICMP
echo-request in and echo-reply out. Remote: outbound only.

```bash
# ufw
sudo ufw allow 443/tcp; sudo ufw allow 443/udp; sudo ufw allow 8443/tcp; sudo ufw allow 51900/udp
# nftables (inet filter input chain)
nft add rule inet filter input tcp dport '{443,8443}' accept
nft add rule inet filter input udp dport '{443,51900}' accept
```

Cloud security groups must allow the same ports.

## 6. Docker

The image (`Dockerfile`, target `runtime`) runs as uid 10001 with file
capabilities on the binary; it needs `--cap-add NET_ADMIN` and
`--device /dev/net/tun`, and should use host networking so that `tun0` and the
carrier ports live on the host:

```bash
docker build -t tuunel .
mkdir -p deploy/docker/edge && docker run --rm tuunel genkey > deploy/docker/edge/node.key
sudo chown 10001:10001 deploy/docker/edge/node.key && sudo chmod 0600 deploy/docker/edge/node.key
cp configs/reverse-edge.yaml deploy/docker/edge/config.yaml   # edit keys; private_key_file: /etc/tuunel/node.key
docker compose --profile edge up -d edge
docker exec tuunel-edge tunnelctl status
```

`HEALTHCHECK` runs `tunnelctl status` (fails when the daemon is unresponsive).
Compose sets `cap_drop: [ALL]` + the three capabilities, `read_only`, a tmpfs
for `/run/tuunel` and log rotation. The Docker path was **not executed** in
the validation environment (no Docker daemon); the non-root + file-capability
runtime model itself was tested (`tests/lab/nonroot.py`). Test procedure:
`scripts/test-docker.sh`.

## 7. Testing the installer under real systemd (procedure)

Not possible in the validation sandbox (no systemd PID 1). On any Docker host:

```bash
docker run -d --name tsd --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  --device /dev/net/tun -v "$PWD:/src" jrei/systemd-ubuntu:24.04
docker exec tsd bash -c 'apt-get update && apt-get install -y iproute2 libcap2-bin curl openssh-client python3 nftables iperf3 golang-go'
docker exec tsd bash -c 'cd /src && make build && scripts/test-install.sh --systemd'
docker rm -f tsd
```

or on a disposable VM: `sudo scripts/test-install.sh --systemd`.

## 8. Uninstall

```bash
sudo bash install.sh --uninstall           # stop/disable service, remove unit and binaries; keep /etc/tuunel
sudo bash install.sh --uninstall --purge   # also remove keys, config, state and the tuunel user
```

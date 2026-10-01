#!/usr/bin/env bash
# tuunel installer / upgrader / uninstaller (Linux, systemd).
#
#   sudo bash install.sh --role=edge   [--peer-key=REMOTE_PUBKEY] [options]
#   sudo bash install.sh --role=remote --edge-address=HOST [--peer-key=EDGE_PUBKEY] [options]
#   sudo bash install.sh                       # upgrade binaries/unit, keep config
#   sudo bash install.sh --uninstall [--purge]
#
# Roles (reverse-tunnel topology, see docs/deployment.md):
#   edge    public node (e.g. the Iran server). LISTENS on tcp/443, quic/443,
#           wss/8443 and udp/51900; tunnel address 10.200.0.1/30.
#   remote  node with unrestricted upstream (e.g. the foreign server). DIALS the
#           edge (no inbound port needed); tunnel address 10.200.0.2/30.
#
# Options:
#   --role=edge|remote     create /etc/tuunel/config.yaml for this role (only if absent)
#   --peer-key=KEY         public key of the other node (fills REPLACE_ME in an existing config)
#   --edge-address=HOST    remote role: IP/hostname of the edge
#   --local-ip=CIDR        tunnel address of this node (default per role)
#   --peer-ip=IP           tunnel address of the other node (default per role)
#   --ports=LIST           carrier ports, default "tcp=443,quic=443,wss=8443,udp=51900"
#   --binary-dir=DIR       use prebuilt tuunel/tunnelctl from DIR
#   --url=BASE_URL         download BASE_URL/tuunel-linux-ARCH.tar.gz (+ BASE_URL/SHA256SUMS)
#   --no-start             install/configure but do not (re)start the service
#   --force-config         regenerate config.yaml (old file kept as config.yaml.bak.TIMESTAMP)
#   --uninstall [--purge]  remove service and binaries [and /etc/tuunel, state, user]
#
# Idempotent: re-running never overwrites an existing private key or config
# (unless --force-config) and only replaces binaries and the systemd unit.
set -euo pipefail
umask 022
ROLE=""; PEER_KEY=""; EDGE_ADDR=""; LOCAL_IP=""; PEER_IP=""; PORTS="tcp=443,quic=443,wss=8443,udp=51900"
BIN_DIR=""; URL=""; START=1; FORCE=0; UNINSTALL=0; PURGE=0
ETC=/etc/tuunel; CFG=$ETC/config.yaml; KEY=$ETC/node.key; UNIT=/etc/systemd/system/tuunel.service
arg() { printf '%s' "${1#*=}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --role=*) ROLE=$(arg "$1");; --role) ROLE=$2; shift;;
    --peer-key=*) PEER_KEY=$(arg "$1");; --peer-key) PEER_KEY=$2; shift;;
    --edge-address=*) EDGE_ADDR=$(arg "$1");; --edge-address) EDGE_ADDR=$2; shift;;
    --local-ip=*) LOCAL_IP=$(arg "$1");; --peer-ip=*) PEER_IP=$(arg "$1");;
    --ports=*) PORTS=$(arg "$1");;
    --binary-dir=*) BIN_DIR=$(arg "$1");; --binary-dir) BIN_DIR=$2; shift;;
    --url=*) URL=$(arg "$1");; --url) URL=$2; shift;;
    --no-start) START=0;; --force-config) FORCE=1;;
    --uninstall) UNINSTALL=1;; --purge) PURGE=1;;
    -h|--help) sed -n '2,32p' "$0" 2>/dev/null || echo "see scripts/install.sh"; exit 0;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2;;
  esac; shift
done

say()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ------------------------------------------------------------------ preflight
[ "$(uname -s)" = Linux ] || die "Linux only"
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null && die "run as root: sudo bash $0 $*" || die "run as root"
fi
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;;
  *) die "unsupported architecture $(uname -m) (supported: amd64, arm64)";;
esac
OSNAME=unknown
if [ -r /etc/os-release ]; then . /etc/os-release; OSNAME="${PRETTY_NAME:-$ID}"
  case "${ID:-}:${VERSION_ID%%.*}" in
    ubuntu:2[2-9]|ubuntu:[3-9][0-9]|debian:1[2-9]) ;;
    *) warn "$OSNAME is not a tested distribution (tested: Ubuntu 22.04+/24.04, Debian 12+)";;
  esac
fi
command -v systemctl >/dev/null && [ -d /run/systemd/system ] || die "systemd is required (systemctl not usable on this host)"

if [ "$UNINSTALL" = 1 ]; then
  say "uninstalling tuunel"
  systemctl disable --now tuunel >/dev/null 2>&1 || true
  rm -f "$UNIT" /usr/local/bin/tuunel /usr/local/bin/tunnelctl
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    rm -rf "$ETC" /var/lib/tuunel; id tuunel >/dev/null 2>&1 && userdel tuunel 2>/dev/null || true
    say "purged configuration, keys, state and the tuunel user"
  else say "kept $ETC (keys/config); use --purge to delete"; fi
  exit 0
fi

[ -c /dev/net/tun ] || { modprobe tun 2>/dev/null || true; }
[ -c /dev/net/tun ] || die "/dev/net/tun missing: enable TUN/TAP for this VPS (provider panel) or load the 'tun' module"
case "$ROLE" in ""|edge|remote) ;; iran) ROLE=edge;; foreign|kharej) ROLE=remote;; *) die "--role must be edge or remote";; esac
[ -z "$PEER_KEY" ] || printf '%s' "$PEER_KEY" | grep -Eq '^[A-Za-z0-9+/]{43}=$' || die "--peer-key must be a 44-character base64 public key (tuunel pubkey)"
say "host: $OSNAME, arch $ARCH"

# ------------------------------------------------------------------ binaries
SRC_DIR=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || pwd)
PKG=""   # directory containing bin/, systemd/, configs/
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
if [ -n "$BIN_DIR" ]; then
  [ -x "$BIN_DIR/tuunel" ] && [ -x "$BIN_DIR/tunnelctl" ] || die "--binary-dir $BIN_DIR lacks tuunel/tunnelctl"
  mkdir -p "$tmp/bin"; cp "$BIN_DIR/tuunel" "$BIN_DIR/tunnelctl" "$tmp/bin/"
elif [ -n "$URL" ]; then
  command -v curl >/dev/null || die "curl is required for --url"
  f=tuunel-linux-$ARCH.tar.gz
  say "downloading $URL/$f"
  curl -fsSL --retry 3 "$URL/$f" -o "$tmp/$f" || die "download failed: $URL/$f"
  curl -fsSL --retry 3 "$URL/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $URL/SHA256SUMS"
  (cd "$tmp" && grep -E " \*?$f\$" SHA256SUMS | sha256sum -c --status -) || die "checksum mismatch for $f"
  tar -xzf "$tmp/$f" -C "$tmp"; PKG=$(find "$tmp" -maxdepth 2 -name install.sh -printf '%h\n' | head -1)
  [ -n "$PKG" ] && [ -x "$PKG/bin/tuunel" ] || die "release archive layout not recognised"
  mkdir -p "$tmp/bin"; cp "$PKG/bin/tuunel" "$PKG/bin/tunnelctl" "$tmp/bin/"
elif [ -x "$SRC_DIR/bin/tuunel" ] && [ -x "$SRC_DIR/bin/tunnelctl" ]; then          # extracted release archive
  PKG=$SRC_DIR; mkdir -p "$tmp/bin"; cp "$PKG/bin/tuunel" "$PKG/bin/tunnelctl" "$tmp/bin/"
elif [ -x "$SRC_DIR/../bin/tuunel" ] && [ -x "$SRC_DIR/../bin/tunnelctl" ]; then    # source checkout after make build
  PKG=$SRC_DIR/..; mkdir -p "$tmp/bin"; cp "$PKG/bin/tuunel" "$PKG/bin/tunnelctl" "$tmp/bin/"
elif command -v go >/dev/null && [ -f "$SRC_DIR/../go.mod" ]; then
  say "building from source with $(go version | awk '{print $3}')"
  PKG=$SRC_DIR/..; mkdir -p "$tmp/bin"
  (cd "$PKG" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/bin/" ./cmd/tuunel ./cmd/tunnelctl)
else die "no binaries: pass --url, --binary-dir, run from an extracted release, or 'make build' first"; fi
[ -z "$PKG" ] && { [ -f "$SRC_DIR/../systemd/tuunel.service" ] && PKG=$SRC_DIR/.. || PKG=$SRC_DIR; }
[ -f "$PKG/systemd/tuunel.service" ] || die "systemd/tuunel.service not found next to the installer"
"$tmp/bin/tuunel" version >/dev/null 2>&1 || die "downloaded/built tuunel binary does not run on this host"

say "1/8 installing binaries ($("$tmp/bin/tuunel" version 2>/dev/null | head -1))"
WAS_ACTIVE=0; systemctl is-active --quiet tuunel && WAS_ACTIVE=1
install -m 0755 "$tmp/bin/tuunel" /usr/local/bin/tuunel.new && mv -f /usr/local/bin/tuunel.new /usr/local/bin/tuunel
install -m 0755 "$tmp/bin/tunnelctl" /usr/local/bin/tunnelctl.new && mv -f /usr/local/bin/tunnelctl.new /usr/local/bin/tunnelctl

say "2/8 system user and directories"
id tuunel >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /var/lib/tuunel --shell /usr/sbin/nologin tuunel
install -d -m 0750 -o root -g tuunel "$ETC"
install -d -m 0750 -o tuunel -g tuunel /var/lib/tuunel

say "3/8 node key"
if [ ! -s "$KEY" ]; then
  (umask 077; /usr/local/bin/tuunel genkey >"$KEY.tmp" 2>/dev/null) && mv -f "$KEY.tmp" "$KEY"
  echo "    generated new key $KEY"
else echo "    keeping existing key $KEY"; fi
chown tuunel:tuunel "$KEY"; chmod 0600 "$KEY"
PUB=$(/usr/local/bin/tuunel pubkey -key "$KEY")
printf '%s\n' "$PUB" >"$ETC/node.pub"; chmod 0644 "$ETC/node.pub"

say "4/8 configuration"
port() { printf '%s' "$PORTS" | tr ',' '\n' | awk -F= -v k="$1" '$1==k{print $2}'; }
TCP_P=$(port tcp); QUIC_P=$(port quic); WSS_P=$(port wss); UDP_P=$(port udp)
gen_config() {
  local pk=${PEER_KEY:-REPLACE_ME_WITH_PEER_PUBLIC_KEY}
  if [ "$ROLE" = edge ]; then
    local me=${LOCAL_IP:-10.200.0.1/30} peer=${PEER_IP:-10.200.0.2}
    { echo "# tuunel EDGE node (listens). Generated by install.sh on $(date -u +%F). Edit and: systemctl reload tuunel"
      echo "node: {id: $(hostname -s 2>/dev/null || echo edge)-edge}"
      echo "interface: {name: tun0, addresses: [\"$me\"], mtu: auto}"
      echo "security: {private_key_file: $KEY}"
      echo "listen:"
      [ -n "$TCP_P" ]  && echo "  - {carrier: tcp,  address: \"0.0.0.0:$TCP_P\"}"
      [ -n "$QUIC_P" ] && echo "  - {carrier: quic, address: \"0.0.0.0:$QUIC_P\"}"
      [ -n "$WSS_P" ]  && echo "  - {carrier: wss,  address: \"0.0.0.0:$WSS_P\"}"
      [ -n "$UDP_P" ]  && echo "  - {carrier: udp,  address: \"0.0.0.0:$UDP_P\"}"
      echo "peers:"
      echo "  - {name: remote, public_key: \"$pk\", allowed_ips: [\"$peer/32\"]}"
      echo "forwarding:            # expose services of the remote node, e.g."
      echo "  tcp: []              # - {listen: \"0.0.0.0:2222\", target: \"$peer:22\"}"
      echo "  udp: []"
      echo "api: {socket: /run/tuunel/tuunel.sock}"
      echo "log: {level: info}"; } 
  else
    [ -n "$EDGE_ADDR" ] || die "--role=remote needs --edge-address=EDGE_IP_OR_HOSTNAME"
    local me=${LOCAL_IP:-10.200.0.2/30} peer=${PEER_IP:-10.200.0.1}
    { echo "# tuunel REMOTE node (dials the edge). Generated by install.sh on $(date -u +%F)."
      echo "node: {id: $(hostname -s 2>/dev/null || echo remote)-remote}"
      echo "interface: {name: tun0, addresses: [\"$me\"], mtu: auto}"
      echo "security: {private_key_file: $KEY}"
      echo "peers:"
      echo "  - name: edge"
      echo "    public_key: \"$pk\""
      echo "    allowed_ips: [\"$peer/32\"]"
      echo "    endpoints: [{name: edge, address: $EDGE_ADDR}]"
      echo "    carriers:            # preference order; failover walks this list"
      [ -n "$TCP_P" ]  && echo "      - {type: tcp,  port: $TCP_P}"
      [ -n "$QUIC_P" ] && echo "      - {type: quic, port: $QUIC_P}"
      [ -n "$WSS_P" ]  && echo "      - {type: wss,  port: $WSS_P}"
      [ -n "$UDP_P" ]  && echo "      - {type: udp,  port: $UDP_P}"
      echo "api: {socket: /run/tuunel/tuunel.sock}"
      echo "log: {level: info}"; }
  fi
}
if [ -f "$CFG" ] && [ "$FORCE" = 0 ]; then
  echo "    keeping existing $CFG"
  if [ -n "$PEER_KEY" ] && grep -q 'REPLACE_ME' "$CFG"; then
    cp -p "$CFG" "$CFG.bak.$(date +%Y%m%d%H%M%S)"
    sed -i -E "s#REPLACE_ME[A-Za-z0-9_]*#$PEER_KEY#" "$CFG"; echo "    filled peer public key into $CFG"
  fi
elif [ -n "$ROLE" ]; then
  [ -f "$CFG" ] && cp -p "$CFG" "$CFG.bak.$(date +%Y%m%d%H%M%S)"
  gen_config >"$CFG.tmp"; install -m 0640 -o root -g tuunel "$CFG.tmp" "$CFG"; rm -f "$CFG.tmp"
  echo "    wrote $CFG for role $ROLE"
else
  echo "    no $CFG and no --role given: installing binaries/unit only"
fi
[ -f "$CFG" ] && { chown root:tuunel "$CFG"; chmod 0640 "$CFG"; }

say "5/8 systemd unit"
install -m 0644 "$PKG/systemd/tuunel.service" "$UNIT"
systemctl daemon-reload

say "6/8 capabilities"
if command -v setcap >/dev/null; then setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service+ep' /usr/local/bin/tuunel || warn "setcap failed (systemd AmbientCapabilities still apply)"
else echo "    setcap not installed (only needed to run tuunel outside systemd)"; fi

say "7/8 validating configuration"
VALID=0
if [ -f "$CFG" ]; then
  if out=$(runuser -u tuunel -- /usr/local/bin/tuunel check -config "$CFG" 2>&1); then VALID=1; echo "    $out"
  else echo "$out" | sed 's/^/    /'; warn "configuration is not valid yet; the service will not be started"; fi
fi

say "8/8 service"
systemctl enable tuunel >/dev/null 2>&1 || true
if [ "$VALID" = 1 ] && [ "$START" = 1 ]; then
  systemctl restart tuunel
  for _ in $(seq 1 50); do systemctl is-active --quiet tuunel && ip link show tun0 >/dev/null 2>&1 && break; sleep 0.2; done
  if systemctl is-active --quiet tuunel; then
    echo "    service active; $(ip -brief addr show tun0 2>/dev/null || echo 'tun0 not present yet')"
    tunnelctl status 2>/dev/null | head -12 || true
  else systemctl --no-pager status tuunel | tail -15; die "service failed to start (journalctl -u tuunel -n 50)"; fi
elif [ "$WAS_ACTIVE" = 1 ] && [ "$VALID" = 1 ]; then systemctl restart tuunel
else echo "    not started"; fi

cat <<MSG

tuunel installed.  role: ${ROLE:-unchanged}   config: $CFG
This node's public key (give it to the other node):
    $PUB
Next steps:
MSG
if grep -qs REPLACE_ME "$CFG"; then
  echo "  - on the other node run: tuunel pubkey -key $KEY   (or cat $ETC/node.pub)"
  echo "  - then here:  sudo bash $0 --peer-key=<OTHER_NODE_PUBLIC_KEY>"
fi
cat <<MSG
  status:       tunnelctl status          diagnostics:  tunnelctl doctor
  logs:         journalctl -u tuunel -f   reload cfg:   systemctl reload tuunel
  uninstall:    sudo bash $0 --uninstall [--purge]
MSG

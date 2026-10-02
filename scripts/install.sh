#!/usr/bin/env bash
# tuunel installer / upgrader / uninstaller (Linux, systemd).  Run with --help.
#
# One-line install from a published release (HTTPS, SHA256 + optional signature):
#   curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --role=edge
# From an extracted release archive or a source checkout:
#   sudo bash install.sh --role=edge
set -euo pipefail
umask 022
# Filled in by scripts/release.sh (left as placeholders in the source tree).
EMBED_BASE_URL="@TUUNEL_BASE_URL@"      # e.g. https://github.com/OWNER/REPO/releases/download
EMBED_VERSION="@TUUNEL_VERSION@"        # release this installer belongs to
EMBED_SIGNER="@TUUNEL_SIGNER@"          # ssh public key that signs SHA256SUMS (optional)
# Official public releases (used when this script is piped from the repository
# without --url and no local binaries exist): latest GitHub release assets.
DEFAULT_RELEASE_URL="https://github.com/salehsayyadi/tuunel/releases/latest/download"
embedded() { case "$1" in @*@|"") return 1;; *) return 0;; esac; }

usage() { cat <<'EOF'
tuunel installer / upgrader / uninstaller

  sudo bash install.sh --role=edge   [--peer-key=REMOTE_PUBKEY] [options]
  sudo bash install.sh --role=remote --edge-address=HOST [--peer-key=EDGE_PUBKEY] [options]
  sudo bash install.sh                        upgrade binaries/unit, keep config
  sudo bash install.sh --uninstall [--purge]

Roles (reverse-tunnel topology, see docs/INSTALL.md):
  edge    public node. LISTENS on tcp/443, quic/443, wss/8443, udp/51900;
          tunnel address 10.200.0.1/30.
  remote  node with unrestricted upstream. DIALS the edge (no inbound port
          needed); tunnel address 10.200.0.2/30.

Options:
  --role=edge|remote     create /etc/tuunel/config.yaml for this role (only if absent)
  --peer-key=KEY         public key of the other node (fills REPLACE_ME in an existing config)
  --edge-address=HOST    remote role: IP/hostname of the edge
  --local-ip=CIDR        tunnel address of this node (default per role)
  --peer-ip=IP           tunnel address of the other node (default per role)
  --ports=LIST           carrier ports, default "tcp=443,quic=443,wss=8443,udp=51900"
  --binary-dir=DIR       use prebuilt tuunel/tunnelctl from DIR
  --url=BASE_URL         download BASE_URL/tuunel-linux-ARCH.tar.gz + BASE_URL/SHA256SUMS
  --version=VERSION      with an embedded release base URL: install that release
  --signing-key=FILE     ssh public key; require a valid SHA256SUMS.sig made with it
  --require-signature    fail if SHA256SUMS.sig is missing or invalid
  --ca-file=FILE         extra CA bundle for HTTPS downloads (private mirrors)
  --no-start             install/configure but do not (re)start the service
  --no-systemd           do not use systemd (containers/image builds); prints run command
  --root=DIR             install below DIR (implies --no-systemd; used by tests)
  --force-config         regenerate config.yaml (old file kept as config.yaml.bak.TIMESTAMP)
  --uninstall [--purge]  remove service and binaries [and /etc/tuunel, state, user]

Downloads are HTTPS-only (curl --proto =https, bounded --max-time) and are
verified against SHA256SUMS before anything is installed. Never put a GitHub
token in a curl | bash command line; publish releases at a public HTTPS URL.

Idempotent: re-running never overwrites an existing private key or config
(unless --force-config) and only replaces binaries and the systemd unit.
EOF
}
ROLE=""; PEER_KEY=""; EDGE_ADDR=""; LOCAL_IP=""; PEER_IP=""; PORTS="tcp=443,quic=443,wss=8443,udp=51900"
BIN_DIR=""; URL=""; START=1; FORCE=0; UNINSTALL=0; PURGE=0
REL_VERSION=""; SIGNING_KEY=""; REQUIRE_SIG=0; CA_FILE=""; SYSTEMD=1; ROOT=""
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
    --version=*) REL_VERSION=$(arg "$1");; --version) REL_VERSION=$2; shift;;
    --signing-key=*) SIGNING_KEY=$(arg "$1");; --require-signature) REQUIRE_SIG=1;;
    --ca-file=*) CA_FILE=$(arg "$1");;
    --root=*) ROOT=$(arg "$1"); SYSTEMD=0;; --no-systemd) SYSTEMD=0;;
    --no-start) START=0;; --force-config) FORCE=1;;
    --uninstall) UNINSTALL=1;; --purge) PURGE=1;;
    -h|--help) usage; exit 0;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2;;
  esac; shift
done

ROOT=${ROOT%/}
ETC=$ROOT/etc/tuunel; CFG=$ETC/config.yaml; KEY=$ETC/node.key; UNIT=$ROOT/etc/systemd/system/tuunel.service
BINDIR=$ROOT/usr/local/bin; STATE=$ROOT/var/lib/tuunel; API_SOCK=$ROOT/run/tuunel/tuunel.sock
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
if [ -r /etc/os-release ]; then
  # read only what is needed (sourcing os-release would clobber variables such as VERSION)
  ID=$(. /etc/os-release && printf '%s' "${ID:-}"); VERSION_ID=$(. /etc/os-release && printf '%s' "${VERSION_ID:-}")
  OSNAME=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-${ID:-unknown}}")
  case "${ID:-}:${VERSION_ID%%.*}" in
    ubuntu:2[2-9]|ubuntu:[3-9][0-9]|debian:1[2-9]) ;;
    *) warn "$OSNAME is not a tested distribution (tested: Ubuntu 22.04+/24.04, Debian 12+)";;
  esac
fi
if [ "$SYSTEMD" = 1 ]; then
  command -v systemctl >/dev/null && [ -d /run/systemd/system ] || die "systemd is required (systemctl not usable on this host); use --no-systemd for containers"
fi
sc() { [ "$SYSTEMD" = 1 ] || return 0; timeout 90 systemctl "$@"; }   # bounded systemctl

if [ "$UNINSTALL" = 1 ]; then
  say "uninstalling tuunel"
  sc disable --now tuunel >/dev/null 2>&1 || true
  rm -f "$UNIT" "$BINDIR/tuunel" "$BINDIR/tunnelctl"
  sc daemon-reload || true
  if [ "$PURGE" = 1 ]; then
    rm -rf "$ETC" "$STATE"
    [ -z "$ROOT" ] && id tuunel >/dev/null 2>&1 && { userdel tuunel 2>/dev/null || true; }
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
[ -f "${BASH_SOURCE[0]:-}" ] || SRC_DIR=/nonexistent        # piped: curl ... | bash
embedded "$EMBED_SIGNER" && [ -z "$SIGNING_KEY" ] && REQUIRE_SIG=1   # signed release: always verify
embedded "$EMBED_SIGNER" || EMBED_SIGNER=""
if [ -z "$URL" ] && [ -z "$BIN_DIR" ] && embedded "$EMBED_BASE_URL"; then
  if [ -n "$REL_VERSION" ] || [ ! -x "$SRC_DIR/bin/tuunel" ]; then
    v=${REL_VERSION:-$EMBED_VERSION}; embedded "$v" || die "no release version known; pass --version=VERSION"
    URL=$EMBED_BASE_URL/$v
  fi
fi
if [ -z "$URL" ] && [ -z "$BIN_DIR" ] && [ "$UNINSTALL" = 0 ] && [ ! -x "$SRC_DIR/bin/tuunel" ] && [ ! -x "$SRC_DIR/../bin/tuunel" ] \
   && ! { command -v go >/dev/null && [ -f "$SRC_DIR/../go.mod" ]; }; then
  if [ -n "$REL_VERSION" ]; then URL=${DEFAULT_RELEASE_URL%/latest/download}/download/$REL_VERSION; else URL=$DEFAULT_RELEASE_URL; fi
fi
[ -n "$REL_VERSION" ] && [ -z "$URL" ] && die "--version needs an installer with an embedded release URL (or use --url)"
PKG=""   # directory containing bin/, systemd/, configs/
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
if [ -n "$BIN_DIR" ]; then
  [ -x "$BIN_DIR/tuunel" ] && [ -x "$BIN_DIR/tunnelctl" ] || die "--binary-dir $BIN_DIR lacks tuunel/tunnelctl"
  mkdir -p "$tmp/bin"; cp "$BIN_DIR/tuunel" "$BIN_DIR/tunnelctl" "$tmp/bin/"
elif [ -n "$URL" ]; then
  command -v curl >/dev/null || die "curl is required for downloads"
  case "$URL" in https://*) ;; *) die "--url must be an https:// URL";; esac
  CURL=(curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 300)
  [ -n "$CA_FILE" ] && CURL+=(--cacert "$CA_FILE")
  f=tuunel-linux-$ARCH.tar.gz
  say "downloading $URL/$f"
  "${CURL[@]}" "$URL/$f" -o "$tmp/$f" || die "download failed: $URL/$f"
  "${CURL[@]}" "$URL/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $URL/SHA256SUMS"
  if [ -n "$SIGNING_KEY" ] || [ "$REQUIRE_SIG" = 1 ]; then
    command -v ssh-keygen >/dev/null || die "ssh-keygen (openssh-client) is required to verify the release signature"
    if [ -n "$SIGNING_KEY" ]; then signer=$(cat "$SIGNING_KEY") || die "cannot read $SIGNING_KEY"; else signer=$EMBED_SIGNER; fi
    [ -n "$signer" ] || die "--require-signature: no signing key known (pass --signing-key=FILE)"
    "${CURL[@]}" "$URL/SHA256SUMS.sig" -o "$tmp/SHA256SUMS.sig" || die "signature SHA256SUMS.sig not available at $URL"
    printf 'tuunel-release %s\n' "$signer" >"$tmp/allowed_signers"
    ssh-keygen -Y verify -f "$tmp/allowed_signers" -I tuunel-release -n tuunel-release \
      -s "$tmp/SHA256SUMS.sig" <"$tmp/SHA256SUMS" >/dev/null 2>&1 || die "SHA256SUMS signature verification FAILED"
    echo "    SHA256SUMS signature verified"
  fi
  (cd "$tmp" && grep -E " \*?$f\$" SHA256SUMS | sha256sum -c --status -) || die "checksum mismatch for $f"
  echo "    sha256 verified: $(grep -E " \*?$f\$" "$tmp/SHA256SUMS" | cut -c1-16)..."
  tar -xzf "$tmp/$f" -C "$tmp" --no-same-owner; PKG=$(find "$tmp" -maxdepth 2 -name install.sh -printf '%h\n' | head -1)
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
[ -f "$PKG/systemd/tuunel.service" ] || die "systemd/tuunel.service not found next to the installer (piped installs need --url or an embedded release URL)"
"$tmp/bin/tuunel" version >/dev/null 2>&1 || die "downloaded/built tuunel binary does not run on this host"

say "1/8 installing binaries ($("$tmp/bin/tuunel" version 2>/dev/null | head -1))"
WAS_ACTIVE=0; sc is-active --quiet tuunel && WAS_ACTIVE=1
install -d -m 0755 "$BINDIR"
install -m 0755 "$tmp/bin/tuunel" "$BINDIR/tuunel.new" && mv -f "$BINDIR/tuunel.new" "$BINDIR/tuunel"
install -m 0755 "$tmp/bin/tunnelctl" "$BINDIR/tunnelctl.new" && mv -f "$BINDIR/tunnelctl.new" "$BINDIR/tunnelctl"

say "2/8 system user and directories"
if [ -z "$ROOT" ]; then
  id tuunel >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /var/lib/tuunel --shell /usr/sbin/nologin tuunel
  OWN=tuunel
else OWN=$(id -un); fi   # --root: files owned by the invoking user, no system user
install -d -m 0750 -o root -g "$OWN" "$ETC"
install -d -m 0750 -o "$OWN" -g "$OWN" "$STATE"
[ -n "$ROOT" ] && install -d -m 0750 "$ROOT/run/tuunel"

say "3/8 node key"
if [ ! -s "$KEY" ]; then
  (umask 077; "$BINDIR/tuunel" genkey >"$KEY.tmp" 2>/dev/null) && mv -f "$KEY.tmp" "$KEY"
  echo "    generated new key $KEY"
else echo "    keeping existing key $KEY"; fi
chown "$OWN:$OWN" "$KEY"; chmod 0600 "$KEY"
PUB=$("$BINDIR/tuunel" pubkey -key "$KEY")
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
      echo "api: {socket: $API_SOCK}"
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
      echo "api: {socket: $API_SOCK}"
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
  gen_config >"$CFG.tmp"; install -m 0640 -o root -g "$OWN" "$CFG.tmp" "$CFG"; rm -f "$CFG.tmp"
  echo "    wrote $CFG for role $ROLE"
else
  echo "    no $CFG and no --role given: installing binaries/unit only"
fi
[ -f "$CFG" ] && { chown "root:$OWN" "$CFG"; chmod 0640 "$CFG"; }

say "5/8 systemd unit"
install -d -m 0755 "$(dirname "$UNIT")"
install -m 0644 "$PKG/systemd/tuunel.service" "$UNIT"
sc daemon-reload

say "6/8 capabilities"
if [ -n "$ROOT" ]; then echo "    skipped (--root)"
elif command -v setcap >/dev/null; then setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service+ep' "$BINDIR/tuunel" || warn "setcap failed (systemd AmbientCapabilities still apply)"
else echo "    setcap not installed (only needed to run tuunel outside systemd)"; fi

say "7/8 validating configuration"
VALID=0
if [ -f "$CFG" ]; then
  if [ -z "$ROOT" ]; then chk=(timeout 30 runuser -u tuunel -- "$BINDIR/tuunel" check -config "$CFG")
  else chk=(timeout 30 "$BINDIR/tuunel" check -config "$CFG"); fi
  if out=$("${chk[@]}" 2>&1); then VALID=1; echo "    $out"
  else echo "$out" | sed 's/^/    /'; warn "configuration is not valid yet; the service will not be started"; fi
fi

say "8/8 service"
DOCTOR=""
if [ "$SYSTEMD" = 0 ]; then
  echo "    systemd not used; start with:  $BINDIR/tuunel run -config $CFG"
else
  sc enable tuunel >/dev/null 2>&1 || true
  if [ "$VALID" = 1 ] && { [ "$START" = 1 ] || [ "$WAS_ACTIVE" = 1 ]; }; then
    sc restart tuunel || warn "systemctl restart timed out or failed"
    for _ in $(seq 1 75); do sc is-active --quiet tuunel && ip link show tun0 >/dev/null 2>&1 && break; sleep 0.2; done
    if sc is-active --quiet tuunel; then
      echo "    service active; $(ip -brief addr show tun0 2>/dev/null || echo 'tun0 not present yet')"
      sleep 2   # give the first carrier a moment before diagnostics
      echo "    --- tunnelctl doctor ---"
      DOCTOR=$(timeout 25 "$BINDIR/tunnelctl" doctor 2>&1) || true
      printf '%s\n' "$DOCTOR" | sed 's/^/    /' | head -40
    else
      timeout 10 systemctl --no-pager status tuunel | tail -15 || true
      timeout 10 journalctl -u tuunel -n 30 --no-pager 2>/dev/null || true
      die "service failed to start (journalctl -u tuunel -n 50)"
    fi
  else echo "    not started"; fi
fi

cat <<MSG

tuunel installed.  role: ${ROLE:-unchanged}   config: $CFG
This node's public key (give it to the other node):
    $PUB
Next steps:
MSG
if grep -qs REPLACE_ME "$CFG"; then
  echo "  - on the other node run: tuunel pubkey -key $KEY   (or cat $ETC/node.pub)"
  echo "  - then here:  sudo bash install.sh --peer-key=<OTHER_NODE_PUBLIC_KEY>   (or re-run the one-line command with --peer-key=...)"
fi
cat <<MSG
  status:       tunnelctl status          diagnostics:  tunnelctl doctor
  logs:         journalctl -u tuunel -f   reload cfg:   systemctl reload tuunel
  uninstall:    sudo bash install.sh --uninstall [--purge]
  firewall:     edge must accept the listen ports (default tcp/443, udp/443, tcp/8443, udp/51900)
MSG

#!/usr/bin/env bash
# tuunel installer for Ubuntu 22.04+ / Debian 12+ (amd64).
#
#   sudo ./scripts/install.sh [--binary-dir DIR] [--url BASE_URL] [--no-start]
#
# Binaries are taken from --binary-dir (default ./bin), built with Go if
# available, or downloaded from --url BASE_URL/{tuunel,tunnelctl} (with
# BASE_URL/SHA256SUMS verification). No URL, key or address is hard-coded.
set -euo pipefail
BIN_DIR=./bin; URL=""; START=1
while [ $# -gt 0 ]; do case "$1" in
  --binary-dir) BIN_DIR=$2; shift 2;; --url) URL=$2; shift 2;; --no-start) START=0; shift;;
  -h|--help) sed -n 2,9p "$0"; exit 0;; *) echo "unknown option $1"; exit 2;; esac; done

die() { echo "ERROR: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
[ "$(uname -s)" = Linux ] || die "Linux only"
case "$(uname -m)" in x86_64|amd64) ;; *) echo "WARN: $(uname -m) is not a tested architecture";; esac
if [ -r /etc/os-release ]; then . /etc/os-release
  case "$ID:${VERSION_ID%%.*}" in ubuntu:2[2-9]|ubuntu:[3-9]*|debian:1[2-9]) ;; *) echo "WARN: $PRETTY_NAME is not a tested distribution";; esac
fi
command -v systemctl >/dev/null || die "systemd is required"
[ -c /dev/net/tun ] || { modprobe tun 2>/dev/null || true; [ -c /dev/net/tun ] || die "/dev/net/tun missing: enable TUN on this host/VPS"; }

echo "==> 1/9 installing binaries"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
if [ -x "$BIN_DIR/tuunel" ] && [ -x "$BIN_DIR/tunnelctl" ]; then cp "$BIN_DIR/tuunel" "$BIN_DIR/tunnelctl" "$tmp/"
elif [ -n "$URL" ]; then
  for f in tuunel tunnelctl SHA256SUMS; do curl -fsSL "$URL/$f" -o "$tmp/$f"; done
  (cd "$tmp" && grep -E ' (tuunel|tunnelctl)$' SHA256SUMS | sha256sum -c -) || die "checksum mismatch"
elif command -v go >/dev/null && [ -f go.mod ]; then
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/" ./cmd/tuunel ./cmd/tunnelctl
else die "no binaries: build with 'make build', pass --binary-dir, or --url"; fi
install -m 0755 "$tmp/tuunel" /usr/local/bin/tuunel
install -m 0755 "$tmp/tunnelctl" /usr/local/bin/tunnelctl

echo "==> 2/9 system user"
id tuunel >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /var/lib/tuunel --shell /usr/sbin/nologin tuunel

echo "==> 3/9 configuration directory"
install -d -m 0750 -o root -g tuunel /etc/tuunel

echo "==> 4/9 state/runtime directories"
install -d -m 0750 -o tuunel -g tuunel /var/lib/tuunel

if [ ! -f /etc/tuunel/node.key ]; then
  /usr/local/bin/tuunel genkey >/etc/tuunel/node.key 2>/etc/tuunel/node.pub
  sed -i 's/^public key: //' /etc/tuunel/node.pub
fi
chown tuunel:tuunel /etc/tuunel/node.key; chmod 0600 /etc/tuunel/node.key
echo "    node public key: $(/usr/local/bin/tuunel pubkey -key /etc/tuunel/node.key)"
NEWCFG=0
if [ ! -f /etc/tuunel/config.yaml ]; then
  src=$(dirname "$0")/../configs/node-a.yaml
  [ -f "$src" ] && install -m 0640 -o root -g tuunel "$src" /etc/tuunel/config.yaml && NEWCFG=1
fi

echo "==> 5/9 systemd service"
install -m 0644 "$(dirname "$0")/../systemd/tuunel.service" /etc/systemd/system/tuunel.service
systemctl daemon-reload

echo "==> 6/9 capabilities (for running outside systemd; the unit grants its own)"
if command -v setcap >/dev/null; then setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service+ep' /usr/local/bin/tuunel || true
else echo "    setcap not found (apt install libcap2-bin); systemd AmbientCapabilities still apply"; fi

echo "==> 7/9 validating configuration"
if [ "$NEWCFG" = 1 ]; then
  echo "    an EXAMPLE configuration was installed at /etc/tuunel/config.yaml."
  echo "    edit node id, addresses, peer public keys and endpoints, then run:"
  echo "      tuunel check -config /etc/tuunel/config.yaml && systemctl enable --now tuunel"
  exit 0
fi
sudo -u tuunel /usr/local/bin/tuunel check -config /etc/tuunel/config.yaml || die "configuration invalid; service not started"

if [ "$START" = 1 ]; then
  echo "==> 8/9 starting service"
  systemctl enable tuunel >/dev/null
  systemctl restart tuunel
  sleep 3
  echo "==> 9/9 status"
  systemctl --no-pager status tuunel | head -15 || true
  tunnelctl status || true
else echo "==> 8-9/9 skipped (--no-start)"; fi

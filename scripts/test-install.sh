#!/usr/bin/env bash
# Installer / release / one-line-install verification. Bounded (timeouts).
#
#   sudo scripts/test-install.sh [--systemd]
#
# Always (no systemd needed, nothing outside a temp dir is modified):
#   1 static: bash -n (and shellcheck if installed) for all scripts
#   2 installer --root mode: edge + remote, peer-key fill, idempotent re-run,
#     --force-config backup, invalid input rejection, --uninstall [--purge]
#   3 generated configs actually work: tests/lab/install_check.py runs them
#     in network namespaces (tunnel up, traffic, doctor, failover)
#   4 release: scripts/release.sh twice -> identical SHA256SUMS (reproducible),
#     amd64 + arm64 archives, version/commit embedded, SSH-signed SHA256SUMS
#   5 one-line install over real HTTPS (local TLS server with a test CA):
#     curl -fsSL --proto =https URL/install.sh | bash -s -- --role=edge ...
#     plus negative tests: tampered archive, tampered SHA256SUMS (bad
#     signature), unsigned release when a signature is required, http:// URL
# --systemd (only on a host/container booted with systemd, e.g. the
#   docker procedure in docs/INSTALL.md): real install, service start,
#   restart, reload, doctor, uninstall.
set -uo pipefail
cd "$(dirname "$0")/.."
ROOTDIR=$(pwd)
BIN=${BIN:-$ROOTDIR/bin}
W=$(mktemp -d /tmp/tuunel-install-test.XXXXXX)
PASS=0; FAIL=0; RES=()
ok()  { PASS=$((PASS+1)); RES+=("PASS $*"); echo "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); RES+=("FAIL $*"); echo "FAIL: $*"; }
t() { timeout "${TMO:-120}" "$@"; }
cleanup() { [ -n "${HTTPS_PID:-}" ] && kill "$HTTPS_PID" 2>/dev/null; rm -rf "$W"; }
trap cleanup EXIT
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)"; exit 2; }
[ -x "$BIN/tuunel" ] || { echo "build first: make build"; exit 2; }

echo "== 1 static checks"
for f in scripts/*.sh tests/lab/*.sh; do bash -n "$f" || { bad "bash -n $f"; continue; }; done; ok "bash -n all scripts"
if command -v shellcheck >/dev/null; then shellcheck -S warning scripts/*.sh && ok shellcheck || bad shellcheck
else echo "SKIP: shellcheck not installed"; fi
bash scripts/install.sh --help | grep -q -- "--role=edge" && ok "--help works" || bad "--help"
bash scripts/install.sh < /dev/null --help >/dev/null 2>&1 && ok "--help works when piped" || bad "--help piped"
bash scripts/install.sh --role=bogus --root="$W/x" --binary-dir="$BIN" >/dev/null 2>&1 && bad "accepts bad role" || ok "rejects invalid --role"
bash scripts/install.sh --peer-key=notakey --root="$W/x" --binary-dir="$BIN" >/dev/null 2>&1 && bad "accepts bad key" || ok "rejects invalid --peer-key"
bash scripts/install.sh --role=remote --root="$W/x" --binary-dir="$BIN" >/dev/null 2>&1 && bad "remote without edge address" || ok "remote requires --edge-address"
bash scripts/install.sh --url=http://example.invalid --root="$W/x" >/dev/null 2>&1 && bad "accepts http url" || ok "rejects non-HTTPS --url"

echo "== 2 installer --root mode"
E=$W/edge; R=$W/remote
t bash scripts/install.sh --root="$E" --role=edge --binary-dir="$BIN" >"$W/edge1.log" 2>&1 && ok "edge install" || { bad "edge install"; tail -20 "$W/edge1.log"; }
EPUB=$(cat "$E/etc/tuunel/node.pub")
t bash scripts/install.sh --root="$R" --role=remote --edge-address=192.0.2.2 --peer-key="$EPUB" --binary-dir="$BIN" >"$W/remote1.log" 2>&1 && ok "remote install" || { bad "remote install"; tail -20 "$W/remote1.log"; }
RPUB=$(cat "$R/etc/tuunel/node.pub")
grep -q REPLACE_ME "$E/etc/tuunel/config.yaml" && ok "edge config waits for peer key" || bad "edge placeholder"
t bash scripts/install.sh --root="$E" --peer-key="$RPUB" --binary-dir="$BIN" >"$W/edge2.log" 2>&1 && ok "edge peer-key fill" || bad "edge peer-key fill"
grep -q "$RPUB" "$E/etc/tuunel/config.yaml" && ! grep -q REPLACE_ME "$E/etc/tuunel/config.yaml" && ok "peer key written" || bad "peer key not written"
for n in E R; do d=${!n}; "$BIN/tuunel" check -config "$d/etc/tuunel/config.yaml" >/dev/null 2>&1 && ok "generated config valid ($n)" || bad "config invalid ($n)"; done
[ "$(stat -c %a "$E/etc/tuunel/node.key")" = 600 ] && ok "key mode 0600" || bad "key mode"
k1=$(sha256sum <"$E/etc/tuunel/node.key"); c1=$(sha256sum <"$E/etc/tuunel/config.yaml")
t bash scripts/install.sh --root="$E" --binary-dir="$BIN" >"$W/edge3.log" 2>&1
[ -s "$E/etc/tuunel/node.key" ] && [ "$k1" = "$(sha256sum <"$E/etc/tuunel/node.key")" ] && [ "$c1" = "$(sha256sum <"$E/etc/tuunel/config.yaml")" ] && ok "re-run is idempotent (key + config kept)" || bad "re-run changed key/config"
cp -a "$E" "$W/edge-copy"
t bash scripts/install.sh --root="$W/edge-copy" --role=edge --force-config --binary-dir="$BIN" >/dev/null 2>&1
ls "$W/edge-copy/etc/tuunel/" | grep -q 'config.yaml.bak.' && ok "--force-config keeps a backup" || bad "--force-config backup"
grep -q 'carrier: tcp' "$E/etc/tuunel/config.yaml" && grep -q 'type: quic' "$R/etc/tuunel/config.yaml" && ok "role configs contain carriers" || bad "role carriers"

echo "== 3 generated configs in the netns lab"
if t python3 tests/lab/install_check.py "$E" "$R" "$W/install_check.json" >"$W/ic.log" 2>&1; then ok "installer-generated edge/remote tunnel works (up, traffic, doctor, failover)"
else bad "installer-generated configs in lab"; tail -30 "$W/ic.log"; fi
cp "$W/install_check.json" "${RESULTS:-/tmp}/install_check.json" 2>/dev/null

echo "== 4 reproducible, signed release"
ssh-keygen -q -t ed25519 -N '' -C tuunel-test-release -f "$W/sign" >/dev/null
export RELEASE_BASE_URL=https://127.0.0.1:18443/rel SIGNING_KEY=$W/sign
TMO=300 t scripts/release.sh v0.0.0-test "$W/dist1" >"$W/rel1.log" 2>&1 && ok "release build 1" || { bad "release build"; tail -20 "$W/rel1.log"; }
TMO=300 t scripts/release.sh v0.0.0-test "$W/dist2" >"$W/rel2.log" 2>&1
cmp -s "$W/dist1/SHA256SUMS" "$W/dist2/SHA256SUMS" && ok "reproducible: identical SHA256SUMS on rebuild" || { bad "not reproducible"; diff "$W/dist1/SHA256SUMS" "$W/dist2/SHA256SUMS"; }
for a in amd64 arm64; do
  tar -xzf "$W/dist1/tuunel-linux-$a.tar.gz" -C "$W" && file "$W/tuunel-v0.0.0-test/bin/tuunel" | grep -q "$( [ $a = amd64 ] && echo x86-64 || echo aarch64 )" && ok "$a archive contains $a binary" || bad "$a archive"
  [ $a = amd64 ] && { "$W/tuunel-v0.0.0-test/bin/tuunel" version | grep -q "v0.0.0-test" && "$W/tuunel-v0.0.0-test/bin/tuunel" version | grep -q "commit:" && ok "version+commit embedded" || bad "version info"; }
  rm -rf "$W/tuunel-v0.0.0-test"
done
grep -q 'EMBED_BASE_URL="https://127.0.0.1:18443/rel"' "$W/dist1/install.sh" && grep -q 'EMBED_VERSION="v0.0.0-test"' "$W/dist1/install.sh" && grep -q 'EMBED_SIGNER="ssh-ed25519 ' "$W/dist1/install.sh" && ok "installer has embedded URL/version/signer" || bad "embedding"
printf 'tuunel-release %s\n' "$(cat "$W/sign.pub")" >"$W/allowed"
ssh-keygen -Y verify -f "$W/allowed" -I tuunel-release -n tuunel-release -s "$W/dist1/SHA256SUMS.sig" <"$W/dist1/SHA256SUMS" >/dev/null 2>&1 && ok "SHA256SUMS signature verifies" || bad "signature"
unset RELEASE_BASE_URL SIGNING_KEY

echo "== 5 one-line install over HTTPS"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 \
  -keyout "$W/tls.key" -out "$W/tls.crt" >/dev/null 2>&1
mkdir -p "$W/www/rel/v0.0.0-test" && cp "$W/dist1/"* "$W/www/rel/v0.0.0-test/"
( cd "$W/www" && exec python3 -c '
import http.server, ssl, sys
s = http.server.ThreadingHTTPServer(("127.0.0.1", 18443), http.server.SimpleHTTPRequestHandler)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain(sys.argv[1], sys.argv[2])
s.socket = c.wrap_socket(s.socket, server_side=True); s.serve_forever()' "$W/tls.crt" "$W/tls.key" ) >/dev/null 2>&1 &
HTTPS_PID=$!; sleep 1
U=https://127.0.0.1:18443/rel/v0.0.0-test
CURL=(curl -fsSL --proto =https --max-time 60 --cacert "$W/tls.crt")
if "${CURL[@]}" "$U/install.sh" | t bash -s -- --root="$W/ol" --role=edge --ca-file="$W/tls.crt" >"$W/ol.log" 2>&1; then
  grep -q "signature verified" "$W/ol.log" && grep -q "sha256 verified" "$W/ol.log" && [ -x "$W/ol/usr/local/bin/tuunel" ] && ok "curl | bash one-line install (HTTPS, sha256 + signature verified)" || { bad "one-line install output"; cat "$W/ol.log"; }
else bad "one-line install"; tail -20 "$W/ol.log"; fi
"$W/ol/usr/local/bin/tuunel" version | grep -q v0.0.0-test && ok "installed the embedded release version" || bad "installed version"
# negative: tampered archive
cp "$W/www/rel/v0.0.0-test/tuunel-linux-amd64.tar.gz" "$W/orig.tgz"; printf 'x' >>"$W/www/rel/v0.0.0-test/tuunel-linux-amd64.tar.gz"
"${CURL[@]}" "$U/install.sh" | t bash -s -- --root="$W/neg1" --role=edge --ca-file="$W/tls.crt" >"$W/neg1.log" 2>&1 && bad "tampered archive accepted" || { grep -q "checksum mismatch" "$W/neg1.log" && [ ! -e "$W/neg1/usr/local/bin/tuunel" ] && ok "tampered archive rejected before install" || bad "tampered archive msg"; }
cp "$W/orig.tgz" "$W/www/rel/v0.0.0-test/tuunel-linux-amd64.tar.gz"
# negative: tampered SHA256SUMS -> signature fails
cp "$W/www/rel/v0.0.0-test/SHA256SUMS" "$W/orig.sums"; echo "0000  extra" >>"$W/www/rel/v0.0.0-test/SHA256SUMS"
"${CURL[@]}" "$U/install.sh" | t bash -s -- --root="$W/neg2" --role=edge --ca-file="$W/tls.crt" >"$W/neg2.log" 2>&1 && bad "bad signature accepted" || { grep -q "signature verification FAILED" "$W/neg2.log" && ok "tampered SHA256SUMS rejected (signature)" || bad "sig msg"; }
cp "$W/orig.sums" "$W/www/rel/v0.0.0-test/SHA256SUMS"
# negative: signature missing while the installer embeds a signer
mv "$W/www/rel/v0.0.0-test/SHA256SUMS.sig" "$W/sig.bak"
"${CURL[@]}" "$U/install.sh" | t bash -s -- --root="$W/neg3" --role=edge --ca-file="$W/tls.crt" >"$W/neg3.log" 2>&1 && bad "unsigned release accepted" || ok "missing signature rejected"
mv "$W/sig.bak" "$W/www/rel/v0.0.0-test/SHA256SUMS.sig"
# negative: untrusted TLS (no CA) must fail
"${CURL[@]}" "$U/install.sh" | t bash -s -- --root="$W/neg4" --role=edge >"$W/neg4.log" 2>&1 && bad "untrusted TLS accepted" || ok "untrusted HTTPS certificate rejected"

echo "== uninstall"
t bash scripts/install.sh --root="$R" --uninstall >/dev/null 2>&1 && [ ! -e "$R/usr/local/bin/tuunel" ] && [ -f "$R/etc/tuunel/node.key" ] && ok "uninstall keeps keys/config" || bad "uninstall"
t bash scripts/install.sh --root="$R" --uninstall --purge >/dev/null 2>&1 && [ ! -e "$R/etc/tuunel" ] && ok "uninstall --purge removes config/keys" || bad "purge"

if [ "${1:-}" = --systemd ]; then
  echo "== systemd (real)"
  [ -d /run/systemd/system ] || { bad "--systemd requested but this host is not running systemd"; }
  if [ -d /run/systemd/system ]; then
    t bash scripts/install.sh --role=edge --binary-dir="$BIN" >"$W/sd1.log" 2>&1; grep -q "REPLACE_ME" /etc/tuunel/config.yaml && ok "systemd install (waits for peer key)" || bad "systemd install"
    t bash scripts/install.sh --peer-key="$RPUB" --binary-dir="$BIN" >"$W/sd2.log" 2>&1
    systemctl is-active --quiet tuunel && ok "service active after peer key" || { bad "service not active"; journalctl -u tuunel -n 30 --no-pager; }
    grep -q -- "--- tunnelctl doctor ---" "$W/sd2.log" && ok "installer ran doctor" || bad "doctor not run"
    systemctl show tuunel -p User -p NoNewPrivileges -p ProtectSystem | tr '\n' ' '; echo
    [ "$(systemctl show tuunel -p User --value)" = tuunel ] && ok "runs as user tuunel" || bad "service user"
    ip link show tun0 >/dev/null 2>&1 && ok "tun0 created under hardened unit" || bad "tun0 missing"
    t systemctl reload tuunel && sleep 1 && systemctl is-active --quiet tuunel && ok "reload" || bad "reload"
    t systemctl restart tuunel && sleep 2 && systemctl is-active --quiet tuunel && ok "restart" || bad "restart"
    pid=$(systemctl show tuunel -p MainPID --value); kill -9 "$pid"; sleep 5; systemctl is-active --quiet tuunel && ok "Restart=on-failure after SIGKILL" || bad "no restart"
    t bash scripts/install.sh --uninstall --purge >/dev/null 2>&1; ! systemctl cat tuunel >/dev/null 2>&1 && ok "systemd uninstall" || bad "systemd uninstall"
  fi
else
  echo "NOT TESTED: systemd (run with --systemd inside a systemd host/container; see docs/INSTALL.md)"
fi

printf '%s\n' "${RES[@]}" >"${RESULTS:-/tmp}/test-install.txt" 2>/dev/null
echo "results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]

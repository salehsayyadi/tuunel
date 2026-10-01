#!/usr/bin/env bash
# Reproducible release build for the one-line installer.
#
#   scripts/release.sh [VERSION] [OUTDIR]
#
# Environment:
#   RELEASE_BASE_URL  HTTPS base under which VERSION/ is published, embedded in
#                     OUTDIR/install.sh so that
#                       curl -fsSL --proto '=https' $RELEASE_BASE_URL/VERSION/install.sh | sudo bash -s -- --role=edge
#                     works without --url (e.g. https://github.com/OWNER/REPO/releases/download
#                     for a PUBLIC repository, or https://dl.example.com/tuunel).
#   SIGNING_KEY       optional ssh private key; signs SHA256SUMS -> SHA256SUMS.sig
#                     (ssh-keygen -Y sign -n tuunel-release) and embeds the public
#                     key in install.sh, which then refuses unsigned/invalid releases.
#   SOURCE_DATE_EPOCH timestamp for archive entries (default: last commit time).
#
# Output: tuunel-linux-{amd64,arm64}.tar.gz, install.sh, SHA256SUMS[, SHA256SUMS.sig]
# Builds are reproducible: -trimpath, -buildid=, fixed VCS stamping, sorted tar
# entries with fixed owner/mtime, gzip -n. Running twice yields identical SHA256SUMS.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
OUT=${2:-dist}
COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)
git diff --quiet 2>/dev/null || COMMIT="$COMMIT-dirty"
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}
export CGO_ENABLED=0 GOFLAGS="-buildvcs=false"
LDFLAGS="-s -w -buildid= -X github.com/salehsayyadi/tuunel/internal/daemon.Version=$VERSION -X github.com/salehsayyadi/tuunel/internal/daemon.Commit=$COMMIT"
case "${RELEASE_BASE_URL:-}" in ""|https://*) ;; *) echo "RELEASE_BASE_URL must be https://" >&2; exit 2;; esac

rm -rf "$OUT"; mkdir -p "$OUT"
SIGNER=""
if [ -n "${SIGNING_KEY:-}" ]; then SIGNER=$(ssh-keygen -y -f "$SIGNING_KEY"); fi
embed() {   # embed release metadata into an installer copy
  sed -e "s#@TUUNEL_BASE_URL@#${RELEASE_BASE_URL:-@TUUNEL_BASE_URL@}#" \
      -e "s#@TUUNEL_VERSION@#$VERSION#" \
      -e "s#@TUUNEL_SIGNER@#${SIGNER:-@TUUNEL_SIGNER@}#" scripts/install.sh >"$1"
  chmod 0755 "$1"
}
for arch in amd64 arm64; do
  d=$(mktemp -d); p="$d/tuunel-$VERSION"
  mkdir -p "$p/bin" "$p/systemd" "$p/configs" "$p/docs"
  for c in tuunel tunnelctl; do
    GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$p/bin/$c" ./cmd/$c
  done
  embed "$p/install.sh"; cp systemd/tuunel.service "$p/systemd/"; cp configs/*.yaml "$p/configs/"
  cp README.md LICENSE "$p/"; cp docs/*.md "$p/docs/"
  chmod -R u=rwX,go=rX "$p"; chmod 0755 "$p/bin/"* "$p/install.sh"
  tar -C "$d" --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
      --pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime -cf - "tuunel-$VERSION" \
    | gzip -n -9 >"$OUT/tuunel-linux-$arch.tar.gz"
  rm -rf "$d"
done
embed "$OUT/install.sh"
(cd "$OUT" && sha256sum tuunel-linux-*.tar.gz install.sh >SHA256SUMS)
if [ -n "${SIGNING_KEY:-}" ]; then
  ssh-keygen -Y sign -q -f "$SIGNING_KEY" -n tuunel-release "$OUT/SHA256SUMS"   # -> SHA256SUMS.sig
fi
ls -l "$OUT"; cat "$OUT/SHA256SUMS"

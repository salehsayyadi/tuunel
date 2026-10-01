#!/usr/bin/env bash
# Build release archives for the one-line installer.
#   scripts/release.sh [VERSION] [OUTDIR]
# Produces OUTDIR/tuunel-linux-{amd64,arm64}.tar.gz, OUTDIR/install.sh and
# OUTDIR/SHA256SUMS. Publish OUTDIR's files together at one HTTPS base URL
# (e.g. a public GitHub Release of a public repo, or your own web server),
# then:  curl -fsSL BASE_URL/install.sh | sudo bash -s -- --url=BASE_URL --role=edge
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
OUT=${2:-dist}
rm -rf "$OUT"; mkdir -p "$OUT"
for arch in amd64 arm64; do
  d=$(mktemp -d); p="$d/tuunel-$VERSION"
  mkdir -p "$p/bin" "$p/systemd" "$p/configs" "$p/docs"
  for c in tuunel tunnelctl; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
      -ldflags "-s -w -X github.com/salehsayyadi/tuunel/internal/daemon.Version=$VERSION" -o "$p/bin/$c" ./cmd/$c
  done
  cp scripts/install.sh "$p/"; cp systemd/tuunel.service "$p/systemd/"; cp configs/*.yaml "$p/configs/"
  cp README.md SECURITY.md TROUBLESHOOTING.md "$p/"; cp docs/deployment.md "$p/docs/"
  tar -C "$d" --owner=0 --group=0 -czf "$OUT/tuunel-linux-$arch.tar.gz" "tuunel-$VERSION"
  rm -rf "$d"
done
cp scripts/install.sh "$OUT/install.sh"
(cd "$OUT" && sha256sum tuunel-linux-*.tar.gz install.sh > SHA256SUMS)
ls -l "$OUT"; cat "$OUT/SHA256SUMS"

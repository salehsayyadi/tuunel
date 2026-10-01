# shellcheck shell=bash
# Shared helpers for the scripts/test-*.sh lab wrappers (sourced, not executed).
# Lab scenarios run in Linux network namespaces on ONE host (root + /dev/net/tun).
set -uo pipefail
SELF=$(readlink -f "$0")
cd "$(dirname "${BASH_SOURCE[0]}")/.."
RESULTS=${RESULTS:-/data/results}
[ -d "$(dirname "$RESULTS")" ] || RESULTS=./results
mkdir -p "$RESULTS"
export TUUNEL_BIN=${TUUNEL_BIN:-$PWD/bin}
lab_preflight() {
  [ "$(id -u)" = 0 ] || exec sudo -E env PATH="$PATH" RESULTS="$RESULTS" TUUNEL_BIN="$TUUNEL_BIN" FULL="${FULL:-}" bash "$SELF" "$@"
  [ -c /dev/net/tun ] || { echo "NOT TESTABLE: /dev/net/tun missing"; exit 3; }
  command -v ip >/dev/null && command -v python3 >/dev/null || { echo "NOT TESTABLE: need iproute2 + python3"; exit 3; }
  ip netns add tuunel-preflight 2>/dev/null && ip netns del tuunel-preflight || { echo "NOT TESTABLE: cannot create network namespaces"; exit 3; }
  if [ ! -x "$TUUNEL_BIN/tuunel" ]; then
    command -v go >/dev/null || { echo "no binaries in $TUUNEL_BIN and no Go toolchain (run: make build)"; exit 2; }
    make build >/dev/null || exit 2
  fi
  if [ ! -x "$TUUNEL_BIN/netimpair" ]; then
    go build -o "$TUUNEL_BIN/netimpair" ./tests/tools/netimpair || exit 2
  fi
}
# run_lab TIMEOUT OUT.json script.py [args...]  -> prints summary, exits with scenario verdict
run_lab() {
  local t=$1 out=$2 script=$3; shift 3
  echo "running: timeout $t python3 $script $out $*"
  timeout --kill-after=15 "$t" python3 "$script" "$out" "$@" 2>&1 | tee "${out%.json}.log" | grep -E '^(PASS|FAIL|RESULT|==|\{|  ")' | tail -40
  local rc=${PIPESTATUS[0]}
  [ "$rc" = 124 ] && echo "FAIL: timed out after ${t}s"
  if [ -s "$out" ]; then python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print("RESULT:",d.get("result"),"failures:",len(d.get("failures",[])))' "$out"; fi
  return "$rc"
}

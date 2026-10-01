#!/usr/bin/env bash
# Soak: long-running tunnel under 1% loss / 10 ms / 2 ms jitter with continuous
# TCP+UDP integrity probes, periodic iperf bursts and periodic carrier blocks
# (BLOCK_DEPTH=2: active carrier and its successor fail back to back).
# Samples RSS / goroutines / fds / carrier switches; flags leaks, flapping,
# dead traffic and integrity errors.
#
#   scripts/test-soak.sh                     # quick smoke soak: 100 s
#   SOAK_SECONDS=1800 scripts/test-soak.sh   # >= 30 min (required for release)
# Exit 0 PASS, 1 FAIL, 3 NOT TESTABLE.
source "$(dirname "$0")/lab-common.sh"
lab_preflight "$@"
S=${SOAK_SECONDS:-100}
if [ "$S" -le 100 ]; then
  export SOAK_SECONDS=$S SAMPLE=${SAMPLE:-10} BLOCK_EVERY=${BLOCK_EVERY:-40} BLOCK_FOR=${BLOCK_FOR:-15} BURST_EVERY=${BURST_EVERY:-30}
else
  export SOAK_SECONDS=$S BLOCK_DEPTH=${BLOCK_DEPTH:-2}
fi
run_lab $((S + 180)) "$RESULTS/soak-$S.json" tests/lab/soak.py

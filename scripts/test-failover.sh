#!/usr/bin/env bash
# Carrier failover, endpoint failover and reverse-tunnel integrity (netns lab).
#
#   scripts/test-failover.sh                 # quick: 1 carrier cycle (~2 min)
#   FULL=1 scripts/test-failover.sh          # 3 cycles + endpoints + reverse (~8 min)
#   scripts/test-failover.sh endpoints       # one section: carriers | endpoints | reverse
# Long-lived inner TCP/UDP flows and TCP/UDP port forwards run during every
# carrier failure (tests/lab/probe.py: sequence numbers + SHA-256 integrity).
# Exit 0 PASS, 1 FAIL, 3 NOT TESTABLE.
source "$(dirname "$0")/lab-common.sh"
lab_preflight "$@"
if [ -n "${FULL:-}" ]; then
  CYCLES=${CYCLES:-3} run_lab 900 "$RESULTS/failover.json" tests/lab/failover.py "$@"
else
  [ $# -gt 0 ] || set -- carriers
  CYCLES=1 run_lab 120 "$RESULTS/failover-quick.json" tests/lab/failover.py "$@"
fi

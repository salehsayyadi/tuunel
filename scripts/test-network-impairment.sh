#!/usr/bin/env bash
# Packet loss / latency / jitter matrix through the tunnel (netns lab, one host).
#
#   scripts/test-network-impairment.sh            # quick: tcp+udp, loss 0/5, delay 50  (~2 min)
#   FULL=1 scripts/test-network-impairment.sh     # all 6 carriers x loss 0/1/5/10/20,
#                                                 # delay 20/50/100/200, jitter (~25 min)
#   scripts/test-network-impairment.sh quic wss   # choose carriers
# Impairment is applied by tests/tools/netimpair (AF_PACKET bridge) because many
# kernels/containers lack sch_netem. Env: RESULTS (dir), TUUNEL_BIN (dir with binaries).
# Exit 0 PASS, 1 FAIL, 3 NOT TESTABLE.
source "$(dirname "$0")/lab-common.sh"
lab_preflight "$@"
if [ -n "${FULL:-}" ]; then
  run_lab 2400 "$RESULTS/impair.json" tests/lab/impair.py "$@"
else
  [ $# -gt 0 ] || set -- tcp udp
  CONDS=${CONDS:-quick} run_lab 120 "$RESULTS/impair-quick.json" tests/lab/impair.py "$@"
fi

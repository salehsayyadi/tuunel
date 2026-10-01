#!/usr/bin/env bash
# MTU sweep: underlay MTU 1200..1500 x carriers (tcp udp quic quic-dgram wss icmp),
# IPv4 + IPv6 inner traffic, DF/PMTU signalling, large-packet integrity.
#
#   scripts/test-mtu.sh                      # quick: udp+tcp at 1280/1500 (~2 min)
#   FULL=1 scripts/test-mtu.sh               # full matrix (~20 min)
#   MTUS="1200 1400" CARRIERS="quic" scripts/test-mtu.sh
# Exit 0 PASS, 1 FAIL, 3 NOT TESTABLE.
source "$(dirname "$0")/lab-common.sh"
lab_preflight "$@"
if [ -n "${FULL:-}" ]; then
  run_lab 1500 "$RESULTS/mtu.json" tests/lab/mtu.py
else
  MTUS=${MTUS:-"1280 1500"} CARRIERS=${CARRIERS:-"udp tcp"} V6MTUS=${V6MTUS:-1280} \
    run_lab 120 "$RESULTS/mtu-quick.json" tests/lab/mtu.py
fi

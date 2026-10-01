#!/usr/bin/env bash
# Docker image / compose verification (bounded). Needs a running Docker daemon.
#
#   scripts/test-docker.sh
#
# 1 docker compose config validation
# 2 build the runtime image; check non-root user, entrypoint, healthcheck, version
# 3 functional: two containers (edge + remote) on a private bridge network,
#   each with only --cap-add NET_ADMIN + /dev/net/tun; tunnel up, ping through
#   the tunnel, container health becomes "healthy", carrier failover
#   (tcp blocked inside the edge container -> udp), clean SIGTERM stop
# Exit 3 = NOT TESTABLE (no Docker daemon).
set -uo pipefail
cd "$(dirname "$0")/.."
command -v docker >/dev/null && timeout 10 docker info >/dev/null 2>&1 || { echo "NOT TESTABLE: no reachable Docker daemon"; exit 3; }
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); echo "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL: $*"; }
T=tuunel-test-$$; W=$(mktemp -d /tmp/tuunel-docker.XXXXXX)
cleanup() { docker rm -f "$T-edge" "$T-remote" >/dev/null 2>&1; docker network rm "$T" >/dev/null 2>&1; rm -rf "$W"; }
trap cleanup EXIT

timeout 60 docker compose config -q && ok "docker compose config" || bad "compose config"
timeout 900 docker build -q --target runtime -t tuunel:test --build-arg VERSION=test --build-arg COMMIT="$(git rev-parse HEAD 2>/dev/null)" . >/dev/null \
  && ok "image build (runs go vet + go test)" || { bad "image build"; exit 1; }
[ "$(docker inspect -f '{{.Config.User}}' tuunel:test)" = "10001:10001" ] && ok "image runs as uid 10001" || bad "image user"
docker inspect -f '{{json .Config.Healthcheck}}' tuunel:test | grep -q tunnelctl && ok "HEALTHCHECK defined" || bad "healthcheck"
docker run --rm tuunel:test version | grep -q "tuunel test" && ok "version" || bad "version"
echo "image size: $(docker image inspect -f '{{.Size}}' tuunel:test | awk '{printf "%.1f MB", $1/1e6}')"

# keys + configs
for n in edge remote; do mkdir -p "$W/$n"; docker run --rm tuunel:test genkey >"$W/$n/node.key"; done
EP=$(docker run --rm -v "$W/edge:/k:ro" tuunel:test pubkey -key /k/node.key)
RP=$(docker run --rm -v "$W/remote:/k:ro" tuunel:test pubkey -key /k/node.key)
cat >"$W/edge/config.yaml" <<YAML
node: {id: edge}
interface: {name: tun0, addresses: ["10.200.0.1/30"]}
security: {private_key_file: /etc/tuunel/node.key}
listen:
  - {carrier: tcp, address: "0.0.0.0:7000"}
  - {carrier: udp, address: "0.0.0.0:7001"}
peers: [{name: remote, public_key: "$RP", allowed_ips: ["10.200.0.2/32"]}]
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s}
YAML
cat >"$W/remote/config.yaml" <<YAML
node: {id: remote}
interface: {name: tun0, addresses: ["10.200.0.2/30"]}
security: {private_key_file: /etc/tuunel/node.key}
peers:
  - name: edge
    public_key: "$EP"
    allowed_ips: ["10.200.0.1/32"]
    endpoints: [{name: edge, address: $T-edge}]
    carriers: [{type: tcp, port: 7000}, {type: udp, port: 7001}]
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s, failed_after_missed: 4}
failover: {backoff_initial: 500ms, backoff_max: 3s, min_hold: 3s}
YAML
sudo chown -R 10001:10001 "$W/edge" "$W/remote" 2>/dev/null || chown -R 10001:10001 "$W/edge" "$W/remote"
chmod 0600 "$W"/*/node.key
docker network create "$T" >/dev/null
for n in edge remote; do
  docker run -d --name "$T-$n" --network "$T" --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW --cap-add NET_BIND_SERVICE \
    --device /dev/net/tun --read-only --tmpfs /run/tuunel:uid=10001,gid=10001,mode=0750 \
    -v "$W/$n:/etc/tuunel:ro" tuunel:test >/dev/null || bad "start $n"
  sleep 1
done
up=0; for _ in $(seq 1 30); do docker exec "$T-remote" tunnelctl status 2>/dev/null | grep -q "UP" && { up=1; break; }; sleep 1; done
[ $up = 1 ] && ok "tunnel up between containers" || { bad "tunnel up"; docker logs "$T-remote" | tail -20; }
docker exec "$T-remote" tunnelctl ping >/dev/null 2>&1 && ok "tunnelctl ping through tunnel" || bad "tunnel ping"
h=""; for _ in $(seq 1 40); do h=$(docker inspect -f '{{.State.Health.Status}}' "$T-edge"); [ "$h" = healthy ] && break; sleep 1; done
[ "$h" = healthy ] && ok "container health: healthy" || bad "health=$h"
docker exec --user 0 "$T-edge" nft -f - <<'NFT' && ok "blocked tcp/7000 inside edge" || bad "nft block"
table ip tblk { chain i { type filter hook input priority 0; policy accept; tcp dport 7000 drop; } }
NFT
sw=0; for _ in $(seq 1 40); do docker exec "$T-remote" tunnelctl -json status 2>/dev/null | grep -q '"carrier": *"udp"' && { sw=1; break; }; sleep 1; done
[ $sw = 1 ] && ok "failover tcp -> udp inside containers" || bad "failover"
s0=$(date +%s); docker stop -t 15 "$T-remote" >/dev/null; rc=$(docker inspect -f '{{.State.ExitCode}}' "$T-remote")
[ "$rc" = 0 ] && [ $(( $(date +%s) - s0 )) -lt 15 ] && ok "SIGTERM: clean exit 0" || bad "stop rc=$rc"
echo "results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]

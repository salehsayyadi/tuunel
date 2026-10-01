#!/usr/bin/env bash
# Reproducible two-node acceptance test using Linux network namespaces and
# real TUN devices. Requires root, iproute2 and iptables. Optional: tc netem.
#
#   sudo tests/lab/netns-acceptance.sh [path/to/bin]
#
# Topology:  [ns ta] tun0 10.200.0.1 --veth 192.0.2.1/24 <-> 192.0.2.2/24 veth-- [ns tb] tun0 10.200.0.2
# Node A (ta) initiates; node B (tb) listens on tcp/7000 quic/7002 wss/7003 udp/7001.
set -u
export PATH="$PATH:/usr/sbin:/sbin"
BIN=${1:-$(cd "$(dirname "$0")/../.." && pwd)/bin}
W=$(mktemp -d /tmp/tuunel-lab.XXXX)
PASS=0; FAIL=0; SKIP=0
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { echo "PASS: $*"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $*"; FAIL=$((FAIL+1)); }
skip() { echo "SKIP: $*"; SKIP=$((SKIP+1)); }
A() { ip netns exec ta "$@"; }
B() { ip netns exec tb "$@"; }
ctlA() { A "$BIN/tunnelctl" -socket "$W/a.sock" -config "$W/a.yaml" "$@"; }
ctlB() { B "$BIN/tunnelctl" -socket "$W/b.sock" -config "$W/b.yaml" "$@"; }
carrierA() { ctlA -json status | python3 -c 'import sys,json; p=json.load(sys.stdin)["engine"]["peers"][0]; print(p["carrier"] if p["up"] else "down")'; }
wait_carrier() { # wait_carrier NAME SECONDS
  for _ in $(seq 1 $(( $2 * 5 ))); do [ "$(carrierA 2>/dev/null)" = "$1" ] && return 0; sleep 0.2; done; return 1; }

stopA() { pkill -f "tuunel run -config $W/a.yaml"; for _ in $(seq 1 50); do pgrep -f "tuunel run -config $W/a.yaml" >/dev/null || break; sleep 0.1; done; }
cleanup() {
  pkill -f "tuunel run -config $W/" 2>/dev/null
  [ -n "${ECHO:-}" ] && kill "$ECHO" 2>/dev/null
  sleep 0.5; ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null
  echo "logs kept in $W"
}
trap cleanup EXIT

say "setup namespaces"
ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null
ip netns add ta && ip netns add tb
ip link add va netns ta type veth peer name vb netns tb
A ip addr add 192.0.2.1/24 dev va; B ip addr add 192.0.2.2/24 dev vb
A ip link set va up; B ip link set vb up; A ip link set lo up; B ip link set lo up

KA=$("$BIN/tuunel" genkey 2>"$W/a.pub"); KB=$("$BIN/tuunel" genkey 2>"$W/b.pub")
PUBA=$(sed 's/public key: //' "$W/a.pub"); PUBB=$(sed 's/public key: //' "$W/b.pub")
umask 077; echo "$KA" >"$W/a.key"; echo "$KB" >"$W/b.key"; umask 022

cat >"$W/b.yaml" <<Y
node: {id: node-b}
interface: {name: tun0, addresses: ["10.200.0.2/30", "fd20::2/64"]}
security: {private_key_file: $W/b.key}
listen:
  - {carrier: tcp,  address: "192.0.2.2:7000"}
  - {carrier: udp,  address: "192.0.2.2:7001"}
  - {carrier: quic, address: "192.0.2.2:7002"}
  - {carrier: wss,  address: "192.0.2.2:7003"}
peers:
  - name: node-a
    public_key: "$PUBA"
    allowed_ips: ["10.200.0.1/32", "fd20::1/128"]
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s}
forwarding:
  tcp: [{listen: "192.0.2.2:8443", target: "10.200.0.1:9443"}]
  udp: [{listen: "192.0.2.2:5353", target: "10.200.0.1:9053"}]
api: {socket: $W/b.sock}
log: {level: info}
Y
cat >"$W/a.yaml" <<Y
node: {id: node-a}
interface: {name: tun0, addresses: ["10.200.0.1/30", "fd20::1/64"]}
security: {private_key_file: $W/a.key, rekey_interval: 1m}
peers:
  - name: node-b
    public_key: "$PUBB"
    allowed_ips: ["10.200.0.2/32", "fd20::2/128"]
    endpoints: [{name: lab-b, address: 192.0.2.2}]
    carriers:
      - {type: tcp,  port: 7000}
      - {type: quic, port: 7002}
      - {type: wss,  port: 7003}
      - {type: udp,  port: 7001}
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s, failed_after_missed: 4}
failover: {backoff_initial: 500ms, backoff_max: 5s, min_hold: 5s, probe_interval: 2s, recovery_successes: 2}
api: {socket: $W/a.sock}
log: {level: info}
Y

say "validate configuration"
A "$BIN/tuunel" check -config "$W/a.yaml" && ok "config A valid" || bad "config A"
B "$BIN/tuunel" check -config "$W/b.yaml" && ok "config B valid" || bad "config B"

say "start daemons"
B "$BIN/tuunel" run -config "$W/b.yaml" >"$W/b.log" 2>&1 & PB=$!
sleep 0.5
A "$BIN/tuunel" run -config "$W/a.yaml" >"$W/a.log" 2>&1 & PA=$!
wait_carrier tcp 10 && ok "tunnel up over TCP" || bad "tunnel did not come up on tcp ($(carrierA))"

say "L3: ping across the tunnel"
A ping -c 3 -W 2 10.200.0.2 >"$W/ping4" && ok "ping 10.200.0.2 (IPv4)" || { bad "ping 10.200.0.2"; cat "$W/ping4"; }
A ping -6 -c 3 -W 2 fd20::2 >"$W/ping6" && ok "ping fd20::2 (IPv6 inside tunnel)" || { bad "ping6"; cat "$W/ping6"; }

say "arbitrary TCP/UDP traffic across the tunnel"
B python3 - >"$W/echo.log" 2>&1 <<'PY' &
import socket, threading
def tcp():
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("10.200.0.2",9000)); s.listen()
    while True:
        c,_=s.accept()
        def h(c):
            while (d:=c.recv(65536)): c.sendall(d)
            c.close()
        threading.Thread(target=h,args=(c,),daemon=True).start()
def udp():
    u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.bind(("10.200.0.2",9001))
    while True:
        d,a=u.recvfrom(65535); u.sendto(d,a)
threading.Thread(target=tcp,daemon=True).start(); udp()
PY
ECHO=$!
sleep 0.5
A python3 - <<'PY' && ok "TCP 5 MiB echo + UDP echo through tunnel" || bad "TCP/UDP through tunnel"
import socket, os, time
data=os.urandom(5*1024*1024)
s=socket.create_connection(("10.200.0.2",9000),timeout=10)
import threading
threading.Thread(target=lambda: s.sendall(data),daemon=True).start()
got=b""
t=time.time()
while len(got)<len(data): got+=s.recv(1<<20)
assert got==data
print("tcp %.1f Mbit/s" % (len(data)*8*2/(time.time()-t)/1e6))
u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.settimeout(2)
for i in range(5):
    u.sendto(b"x"*1000,("10.200.0.2",9001)); assert u.recvfrom(2000)[0]==b"x"*1000
PY

say "reverse tunnel + TCP/UDP forwarding (A only dials out; B exposes A's services)"
A iptables -A INPUT -i va -p tcp --syn -j DROP 2>/dev/null && ok "A drops all inbound TCP SYN on its underlay" || skip "iptables unavailable in namespace"
A python3 - >/dev/null 2>&1 <<'PY' &
import socket, threading
def tcp():
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("10.200.0.1",9443)); s.listen()
    while True:
        c,_=s.accept(); c.sendall(b"hello-from-A\n"); c.close()
def udp():
    u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.bind(("10.200.0.1",9053))
    while True:
        d,a=u.recvfrom(65535); u.sendto(b"A:"+d,a)
threading.Thread(target=tcp,daemon=True).start(); udp()
PY
SVC=$!
sleep 0.5
out=$(B python3 -c 'import socket;s=socket.create_connection(("192.0.2.2",8443),timeout=5);print(s.recv(100).decode().strip())' 2>&1)
[ "$out" = "hello-from-A" ] && ok "TCP forward 192.0.2.2:8443 -> tunnel -> A:9443" || bad "TCP forward ($out)"
out=$(B python3 -c 'import socket;u=socket.socket(2,2);u.settimeout(3);u.sendto(b"q",("192.0.2.2",5353));print(u.recvfrom(100)[0].decode())' 2>&1)
[ "$out" = "A:q" ] && ok "UDP forward 192.0.2.2:5353 -> tunnel -> A:9053" || bad "UDP forward ($out)"
kill $SVC 2>/dev/null

say "carrier failover: block TCP/7000 on B"
B iptables -A INPUT -p tcp --dport 7000 -j DROP
T0=$(date +%s.%N)
if wait_carrier quic 30; then ok "switched TCP -> QUIC in $(python3 -c "import time;print(round(time.time()-$T0,1))")s"; else bad "no failover to quic ($(carrierA))"; fi
A ping -c 3 -W 2 10.200.0.2 >/dev/null && ok "same tunnel IPs keep working after failover" || bad "ping after failover"

say "second failure: block QUIC/7002 as well"
B iptables -A INPUT -p udp --dport 7002 -j DROP
wait_carrier wss 30 && ok "switched QUIC -> WSS" || bad "no failover to wss ($(carrierA))"
A ping -c 3 -W 2 10.200.0.2 >/dev/null && ok "ping over WSS" || bad "ping over wss"

say "diagnostics while TCP and QUIC are blocked"
ctlA status | tee "$W/status1"
grep -q "Carrier: WSS" "$W/status1" && ok "status reports WSS" || bad "status carrier"
ctlA test-carriers | tee "$W/tc1"
grep -E "^node-b +lab-b +tcp +FAILED" "$W/tc1" >/dev/null && grep -E "^node-b +lab-b +wss +OK" "$W/tc1" >/dev/null && ok "test-carriers: tcp FAILED, wss OK" || bad "test-carriers output"
ctlA doctor | tee "$W/doctor1"
grep -q "peer node-b UP via lab-b/wss" "$W/doctor1" && grep -qE "reachability.*tcp.*no response" "$W/doctor1" && ok "doctor explains blocked carriers" || bad "doctor output"

say "recovery: unblock TCP and QUIC; manager must return to TCP (preempt with hysteresis)"
B iptables -D INPUT -p tcp --dport 7000 -j DROP; B iptables -D INPUT -p udp --dport 7002 -j DROP
T0=$(date +%s)
wait_carrier tcp 40 && ok "recovered to TCP after $(( $(date +%s) - T0 ))s" || bad "did not return to tcp ($(carrierA))"
A ping -c 3 -W 2 10.200.0.2 >/dev/null && ok "ping after recovery" || bad "ping after recovery"

say "packet loss: 40% random drop on TCP/7000 → DEGRADED then switch"
if B iptables -A INPUT -p tcp --dport 7000 -m statistic --mode random --probability 0.4 -j DROP 2>/dev/null; then
  wait_carrier quic 60 && ok "left lossy TCP link" || bad "stayed on lossy link ($(carrierA))"
  B iptables -D INPUT -p tcp --dport 7000 -m statistic --mode random --probability 0.4 -j DROP
else skip "iptables statistic match unavailable"; fi

say "latency/jitter (tc netem)"
if B tc qdisc add dev vb root netem delay 80ms 20ms 2>/dev/null; then
  sleep 4; ctlA status | grep -E "RTT|Jitter"; ok "netem applied (see RTT/Jitter above)"; B tc qdisc del dev vb root
else skip "tc netem not supported by this kernel (sch_netem missing); run on a kernel with sch_netem (see docker-compose.yml lab service)"; fi

say "MTU problem: shrink underlay MTU to 1200 and restart A"
stopA
A ip link set va mtu 1200; B ip link set vb mtu 1200
A "$BIN/tuunel" run -config "$W/a.yaml" >"$W/a2.log" 2>&1 & PA=$!
for _ in $(seq 1 50); do [ "$(carrierA 2>/dev/null)" = tcp ] && break; sleep 0.2; done
MTU=$(A cat /sys/class/net/tun0/mtu)
# inner IPv6 is configured, so the protocol floor is 1280; the plan must drop from 1380 to that floor and warn
[ "$MTU" -lt 1380 ] && grep -q "path 1200" "$W/a2.log" && ok "auto MTU re-planned for path MTU 1200: tun0 mtu=$MTU (IPv6 floor 1280), warnings logged" || bad "tun0 mtu=$MTU"
grep -q "pathological" "$W/a2.log" && ok "pathological fragmentation reported for the UDP carrier" || bad "no pathological report"
A ping -c 2 -W 2 -s $((MTU-28)) -M do 10.200.0.2 >/dev/null && ok "largest DF ping ($((MTU-28)) bytes) passes" || bad "max-size DF ping"
A ping -c 2 -W 2 -s 3000 10.200.0.2 >/dev/null && ok "3000-byte ping fragmented by the kernel at tun0 MTU and delivered" || bad "fragmented ping"
grep -i "mtu" "$W/a2.log" | head -3

say "endpoint failure: stop B entirely"
pkill -f "tuunel run -config $W/b.yaml"; sleep 1
sleep 8
ctlA status | grep -q "Tunnel: DOWN" && ok "status reports DOWN when no path exists" || bad "status after endpoint failure"
ctlA status | grep -q "No usable path" && ok "status explains no usable path" || bad "no-path explanation"

say "results: $PASS passed, $FAIL failed, $SKIP skipped"
[ $FAIL -eq 0 ]

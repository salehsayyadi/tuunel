#!/usr/bin/env bash
# Extended real-TUN verification lab (root, iproute2, iptables, python3, curl).
# Complements netns-acceptance.sh with: per-carrier checks (incl. experimental
# ICMP and QUIC DATAGRAM), carrier reconnect + failure-detection timing,
# repeated failover cycles with packet-loss measurement and a long-lived TCP
# stream, reverse-direction traffic, endpoint failover across three endpoints,
# an underlay MTU sweep, clean shutdown/restart and health metric fields.
#
#   sudo tests/lab/netns-extended.sh [bin-dir] [sections...]
#   sections: carriers failover endpoints mtu shutdown reverse   (default: all)
set -u
export PATH="$PATH:/usr/sbin:/sbin"
BIN=${1:-$(cd "$(dirname "$0")/../.." && pwd)/bin}; shift || true
SECTIONS=${*:-"carriers failover endpoints mtu shutdown reverse"}
W=$(mktemp -d /tmp/tuunel-ext.XXXX)
PASS=0; FAIL=0; SKIP=0
say()  { printf '\n== %s\n' "$*"; }
ok()   { echo "PASS: $*"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $*"; FAIL=$((FAIL+1)); }
skip() { echo "SKIP: $*"; SKIP=$((SKIP+1)); }
A() { ip netns exec ta "$@"; }
B() { ip netns exec tb "$@"; }
now() { date +%s.%N; }
since() { python3 -c "print(round($(now)-$1,2))"; }
stA() { A curl -s --max-time 2 --unix-socket "$W/a.sock" http://x/api/status; }
stB() { B curl -s --max-time 2 --unix-socket "$W/b.sock" http://x/api/status; }
peerA() { stA | python3 -c "import sys,json; p=json.load(sys.stdin)['engine']['peers'][0]; print($1)" 2>/dev/null; }
carrierA() { peerA 'p["carrier"]+"/"+p["endpoint"] if p["up"] else "down"'; }
wait_for() { # wait_for PATTERN SECONDS  (matches carrier/endpoint string)
  for _ in $(seq 1 $(( $2 * 10 ))); do case "$(carrierA)" in $1) return 0;; esac; sleep 0.1; done; return 1; }
startA() { A "$BIN/tuunel" run -config "$W/a.yaml" >>"$W/a.log" 2>&1 & }
startB() { B "$BIN/tuunel" run -config "$W/b.yaml" >>"$W/b.log" 2>&1 & }
stop()   { pkill -f "tuunel run -config $W/$1.yaml"; for _ in $(seq 1 60); do pgrep -f "tuunel run -config $W/$1.yaml" >/dev/null || return 0; sleep 0.1; done; pkill -9 -f "tuunel run -config $W/$1.yaml"; return 1; }
cleanup() {
  pkill -f "tuunel run -config $W/" 2>/dev/null; pkill -f "tuunel-ext-helper" 2>/dev/null
  sleep 0.3; ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null
  echo "logs kept in $W"
}
trap cleanup EXIT

setup_ns() { # setup_ns MTU
  pkill -f "tuunel run -config $W/" 2>/dev/null; pkill -f tuunel-ext-helper 2>/dev/null; sleep 0.3
  ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null
  ip netns add ta; ip netns add tb
  ip link add va netns ta type veth peer name vb netns tb
  A ip addr add 192.0.2.1/24 dev va
  for i in 2 3 4; do B ip addr add 192.0.2.$i/24 dev vb; done
  A ip link set va mtu "${1:-1500}"; B ip link set vb mtu "${1:-1500}"
  A ip link set va up; B ip link set vb up; A ip link set lo up; B ip link set lo up
}
keys() {
  KA=$("$BIN/tuunel" genkey 2>"$W/a.pub"); KB=$("$BIN/tuunel" genkey 2>"$W/b.pub")
  PUBA=$(sed 's/public key: //' "$W/a.pub"); PUBB=$(sed 's/public key: //' "$W/b.pub")
  umask 077; echo "$KA" >"$W/a.key"; echo "$KB" >"$W/b.key"; umask 022
}
# write_b "listen-lines" [extra-yaml]
write_b() { cat >"$W/b.yaml" <<Y
node: {id: node-b}
interface: {name: tun0, addresses: ["10.200.0.2/30"]}
security: {private_key_file: $W/b.key}
listen:
$1
peers: [{name: node-a, public_key: "$PUBA", allowed_ips: ["10.200.0.1/32"]}]
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s}
api: {socket: $W/b.sock}
log: {level: info}
${2:-}
Y
}
# write_a "endpoints" "carriers" [extra-yaml]
write_a() { cat >"$W/a.yaml" <<Y
node: {id: node-a}
interface: {name: tun0, addresses: ["10.200.0.1/30"]}
security: {private_key_file: $W/a.key}
peers:
  - name: node-b
    public_key: "$PUBB"
    allowed_ips: ["10.200.0.2/32"]
    endpoints: $1
    carriers: $2
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s, failed_after_missed: 4}
failover: {backoff_initial: 500ms, backoff_max: 3s, min_hold: 3s, probe_interval: 2s, recovery_successes: 2}
api: {socket: $W/a.sock}
log: {level: info}
${3:-}
Y
}
helpers_b() { # TCP echo :9000, TCP sink :9002, UDP echo :9001 on 10.200.0.2
  B python3 - tuunel-ext-helper >/dev/null 2>&1 <<'PY' &
import socket, threading
def serve(port, echo):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",port)); s.listen()
    while True:
        c,_=s.accept()
        def h(c):
            try:
                while (d:=c.recv(65536)):
                    if echo: c.sendall(d)
                if not echo: c.sendall(b"k")
            except OSError: pass
            c.close()
        threading.Thread(target=h,args=(c,),daemon=True).start()
def udp():
    u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.bind(("0.0.0.0",9001))
    while True:
        d,a=u.recvfrom(65535); u.sendto(d,a)
threading.Thread(target=serve,args=(9000,True),daemon=True).start()
threading.Thread(target=serve,args=(9002,False),daemon=True).start()
udp()
PY
  sleep 0.4
}
xfer() { # xfer MiB -> prints Mbit/s or ERR
  A timeout 60 python3 -c "
import socket,time
d=b'x'*(1<<20); s=socket.create_connection(('10.200.0.2',9002),timeout=20); t=time.time()
for _ in range($1): s.sendall(d)
s.shutdown(socket.SHUT_WR); assert s.recv(1)==b'k'; print('%.0f' % ($1*8*1.048576/(time.time()-t)))" 2>/dev/null || echo ERR; }
udpecho() { A timeout 10 python3 -c "
import socket
u=socket.socket(2,2); u.settimeout(2); n=0
for i in range(20):
    try:
        u.sendto(b'u'*${1:-1000},('10.200.0.2',9001)); n+= u.recvfrom(70000)[0]==b'u'*${1:-1000}
    except OSError: pass
print(n)" 2>/dev/null || echo 0; }
udp1() { A timeout 1 python3 -c "
import socket
u=socket.socket(2,2); u.settimeout(0.3); u.sendto(b'p',('10.200.0.2',9001)); assert u.recvfrom(10)[0]==b'p'" 2>/dev/null; }
rtt() { A ping -c 10 -i 0.1 -q -W 1 10.200.0.2 2>/dev/null | awk -F/ '/rtt/{print $5}'; }

keys

# ---------------------------------------------------------------- carriers
if [[ " $SECTIONS " == *" carriers "* ]]; then
say "per-carrier verification (connect, handshake, transfer, RTT, throughput, reconnect, failure detection)"
printf "%-11s %-6s %-9s %-9s %-10s %-8s %-11s %-11s\n" CARRIER UP CONNECT_s RTT_ms TCP_Mbit UDP/20 RECONNECT_s DETECT_s | tee "$W/carriers.tsv"
for spec in "tcp tcp 7000" "udp udp 7001" "quic quic 7002" "quic-dgram quic 7004" "wss wss 7003" "ws ws 7005" "icmp icmp 0"; do
  set -- $spec; name=$1; typ=$2; port=$3; lx=""; cx=""; exp=""
  [ "$name" = quic-dgram ] && { lx=", datagrams: true"; cx=", datagrams: true"; }
  if [ "$typ" = icmp ]; then
    exp="experimental: {icmp: true}"
    write_b "  - {carrier: icmp, address: \"192.0.2.2\"}" "$exp"
    write_a "[{name: e1, address: 192.0.2.2}]" "[{type: icmp}]" "$exp"
  else
    write_b "  - {carrier: $typ, address: \"192.0.2.2:$port\"$lx}"
    write_a "[{name: e1, address: 192.0.2.2}]" "[{type: $typ, port: $port$cx}]"
  fi
  setup_ns 1500; [ "$typ" = icmp ] && B sysctl -qw net.ipv4.icmp_echo_ignore_all=1
  helpers_b; : >"$W/a.log"; : >"$W/b.log"
  startB; sleep 0.4; t0=$(now); startA
  if wait_for "$typ/*" 15; then up=yes; ct=$(since $t0); else up=NO; ct=-; fi
  if [ "$typ" = icmp ]; then r="n/a(echo-ignored)"; else r=$(rtt); fi; gp=-; ue=0; rc=-; dt=-
  if [ $up = yes ]; then
    gp=$(xfer 20); ue=$(udpecho 1000)
    # reconnect: restart B, time until A is back on this carrier and ping passes
    stop b; t1=$(now); startB
    # (UDP probe, not ping: with the ICMP carrier B sets icmp_echo_ignore_all,
    # which also silences echo replies for its tunnel address)
    for _ in $(seq 1 200); do udp1 && { rc=$(since $t1); break; }; sleep 0.1; done
    # failure detection: blackhole the carrier on B, time until A reports down
    if [ "$typ" = icmp ]; then B iptables -I INPUT -p icmp -j DROP
    elif [ "$typ" = udp ] || [ "$typ" = quic ]; then B iptables -I INPUT -p udp --dport $port -j DROP
    else B iptables -I INPUT -p tcp --dport $port -j DROP; fi
    t2=$(now)
    wait_for down 20 && dt=$(since $t2)
    B iptables -F INPUT
  fi
  printf "%-11s %-6s %-9s %-9s %-10s %-8s %-11s %-11s\n" "$name" "$up" "$ct" "${r:--}" "$gp" "$ue" "$rc" "$dt" | tee -a "$W/carriers.tsv"
  if [ $up = yes ] && [ "$gp" != ERR ] && [ "$ue" -ge 18 ] && [ "$rc" != - ] && [ "$dt" != - ]; then ok "carrier $name: up, transfer, reconnect, failure detection"
  else bad "carrier $name (up=$up tcp=$gp udp=$ue reconnect=$rc detect=$dt)"; grep -iE "error|warn" "$W/a.log" | tail -3; fi
  stop a; stop b
done
fi

# ---------------------------------------------------------------- failover cycles
LISTEN4='  - {carrier: tcp,  address: "192.0.2.2:7000"}
  - {carrier: udp,  address: "192.0.2.2:7001"}
  - {carrier: quic, address: "192.0.2.2:7002"}
  - {carrier: wss,  address: "192.0.2.2:7003"}'
CARR4='[{type: tcp, port: 7000}, {type: quic, port: 7002}, {type: wss, port: 7003}, {type: udp, port: 7001}]'
if [[ " $SECTIONS " == *" failover "* ]]; then
say "failover cycles TCP -> QUIC -> WSS -> TCP (x3) with 20 pps ping loss and a long-lived TCP stream"
setup_ns 1500; helpers_b; : >"$W/a.log"; : >"$W/b.log"
write_b "$LISTEN4"; write_a "[{name: e1, address: 192.0.2.2}]" "$CARR4"
startB; sleep 0.4; startA
wait_for "tcp/*" 15 && ok "initial carrier TCP" || bad "initial carrier ($(carrierA))"
# long-lived TCP echo stream: sends 1 KiB every 50 ms and verifies echoes; reports stalls
A python3 - tuunel-ext-helper >"$W/stream.log" 2>&1 <<'PY' &
import socket,time,os,sys
s=socket.create_connection(("10.200.0.2",9000),timeout=60); s.settimeout(60)
sent=0; maxgap=0; last=time.time(); end=time.time()+600
while time.time()<end:
    d=os.urandom(1024); s.sendall(d); got=b""
    while len(got)<1024: got+=s.recv(1024-len(got))
    assert got==d
    n=time.time(); maxgap=max(maxgap,n-last); last=n; sent+=1
    if sent%20==0: print("ok",sent,round(maxgap,2),flush=True)
    time.sleep(0.05)
PY
STREAM=$!
for cyc in 1 2 3; do
  for step in "quic 7000 tcp" "wss 7002 udp" "tcp -"; do
    set -- $step; want=$1; port=$2; proto=${3:-}
    A ping -i 0.05 -W 1 -c 400 10.200.0.2 >"$W/fping" 2>&1 & PP=$!
    sleep 0.5
    t0=$(now)
    if [ "$port" = - ]; then B iptables -F INPUT; else B iptables -A INPUT -p $proto --dport $port -j DROP; fi
    if wait_for "$want/*" 40; then sw=$(since $t0); else sw=FAIL; fi
    sleep 2; kill -INT $PP 2>/dev/null; wait $PP 2>/dev/null
    tx=$(awk '/transmitted/{print $1}' "$W/fping"); rxp=$(awk '/transmitted/{print $4}' "$W/fping")
    lost=$(( ${tx:-0} - ${rxp:-0} ))
    if [ "$sw" != FAIL ]; then ok "cycle $cyc -> $want in ${sw}s, ping lost $lost/$tx (~$(python3 -c "print(round($lost*0.05,2))")s outage)"
    else bad "cycle $cyc: no switch to $want ($(carrierA))"; fi
  done
done
B iptables -F INPUT
sleep 1
if kill -0 $STREAM 2>/dev/null; then ok "long-lived inner TCP connection survived 9 carrier switches ($(tail -1 "$W/stream.log"))"; kill $STREAM
else bad "inner TCP stream died: $(tail -2 "$W/stream.log")"; fi
say "reverse-direction traffic during failover (ping + transfer initiated from B)"
A python3 -c 'import socket;s=socket.socket(2,2);s.bind(("10.200.0.1",9101));
while True: d,a=s.recvfrom(9000); s.sendto(d,a)' tuunel-ext-helper >/dev/null 2>&1 &
B ping -i 0.05 -W 1 -c 300 10.200.0.1 >"$W/rping" 2>&1 & PP=$!
sleep 0.5; t0=$(now); B iptables -A INPUT -p tcp --dport 7000 -j DROP
wait_for "quic/*" 40 && ok "B->A traffic: failover to QUIC in $(since $t0)s" || bad "reverse failover"
sleep 2; kill -INT $PP 2>/dev/null; wait $PP 2>/dev/null
grep transmitted "$W/rping"
B python3 -c 'import socket;u=socket.socket(2,2);u.settimeout(2);u.sendto(b"r"*1200,("10.200.0.1",9101));assert u.recvfrom(2000)[0]==b"r"*1200' && ok "UDP from B to A after failover" || bad "UDP B->A"
B iptables -F INPUT
health_json=$(stA)
echo "$health_json" | python3 -c '
import sys,json; p=json.load(sys.stdin)["engine"]["peers"][0]
need=["carrier","endpoint","tx_bytes","rx_bytes","tx_packets","rx_packets","reconnects","carrier_switches","uptime_ns"]
h=p["health"]; print({k:p[k] for k in need}); print({k:h[k] for k in ["state","rtt_ns","avg_rtt_ns","jitter_ns","loss_pct","probes_sent","probes_received"]})
assert all(k in p for k in need) and p["tx_packets"]>0 and p["rx_packets"]>0 and p["carrier_switches"]>=9 and h["rtt_ns"]>0
states={c["state"] for c in p["candidates"]}; print("candidate states:",states)
' && ok "health metrics present: RTT/jitter/loss/TX/RX/packets/uptime/reconnects/carrier/endpoint" || bad "health metric fields"
A curl -s --unix-socket "$W/a.sock" http://x/api/metrics | grep -E "^tuunel_(goroutines|open_fds|heap_inuse_bytes|reconnects_total)" 
stop a; stop b
fi

# ---------------------------------------------------------------- endpoints
if [[ " $SECTIONS " == *" endpoints "* ]]; then
say "endpoint failover: three endpoints (192.0.2.2/.3/.4), priority order"
setup_ns 1500; helpers_b; : >"$W/a.log"
write_b '  - {carrier: tcp, address: "0.0.0.0:7000"}'
write_a "[{name: e1, address: 192.0.2.2, priority: 1}, {name: e2, address: 192.0.2.3, priority: 2}, {name: e3, address: 192.0.2.4, priority: 3}]" "[{type: tcp, port: 7000}]"
startB; sleep 0.4; startA
wait_for "tcp/e1" 15 && ok "active endpoint e1" || bad "initial endpoint ($(carrierA))"
A python3 - tuunel-ext-helper >"$W/estream.log" 2>&1 <<'PY' &
import socket,time
s=socket.create_connection(("10.200.0.2",9000),timeout=60); s.settimeout(60); n=0
while True:
    s.sendall(b"e"*512); g=b""
    while len(g)<512: g+=s.recv(512-len(g))
    n+=1; time.sleep(0.05)
PY
ES=$!
t0=$(now); B iptables -A INPUT -d 192.0.2.2 -p tcp --dport 7000 -j DROP
wait_for "tcp/e2" 30 && ok "e1 failed -> e2 in $(since $t0)s" || bad "no switch to e2 ($(carrierA))"
t0=$(now); B iptables -A INPUT -d 192.0.2.3 -p tcp --dport 7000 -j DROP
wait_for "tcp/e3" 30 && ok "e2 failed -> e3 in $(since $t0)s" || bad "no switch to e3 ($(carrierA))"
A ping -c 3 -W 2 10.200.0.2 >/dev/null && ok "TUN address unchanged and reachable via e3" || bad "ping via e3"
t0=$(now); B iptables -F INPUT
wait_for "tcp/e1" 40 && ok "preempted back to e1 in $(since $t0)s" || bad "did not return to e1 ($(carrierA))"
kill -0 $ES 2>/dev/null && ok "inner TCP connection survived endpoint failovers" || bad "inner TCP died across endpoint failover"
kill $ES 2>/dev/null
stop a; stop b
fi

# ---------------------------------------------------------------- MTU sweep
if [[ " $SECTIONS " == *" mtu "* ]]; then
say "underlay MTU sweep (auto TUN MTU) per carrier: DF ping at tun MTU, oversize DF rejected, 4 MiB TCP, 1400-byte UDP"
MTU_FAIL0=$FAIL
printf "%-6s %-6s %-8s %-8s %-9s %-8s %-6s\n" PATH CARR TUN_MTU DF_MAX DF_OVER TCP UDP | tee "$W/mtu.tsv"
for m in ${MTU_LIST:-1200 1280 1300 1350 1400 1450 1500}; do
  for spec in ${MTU_CARRIERS:-"tcp 7000" "udp 7001" "quic 7002" "wss 7003"}; do
    set -- ${spec/:/ }; c=$1; port=$2
    if [ $c = quic ] && [ $m -lt 1228 ]; then skip "mtu $m/quic: QUIC needs >=1200-byte UDP datagrams (path MTU >=1228, RFC 9000 s14); other carriers take over"; continue; fi
    setup_ns $m; helpers_b; : >"$W/a.log"
    write_b "  - {carrier: $c, address: \"192.0.2.2:$port\"}"; write_a "[{name: e1, address: 192.0.2.2}]" "[{type: $c, port: $port}]"
    startB; sleep 0.3; startA
    if ! wait_for "$c/*" 15; then printf "%-6s %-6s DOWN\n" $m $c | tee -a "$W/mtu.tsv"; bad "mtu $m/$c: tunnel down"; stop a; stop b; continue; fi
    tm=$(A cat /sys/class/net/tun0/mtu)
    A ping -c 2 -W 2 -M do -s $((tm-28)) 10.200.0.2 >/dev/null 2>&1 && dm=ok || dm=FAIL
    A ping -c 1 -W 1 -M do -s $((tm-27)) 10.200.0.2 >/dev/null 2>&1 && dov=LEAK || dov=blocked
    g=$(xfer 4); u=$(udpecho 1400)
    printf "%-6s %-6s %-8s %-8s %-9s %-8s %-6s\n" $m $c $tm $dm $dov $g "$u/20" | tee -a "$W/mtu.tsv"
    if [ $dm = ok ] && [ $dov = blocked ] && [ "$g" != ERR ] && [ "$u" -ge 18 ]; then :; else bad "mtu $m/$c (df=$dm over=$dov tcp=$g udp=$u)"
      [ -n "${MTU_DEBUG:-}" ] && for n in a b; do echo "-- $n drops:"; ${n^^} curl -s --unix-socket "$W/$n.sock" http://x/api/metrics | grep -E "dropped|oversize|frag" | grep -v " 0$"; done; fi
    stop a; stop b
  done
done
[ "$FAIL" -eq "$MTU_FAIL0" ] && ok "MTU sweep ${MTU_LIST:-1200..1500} x ${MTU_CARRIERS:-tcp/udp/quic/wss}: all delivered" || bad "MTU sweep: $((FAIL-MTU_FAIL0)) combination(s) failed (see table)"
fi

# ---------------------------------------------------------------- shutdown/restart
if [[ " $SECTIONS " == *" shutdown "* ]]; then
say "clean shutdown, interface destruction and restart"
setup_ns 1500; : >"$W/a.log"
write_b '  - {carrier: tcp, address: "192.0.2.2:7000"}'; write_a "[{name: e1, address: 192.0.2.2}]" "[{type: tcp, port: 7000}]"
startB; sleep 0.3; startA; wait_for "tcp/*" 15
A ip -o link show tun0 >/dev/null 2>&1 && A ip -o addr show tun0 | grep -q 10.200.0.1/30 && ok "tun0 created with 10.200.0.1/30" || bad "tun0 not created"
A ip route get 10.200.0.2 | grep -q "dev tun0" && ok "route to 10.200.0.2 via tun0" || bad "route"
t0=$(now); stop a && ok "SIGTERM: daemon exited in $(since $t0)s" || bad "daemon did not exit on SIGTERM"
A ip link show tun0 >/dev/null 2>&1 && bad "tun0 still present after shutdown" || ok "tun0 removed on shutdown"
grep -qiE "panic|goroutine [0-9]+ \[" "$W/a.log" && bad "panic in log" || ok "no panic on shutdown"
for i in 1 2 3; do startA; wait_for "tcp/*" 15 && A ping -c1 -W2 10.200.0.2 >/dev/null && r=ok || r=FAIL; stop a; [ $r = ok ] || break; done
[ $r = ok ] && ok "restart x3: tunnel back up each time" || bad "restart cycle"
stop b
fi

# ---------------------------------------------------------------- reverse tunnel, multiple mappings
if [[ " $SECTIONS " == *" reverse "* ]]; then
say "reverse tunnel: REMOTE (ta) dials out only; EDGE (tb) exposes 2 TCP + 2 UDP services; remote reconnect"
setup_ns 1500; : >"$W/a.log"; : >"$W/b.log"
write_b '  - {carrier: tcp, address: "192.0.2.2:443"}
  - {carrier: wss, address: "192.0.2.2:8443"}' 'forwarding:
  tcp: [{listen: "192.0.2.2:8080", target: "10.200.0.1:80"}, {listen: "192.0.2.2:2222", target: "10.200.0.1:22", max_connections: 5}]
  udp: [{listen: "192.0.2.2:5353", target: "10.200.0.1:53"}, {listen: "192.0.2.2:51820", target: "10.200.0.1:51820"}]'
write_a "[{name: edge, address: 192.0.2.2}]" "[{type: tcp, port: 443}, {type: wss, port: 8443}]"
A iptables -A INPUT -i va -p tcp --syn -j DROP; A iptables -A INPUT -i va -p udp -m state --state NEW -j DROP 2>/dev/null
startB; sleep 0.3; startA; wait_for "tcp/*" 15 && ok "remote dialed edge over TCP/443" || bad "reverse up"
# services bind 10.200.0.1, which only exists once tun0 is up
A python3 - tuunel-ext-helper >/dev/null 2>&1 <<'PY' &
import socket, threading
def t(port, tag):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("10.200.0.1",port)); s.listen()
    while True:
        c,_=s.accept(); c.sendall(tag); c.close()
def u(port, tag):
    x=socket.socket(2,2); x.bind(("10.200.0.1",port))
    while True:
        d,a=x.recvfrom(9000); x.sendto(tag+d,a)
for p,tg in ((80,b"http"),(22,b"ssh")): threading.Thread(target=t,args=(p,tg),daemon=True).start()
threading.Thread(target=u,args=(53,b"dns:"),daemon=True).start(); u(51820,b"wg:")
PY
sleep 0.5
check_maps() {
  local r; r=$(B python3 -c '
import socket
def tc(p):
    s=socket.create_connection(("192.0.2.2",p),timeout=3); return s.recv(10).decode()
def uc(p):
    u=socket.socket(2,2); u.settimeout(3); u.sendto(b"q",("192.0.2.2",p)); return u.recvfrom(100)[0].decode()
print(tc(8080),tc(2222),uc(5353),uc(51820))' 2>&1)
  [ "$r" = "http ssh dns:q wg:q" ]; }
check_maps && ok "4 mappings (tcp 8080,2222 / udp 5353,51820) through reverse tunnel" || bad "mappings"
stop a; sleep 1
check_maps && bad "mappings answered while remote down" || ok "mappings fail cleanly while remote is down"
t0=$(now); startA; wait_for "tcp/*" 20
check_maps && ok "remote reconnected in $(since $t0)s; all mappings restored" || bad "mappings after reconnect"
B iptables -A INPUT -p tcp --dport 443 -j DROP; wait_for "wss/*" 30 && check_maps && ok "reverse tunnel carrier failover to WSS keeps mappings" || bad "reverse failover"
B iptables -F INPUT
stop a; stop b
fi

say "results: $PASS passed, $FAIL failed, $SKIP skipped"
[ $FAIL -eq 0 ]

#!/usr/bin/env bash
# Bounded long-run resource test through real TUN devices (root).
#   sudo tests/lab/netns-soak.sh [bin-dir] [seconds=360]
# Continuous inner TCP stream (~rate-limited) + UDP echo + ping, a forced
# carrier failover every 60 s (TCP blocked / unblocked) and periodic samples of
# RSS, goroutines, heap, open fds and CPU of both daemons. Fails on goroutine
# or fd growth, RSS doubling, stream death or panics.
set -u
export PATH="$PATH:/usr/sbin:/sbin"
BIN=${1:-$(cd "$(dirname "$0")/../.." && pwd)/bin}; DUR=${2:-360}
W=$(mktemp -d /tmp/tuunel-soak.XXXX)
A() { ip netns exec ta "$@"; }; B() { ip netns exec tb "$@"; }
cleanup() { pkill -f "tuunel run -config $W/" 2>/dev/null; pkill -f tuunel-soak-helper 2>/dev/null; sleep 0.3; ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null; echo "logs kept in $W"; }
trap cleanup EXIT
ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null
ip netns add ta; ip netns add tb
ip link add va netns ta type veth peer name vb netns tb
A ip addr add 192.0.2.1/24 dev va; B ip addr add 192.0.2.2/24 dev vb
for n in ta tb; do ip netns exec $n ip link set lo up; done; A ip link set va up; B ip link set vb up
KA=$("$BIN/tuunel" genkey 2>"$W/a.pub"); KB=$("$BIN/tuunel" genkey 2>"$W/b.pub")
PUBA=$(sed 's/public key: //' "$W/a.pub"); PUBB=$(sed 's/public key: //' "$W/b.pub")
umask 077; echo "$KA" >"$W/a.key"; echo "$KB" >"$W/b.key"; umask 022
cat >"$W/b.yaml" <<Y
node: {id: b}
interface: {name: tun0, addresses: ["10.200.0.2/30"]}
security: {private_key_file: $W/b.key, rekey_interval: 1m}
listen: [{carrier: tcp, address: "192.0.2.2:7000"}, {carrier: quic, address: "192.0.2.2:7002"}, {carrier: wss, address: "192.0.2.2:7003"}]
peers: [{name: a, public_key: "$PUBA", allowed_ips: ["10.200.0.1/32"]}]
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s}
api: {socket: $W/b.sock}
log: {level: info}
Y
cat >"$W/a.yaml" <<Y
node: {id: a}
interface: {name: tun0, addresses: ["10.200.0.1/30"]}
security: {private_key_file: $W/a.key, rekey_interval: 1m}
peers:
  - {name: b, public_key: "$PUBB", allowed_ips: ["10.200.0.2/32"], endpoints: [{name: b, address: 192.0.2.2}],
     carriers: [{type: tcp, port: 7000}, {type: quic, port: 7002}, {type: wss, port: 7003}]}
health: {interval: 500ms, ping_timeout: 1s, idle_timeout: 5s, failed_after_missed: 4}
failover: {backoff_initial: 500ms, backoff_max: 3s, min_hold: 3s, probe_interval: 2s, recovery_successes: 2}
api: {socket: $W/a.sock}
log: {level: info}
Y
B "$BIN/tuunel" run -config "$W/b.yaml" >"$W/b.log" 2>&1 &
sleep 0.4
A "$BIN/tuunel" run -config "$W/a.yaml" >"$W/a.log" 2>&1 &
for _ in $(seq 1 50); do A ping -c1 -W1 10.200.0.2 >/dev/null 2>&1 && break; sleep 0.2; done
PA=$(pgrep -f "tuunel run -config $W/a.yaml"); PB=$(pgrep -f "tuunel run -config $W/b.yaml")
B python3 - tuunel-soak-helper >/dev/null 2>&1 <<'PY' &
import socket, threading
def tcp():
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("10.200.0.2",9000)); s.listen()
    while True:
        c,_=s.accept()
        def h(c):
            try:
                while (d:=c.recv(65536)): c.sendall(d)
            except OSError: pass
        threading.Thread(target=h,args=(c,),daemon=True).start()
def udp():
    u=socket.socket(2,2); u.bind(("10.200.0.2",9001))
    while True:
        d,a=u.recvfrom(65535); u.sendto(d,a)
threading.Thread(target=tcp,daemon=True).start(); udp()
PY
sleep 0.5
A python3 - tuunel-soak-helper "$DUR" >"$W/stream.log" 2>&1 <<'PY' &
import socket,time,os,sys,threading
dur=float(sys.argv[2]); end=time.time()+dur
s=socket.create_connection(("10.200.0.2",9000),timeout=60); s.settimeout(60)
blk=os.urandom(16384); sent=[0]; recv=[0]
def rd():
    while recv[0]<1<<62:
        d=s.recv(1<<16)
        if not d: break
        recv[0]+=len(d)
threading.Thread(target=rd,daemon=True).start()
u=socket.socket(2,2); u.settimeout(1); uok=0; un=0
while time.time()<end:
    t=time.time()
    for _ in range(40): s.sendall(blk); sent[0]+=len(blk)   # ~5 Mbit/s per 100ms tick budget ~ 50 Mbit/s
    un+=1
    try:
        u.sendto(b"x"*512,("10.200.0.2",9001)); uok+= u.recvfrom(1000)[0]==b"x"*512
    except OSError: pass
    time.sleep(max(0,0.1-(time.time()-t)))
time.sleep(3)
print("tcp_sent=%d tcp_echoed=%d udp=%d/%d" % (sent[0],recv[0],uok,un), flush=True)
assert recv[0] >= sent[0]*0.99
PY
STREAM=$!
A ping -i 0.2 -W 1 -w "$DUR" 10.200.0.2 >"$W/ping.log" 2>&1 &
metric() { $1 curl -s --max-time 2 --unix-socket "$2" http://x/api/metrics | awk -v k="$3" '$1==k{print $2}'; }
sample() {
  printf "%5s %-6s | A rss=%6sKB gor=%4s fds=%3s cpu=%5ss | B rss=%6sKB gor=%4s fds=%3s cpu=%5ss\n" "$1" "$(A curl -s --max-time 2 --unix-socket $W/a.sock http://x/api/status | python3 -c 'import sys,json;p=json.load(sys.stdin)["engine"]["peers"][0];print(p["carrier"] if p["up"] else "down")' 2>/dev/null)" \
    "$(awk '/VmRSS/{print $2}' /proc/$PA/status)" "$(metric A $W/a.sock tuunel_goroutines)" "$(ls /proc/$PA/fd | wc -l)" "$(awk -v t=$(getconf CLK_TCK) '{print ($14+$15)/t}' /proc/$PA/stat)" \
    "$(awk '/VmRSS/{print $2}' /proc/$PB/status)" "$(metric B $W/b.sock tuunel_goroutines)" "$(ls /proc/$PB/fd | wc -l)" "$(awk -v t=$(getconf CLK_TCK) '{print ($14+$15)/t}' /proc/$PB/stat)"
}
T0=$(date +%s); i=0
while [ $(( $(date +%s) - T0 )) -lt "$DUR" ]; do
  el=$(( $(date +%s) - T0 ))
  sample "${el}s" | tee -a "$W/samples"
  # forced failover: block TCP for 20 s every minute
  if [ $(( el % 60 )) -lt 15 ] && [ $el -ge 30 ] && ! B iptables -C INPUT -p tcp --dport 7000 -j DROP 2>/dev/null; then B iptables -A INPUT -p tcp --dport 7000 -j DROP; echo "      [block tcp]"; fi
  if [ $(( el % 60 )) -ge 35 ] && B iptables -C INPUT -p tcp --dport 7000 -j DROP 2>/dev/null; then B iptables -D INPUT -p tcp --dport 7000 -j DROP; echo "      [unblock tcp]"; fi
  sleep 15
done
B iptables -F INPUT
wait $STREAM; SR=$?
sample end | tee -a "$W/samples"
echo "stream: $(cat "$W/stream.log" | tail -2)"; grep -E "packet loss" "$W/ping.log"
grep -E "carrier_switches|reconnects" <(A curl -s --unix-socket $W/a.sock http://x/api/status | python3 -m json.tool) | head -2
python3 - "$W/samples" "$SR" "$W" <<'PY'
import re,sys
rows=[l for l in open(sys.argv[1]) if "rss=" in l]
def parse(l):
    v=re.findall(r"rss=\s*(\d+)KB gor=\s*(\d+) fds=\s*(\d+)",l); return [tuple(map(int,x)) for x in v]
first=parse(rows[2] if len(rows)>3 else rows[0]); last=parse(rows[-1]); bad=[]
for side,(f,l) in zip("AB",zip(first,last)):
    if l[1]>f[1]+25: bad.append(f"{side}: goroutines {f[1]}->{l[1]}")
    if l[2]>f[2]+10: bad.append(f"{side}: fds {f[2]}->{l[2]}")
    if l[0]>2*f[0]: bad.append(f"{side}: rss {f[0]}->{l[0]}KB")
if sys.argv[2]!="0": bad.append("inner TCP stream failed")
log=open(sys.argv[3]+"/a.log").read()+open(sys.argv[3]+"/b.log").read()
if "panic" in log: bad.append("panic in logs")
# Longest ping outage: each forced block should cost one failover (~3-4 s at
# 5 pps); a much longer gap means a dead link went undetected.
seqs={int(x) for x in re.findall(r"icmp_seq=(\d+)",open(sys.argv[3]+"/ping.log").read())}
gap=run=0
for i in range(1,max(seqs or {0})+1):
    run=run+1 if i not in seqs else 0; gap=max(gap,run)
print("longest ping outage: %d pings (~%.1fs)" % (gap, gap*0.2))
if gap*0.2>10: bad.append("ping outage %.1fs > 10s" % (gap*0.2))
print("SOAK RESULT:", "FAIL "+"; ".join(bad) if bad else "PASS (no goroutine/fd growth, RSS stable, stream intact, outages <=10s)")
sys.exit(1 if bad else 0)
PY

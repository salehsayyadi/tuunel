#!/usr/bin/env bash
# Per-carrier end-to-end benchmark through real TUN devices in network
# namespaces: TCP goodput through the tunnel, in-tunnel RTT, daemon CPU and RSS.
#   sudo tests/lab/netns-bench.sh [bin-dir] [MiB]
set -u
export PATH="$PATH:/usr/sbin:/sbin"
BIN=${1:-$(cd "$(dirname "$0")/../.." && pwd)/bin}; MB=${2:-50}
W=$(mktemp -d /tmp/tuunel-bench.XXXX)
A() { ip netns exec ta "$@"; }; B() { ip netns exec tb "$@"; }
cleanup() { pkill -f "tuunel run -config $W/" 2>/dev/null; [ -n "${SINK:-}" ] && kill "$SINK" 2>/dev/null; sleep 0.3; ip netns del ta 2>/dev/null; ip netns del tb 2>/dev/null; }
trap cleanup EXIT
SINK=
cleanup
ip netns add ta; ip netns add tb
ip link add va netns ta type veth peer name vb netns tb
A ip addr add 192.0.2.1/24 dev va; B ip addr add 192.0.2.2/24 dev vb
for n in ta tb; do ip netns exec $n ip link set lo up; done; A ip link set va up; B ip link set vb up
KA=$("$BIN/tuunel" genkey 2>"$W/a.pub"); KB=$("$BIN/tuunel" genkey 2>"$W/b.pub")
PUBA=$(sed 's/public key: //' "$W/a.pub"); PUBB=$(sed 's/public key: //' "$W/b.pub")
umask 077; echo "$KA" >"$W/a.key"; echo "$KB" >"$W/b.key"; umask 022
B python3 -c '
import socket,sys
sys.argv[0]="tuunel-bench-sink"
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",9000)); s.listen()
while True:
    c,_=s.accept()
    while c.recv(1<<20): pass
    c.sendall(b"k"); c.close()' tuunel-bench-sink &
SINK=$!
cpu() { awk '{print $14+$15}' /proc/$1/stat; }
rss() { awk '/VmRSS/{print $2}' /proc/$1/status; }
printf "%-12s %12s %10s %12s %12s %10s %10s\n" CARRIER "GOODPUT" "RTT" "CPU A(s)" "CPU B(s)" "RSS A" "RSS B"
for spec in "tcp 7000" "udp 7001" "quic 7002" "quic-dgram 7004" "wss 7003"; do
  set -- $spec; name=$1; port=$2; typ=$name; extra=""
  [ "$name" = quic-dgram ] && { typ=quic; extra=", datagrams: true"; }
  cat >"$W/b.yaml" <<Y
node: {id: b}
interface: {name: tun0, addresses: ["10.200.0.2/30"]}
security: {private_key_file: $W/b.key}
listen: [{carrier: $typ, address: "192.0.2.2:$port"$extra}]
peers: [{name: a, public_key: "$PUBA", allowed_ips: ["10.200.0.1/32"]}]
api: {socket: $W/b.sock}
log: {level: warn}
Y
  cat >"$W/a.yaml" <<Y
node: {id: a}
interface: {name: tun0, addresses: ["10.200.0.1/30"]}
security: {private_key_file: $W/a.key}
peers:
  - {name: b, public_key: "$PUBB", allowed_ips: ["10.200.0.2/32"], endpoints: [{name: b, address: 192.0.2.2}], carriers: [{type: $typ, port: $port$extra}]}
api: {socket: $W/a.sock}
log: {level: warn}
Y
  B "$BIN/tuunel" run -config "$W/b.yaml" >"$W/b-$name.log" 2>&1 &
  sleep 0.4
  A "$BIN/tuunel" run -config "$W/a.yaml" >"$W/a-$name.log" 2>&1 &
  for _ in $(seq 1 50); do A ping -c1 -W1 10.200.0.2 >/dev/null 2>&1 && break; sleep 0.2; done
  PA=$(pgrep -f "tuunel run -config $W/a.yaml"); PB=$(pgrep -f "tuunel run -config $W/b.yaml")
  rtt=$(A ping -c 20 -i 0.05 -q 10.200.0.2 | awk -F/ '/rtt/{print $5"ms"}')
  ca=$(cpu $PA); cb=$(cpu $PB)
  gp=$(A python3 -c "
import socket,time
d=b'x'*(1<<20); s=socket.create_connection(('10.200.0.2',9000),timeout=30); t=time.time()
for _ in range($MB): s.sendall(d)
s.shutdown(socket.SHUT_WR); s.recv(1); print('%.0f Mbit/s' % ($MB*8*1.048576/(time.time()-t)))" 2>&1 | tail -1)
  tck=$(getconf CLK_TCK)
  da=$(( $(cpu $PA) - ca )); db=$(( $(cpu $PB) - cb ))
  printf "%-12s %12s %10s %12s %12s %8sKB %8sKB\n" "$name" "$gp" "${rtt:-?}" "$(awk "BEGIN{print $da/$tck}")" "$(awk "BEGIN{print $db/$tck}")" "$(rss $PA)" "$(rss $PB)"
  pkill -f "tuunel run -config $W/"; sleep 0.8
done
echo "($MB MiB TCP stream per carrier through tun0; CPU = user+sys seconds consumed by each daemon during the transfer)"

#!/usr/bin/env python3
"""Route-all ("exit") mode with installer-generated configs (--route-all).

Topology: edge = namespace b (192.0.2.2, listener), remote = namespace a
(192.0.2.1, dialer), plus an "internet" namespace x reachable ONLY from the
remote (203.0.113.10). Checks:
  - edge-originated TCP/ICMP to the internet goes through the tunnel and is
    NATed by the remote (web server sees 203.0.113.1)
  - inbound services on the edge keep working (reply direction -> main table),
    including a TCP connection established BEFORE route-all was applied
  - IPv6 outbound from the edge is blocked (no leak)
  - exit-down removes every rule

  sudo python3 tests/lab/exit_check.py EDGE_ROOT REMOTE_ROOT out.json
"""
import json, os, re, subprocess, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab, BIN, sh

edge, remote, out = sys.argv[1], sys.argv[2], sys.argv[3]
lab = Lab("ex", impair=False)
nx = "exx"
R, ok, procs = {}, False, []
def X(cmd, t=20): return sh(f"ip netns exec {nx} {cmd}", t)
def bg(ns, args, log):
    p = subprocess.Popen(["ip", "netns", "exec", ns] + args, stdout=subprocess.DEVNULL, stderr=open(log, "w"))
    procs.append(p); return p
try:
    lab.up()
    sh(f"ip netns del {nx} 2>/dev/null; ip netns add {nx}")
    sh(f"ip link add vx netns {nx} type veth peer name ax netns {lab.na}", check=True)
    sh(f"ip -n {nx} addr add 203.0.113.10/24 dev vx; ip -n {nx} link set vx up; ip -n {nx} link set lo up")
    sh(f"ip -n {lab.na} addr add 203.0.113.1/24 dev ax; ip -n {lab.na} link set ax up")
    for who, root in (("b", edge), ("a", remote)):
        cfg = open(f"{root}/etc/tuunel/config.yaml").read()
        open(f"{lab.w}/{who}.yaml", "w").write(cfg)
        sock = re.search(r"socket: (\S+)\}", cfg).group(1)
        if os.path.lexists(f"{lab.w}/{who}.sock"): os.unlink(f"{lab.w}/{who}.sock")
        os.symlink(sock, f"{lab.w}/{who}.sock")
    R["exit_lines"] = [l for l in open(f"{lab.w}/b.yaml").read().splitlines() + open(f"{lab.w}/a.yaml").read().splitlines() if l.startswith("exit:")]
    open(f"{lab.w}/hello.txt", "w").write("ok\n")
    bg(nx, ["python3", "-m", "http.server", "18080", "--bind", "203.0.113.10", "--directory", lab.w], f"{lab.w}/inet-web.log")
    bg(lab.nb, ["python3", "-m", "http.server", "18081", "--bind", "192.0.2.2", "--directory", lab.w], f"{lab.w}/edge-web.log")
    # long-lived inbound TCP connection opened BEFORE route-all is applied
    bg(lab.nb, ["python3", "-c", "import socket\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('192.0.2.2',18082));s.listen()\n"
               "c,_=s.accept()\nwhile True:\n d=c.recv(100)\n if not d: break\n c.sendall(d)"], f"{lab.w}/echo.log")
    time.sleep(1)
    cl = subprocess.Popen(["ip", "netns", "exec", lab.na, "python3", "-c",
        "import socket,sys,time\ns=socket.create_connection(('192.0.2.2',18082),5);s.sendall(b'a');assert s.recv(1)==b'a'\n"
        "sys.stdout.write('connected\\n');sys.stdout.flush();sys.stdin.readline();s.settimeout(8);s.sendall(b'b')\n"
        "print('echo-after:'+s.recv(1).decode())"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    R["preexisting_connected"] = cl.stdout.readline().strip() == "connected"
    R["edge_direct_before"] = lab.B("curl -s -m 3 -o /dev/null -w %{http_code} http://203.0.113.10:18080/hello.txt", 6).stdout.strip()
    lab.start("b"); time.sleep(0.5); lab.start("a")
    R["up_s"] = lab.wait_carrier(".*", 20)
    for who, ns in (("b", lab.nb), ("a", lab.na)):
        r = sh(f"ip netns exec {ns} {BIN}/tuunel exit-up -config {lab.w}/{who}.yaml", 40)
        R[f"exit_up_{who}"] = [l for l in (r.stdout + r.stderr).strip().splitlines() if l.startswith("exit")]
    # kernels without nft NAT (some CI/sandbox kernels): emulate an Internet that routes the tunnel subnet back
    R["nat_supported"] = not any("NAT" in l and "unavailable" in l for l in R["exit_up_a"])
    if not R["nat_supported"]:
        X("ip route add 10.200.0.0/30 via 203.0.113.1")
    R["edge_ip_rules"] = [l for l in lab.B("ip rule").stdout.splitlines() if "52" in l.split(":")[0]][:4]
    R["edge_to_internet"] = lab.B("curl -s -m 8 -o /dev/null -w %{http_code} http://203.0.113.10:18080/hello.txt", 12).stdout.strip()
    R["edge_ping_internet"] = lab.B("ping -c 3 -W 2 203.0.113.10", 10).returncode == 0
    R["dbg_route_get"] = lab.B("ip route get 203.0.113.10").stdout.strip()
    R["dbg_a_forward"] = lab.A("cat /proc/sys/net/ipv4/ip_forward").stdout.strip()
    R["dbg_x_route"] = X("ip route").stdout.strip()
    for who in ("a", "b"):
        st = json.loads(lab.api(who, "/api/status") or "{}")
        eng = st.get("engine", st)
        R[f"dbg_{who}_drops"] = eng.get("drops")
        R[f"dbg_{who}_peers"] = [(p.get("name"), p.get("tx_packets"), p.get("rx_packets"), p.get("drops")) for p in eng.get("peers", [])]
    R["inbound_to_edge"] = lab.A("curl -s -m 5 -o /dev/null -w %{http_code} http://192.0.2.2:18081/hello.txt", 8).stdout.strip()
    cl.stdin.write("go\n"); cl.stdin.flush()
    R["preexisting_after"] = (cl.communicate(timeout=15)[0] or "").strip()
    R["edge_ipv6_blocked"] = lab.B("curl -s -m 3 -o /dev/null -w %{http_code} http://[2001:db8::1]:9/", 6).stdout.strip() in ("000", "")
    R["edge_ipv6_err"] = lab.B("ping -6 -c 1 -W 1 2001:db8::1", 4).stderr.strip()[:80]
    iw = open(f"{lab.w}/inet-web.log").read()
    R["internet_saw_remote_nat"] = ("203.0.113.1 - -" if R["nat_supported"] else "10.200.0.1 - -") in iw
    ew = open(f"{lab.w}/edge-web.log").read()
    R["edge_saw_client"] = "192.0.2.1 - -" in ew
    sh(f"ip netns exec {lab.nb} {BIN}/tuunel exit-down -config {lab.w}/b.yaml", 20)
    sh(f"ip netns exec {lab.na} {BIN}/tuunel exit-down -config {lab.w}/a.yaml", 20)
    rules = lab.B("ip rule").stdout + lab.B("ip -6 rule").stdout
    R["clean_after_down"] = not re.search(r"^52\d\d:", rules, re.M) and "tuunel_exit" not in lab.B("nft list tables").stdout \
        and "tuunel_exit" not in lab.A("nft list tables").stdout
    lab.stop("a"); R["edge_exit"] = lab.stop("b")
    ok = (R["edge_direct_before"] in ("000", "") and R["edge_to_internet"] == "200" and R["internet_saw_remote_nat"] and
          R["edge_ping_internet"] and R["inbound_to_edge"] == "200" and R["edge_saw_client"] and
          R["preexisting_connected"] and R["preexisting_after"] == "echo-after:b" and R["edge_ipv6_blocked"] and
          R["clean_after_down"] and len(R["exit_lines"]) == 2)
finally:
    for p in procs:
        p.kill()
    sh(f"ip netns del {nx}")
    lab.cleanup()
R["result"] = "PASS" if ok else "FAIL"
json.dump(R, open(out, "w"), indent=1)
print(json.dumps(R, indent=1))
sys.exit(0 if ok else 1)

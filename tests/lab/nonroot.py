#!/usr/bin/env python3
"""Unprivileged-runtime check (root to set up). Emulates the production
runtimes without systemd/Docker: both daemons run as uid/gid 10001 with only
file capabilities (cap_net_admin,cap_net_raw,cap_net_bind_service) on the
binary -- the same model as the Docker runtime image (setcap + USER 10001)
and close to systemd's User=tuunel + AmbientCapabilities. nft gets
cap_net_admin (copy, PATH-preferred) for the ICMP reply filter.
Every carrier is forced in turn by blocking the others; each must carry
ping and TCP traffic. Also checks the process really is non-root."""
import json, os, re, shutil, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab, sh, BIN

ORDER = ["tcp", "udp", "quic", "wss", "icmp"]
lab = Lab(os.environ.get("PREFIX", "nr"))
R, ok = {"uid": 10001, "carriers": {}}, True
try:
    caps = os.path.join(lab.w, "caps"); os.makedirs(caps)
    shutil.copy(f"{BIN}/tuunel", caps); shutil.copy(shutil.which("nft") or "/usr/sbin/nft", caps)
    lab.bin_a = lab.bin_b = f"{caps}/tuunel"
    lab.wrap = ["setpriv", "--reuid=10001", "--regid=10001", "--clear-groups", "env", f"PATH={caps}:/usr/sbin:/usr/bin"]
    lab.up(); lab.keys(); lab.configure(ORDER); lab.helpers_b()
    sh(f"chown -R 10001:10001 {lab.w}; chmod 0700 {lab.w}", check=True)
    # setcap AFTER chown: chown(2) clears security.capability
    sh(f"chown root:root {caps}/tuunel {caps}/nft", check=True)
    sh(f"setcap cap_net_admin,cap_net_raw,cap_net_bind_service+ep {caps}/tuunel", check=True)
    sh(f"setcap cap_net_admin+ep {caps}/nft", check=True)
    lab.start("b"); time.sleep(0.5); lab.start("a")
    R["connect_s"] = lab.wait_carrier("tcp/.*", 20)
    for who in ("a", "b"):
        pid = lab.procs[who].pid
        # the Popen child is "ip netns exec" which execs setpriv -> env -> tuunel (same pid)
        st = open(f"/proc/{pid}/status").read()
        R[f"{who}_uid"] = re.search(r"Uid:\s+(\d+)", st).group(1)
        R[f"{who}_capeff"] = re.search(r"CapEff:\s+(\w+)", st).group(1)
    for c in ORDER:
        lab.block(*[x for x in ORDER if x != c])
        t = lab.wait_carrier(f"{c}/.*", 45)
        r = {"switch_s": t, "ping": lab.ping(count=50, interval=0.02), "tcp": lab.iperf(2)}
        if c == "icmp":
            out = lab.B("nft list table ip tuunel_icmp").stdout
            m = re.search(r"counter packets (\d+)", out)
            r["reply_filter_installed_by_nonroot"] = bool(m)
            r["kernel_replies_dropped"] = int(m.group(1)) if m else 0
        r["ok"] = t is not None and r["ping"]["loss_pct"] <= 4 and (r["tcp"].get("mbit") or 0) > 1
        ok &= r["ok"]
        R["carriers"][c] = r
        print(c, json.dumps(r), flush=True)
    lab.block()
    lab.stop("a"); R["b_exit"] = lab.stop("b")
    R["filter_removed"] = lab.B("nft list table ip tuunel_icmp").returncode != 0
    ok &= R["a_uid"] == "10001" and R["b_uid"] == "10001" and R["b_exit"] == 0 and R["filter_removed"] \
        and R["carriers"]["icmp"].get("reply_filter_installed_by_nonroot", False)
finally:
    lab.cleanup()
R["result"] = "PASS" if ok else "FAIL"
print(json.dumps(R, indent=1))
sys.exit(0 if ok else 1)

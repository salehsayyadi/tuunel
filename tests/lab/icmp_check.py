#!/usr/bin/env python3
"""ICMP carrier host-impact check (root). Verifies the tunnel works WITHOUT
net.ipv4.icmp_echo_ignore_all, that normal ping to the host still works, that
the narrow nftables reply filter is installed while running and removed on
shutdown, and that kernel replies to tunnel requests are actually dropped."""
import json, re, sys, time
sys.path.insert(0, __import__("os").path.dirname(__file__))
from labkit import Lab

res, ok = {}, True
lab = Lab("ic", impair=False)
try:
    lab.up(); lab.keys(); lab.configure(["icmp"]); lab.helpers_b()
    res["sysctl_echo_ignore_all_before"] = lab.B("sysctl -n net.ipv4.icmp_echo_ignore_all").stdout.strip()
    lab.start("b"); time.sleep(0.4); lab.start("a")
    res["connect_s"] = lab.wait_carrier("icmp/.*", 15)
    res["inner_ping"] = lab.ping(count=100, interval=0.02)
    res["inner_tcp"] = lab.iperf(3)
    res["host_ping_underlay"] = lab.ping("192.0.2.2", count=10, interval=0.1)
    res["host_ping_tunnel_ip"] = lab.ping("10.200.0.2", count=10, interval=0.1)
    t = lab.B("nft list table ip tuunel_icmp").stdout
    m = re.search(r"counter packets (\d+)", t)
    res["filter_installed"] = bool(m)
    res["kernel_replies_dropped"] = int(m.group(1)) if m else 0
    res["sysctl_echo_ignore_all_during"] = lab.B("sysctl -n net.ipv4.icmp_echo_ignore_all").stdout.strip()
    lab.stop("a"); rc = lab.stop("b")
    res["b_exit_code"] = rc
    res["filter_removed_on_shutdown"] = lab.B("nft list table ip tuunel_icmp").returncode != 0
    ok = (res["connect_s"] is not None and res["inner_ping"]["loss_pct"] <= 2 and
          res["host_ping_underlay"]["loss_pct"] == 0 and res["host_ping_tunnel_ip"]["loss_pct"] == 0 and
          res["filter_installed"] and res["kernel_replies_dropped"] > 0 and res["filter_removed_on_shutdown"]
          and res["sysctl_echo_ignore_all_during"] == "0" and rc == 0)
finally:
    lab.cleanup()
res["result"] = "PASS" if ok else "FAIL"
print(json.dumps(res, indent=1))
sys.exit(0 if ok else 1)

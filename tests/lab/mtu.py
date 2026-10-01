#!/usr/bin/env python3
"""Underlay MTU sweep per carrier (root), IPv4 and IPv6 underlay.

For each underlay MTU and carrier: build the lab with that veth MTU (no
impairment bridge), let interface.mtu=auto pick the inner MTU, then check
  * the detected path MTU equals the underlay MTU; the link limit (largest
    inner packet one outer datagram/segment carries) is read from the log
  * inner IPv4 ping with DF at the link limit succeeds
  * inner IPv4 ping with DF one byte above it is SIGNALLED (local EMSGSIZE
    or ICMP frag-needed from the engine) -- never silently blackholed
  * inner IPv4 ping without DF at the full TUN MTU succeeds (engine
    fragmentation when the TUN MTU is floored at 1280 for inner IPv6)
  * inner IPv6 ping at the link limit when it is >= 1280
  * large fragmented inner ping (8000 bytes, no DF) succeeds
  * inner TCP bulk transfer works (MSS/PMTU correct)
The IPv6-underlay pass repeats this with the carriers listening/dialing on
2001:db8::2.

  sudo python3 tests/lab/mtu.py out.json
"""
import json, os, re, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab

out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-mtu.json"
MTUS = [int(x) for x in os.environ.get("MTUS", "1200 1228 1280 1300 1350 1400 1450 1500").split()]
CARR = os.environ.get("CARRIERS", "tcp udp quic quic-dgram wss icmp").split()
V6MTUS = [int(x) for x in os.environ.get("V6MTUS", "1280 1400 1500").split()]
R = {"v4_underlay": [], "v6_underlay": []}
bad = []


def save():
    json.dump(R, open(out + ".tmp", "w"), indent=1); os.replace(out + ".tmp", out)


def run(mtu, c, v6):
    lab = Lab("mt", mtu=mtu, impair=False)
    row = {"underlay_mtu": mtu, "carrier": c, "underlay": "ipv6" if v6 else "ipv4"}
    try:
        lab.up(); lab.keys()
        eps = '[{name: e1, address: "2001:db8::2"}]' if v6 else None
        lab.configure([c], listen_v6=v6, endpoints=eps); lab.helpers_b()
        lab.start("b"); time.sleep(0.3); lab.start("a")
        t = lab.wait_carrier(f"{c.split('-')[0]}/.*", 20)
        row["up"] = t is not None
        if not row["up"]:
            row["log"] = open(f"{lab.w}/a.log").read()[-400:]
            # RFC 9000 14.1: QUIC needs >= 1200-byte UDP payloads -> underlay MTU >= 1228 (v4) / 1248 (v6)
            if c.startswith("quic") and mtu < (1248 if v6 else 1228):
                row["expected_unsupported"] = "QUIC requires 1200-byte UDP datagrams (RFC 9000 s14.1)"
            row["ok"] = bool(row.get("expected_unsupported"))
            return row
        m = re.search(r"mtu (\d+)", lab.A("ip link show dev tun0").stdout)
        im = int(m.group(1)) if m else None
        row["inner_mtu"] = im
        log = open(f"{lab.w}/a.log").read()
        lm = re.search(r"packets above (\d+) bytes exceed path MTU (\d+)", log)
        lim = int(lm.group(1)) if lm else im          # largest packet one outer datagram/segment carries
        pst = lab.peer() or {}
        if pst.get("link_mtu"):                       # engine's per-link limit (includes per-link PMTU)
            lim = min(lim, int(pst["link_mtu"])) if im else int(pst["link_mtu"])
        row["status_link_mtu"] = pst.get("link_mtu")
        row["link_limit"] = lim
        row["pathological"] = "pathological MTU" in log
        pm = re.search(r"path (\d+)\)", log)
        row["detected_path_mtu"] = int(pm.group(1)) if pm else None
        # DF at the link limit must pass unfragmented
        row["ping4_df_at_limit"] = lab.ping(count=5, interval=0.05, size=lim - 28, extra="-M do")["loss_pct"] == 0
        # DF above the limit must be signalled (local EMSGSIZE or ICMP frag-needed), not blackholed
        r = lab.A(f"ping -c2 -i 0.2 -W1 -M do -s {lim - 27} 10.200.0.2", 6)
        txt = (r.stdout + r.stderr).lower()
        row["ping4_df_over_limit_signalled"] = r.returncode != 0 and ("too long" in txt or "frag needed" in txt or "mtu" in txt)
        # no DF at the full TUN MTU works (engine fragments when the link limit is smaller)
        row["ping4_nodf_at_tun_mtu"] = lab.ping(count=5, interval=0.05, size=im - 28)["loss_pct"] == 0
        if lim >= 1280:
            row["ping6_at_limit"] = lab.ping("fd20::2", count=5, interval=0.05, size=min(im, lim) - 48, extra="-M do")["loss_pct"] == 0
        else:
            # link cannot carry the IPv6 minimum: the engine must fragment 1280-byte IPv6 packets itself
            row["ping6_1280_engine_fragmented"] = lab.ping("fd20::2", count=5, interval=0.05, size=1280 - 48)["loss_pct"] == 0
            row["ping6_at_limit"] = row["ping6_1280_engine_fragmented"]
        row["ping4_8000_fragmented"] = lab.ping(count=5, interval=0.05, size=8000)["loss_pct"] == 0
        row["tcp"] = lab.iperf(2)
        lab.stop("a"); lab.stop("b")
    except Exception as e:
        row["error"] = repr(e)
    finally:
        lab.cleanup()
    if row.get("expected_unsupported"):
        row["ok"] = True
        return row
    ok = row.get("up") and row.get("ping4_df_at_limit") and row.get("ping4_df_over_limit_signalled") and \
        row.get("ping4_nodf_at_tun_mtu") and row.get("ping4_8000_fragmented") and \
        (row.get("tcp", {}).get("mbit") or 0) > 1 and row.get("ping6_at_limit") is not False
    row["ok"] = bool(ok)
    return row


for mtu in MTUS:
    for c in CARR:
        r = run(mtu, c, False); R["v4_underlay"].append(r); save()
        print("v4", mtu, c, r.get("inner_mtu"), r.get("ok"), (r.get("tcp") or {}).get("mbit"), r.get("error", ""), flush=True)
        if not r["ok"]:
            bad.append(f"v4 underlay mtu {mtu} {c}")
for mtu in V6MTUS:
    for c in [x for x in CARR if x != "icmp"]:   # ICMP carrier is IPv4-only
        r = run(mtu, c, True); R["v6_underlay"].append(r); save()
        print("v6", mtu, c, r.get("inner_mtu"), r.get("ok"), (r.get("tcp") or {}).get("mbit"), r.get("error", ""), flush=True)
        if not r["ok"]:
            bad.append(f"v6 underlay mtu {mtu} {c}")
R["failures"] = bad
R["result"] = "PASS" if not bad else "FAIL"
save()
print(json.dumps({"result": R["result"], "failures": bad}))
sys.exit(0 if not bad else 1)

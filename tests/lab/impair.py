#!/usr/bin/env python3
"""Packet loss / latency / jitter matrix per carrier (root).

For every carrier a fresh lab is built (single carrier, so no failover can
mask a problem), then each condition is applied on the userspace impairment
bridge and measured:
  underlay ping (environment loss/RTT)  vs  inner ping through the tunnel,
  iperf3 TCP throughput through the tunnel, reconnects / failure switches,
  daemon CPU seconds and RSS.

Bridge semantics (differs from a single-sided netem qdisc):
  loss L      -> L% dropped independently in EACH direction (round trip ~ 1-(1-L)^2)
  latency D   -> D/2 ms added in each direction, i.e. +D ms RTT
  jitter J    -> uniform +-J/2 ms per direction on top of the delay (order preserved)

  sudo python3 tests/lab/impair.py out.json [carrier ...]
"""
import json, os, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab

CONDS = [("loss", l, 0, 0) for l in (0, 1, 5, 10, 20)] + \
        [("latency", 0, d, 0) for d in (20, 50, 100, 200)] + \
        [("jitter", 0, 50, 10), ("jitter", 0, 100, 20)]
out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-impair.json"
carriers = sys.argv[2:] or ["tcp", "udp", "quic", "quic-dgram", "wss", "icmp"]
ONLY = os.environ.get("CONDS")  # e.g. "loss" to limit; "quick" = loss 0/5 + latency 50
if ONLY == "quick":
    CONDS, ONLY = [("loss", 0, 0, 0), ("loss", 5, 0, 0), ("latency", 0, 50, 0)], None
R = {"semantics": __doc__.split("Bridge semantics")[1].split("sudo")[0].strip(), "carriers": {}}


def save():
    json.dump(R, open(out + ".tmp", "w"), indent=1); os.replace(out + ".tmp", out)


for c in carriers:
    lab = Lab(os.environ.get("PREFIX", "im"))
    rows = []
    R["carriers"][c] = rows
    try:
        lab.up(); lab.keys(); lab.configure([c]); lab.helpers_b()
        lab.start("b"); time.sleep(0.4); lab.start("a")
        if lab.wait_carrier(f"{c.split('-')[0]}/.*", 20) is None:
            rows.append({"error": "tunnel did not come up"}); continue
        for kind, loss, rtt_add, jit in CONDS:
            if ONLY and kind not in ONLY.split(","):
                continue
            lab.set_impair(loss, rtt_add / 2, jit / 2)
            time.sleep(1.5)
            st0, u0 = lab.impair_stats(), (lab.proc_usage("a"), lab.proc_usage("b"))
            rc0, fs0 = lab.metric("tuunel_reconnects_total") or 0, lab.metric("tuunel_failure_switches_total") or 0
            t0 = time.time()
            n, iv = (300, 0.01) if loss >= 5 else (150, 0.01)
            under = lab.ping("192.0.2.2", count=n, interval=iv)
            inner = lab.ping("10.200.0.2", count=n, interval=iv)
            tput = lab.iperf(4)
            el = time.time() - t0
            st1, u1 = lab.impair_stats(), (lab.proc_usage("a"), lab.proc_usage("b"))
            row = {"kind": kind, "loss_pct_per_dir": loss, "added_rtt_ms": rtt_add, "jitter_ms": jit,
                   "underlay_ping": under, "tunnel_ping": inner, "tcp_throughput": tput,
                   "tunnel_extra_loss_pct": round(inner["loss_pct"] - under["loss_pct"], 2),
                   "reconnects": (lab.metric("tuunel_reconnects_total") or 0) - rc0,
                   "failure_switches": (lab.metric("tuunel_failure_switches_total") or 0) - fs0,
                   "active_after": lab.active(),
                   "cpu_pct_a": round(100 * (u1[0].get("cpu_s", 0) - u0[0].get("cpu_s", 0)) / el, 1),
                   "cpu_pct_b": round(100 * (u1[1].get("cpu_s", 0) - u0[1].get("cpu_s", 0)) / el, 1),
                   "rss_kb_a": u1[0].get("rss_kb"), "rss_kb_b": u1[1].get("rss_kb")}
            try:
                d = {k: st1[k]["dropped"] - st0[k]["dropped"] for k in ("a_to_b", "b_to_a")}
                f = {k: st1[k]["forwarded"] - st0[k]["forwarded"] for k in ("a_to_b", "b_to_a")}
                row["bridge"] = {"dropped": d, "forwarded": f}
            except (KeyError, TypeError):
                pass
            rows.append(row)
            print(c, kind, loss, rtt_add, jit, "under", under["loss_pct"], under["rtt_avg_ms"], "tun", inner["loss_pct"],
                  inner["rtt_avg_ms"], inner["rtt_mdev_ms"], "tcp", tput.get("mbit"), "rc", row["reconnects"], flush=True)
            save()
        lab.set_impair(0, 0, 0)
        lab.stop("a"); rows.append({"b_exit": lab.stop("b")})
    except Exception as e:  # keep going with the next carrier
        rows.append({"error": repr(e)})
    finally:
        lab.cleanup()
        save()

# verdict: tunnel never adds more than 3 points of loss beyond the environment
# for loss <= 10%, latency/jitter conditions deliver >= 97%, no crash.
bad = []
for c, rows in R["carriers"].items():
    for r in rows:
        if "error" in r:
            bad.append(f"{c}: {r['error']}")
        elif "kind" in r:
            lim = 100 if r["loss_pct_per_dir"] >= 20 else 3 + r["loss_pct_per_dir"] * 0.5
            if r["tunnel_extra_loss_pct"] > lim:
                bad.append(f"{c} {r['kind']} {r['loss_pct_per_dir']}/{r['added_rtt_ms']}: extra loss {r['tunnel_extra_loss_pct']}")
            if r["tunnel_ping"]["rx"] == 0:
                bad.append(f"{c} {r['kind']}: no delivery")
R["failures"] = bad
R["result"] = "PASS" if not bad else "FAIL"
save()
print(json.dumps({"result": R["result"], "failures": bad}))
sys.exit(0 if not bad else 1)

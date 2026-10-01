#!/usr/bin/env python3
"""Metrics reflect reality (root). Each metric is triggered on purpose and
compared with an independent measurement:

  RTT / jitter      +50 ms RTT, +-5 ms jitter on the bridge  vs  tuunel_rtt_seconds / tuunel_jitter_seconds
  loss              10 %/dir loss on the bridge             vs  tuunel_packet_loss_ratio > 0
  bytes / packets   N pings of S bytes                      vs  tx/rx bytes and packets deltas
  active carrier    tuunel_active_carrier_info == status
  switches          block udp -> carrier_switches +1, failure_switches +1, reconnects +1
  preempt           unblock   -> carrier_switches +1, failure_switches +0
  tunnel_up, build_info, self-metrics present; every sample line parses

  sudo python3 tests/lab/metrics_check.py out.json
"""
import json, os, re, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab

out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-metrics.json"
R, bad = {"checks": {}}, []
SAMPLE = re.compile(r'^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="(\\.|[^"\\])*",?)*\})? -?[0-9.eE+-]+$|^[a-zA-Z_:][a-zA-Z0-9_:]*(\{.*\})? (NaN|\+Inf|-Inf)$')


def check(name, ok, **detail):
    R["checks"][name] = {"ok": bool(ok), **detail}
    print("PASS" if ok else "FAIL", name, detail, flush=True)
    if not ok:
        bad.append(name)


lab = Lab(os.environ.get("PREFIX", "me"))
try:
    lab.up(); lab.keys()
    lab.configure(["udp", "tcp"], failover="{backoff_initial: 500ms, backoff_max: 3s, min_hold: 3s, probe_interval: 2s, recovery_successes: 2, switch_on_degraded: false}")
    lab.helpers_b(); lab.start("b"); time.sleep(0.4); lab.start("a")
    check("tunnel up on udp", lab.wait_carrier("udp/.*", 20) is not None)
    text = lab.metrics()
    lines = [l for l in text.splitlines() if l and not l.startswith("#")]
    badl = [l for l in lines if not SAMPLE.match(l)]
    check("prometheus text parses", lines and not badl, samples=len(lines), bad=badl[:3])
    for m in ("tuunel_tunnel_up", "tuunel_build_info", "tuunel_goroutines", "tuunel_heap_inuse_bytes", "tuunel_open_fds"):
        check(f"{m} present", lab.metric(m) is not None, value=lab.metric(m))
    check("tunnel_up == 1", lab.metric("tuunel_tunnel_up") == 1)
    check("active_carrier_info matches status", lab.metric("tuunel_active_carrier_info", labels='carrier="udp"') == 1, status=lab.active())

    # bytes / packets: 200 pings x 1000 bytes payload (1028-byte IP packets) each way
    tx0, rx0 = lab.metric("tuunel_tx_bytes_total"), lab.metric("tuunel_rx_bytes_total")
    tp0, rp0 = lab.metric("tuunel_tx_packets_total"), lab.metric("tuunel_rx_packets_total")
    p = lab.ping(count=200, interval=0.01, size=1000)
    time.sleep(0.5)
    dtx, drx = lab.metric("tuunel_tx_bytes_total") - tx0, lab.metric("tuunel_rx_bytes_total") - rx0
    dtp, drp = lab.metric("tuunel_tx_packets_total") - tp0, lab.metric("tuunel_rx_packets_total") - rp0
    exp = p["rx"] * 1028
    check("tx bytes >= ping bytes (+ health overhead <= 25%)", exp <= dtx <= exp * 1.25 + 20000, delta=dtx, expected_min=exp)
    check("rx bytes >= ping bytes", exp <= drx <= exp * 1.25 + 20000, delta=drx, expected_min=exp)
    check("tx/rx packets >= pings", dtp >= p["tx"] and drp >= p["rx"], tx=dtp, rx=drp, pings=p["tx"])

    # RTT / jitter
    lab.set_impair(0, 25, 5); time.sleep(12)
    rtt, jit = lab.metric("tuunel_rtt_seconds"), lab.metric("tuunel_jitter_seconds")
    pr = lab.ping(count=40, interval=0.05)
    check("rtt metric ~ measured ping RTT (+50ms)", rtt and abs(rtt * 1000 - pr["rtt_avg_ms"]) < 15 and rtt > 0.04,
          metric_ms=rtt and round(rtt * 1000, 2), ping_ms=pr["rtt_avg_ms"])
    check("jitter metric > 0 with injected jitter", jit is not None and jit > 0.0005, metric_ms=jit and round(jit * 1000, 2), ping_mdev_ms=pr["rtt_mdev_ms"])

    # loss
    lab.set_impair(10, 0, 0); time.sleep(20)
    loss = lab.metric("tuunel_packet_loss_ratio")
    check("loss metric > 0 under 10%/dir loss", loss is not None and 0 < loss < 0.6, metric=loss)
    lab.set_impair(0, 0, 0); time.sleep(25)
    loss2 = lab.metric("tuunel_packet_loss_ratio")
    check("loss metric returns to ~0", loss2 is not None and loss2 < 0.1, metric=loss2)

    # failure switch
    c0 = {m: lab.metric(m) or 0 for m in ("tuunel_carrier_switches_total", "tuunel_failure_switches_total", "tuunel_reconnects_total", "tuunel_endpoint_switches_total")}
    lab.block("udp")
    ok = lab.wait_carrier("tcp/.*", 30) is not None; time.sleep(1)
    c1 = {m: lab.metric(m) or 0 for m in c0}
    d = {m: c1[m] - c0[m] for m in c0}
    check("block udp -> switch to tcp", ok, active=lab.active())
    check("failure switch counted", d["tuunel_carrier_switches_total"] == 1 and d["tuunel_failure_switches_total"] == 1 and d["tuunel_reconnects_total"] >= 1 and d["tuunel_endpoint_switches_total"] == 0, delta=d)
    check("active_carrier_info follows switch", lab.metric("tuunel_active_carrier_info", labels='carrier="tcp"') == 1)
    lab.block()
    ok = lab.wait_carrier("udp/.*", 60) is not None; time.sleep(1)
    c2 = {m: lab.metric(m) or 0 for m in c0}
    d = {m: c2[m] - c1[m] for m in c0}
    check("preempt back to udp", ok, active=lab.active())
    check("preempt counted as switch, not failure", d["tuunel_carrier_switches_total"] == 1 and d["tuunel_failure_switches_total"] == 0, delta=d)
    lab.stop("a"); R["b_exit"] = lab.stop("b")
except Exception as e:
    bad.append(repr(e))
finally:
    lab.cleanup()
R["failures"], R["result"] = bad, ("PASS" if not bad else "FAIL")
json.dump(R, open(out, "w"), indent=1)
print(json.dumps({"result": R["result"], "failures": bad}))
sys.exit(0 if not bad else 1)

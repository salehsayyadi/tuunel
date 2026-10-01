#!/usr/bin/env python3
"""Long-run soak (root). Multi-carrier tunnel (udp, quic, tcp, wss) through
the impairment bridge (default 1% loss/dir, 10±2 ms delay/dir) with:
  * a long-lived TCP echo stream (integrity-checked) for the whole run,
  * a continuous UDP echo flow (integrity-checked, loss/outage measured),
  * an iperf3 TCP burst every BURST_EVERY seconds,
  * the active carrier blocked for BLOCK_FOR seconds every BLOCK_EVERY seconds
    (BLOCK_DEPTH=2: then the new active one too -> consecutive failures),
    asserting that tunnel traffic flows again after every switch,
and samples RSS / Go heap / goroutines / fds / CPU / counters of both daemons
every SAMPLE seconds. Leak verdict compares the first and last 20% of samples.

  SOAK_SECONDS=1800 TUUNEL_BIN=/path/bin sudo -E python3 tests/lab/soak.py out.json
"""
import json, os, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab

DUR = int(os.environ.get("SOAK_SECONDS", "1800"))
SAMPLE = int(os.environ.get("SAMPLE", "30"))
BLOCK_EVERY, BLOCK_FOR = int(os.environ.get("BLOCK_EVERY", "600")), int(os.environ.get("BLOCK_FOR", "60"))
BURST_EVERY = int(os.environ.get("BURST_EVERY", "300"))
DEPTH = int(os.environ.get("BLOCK_DEPTH", "1"))  # 2 = active and its successor fail back to back
LOSS, DELAY, JIT = (float(x) for x in os.environ.get("IMPAIR", "1 10 2").split())
out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-soak.json"

lab = Lab(os.environ.get("PREFIX", "sk"))
R = {"duration_s": DUR, "impair": [LOSS, DELAY, JIT], "samples": [], "blocks": [], "bursts": []}


def snap(t0):
    s = {"t": round(time.time() - t0), "active": lab.active()}
    for w in ("a", "b"):
        s[w] = dict(lab.proc_usage(w))
        for m, k in (("tuunel_goroutines", "gr"), ("tuunel_heap_inuse_bytes", "heap"), ("tuunel_open_fds", "fds_m")):
            s[w][k] = lab.metric(m, w)
    for m in ("tuunel_carrier_switches_total", "tuunel_failure_switches_total", "tuunel_reconnects_total",
              "tuunel_tx_bytes_total", "tuunel_rx_bytes_total"):
        s[m.replace("tuunel_", "").replace("_total", "")] = lab.metric(m, "a")
    return s


def save():
    tmp = out + ".tmp"
    json.dump(R, open(tmp, "w"), indent=1)
    os.replace(tmp, out)


try:
    lab.up(); lab.keys(); lab.configure(["udp", "quic", "tcp", "wss"]); lab.helpers_b()
    lab.set_impair(LOSS, DELAY, JIT)
    lab.start("b"); time.sleep(0.4); lab.start("a")
    R["connect_s"] = lab.wait_carrier("udp/.*", 20)
    t0 = time.time()
    ptcp = lab.probe("tcp", "tcp", duration=DUR + 5, interval=0.1)
    pudp = lab.probe("udp", "udp", duration=DUR + 5, interval=0.02, size=512)
    next_block, next_burst, blocked_until = BLOCK_EVERY, BURST_EVERY, None
    while time.time() - t0 < DUR:
        el = time.time() - t0
        if blocked_until is None and el >= next_block:
            chain, sws = [], []
            for _ in range(DEPTH):   # consecutive failures: kill the active carrier DEPTH times
                act = lab.active().split("/")[0]
                if act not in ("udp", "quic", "tcp", "wss") or act in chain:
                    break
                chain.append(act); lab.block(*chain)
                sws.append(lab.wait_carrier(f"(?!{act}/).*/.*", 30))
                time.sleep(3)
            blocked_until = el + BLOCK_FOR
            R["blocks"].append({"t": round(el), "blocked": chain, "switch_s": sws[0] if sws else None, "switch_each_s": sws,
                                "traffic_ok_after": lab.ping(count=20, interval=0.05)["loss_pct"] < 50})
            next_block += BLOCK_EVERY
        if blocked_until is not None and el >= blocked_until:
            lab.block(); R["blocks"][-1]["restored_t"] = round(el)
            R["blocks"][-1]["recovered_to_udp_s"] = lab.wait_carrier("udp/.*", 60)
            blocked_until = None
        if el >= next_burst:
            R["bursts"].append({"t": round(el), **lab.iperf(5)})
            next_burst += BURST_EVERY
        R["samples"].append(snap(t0))
        R["probe_tcp"], R["probe_udp"] = lab.read_probe(ptcp), lab.read_probe(pudp)
        save()
        time.sleep(SAMPLE)
    lab.block()
    time.sleep(8)
    R["probe_tcp"], R["probe_udp"] = lab.read_probe(ptcp), lab.read_probe(pudp)
    R["final"] = snap(t0)
    R["impair_stats"] = lab.impair_stats()
    lab.stop("a"); R["b_exit"] = lab.stop("b")
finally:
    lab.cleanup()

S = R["samples"]
n = max(1, len(S) // 5)
def avg(key, w):
    head = [x[w].get(key) or 0 for x in S[:n]]; tail = [x[w].get(key) or 0 for x in S[-n:]]
    return round(sum(head) / len(head)), round(sum(tail) / len(tail))
R["trend"] = {w: {k: avg(k, w) for k in ("rss_kb", "heap", "gr", "fds")} for w in ("a", "b")}
leak = []
for w in ("a", "b"):
    t = R["trend"][w]
    if t["rss_kb"][1] > 1.5 * t["rss_kb"][0] + 8192: leak.append(f"{w} rss")
    if t["gr"][1] > t["gr"][0] + 20: leak.append(f"{w} goroutines")
    if t["fds"][1] > t["fds"][0] + 10: leak.append(f"{w} fds")
pt, pu = R.get("probe_tcp", {}), R.get("probe_udp", {})
R["leaks"] = leak
# liveness at the end (traffic must flow after the last restore) and
# stability (switches beyond the induced blocks indicate flapping)
problems = []
if (pt.get("cur_gap_s") or 0) > 30 or (pu.get("cur_gap_s") or 0) > 30:
    problems.append(f"traffic dead at end: tcp gap {pt.get('cur_gap_s')}s udp gap {pu.get('cur_gap_s')}s")
if any(b.get("mbit") is None for b in R["bursts"]):
    problems.append(f"{sum(b.get('mbit') is None for b in R['bursts'])} iperf bursts failed")
sw = (R.get("final") or {}).get("carrier_switches") or 0
induced = sum(2 * max(1, len(b["blocked"]) if isinstance(b["blocked"], list) else 1) for b in R["blocks"])
R["switches"] = {"total": sw, "induced_upper_bound": induced, "per_hour": round(sw * 3600 / max(1, DUR), 1)}
if sw > induced + DUR / 600:   # allow ~1 spontaneous switch per 10 min
    problems.append(f"flapping: {sw} carrier switches for {len(R['blocks'])} induced outages")
R["problems"] = problems
R["result"] = "PASS" if (not leak and not problems and pt.get("reconnects") == 0 and pt.get("integrity_errors") == 0
                         and pu.get("integrity_errors") == 0 and all(b.get("switch_s") is not None and b.get("traffic_ok_after") for b in R["blocks"])
                         and R.get("b_exit") == 0) else "FAIL"
save()
print(json.dumps({k: R[k] for k in ("result", "leaks", "problems", "switches", "trend", "blocks", "bursts", "probe_tcp", "probe_udp", "final")}, indent=1))
sys.exit(0 if R["result"] == "PASS" else 1)

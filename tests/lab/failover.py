#!/usr/bin/env python3
"""Real carrier / endpoint failover with live traffic (root).

Sections (argv[2:], default all):
  carriers   udp > quic > tcp > wss > icmp. Consecutive real failures: the
             ACTIVE carrier is blocked (nftables drop on B's underlay, so the
             carrier dies mid-flow, no clean close) until only ICMP is left,
             then ICMP too (total outage), then ICMP restored, then everything
             restored (preemption back to udp). Repeated CYCLES times.
             Live during the whole run, all integrity-checked:
               inner long-lived TCP stream + inner UDP flow (A->B tunnel IP),
               forwarded TCP + UDP (A -> B:8080/5353 -> tunnel -> A:9100/9101),
             asserting detection/switch time, outage, TCP survival and the
             Prometheus counters (carrier/failure switches, active carrier info).
  endpoints  2 endpoints (e1 192.0.2.2, e2 192.0.2.3): e1 address blocked ->
             failover to e2, restore -> preempt back to e1.
  reverse    reverse-tunnel data integrity: 32 MiB random data through the
             forwarded TCP service (edge listens, remote dials) with sha256
             compare, while the active carrier is blocked mid-transfer.

  sudo python3 tests/lab/failover.py out.json [sections...]
"""
import hashlib, json, os, socket, subprocess, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab

out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-failover.json"
SECTIONS = sys.argv[2:] or ["carriers", "endpoints", "reverse"]
CYCLES = int(os.environ.get("CYCLES", "3"))
ORDER = ["udp", "quic", "tcp", "wss", "icmp"]
FWD = '''forwarding:
  tcp: [{listen: "192.0.2.2:8080", target: "10.200.0.1:9100"}]
  udp: [{listen: "192.0.2.2:5353", target: "10.200.0.1:9101"}]'''
R = {"sections": {}}
bad = []


def save():
    json.dump(R, open(out + ".tmp", "w"), indent=1); os.replace(out + ".tmp", out)


def counters(lab):
    return {k: lab.metric(f"tuunel_{k}_total") or 0 for k in ("carrier_switches", "failure_switches", "endpoint_switches", "reconnects")}


def active_info(lab):
    for line in lab.metrics("a").splitlines():
        if line.startswith("tuunel_active_carrier_info{"):
            return line.split("{", 1)[1].split("}")[0]
    return ""


def gaps_between(probe, t0, t1):
    """Longest outage of a probe that started within [t0-1s, t1]."""
    g = [s for (st, s) in probe.get("gaps", []) if t0 - 1.0 <= st <= t1]
    return max(g) if g else 0.0


def event(lab, probes, name, blocked, expect, timeout=60):
    c0, t0 = counters(lab), time.time()
    lab.block(*blocked)
    sw = lab.wait_carrier(expect, timeout)
    time.sleep(2.5)   # let probes record the end of the outage
    t1 = time.time()
    c1 = counters(lab)
    ev = {"event": name, "blocked": list(blocked), "expect": expect, "switch_s": sw, "active": lab.active(),
          "active_info": active_info(lab), "delta": {k: c1[k] - c0[k] for k in c1}, "t0": t0, "t1": t1}
    return ev


def attribute_outages(evs, probes, lab):
    """Post-hoc: a probe outage belongs to the event during whose window it
    started (gaps are only logged once traffic resumes, possibly after t1)."""
    data = {n: lab.read_probe(p) for n, p in probes.items()}
    for e in evs:
        for n, d in data.items():
            e[f"outage_{n}_s"] = gaps_between(d, e["t0"], e["t1"])


def sec_carriers():
    lab = Lab("fo")
    res = {"cycles": []}
    try:
        lab.up(); lab.keys(); lab.configure(ORDER, b_extra=FWD); lab.helpers_b(); lab.helpers_a()
        lab.start("b"); time.sleep(0.4); lab.start("a")
        res["connect_s"] = lab.wait_carrier("udp/.*", 20)
        time.sleep(1)
        dur = CYCLES * 200 + 60
        probes = {"inner_tcp": lab.probe("itcp", "tcp", duration=dur, interval=0.1),
                  "inner_udp": lab.probe("iudp", "udp", duration=dur, interval=0.02, size=512),
                  "fwd_tcp": lab.probe("ftcp", "tcp", dst="192.0.2.2", port=8080, duration=dur, interval=0.1),
                  "fwd_udp": lab.probe("fudp", "udp", dst="192.0.2.2", port=5353, duration=dur, interval=0.02, size=512)}
        time.sleep(3)
        for cyc in range(CYCLES):
            evs = []
            for i, c in enumerate(ORDER[:-1]):
                evs.append(event(lab, probes, f"{c} fails", ORDER[:i + 1], f"{ORDER[i + 1]}/.*"))
            evs.append(event(lab, probes, "all carriers fail", ORDER, "down", 30))
            time.sleep(8)  # total outage window
            evs.append(event(lab, probes, "icmp restored", ORDER[:-1], "icmp/.*", 60))
            evs.append(event(lab, probes, "all restored (preempt to udp)", [], "udp/.*", 90))
            time.sleep(3)
            attribute_outages(evs, probes, lab)
            for e in evs:
                print(cyc, e["event"], "switch", e["switch_s"], "->", e["active"], {k: v for k, v in e.items() if k.startswith("outage")}, e["delta"], flush=True)
            res["cycles"].append(evs)
            R["sections"]["carriers"] = res; save()
        time.sleep(3)
        res["probes"] = {n: lab.read_probe(p) for n, p in probes.items()}
        for n in probes:
            res["probes"][n].pop("gaps", None)
        lab.block(); lab.stop("a"); res["b_exit"] = lab.stop("b")
    finally:
        lab.cleanup()
    # assertions
    for cyc in res["cycles"]:
        for e in cyc:
            if e["switch_s"] is None:
                bad.append(f"carriers: {e['event']} never reached {e['expect']}")
            if e["expect"] != "down" and e["expect"].split("/")[0] not in e["active_info"]:
                bad.append(f"carriers: active_carrier_info {e['active_info']!r} after {e['event']}")
            # the switch away from a dying carrier is either a failure switch
            # (link declared dead) or a degrade switch (loss rose first);
            # both must be counted as carrier switches
            if e["event"].endswith("fails") and e["delta"]["carrier_switches"] < 1:
                bad.append(f"carriers: no carrier switch counted on {e['event']}")
            # a switch is a change of the active candidate; "icmp restored"
            # re-establishes the candidate that was active before the outage
            if e["event"] != "icmp restored" and e["expect"] != "down" and e["delta"]["carrier_switches"] < 1:
                bad.append(f"carriers: carrier_switches not incremented on {e['event']}")
            if e["event"] == "icmp restored" and e["delta"]["reconnects"] < 1:
                bad.append("carriers: reconnects not incremented when icmp came back")
    for n in ("inner_tcp", "fwd_tcp"):
        p = res.get("probes", {}).get(n, {})
        if p.get("reconnects") != 0 or p.get("integrity_errors") != 0 or not p.get("received"):
            bad.append(f"carriers: {n} did not survive: {p.get('reconnects')} reconnects {p.get('integrity_errors')} integrity errors")
    for n in ("inner_udp", "fwd_udp"):
        p = res.get("probes", {}).get(n, {})
        if p.get("integrity_errors") != 0 or not p.get("received"):
            bad.append(f"carriers: {n} integrity/delivery failure")
    if res.get("b_exit") != 0:
        bad.append("carriers: B exit code")
    fs = sum(e["delta"]["failure_switches"] for c in res["cycles"] for e in c if e["event"].endswith("fails"))
    if fs < 1:
        bad.append("carriers: failure_switches never incremented")
    res["failure_vs_degrade"] = {"failure_switches": fs, "fail_events": sum(1 for c in res["cycles"] for e in c if e["event"].endswith("fails"))}
    return res


def sec_endpoints():
    lab = Lab("fe", endpoints=2)
    res = {}
    try:
        lab.up(); lab.keys(); lab.configure(["udp", "tcp"]); lab.helpers_b()
        lab.start("b"); time.sleep(0.4); lab.start("a")
        res["connect_s"] = lab.wait_carrier("udp/e1", 20)
        probes = {"inner_tcp": lab.probe("etcp", "tcp", duration=150, interval=0.1),
                  "inner_udp": lab.probe("eudp", "udp", duration=150, interval=0.02, size=512)}
        time.sleep(2)
        evs = []
        for r in range(2):
            evs.append(event_addr(lab, probes, "endpoint e1 unreachable", ["192.0.2.2"], "(udp|tcp)/e2"))
            evs.append(event(lab, probes, "e1 restored (preempt)", [], "udp/e1", 90))
        time.sleep(3)
        attribute_outages(evs, probes, lab)
        for e in evs:
            print("endpoints", e["event"], e["switch_s"], e["active"], e["delta"], {k: v for k, v in e.items() if k.startswith("outage")}, flush=True)
        res["events"] = evs
        res["probes"] = {n: lab.read_probe(p) for n, p in probes.items()}
        for n in probes:
            res["probes"][n].pop("gaps", None)
        lab.block(); lab.stop("a"); res["b_exit"] = lab.stop("b")
    finally:
        lab.cleanup()
    for e in res.get("events", []):
        if e["switch_s"] is None:
            bad.append(f"endpoints: {e['event']} never reached {e['expect']}")
    if not any(e["delta"]["endpoint_switches"] >= 1 for e in res.get("events", [])):
        bad.append("endpoints: endpoint_switches never incremented")
    p = res.get("probes", {}).get("inner_tcp", {})
    if p.get("reconnects") != 0 or p.get("integrity_errors") != 0:
        bad.append("endpoints: TCP stream did not survive endpoint failover")
    return res


def event_addr(lab, probes, name, addrs, expect, timeout=60):
    c0, t0 = counters(lab), time.time()
    lab.block(addrs=addrs)
    sw = lab.wait_carrier(expect, timeout)
    time.sleep(2.5)
    t1 = time.time(); c1 = counters(lab)
    return {"event": name, "blocked": addrs, "expect": expect, "switch_s": sw, "active": lab.active(),
            "delta": {k: c1[k] - c0[k] for k in c1}, "t0": t0, "t1": t1}


def sec_reverse():
    lab = Lab("fr")
    res = {}
    try:
        lab.up(); lab.keys(); lab.configure(["tcp", "wss", "udp"], b_extra=FWD); lab.helpers_a()
        # remote (A) accepts no inbound underlay connections at all: it only dials out
        r = lab.A("sh -c 'iptables -A INPUT -i va -p tcp --syn -j DROP && iptables -A INPUT -i va -p udp -m conntrack --ctstate NEW -j DROP'")
        res["remote_inbound_blocked"] = r.returncode == 0
        lab.start("b"); time.sleep(0.4); lab.start("a")
        res["connect_s"] = lab.wait_carrier("tcp/.*", 20)
        time.sleep(1)
        code = r'''
import hashlib, os, socket, sys, threading, time
n = int(sys.argv[1]); data = os.urandom(n); want = hashlib.sha256(data).hexdigest()
s = socket.create_connection(("192.0.2.2", 8080), timeout=10); s.settimeout(60)
def tx():
    v = memoryview(data)
    for i in range(0, n, 65536):
        s.sendall(v[i:i+65536])
threading.Thread(target=tx, daemon=True).start()
h, got, t0 = hashlib.sha256(), 0, time.time()
while got < n:
    d = s.recv(1 << 20)
    if not d: break
    h.update(d); got += len(d)
print(want == h.hexdigest(), got, round(time.time() - t0, 2))
'''
        runs = []
        for label, blk in (("steady", None), ("carrier blocked mid-transfer", "tcp")):
            p = subprocess.Popen(["ip", "netns", "exec", lab.na, "timeout", "110", "python3", "-c", code, str(32 << 20)],
                                 stdout=subprocess.PIPE, text=True)
            if blk:
                time.sleep(0.5); lab.block(blk)
            o = p.communicate(timeout=120)[0].split()
            runs.append({"case": label, "sha256_match": o[:1] == ["True"], "bytes": int(o[1]) if len(o) > 1 else 0,
                         "seconds": float(o[2]) if len(o) > 2 else None, "active_after": lab.active()})
            print("reverse", runs[-1], flush=True)
            lab.block()
            lab.wait_carrier("tcp/.*", 60)
        res["transfers"] = runs
        # UDP mapping integrity through the reverse tunnel
        uc = r'''
import os, socket
u = socket.socket(2, 2); u.settimeout(2); ok = 0
for i in range(200):
    d = os.urandom(1200); u.sendto(d, ("192.0.2.2", 5353))
    try:
        ok += u.recvfrom(2000)[0] == d
    except OSError: pass
print(ok)
'''
        res["udp_mapping_ok_of_200"] = int(lab.A(f"python3 -c '{uc}'", 60).stdout.strip() or 0)
        lab.stop("a"); res["b_exit"] = lab.stop("b")
    finally:
        lab.cleanup()
    for r in res.get("transfers", []):
        if not r["sha256_match"]:
            bad.append(f"reverse: integrity failure ({r['case']})")
    if res.get("udp_mapping_ok_of_200", 0) < 190:
        bad.append("reverse: UDP mapping delivery < 95%")
    return res


for s in SECTIONS:
    R["sections"][s] = {"carriers": sec_carriers, "endpoints": sec_endpoints, "reverse": sec_reverse}[s]()
    save()
R["failures"] = bad
R["result"] = "PASS" if not bad else "FAIL"
save()
print(json.dumps({"result": R["result"], "failures": bad}, indent=1))
sys.exit(0 if not bad else 1)

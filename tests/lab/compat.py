#!/usr/bin/env python3
"""Mixed-version compatibility (root): OLD dialer <-> NEW listener and
NEW dialer <-> OLD listener over every stream/datagram carrier, plus one
failover (udp blocked -> next carrier) per pairing.

  OLD_BIN=/path/to/old/bin sudo -E python3 tests/lab/compat.py out.json
Build OLD_BIN from a previous release/commit, e.g.:
  git worktree add /tmp/old <commit> && (cd /tmp/old && go build -o /tmp/oldbin/ ./cmd/tuunel)
"""
import json, os, subprocess, sys, time
sys.path.insert(0, os.path.dirname(__file__))
from labkit import Lab, BIN

OLD = os.environ.get("OLD_BIN", "/data/oldbin")
out = sys.argv[1] if len(sys.argv) > 1 else "/tmp/tuunel-compat.json"
CARR = ["udp", "quic", "quic-dgram", "tcp", "wss"]
ver = lambda b: subprocess.run([f"{b}/tuunel", "version"], capture_output=True, text=True).stdout.splitlines()[:2]
R = {"old": ver(OLD), "new": ver(BIN), "pairs": []}
bad = []
for name, a, b in (("old-dialer/new-listener", OLD, BIN), ("new-dialer/old-listener", BIN, OLD)):
    for c in CARR:
        lab = Lab("cp", impair=False)
        row = {"pair": name, "carrier": c}
        try:
            lab.bin_a, lab.bin_b = f"{a}/tuunel", f"{b}/tuunel"
            lab.up(); lab.keys(); lab.configure([c, "tcp"] if c != "tcp" else ["tcp", "udp"]); lab.helpers_b()
            lab.start("b"); time.sleep(0.4); lab.start("a")
            row["up_s"] = lab.wait_carrier(f"{c.split('-')[0]}/.*", 20)
            row["ping"] = lab.ping(count=30, interval=0.02)
            row["tcp"] = lab.iperf(2)
            lab.block(c)
            row["failover_s"] = lab.wait_carrier(f"(?!{c.split('-')[0]}/).*/.*", 30)
            time.sleep(2)
            row["ping_after_failover"] = lab.ping(count=20, interval=0.05)
            lab.block()
            lab.stop("a"); row["b_exit"] = lab.stop("b")
        except Exception as e:
            row["error"] = repr(e)
        finally:
            lab.cleanup()
        row["ok"] = (row.get("up_s") is not None and row.get("ping", {}).get("loss_pct", 100) <= 5 and
                     (row.get("tcp", {}).get("mbit") or 0) > 1 and row.get("failover_s") is not None and
                     row.get("ping_after_failover", {}).get("loss_pct", 100) <= 10)
        if not row["ok"]:
            bad.append(f"{name} {c}")
        R["pairs"].append(row)
        print(name, c, row.get("up_s"), row.get("ping", {}).get("loss_pct"), row.get("tcp", {}).get("mbit"),
              row.get("failover_s"), row.get("ping_after_failover", {}).get("loss_pct"), row.get("error", ""), flush=True)
        json.dump(R, open(out, "w"), indent=1)
R["failures"] = bad
R["result"] = "PASS" if not bad else "FAIL"
json.dump(R, open(out, "w"), indent=1)
print(json.dumps({"result": R["result"], "failures": bad}))
sys.exit(0 if not bad else 1)

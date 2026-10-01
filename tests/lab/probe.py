#!/usr/bin/env python3
"""Traffic probe for the lab: a long-lived TCP echo stream or a UDP echo flow
with sequence numbers and payload integrity checks. Writes JSON stats to
--out once per second (atomic rename) and on exit.

  tcp: one connection for the whole run; every --interval send a payload
       (8-byte seq + sha-derived bytes), read back exactly that many bytes and
       compare. Survival = reconnects == 0 and integrity_errors == 0.
  udp: send seq-numbered datagrams; receiver thread validates and records
       per-seq delivery. Reports loss and the longest delivery gap (outage).
"""
import argparse, hashlib, json, os, socket, struct, threading, time

ap = argparse.ArgumentParser()
ap.add_argument("--mode", choices=["tcp", "udp"], required=True)
ap.add_argument("--dst", required=True)
ap.add_argument("--port", type=int, required=True)
ap.add_argument("--interval", type=float, default=0.05)
ap.add_argument("--size", type=int, default=1024)
ap.add_argument("--duration", type=float, default=60)
ap.add_argument("--out", required=True)
a = ap.parse_args()

fam = socket.AF_INET6 if ":" in a.dst else socket.AF_INET
S = {"mode": a.mode, "dst": a.dst, "port": a.port, "sent": 0, "received": 0, "integrity_errors": 0,
     "reconnects": 0, "errors": [], "max_gap_s": 0.0, "gaps_over_1s": 0, "bytes_verified": 0, "gaps": []}
lock = threading.Lock()
last_ok = [time.time()]
in_gap = [False]


def payload(seq):
    h = hashlib.sha256(struct.pack("!Q", seq)).digest()
    body = (h * (a.size // 32 + 1))[: max(0, a.size - 8)]
    return struct.pack("!Q", seq) + body


def ok_event():
    now = time.time()
    with lock:
        g = now - last_ok[0]
        if g > S["max_gap_s"]:
            S["max_gap_s"] = round(g, 3)
        if g > 1.0:
            S["gaps_over_1s"] += 1
        if g > 0.3 and len(S["gaps"]) < 500:   # outage log: start (epoch) and length
            S["gaps"].append([round(last_ok[0], 3), round(g, 3)])
        last_ok[0] = now


def dump(final=False):
    with lock:
        S["cur_gap_s"] = round(time.time() - last_ok[0], 3)
        S["loss_pct"] = round(100.0 * (S["sent"] - S["received"]) / S["sent"], 3) if S["sent"] else 0.0
        S["final"] = final
        tmp = a.out + ".tmp"
        with open(tmp, "w") as f:
            json.dump(S, f)
        os.replace(tmp, a.out)


def err(e):
    with lock:
        if len(S["errors"]) < 20:
            S["errors"].append(f"{time.strftime('%H:%M:%S')} {e}")


def recv_exact(s, n):
    b = b""
    while len(b) < n:
        d = s.recv(n - len(b))
        if not d:
            raise ConnectionError("eof")
        b += d
    return b


end = time.time() + a.duration
stop = threading.Event()


def dumper():
    while not stop.wait(1.0):
        dump()


threading.Thread(target=dumper, daemon=True).start()

if a.mode == "tcp":
    seq, s = 0, None
    while time.time() < end:
        try:
            if s is None:
                s = socket.create_connection((a.dst, a.port), timeout=5)
                # inner TCP retransmits across carrier switches; after repeated
                # back-to-back outages its RTO backoff can reach tens of
                # seconds. Survival = no reset/EOF, so wait long (the stall is
                # reported as max_gap_s).
                s.settimeout(float(os.environ.get("PROBE_TCP_TIMEOUT", "300")))
                if seq:
                    with lock:
                        S["reconnects"] += 1
            p = payload(seq)
            s.sendall(p)
            with lock:
                S["sent"] += 1
            r = recv_exact(s, len(p))
            with lock:
                if r == p:
                    S["received"] += 1
                    S["bytes_verified"] += len(p)
                else:
                    S["integrity_errors"] += 1
            ok_event()
            seq += 1
            time.sleep(a.interval)
        except OSError as e:
            err(e)
            try:
                s and s.close()
            except OSError:
                pass
            s = None
            time.sleep(0.5)
else:
    u = socket.socket(fam, socket.SOCK_DGRAM)
    u.connect((a.dst, a.port))
    u.settimeout(0.5)
    seen = set()

    def rx():
        while not stop.is_set():
            try:
                d = u.recv(65535)
            except socket.timeout:
                continue
            except OSError as e:
                err(e); time.sleep(0.1); continue
            if len(d) < 8:
                continue
            q = struct.unpack("!Q", d[:8])[0]
            with lock:
                if d != payload(q):
                    S["integrity_errors"] += 1
                    continue
                if q in seen:
                    S.setdefault("duplicates", 0)
                    S["duplicates"] += 1
                    continue
                seen.add(q)
                S["received"] += 1
                S["bytes_verified"] += len(d)
            ok_event()

    threading.Thread(target=rx, daemon=True).start()
    seq = 0
    while time.time() < end:
        try:
            u.send(payload(seq))
        except OSError as e:
            err(e)
        with lock:
            S["sent"] += 1
        seq += 1
        time.sleep(a.interval)
    time.sleep(1.5)  # drain in-flight replies
stop.set()
dump(final=True)

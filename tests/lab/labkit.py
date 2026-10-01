"""labkit: shared helpers for the real-TUN network lab (root required).

Topology (one kernel, three network namespaces):

    <P>a 192.0.2.1 --veth-- <P>m [netimpair bridge] --veth-- <P>b 192.0.2.2(.3,.4)
    tunnel: A 10.200.0.1 (dialer) <-> B 10.200.0.2 (listener)

All commands are bounded by timeouts. Results are returned as dicts so the
scenario scripts can emit JSON.
"""
import json, os, re, shutil, signal, socket, subprocess, tempfile, time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
BIN = os.environ.get("TUUNEL_BIN", os.path.join(ROOT, "bin"))

CARRIERS = {  # name -> (type, port, extra yaml)
    "tcp": ("tcp", 7000, ""), "udp": ("udp", 7001, ""), "quic": ("quic", 7002, ""),
    "quic-dgram": ("quic", 7004, ", datagrams: true"), "wss": ("wss", 7003, ""),
    "ws": ("ws", 7005, ""), "icmp": ("icmp", 0, ""),
}


def sh(cmd, timeout=30, check=False, input=None):
    try:
        r = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout, input=input)
    except subprocess.TimeoutExpired as e:
        out = e.stdout.decode(errors="replace") if isinstance(e.stdout, bytes) else (e.stdout or "")
        r = subprocess.CompletedProcess(cmd, 124, out, "timeout")
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd}: rc={r.returncode} {r.stderr.strip()}")
    return r


class Lab:
    def __init__(self, prefix="lk", mtu=1500, impair=True, endpoints=1):
        self.p = prefix
        self.na, self.nm, self.nb = prefix + "a", prefix + "m", prefix + "b"
        self.mtu, self.impair, self.endpoints = mtu, impair, endpoints
        self.w = tempfile.mkdtemp(prefix=f"/tmp/tuunel-{prefix}.")
        self.procs = {}
        self.helpers = []

    # ---------------------------------------------------------------- topology
    def up(self):
        self.down(quiet=True)
        for n in (self.na, self.nm, self.nb):
            sh(f"ip netns add {n}", check=True); sh(f"ip -n {n} link set dev lo up")
        if self.impair:
            sh(f"ip link add va netns {self.na} type veth peer name ma netns {self.nm}", check=True)
            sh(f"ip link add vb netns {self.nb} type veth peer name mb netns {self.nm}", check=True)
            ends = [(self.na, "va"), (self.nm, "ma"), (self.nm, "mb"), (self.nb, "vb")]
        else:
            sh(f"ip link add va netns {self.na} type veth peer name vb netns {self.nb}", check=True)
            ends = [(self.na, "va"), (self.nb, "vb")]
        sh(f"ip -n {self.na} addr add 192.0.2.1/24 dev va; ip -n {self.na} addr add 2001:db8::1/64 dev va nodad")
        for i in range(2, 2 + self.endpoints):
            sh(f"ip -n {self.nb} addr add 192.0.2.{i}/24 dev vb; ip -n {self.nb} addr add 2001:db8::{i}/64 dev vb nodad")
        for ns, dev in ends:
            sh(f"ip -n {ns} link set dev {dev} mtu {self.mtu}")
            sh(f"ip -n {ns} link set dev {dev} up")
            if self.impair:
                sh(f"ip netns exec {ns} ethtool -K {dev} tso off gso off gro off tx off rx off")
        if self.impair:
            sh(f"ip -n {self.nm} link set dev ma promisc on; ip -n {self.nm} link set dev mb promisc on")
            self.ctl = os.path.join(self.w, "impair.ctl")
            self.set_impair(0, 0, 0)
            self.stats_path = os.path.join(self.w, "impair.json")
            self.procs["impair"] = subprocess.Popen(
                ["ip", "netns", "exec", self.nm, os.path.join(BIN, "netimpair"), "-a", "ma", "-b", "mb",
                 "-ctl", self.ctl, "-stats", self.stats_path],
                stdout=open(os.path.join(self.w, "impair.log"), "w"), stderr=subprocess.STDOUT)
        # wait for underlay reachability
        for _ in range(50):
            if self.A("ping -c1 -W1 192.0.2.2", 3).returncode == 0:
                return
            time.sleep(0.1)
        raise RuntimeError("underlay not reachable")

    def down(self, quiet=False):
        for k, p in list(self.procs.items()):
            self._stop(p)
            self.procs.pop(k, None)
        for h in self.helpers:
            self._stop(h)
        self.helpers = []
        for n in (self.na, self.nm, self.nb):
            sh(f"ip netns del {n}")

    def cleanup(self, keep_logs=True):
        self.down()
        if not keep_logs:
            shutil.rmtree(self.w, ignore_errors=True)

    @staticmethod
    def _stop(p, timeout=5):
        if p.poll() is None:
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(timeout)
            except subprocess.TimeoutExpired:
                p.kill(); p.wait(2)

    def A(self, cmd, timeout=30):
        return sh(f"ip netns exec {self.na} {cmd}", timeout)

    def B(self, cmd, timeout=30):
        return sh(f"ip netns exec {self.nb} {cmd}", timeout)

    def set_impair(self, loss=0, delay_ms=0, jitter_ms=0):
        with open(self.ctl, "w") as f:
            f.write(f"{loss} {delay_ms} {jitter_ms}\n")
        time.sleep(0.5)  # bridge polls every 200 ms

    def impair_stats(self):
        try:
            return json.load(open(self.stats_path))
        except (OSError, ValueError, AttributeError):
            return {}

    # ---------------------------------------------------------------- tuunel
    def keys(self):
        for n in ("a", "b"):
            r = sh(f"{BIN}/tuunel genkey", check=True)
            k = os.path.join(self.w, f"{n}.key")
            with open(k, "w") as f:
                f.write(r.stdout.strip() + "\n")
            os.chmod(k, 0o600)
            setattr(self, f"pub{n}", sh(f"{BIN}/tuunel pubkey -key {k}", check=True).stdout.strip())

    def configure(self, carriers, a_extra="", b_extra="", health=None, failover=None, listen_v6=False, endpoints=None):
        """carriers: list of names from CARRIERS (preference order)."""
        exp = "experimental: {icmp: true}" if "icmp" in carriers else ""
        listen = []
        for c in carriers:
            t, port, x = CARRIERS[c]
            for i in range(2, 2 + self.endpoints):
                addr = f"192.0.2.{i}" if t == "icmp" else f"192.0.2.{i}:{port}"
                listen.append(f'  - {{carrier: {t}, address: "{addr}"{x}}}')
                if listen_v6 and t != "icmp":
                    listen.append(f'  - {{carrier: {t}, address: "[2001:db8::{i}]:{port}"{x}}}')
        cl = []
        for c in carriers:
            t, port, x = CARRIERS[c]
            cl.append(f"{{type: {t}" + (f", port: {port}" if port else "") + f"{x}}}")
        eps = endpoints or "[" + ", ".join(f"{{name: e{i-1}, address: 192.0.2.{i}}}" for i in range(2, 2 + self.endpoints)) + "]"
        health = health or "{interval: 500ms, ping_timeout: 1s, idle_timeout: 5s, failed_after_missed: 4}"
        failover = failover or "{backoff_initial: 500ms, backoff_max: 3s, min_hold: 3s, probe_interval: 2s, recovery_successes: 2}"
        w = self.w
        open(f"{w}/b.yaml", "w").write(f"""node: {{id: node-b}}
interface: {{name: tun0, addresses: ["10.200.0.2/30", "fd20::2/64"]}}
security: {{private_key_file: {w}/b.key}}
listen:
{chr(10).join(listen)}
peers: [{{name: node-a, public_key: "{self.puba}", allowed_ips: ["10.200.0.1/32", "fd20::1/128"]}}]
health: {{interval: 500ms, ping_timeout: 1s, idle_timeout: 5s}}
api: {{socket: {w}/b.sock}}
log: {{level: info}}
{exp}
{b_extra}
""")
        open(f"{w}/a.yaml", "w").write(f"""node: {{id: node-a}}
interface: {{name: tun0, addresses: ["10.200.0.1/30", "fd20::1/64"]}}
security: {{private_key_file: {w}/a.key}}
peers:
  - name: node-b
    public_key: "{self.pubb}"
    allowed_ips: ["10.200.0.2/32", "fd20::2/128"]
    endpoints: {eps}
    carriers: [{", ".join(cl)}]
health: {health}
failover: {failover}
api: {{socket: {w}/a.sock}}
log: {{level: info}}
{exp}
{a_extra}
""")

    def start(self, who):
        ns = self.na if who == "a" else self.nb
        log = open(os.path.join(self.w, f"{who}.log"), "a")
        exe = getattr(self, f"bin_{who}", None) or f"{BIN}/tuunel"   # per-node binary (compat tests)
        self.procs[who] = subprocess.Popen(["ip", "netns", "exec", ns] + list(getattr(self, "wrap", [])) +
                                           [exe, "run", "-config", f"{self.w}/{who}.yaml"],
                                           stdout=log, stderr=subprocess.STDOUT)
        for _ in range(50):
            if os.path.exists(f"{self.w}/{who}.sock"):
                return
            time.sleep(0.1)

    def stop(self, who):
        p = self.procs.pop(who, None)
        if p:
            self._stop(p, 10)
            return p.returncode
        return None

    def api(self, who, path, timeout=2):
        s = socket.socket(socket.AF_UNIX)
        s.settimeout(timeout)
        try:
            s.connect(f"{self.w}/{who}.sock")
            s.sendall(f"GET {path} HTTP/1.0\r\nHost: x\r\n\r\n".encode())
            data = b""
            while (d := s.recv(65536)):
                data += d
        except OSError:
            return None
        finally:
            s.close()
        return data.split(b"\r\n\r\n", 1)[-1].decode(errors="replace")

    def status(self, who="a"):
        try:
            return json.loads(self.api(who, "/api/status") or "")
        except ValueError:
            return None

    def peer(self, who="a"):
        st = self.status(who)
        try:
            return st["engine"]["peers"][0]
        except (TypeError, KeyError, IndexError):
            return None

    def active(self):
        p = self.peer()
        return f'{p["carrier"]}/{p["endpoint"]}' if p and p.get("up") else "down"

    def wait_carrier(self, pattern, timeout):
        rx = re.compile(pattern)
        t0 = time.time()
        while time.time() - t0 < timeout:
            if rx.fullmatch(self.active()):
                return round(time.time() - t0, 2)
            time.sleep(0.1)
        return None

    def metrics(self, who="a"):
        return self.api(who, "/api/metrics") or ""

    def proc_usage(self, who):
        p = self.procs.get(who)
        if not p:
            return {}
        try:
            st = open(f"/proc/{p.pid}/stat").read().rsplit(")", 1)[1].split()
            cpu = (int(st[11]) + int(st[12])) / os.sysconf("SC_CLK_TCK")
            rss = int(re.search(r"VmRSS:\s+(\d+)", open(f"/proc/{p.pid}/status").read()).group(1))
            fds = len(os.listdir(f"/proc/{p.pid}/fd"))
            return {"cpu_s": round(cpu, 2), "rss_kb": rss, "fds": fds}
        except (OSError, AttributeError, IndexError):
            return {}


    # ---------------------------------------------------------------- carrier blocking
    def block(self, *names, addrs=()):
        """Drop B's inbound traffic for the given carriers (and/or underlay
        destination addresses) with nftables (IPv4) / ip6tables (IPv6) in B's namespace.
        block() with no arguments removes all blocks."""
        self.B("nft delete table ip tlab")
        self.B("sh -c 'ip6tables -D INPUT -j TLAB; ip6tables -F TLAB; ip6tables -X TLAB'")
        r4, r6 = [], []
        for n in names:
            t, port, _ = CARRIERS[n]
            if t == "icmp":
                r4.append("iifname \"vb\" icmp type echo-request drop")
            else:
                proto = "udp" if t in ("udp", "quic") else "tcp"
                r4.append(f"iifname \"vb\" {proto} dport {port} drop"); r6.append(f"-i vb -p {proto} --dport {port}")
        for ad in addrs:
            if ":" in ad:
                r6.append(f"-i vb -d {ad}")
            else:
                r4.append(f"iifname \"vb\" ip daddr {ad} drop")
        if r4:
            body = "\n".join(r4)
            r = sh(f"ip netns exec {self.nb} nft -f -", input=f"table ip tlab {{\n chain tin {{\n  type filter hook input priority 0; policy accept;\n{body}\n }}\n}}\n")
            if r.returncode:
                raise RuntimeError("nft block failed: " + r.stderr)
        if r6:  # the ip6 nft family is unavailable on some kernels; use ip6tables
            cmds = ["ip6tables -N TLAB", "ip6tables -I INPUT -j TLAB"] + [f"ip6tables -A TLAB {x} -j DROP" for x in r6]
            r = self.B("sh -c '" + " && ".join(cmds) + "'")
            if r.returncode:
                raise RuntimeError("ip6tables block failed: " + r.stderr)
        self.blocked = list(names) + list(addrs)

    # ---------------------------------------------------------------- probes
    def probe(self, name, mode, dst="10.200.0.2", port=None, duration=60, interval=0.05, size=1024, ns="a"):
        port = port or (9000 if mode == "tcp" else 9001)
        out = os.path.join(self.w, f"probe-{name}.json")
        p = subprocess.Popen(["ip", "netns", "exec", self.na if ns == "a" else self.nb, "python3",
                              os.path.join(os.path.dirname(os.path.abspath(__file__)), "probe.py"), "--mode", mode,
                              "--dst", dst, "--port", str(port), "--duration", str(duration), "--interval",
                              str(interval), "--size", str(size), "--out", out],
                             stdout=subprocess.DEVNULL, stderr=open(out + ".err", "w"))
        self.helpers.append(p)
        return out

    @staticmethod
    def read_probe(out):
        try:
            return json.load(open(out))
        except (OSError, ValueError):
            return {}

    def metric(self, name, who="a", labels=""):
        """Sum of samples of a Prometheus metric whose label string contains labels."""
        tot, found = 0.0, False
        for line in self.metrics(who).splitlines():
            if line.startswith(name + "{") or line.startswith(name + " "):
                if labels in line:
                    try:
                        tot += float(line.rsplit(" ", 1)[1]); found = True
                    except ValueError:
                        pass
        return tot if found else None

    # ---------------------------------------------------------------- traffic
    def helpers_b(self):
        """TCP echo :9000 (IPv4) and :9010 (IPv6), TCP sink :9002, UDP echo :9001 (v4+v6)."""
        code = r'''
import socket, threading
def serve(fam, addr, port, echo):
    s=socket.socket(fam)
    if fam==socket.AF_INET6: s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind((addr,port)); s.listen(64)
    while True:
        c,_=s.accept()
        def h(c):
            try:
                while (d:=c.recv(65536)):
                    if echo: c.sendall(d)
                if not echo: c.sendall(b"k")
            except OSError: pass
            c.close()
        threading.Thread(target=h,args=(c,),daemon=True).start()
def udp(fam, addr):
    u=socket.socket(fam,socket.SOCK_DGRAM)
    if fam==socket.AF_INET6: u.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
    u.bind((addr,9001))
    while True:
        d,a=u.recvfrom(65535); u.sendto(d,a)
threading.Thread(target=serve,args=(socket.AF_INET,"0.0.0.0",9000,True),daemon=True).start()
threading.Thread(target=serve,args=(socket.AF_INET,"0.0.0.0",9002,False),daemon=True).start()
threading.Thread(target=serve,args=(socket.AF_INET6,"::",9010,True),daemon=True).start()
threading.Thread(target=udp,args=(socket.AF_INET6,"::"),daemon=True).start()
udp(socket.AF_INET,"0.0.0.0")
'''
        p = subprocess.Popen(["ip", "netns", "exec", self.nb, "python3", "-c", code, "tuunel-lab-helper"],
                             stdout=subprocess.DEVNULL, stderr=open(os.path.join(self.w, "helper.err"), "w"))
        self.helpers.append(p)
        time.sleep(0.4)
        if p.poll() is not None:
            raise RuntimeError("helper died: " + open(os.path.join(self.w, "helper.err")).read()[-300:])

    def helpers_a(self):
        """Services on the dialing node A for forwarding tests: TCP echo :9100, UDP echo :9101."""
        code = r'''
import socket, threading
def t():
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",9100)); s.listen(64)
    while True:
        c,_=s.accept()
        def h(c):
            try:
                while (d:=c.recv(65536)): c.sendall(d)
            except OSError: pass
            c.close()
        threading.Thread(target=h,args=(c,),daemon=True).start()
threading.Thread(target=t,daemon=True).start()
u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.bind(("0.0.0.0",9101))
while True:
    d,a=u.recvfrom(65535); u.sendto(d,a)
'''
        p = subprocess.Popen(["ip", "netns", "exec", self.na, "python3", "-c", code, "tuunel-lab-helper-a"],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.helpers.append(p)
        time.sleep(0.3)

    def ping(self, dst="10.200.0.2", count=100, interval=0.05, size=56, ns="a", extra=""):
        """Returns delivery, rtt avg and mdev (jitter proxy), in ms."""
        # no -w: with a deadline, ping keeps sending until COUNT replies arrive,
        # which inflates "transmitted" and reports in-flight packets as lost.
        w = int(count * interval) + 10
        r = sh(f"ip netns exec {self.na if ns == 'a' else self.nb} ping -q -n -c {count} -i {interval} -s {size} -W 2 {extra} {dst}", w)
        out = r.stdout
        m = re.search(r"(\d+) packets transmitted, (\d+) received", out)
        tx, rx = (int(m.group(1)), int(m.group(2))) if m else (count, 0)
        rtt = re.search(r"= ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)", out)
        return {"tx": tx, "rx": rx, "loss_pct": round(100 * (tx - rx) / tx, 2) if tx else 100.0,
                "rtt_avg_ms": float(rtt.group(2)) if rtt else None, "rtt_mdev_ms": float(rtt.group(4)) if rtt else None}

    def iperf(self, seconds=4, dst="10.200.0.2", udp_rate=None):
        srv = subprocess.Popen(["ip", "netns", "exec", self.nb, "timeout", str(seconds + 15), "iperf3", "-s", "-1",
                                "-B", dst], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(0.4)
        args = f"-u -b {udp_rate} -l 1200" if udp_rate else ""
        r = self.A(f"timeout {seconds + 12} iperf3 -J -c {dst} -t {seconds} {args} --connect-timeout 3000", seconds + 15)
        self._stop(srv, 3)
        try:
            j = json.loads(r.stdout)
            e = j["end"]
            if udp_rate:
                s = e["sum"]
                return {"mbit": round(s["bits_per_second"] / 1e6, 1), "udp_lost_pct": round(s["lost_percent"], 2),
                        "jitter_ms": round(s["jitter_ms"], 3)}
            return {"mbit": round(e["sum_received"]["bits_per_second"] / 1e6, 1),
                    "retransmits": e["sum_sent"].get("retransmits")}
        except (ValueError, KeyError):
            return {"mbit": None, "error": (r.stdout + r.stderr)[-200:]}

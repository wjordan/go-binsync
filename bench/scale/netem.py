#!/usr/bin/env python3
"""netem.py --obj NAME=PATH ... [--profiles A,B,C,D] [--cc cubic,bbr] [--reps 3] [--out FILE]

Real WAN experiment: two network namespaces (bs-srv, bs-cli) joined by a veth pair, netem
(delay RTT/2, loss p, rate R) on BOTH ends, a Go static server (bench/out/tools/netsrv,
Range + 304 capable) in bs-srv, curl in bs-cli.

Per profile x object: single-connection GET (reps, median), for objects >= 4 MB also 4-way
and 8-way parallel ranged GETs (curl -r, one process per range, verified by cmp), a
conditional GET (curl -z <file> -> 304), and connect/TTFB of the smallest object.
Adaptive reps: if the first run of a measurement takes > 150 s it is not repeated.
curl --max-time 600 on every request. Everything runs through `sudo -n`.
"""
import json, os, statistics, subprocess, sys, time

BENCH = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRV = os.path.join(BENCH, "out", "tools", "netsrv")
SRV_IP, CLI_IP, PORT = "10.99.0.1", "10.99.0.2", 8080
NS_S, NS_C, V_S, V_C = "bs-srv", "bs-cli", "veth-srv", "veth-cli"
PROFILES = {"A": (100, 100, 0.0), "B": (20, 200, 0.0), "C": (20, 200, 1.0), "D": (5, 300, 2.0)}  # Mbit, RTT ms, loss %
MAXTIME = 600
TMP = os.path.join(BENCH, "out", "netem-tmp"); os.makedirs(TMP, exist_ok=True)

def sh(cmd, check=True, **kw):
    return subprocess.run(cmd, shell=isinstance(cmd, str), check=check, text=True, capture_output=True, **kw)

def sudo(cmd, check=True):
    return sh(["sudo", "-n"] + cmd, check=check)

def ns_exec(ns, cmd, check=True, timeout=None):
    return subprocess.run(["sudo", "-n", "ip", "netns", "exec", ns, "sudo", "-u", os.environ.get("USER", "will"), "-n"] + cmd,
                          check=check, text=True, capture_output=True, timeout=timeout)

def setup(cc):
    teardown()
    for ns in (NS_S, NS_C): sudo(["ip", "netns", "add", ns])
    sudo(["ip", "link", "add", V_S, "type", "veth", "peer", "name", V_C])
    sudo(["ip", "link", "set", V_S, "netns", NS_S]); sudo(["ip", "link", "set", V_C, "netns", NS_C])
    for ns, v, ip in ((NS_S, V_S, SRV_IP), (NS_C, V_C, CLI_IP)):
        sudo(["ip", "-n", ns, "addr", "add", f"{ip}/24", "dev", v])
        sudo(["ip", "-n", ns, "link", "set", "lo", "up"]); sudo(["ip", "-n", ns, "link", "set", v, "up"])
        sudo(["ip", "netns", "exec", ns, "ethtool", "-K", v, "tso", "off", "gso", "off", "gro", "off"])  # real 1500 B packets for loss/rate
        sudo(["ip", "netns", "exec", ns, "sysctl", "-q", "-w", f"net.ipv4.tcp_congestion_control={cc}"])
    got = sudo(["ip", "netns", "exec", NS_S, "sysctl", "-n", "net.ipv4.tcp_congestion_control"]).stdout.strip()
    host = sh(["sysctl", "-n", "net.ipv4.tcp_congestion_control"]).stdout.strip()
    print(f"# namespaces up; server cc={got} (host cc={host})", flush=True)
    return got

def apply_profile(mbit, rtt_ms, loss):
    limit = max(1000, int(2 * mbit * 1e6 * rtt_ms / 1000 / 8 / 1500))   # ~2 BDP of packets: delay queue + ~1 RTT of bottleneck buffer
    for ns, v in ((NS_S, V_S), (NS_C, V_C)):
        args = ["delay", f"{rtt_ms/2:g}ms", "rate", f"{mbit}mbit", "limit", str(limit)]
        if loss > 0: args += ["loss", f"{loss:g}%"]
        sudo(["ip", "netns", "exec", ns, "tc", "qdisc", "replace", "dev", v, "root", "netem"] + args)
    return sudo(["ip", "netns", "exec", NS_S, "tc", "qdisc", "show", "dev", V_S]).stdout.strip()

def start_server(dirpath):
    p = subprocess.Popen(["sudo", "-n", "ip", "netns", "exec", NS_S, SRV, "-dir", dirpath, "-addr", f"{SRV_IP}:{PORT}"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(50):
        time.sleep(0.1)
        if ns_exec(NS_C, ["curl", "-s", "-o", "/dev/null", "--max-time", "2", f"http://{SRV_IP}:{PORT}/"], check=False).returncode == 0:
            return p
    raise SystemExit("server did not come up")

def teardown():
    subprocess.run(["sudo", "-n", "pkill", "-f", SRV], check=False, capture_output=True)
    for ns in (NS_S, NS_C):
        subprocess.run(["sudo", "-n", "ip", "netns", "del", ns], check=False, capture_output=True)

CURL_W = "%{time_connect} %{time_starttransfer} %{time_total} %{speed_download} %{http_code} %{size_download} %{num_connects}"

def curl(args, timeout=MAXTIME + 30):
    t0 = time.time()
    r = ns_exec(NS_C, ["curl", "-s", "-S", "--max-time", str(MAXTIME), "-w", CURL_W] + args, check=False, timeout=timeout)
    parts = r.stdout.strip().split("\n")[-1].split()
    try:
        tc, ttfb, tt, spd, code, sz, nc = float(parts[0]), float(parts[1]), float(parts[2]), float(parts[3]), int(parts[4]), int(float(parts[5])), int(parts[6])
    except Exception:
        tc = ttfb = tt = spd = float("nan"); code = 0; sz = 0; nc = 0
    return dict(connect=tc, ttfb=ttfb, total=tt, speed=spd, code=code, size=sz, rc=r.returncode, wall=time.time() - t0)

def single(url, size, reps):
    runs = []
    for i in range(reps):
        r = curl(["-o", "/dev/null", url]); runs.append(r)
        if r["rc"] != 0 or r["total"] > 150: break
    return runs

def parallel(url, path, size, n, reps):
    runs = []
    step = (size + n - 1) // n
    for i in range(reps):
        t0 = time.time(); procs = []
        for k in range(n):
            a, b = k * step, min(size, (k + 1) * step) - 1
            out = os.path.join(TMP, f"part-{n}-{k}")
            procs.append(subprocess.Popen(["sudo", "-n", "ip", "netns", "exec", NS_C, "sudo", "-u", os.environ.get("USER", "will"), "-n",
                                           "curl", "-s", "-S", "--max-time", str(MAXTIME), "-r", f"{a}-{b}", "-o", out, "-w", CURL_W, url],
                                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True))
        outs = [p.communicate()[0] for p in procs]; rcs = [p.returncode for p in procs]
        wall = time.time() - t0
        cat = os.path.join(TMP, f"cat-{n}")
        with open(cat, "wb") as f:
            for k in range(n):
                f.write(open(os.path.join(TMP, f"part-{n}-{k}"), "rb").read())
        ok = subprocess.run(["cmp", "-s", cat, path]).returncode == 0
        per = [o.strip().split()[2] for o in outs if o.strip()]
        runs.append(dict(wall=wall, ok=ok, rcs=rcs, per_conn_total=[float(x) for x in per], speed=size / wall))
        if max(rcs) != 0 or wall > 150: break
    return runs

def med(xs): return statistics.median(xs) if xs else float("nan")

def main():
    objs, profiles, ccs, reps, out = [], list(PROFILES), ["cubic"], 3, os.path.join(BENCH, "out", "results", "netem.json")
    a = sys.argv[1:]
    while a:
        k = a.pop(0)
        if k == "--obj": n, p = a.pop(0).split("=", 1); objs.append((n, os.path.abspath(p)))
        elif k == "--profiles": profiles = a.pop(0).split(",")
        elif k == "--cc": ccs = a.pop(0).split(",")
        elif k == "--reps": reps = int(a.pop(0))
        elif k == "--out": out = a.pop(0)
    if not objs: raise SystemExit("need --obj NAME=PATH")
    srvdir = os.path.join(TMP, "netsrv-root"); os.makedirs(srvdir, exist_ok=True)
    for n, p in objs:
        dst = os.path.join(srvdir, n)
        if not os.path.exists(dst) or os.path.getsize(dst) != os.path.getsize(p):
            subprocess.run(["cp", "-p", p, dst], check=True)
    os.chmod(srvdir, 0o755)
    results = []
    T0 = time.time()
    try:
        for cc in ccs:
            got = setup(cc)
            srv = start_server(srvdir)
            for prof in profiles:
                mbit, rtt, loss = PROFILES[prof]
                q = apply_profile(mbit, rtt, loss)
                print(f"\n## cc={cc} profile {prof}: {mbit} Mbit/s, {rtt} ms RTT, {loss}% loss each way  [{q}]", flush=True)
                smallest = min(objs, key=lambda o: os.path.getsize(o[1]))
                for name, path in objs:
                    size = os.path.getsize(path); url = f"http://{SRV_IP}:{PORT}/{name}"
                    rec = dict(cc=cc, profile=prof, mbit=mbit, rtt_ms=rtt, loss=loss, object=name, bytes=size)
                    s = single(url, size, reps)
                    rec["single"] = s
                    rec["single_med_s"] = med([r["total"] for r in s if r["rc"] == 0]) if any(r["rc"] == 0 for r in s) else None
                    rec["single_med_speed"] = med([r["speed"] for r in s if r["rc"] == 0]) if rec["single_med_s"] else None
                    line = f"{name:<12} {size:>10,} B  single: " + (f"{rec['single_med_s']:.2f} s ({rec['single_med_speed']*8/1e6:.2f} Mbit/s, n={len(s)})" if rec["single_med_s"] else f"FAILED/TIMEOUT rc={s[-1]['rc']} got {s[-1]['size']:,} B in {s[-1]['total']:.0f} s")
                    if size >= 4 << 20:
                        for n in (4, 8):
                            pr = parallel(url, path, size, n, reps)
                            okr = [r for r in pr if r["ok"]]
                            rec[f"par{n}"] = pr; rec[f"par{n}_med_s"] = med([r["wall"] for r in okr]) if okr else None
                            line += f"  {n}-way: " + (f"{rec[f'par{n}_med_s']:.2f} s ({size*8/rec[f'par{n}_med_s']/1e6:.2f} Mbit/s, n={len(pr)})" if okr else f"FAILED rcs={pr[-1]['rcs']}")
                    if name == smallest[0]:
                        c = [curl(["-o", "/dev/null", "-z", path, url]) for _ in range(reps)]
                        rec["cond304"] = c; rec["cond304_med_s"] = med([r["total"] for r in c]); rec["cond304_codes"] = [r["code"] for r in c]
                        rec["connect_med_s"] = med([r["connect"] for r in s]); rec["ttfb_med_s"] = med([r["ttfb"] for r in s])
                        line += f"  connect {rec['connect_med_s']*1000:.0f} ms, TTFB {rec['ttfb_med_s']*1000:.0f} ms; conditional GET -> {rec['cond304_codes']} in {rec['cond304_med_s']*1000:.0f} ms"
                    print(f"[{time.time()-T0:6.0f}s] {line}", flush=True); results.append(rec)
                save(out, results, T0)   # incremental: a crash keeps finished profiles
            teardown()
    finally:
        teardown()
    save(out, results, T0)
    print(f"\nwrote {out}; namespaces removed: {sh('ip netns list').stdout.count('bs-')==0}")

def save(out, results, T0):
    os.makedirs(os.path.dirname(out), exist_ok=True)
    prev = json.load(open(out)) if os.path.exists(out) else []
    mine = {id(r) for r in results}
    keep = [r for r in prev if r.get("run_id") != T0]
    for r in results: r["run_id"] = T0
    json.dump(keep + results, open(out, "w"), indent=1)

if __name__ == "__main__":
    main()

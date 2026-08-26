#!/usr/bin/env python3
"""delta_scale.py [--workers N] [--reps N] [--only m1,m2] [--pairs label=OLD:NEW,...] [--big-only|--small-only] [--out FILE.json]

Patch size, encode/apply wall time (min of reps) and peak RSS (max of reps) for the
real-world corpus pairs (bench/out/corpus) and the synthetic v1->v5 / v4->v5 pairs.
Every apply is cmp'd against NEW. Per-command timeout 840 s (< 15 min).
Writes bench/out/results/scale.json (merged by label+method) and a markdown table.

Method set (see benchmark-scale.md):
  bsdiff, hdiffz-m6 (-m-6 -SD -c-zstd-21-24, -p-1), hdiffz-m6-p8, hdiffz-s64 (stream),
  zstd-19 (--patch-from, --long=L where 2^L >= max(old,new), single thread), zstd-19-T0,
  xdelta3-9-lzma (-B filesize), full-zstd-19, full-zstd-19-T0.
Pairs with max(old,new) > 200 MB: reps=1, bsdiff single run, workers=1 (memory).
"""
import json, math, os, signal, subprocess, sys, tempfile, time
from concurrent.futures import ThreadPoolExecutor

BENCH = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CORPUS = os.path.join(BENCH, "out", "corpus")
BIN = os.path.join(BENCH, "out", "bin")
PATCHES = os.path.join(BENCH, "out", "patches-scale")
RESULTS = os.path.join(BENCH, "out", "results")
HD = os.path.join(BENCH, "out", "tools", "HDiffPatch")
HDIFFZ, HPATCHZ = os.path.join(HD, "hdiffz"), os.path.join(HD, "hpatchz")
TIMEOUT = 840
BIG = 200 << 20

def zstd_long(old, new):
    return max(27, math.ceil(math.log2(max(os.path.getsize(old), os.path.getsize(new)))))

def methods(old, new):
    L = zstd_long(old, new)
    mem = f"--memory={(1 << L) // (1 << 20) + 64}MB"
    B = str(max(os.path.getsize(old), os.path.getsize(new)))
    zdec = ["zstd", "-q", "-f", "-d", f"--long={L}", mem, f"--patch-from={old}", "{patch}", "-o", "{out}"]
    return {
        "bsdiff":          (["bsdiff", old, new, "{patch}"], ["bspatch", old, "{out}", "{patch}"]),
        "hdiffz-m6":       ([HDIFFZ, "-m-6", "-SD", "-d", "-f", "-p-1", "-c-zstd-21-24", old, new, "{patch}"], [HPATCHZ, "-f", "-p-1", old, "{patch}", "{out}"]),
        "hdiffz-m6-p8":    ([HDIFFZ, "-m-6", "-SD", "-d", "-f", "-p-8", "-c-zstd-21-24", old, new, "{patch}"], [HPATCHZ, "-f", "-p-8", old, "{patch}", "{out}"]),
        "hdiffz-s64":      ([HDIFFZ, "-s-64", "-SD", "-d", "-f", "-p-1", "-c-zstd-21-24", old, new, "{patch}"], [HPATCHZ, "-f", "-p-1", old, "{patch}", "{out}"]),
        "zstd-19":         (["zstd", "-q", "-f", "-19", f"--long={L}", mem, f"--patch-from={old}", new, "-o", "{patch}"], zdec),
        "zstd-19-T0":      (["zstd", "-q", "-f", "-19", "-T0", f"--long={L}", mem, f"--patch-from={old}", new, "-o", "{patch}"], zdec),
        "xdelta3-9-lzma":  (["xdelta3", "-f", "-e", "-9", "-S", "lzma", "-B", B, "-W", "16777216", "-s", old, new, "{patch}"], ["xdelta3", "-f", "-d", "-B", B, "-s", old, "{patch}", "{out}"]),
        "full-zstd-19":    (["zstd", "-q", "-f", "-19", new, "-o", "{patch}"], ["zstd", "-q", "-f", "-d", "{patch}", "-o", "{out}"]),
        "full-zstd-19-T0": (["zstd", "-q", "-f", "-19", "-T0", new, "-o", "{patch}"], ["zstd", "-q", "-f", "-d", "{patch}", "-o", "{out}"]),
    }
MT = {"hdiffz-m6-p8", "zstd-19-T0", "full-zstd-19-T0"}
BIG_SKIP = {"zstd-19", "full-zstd-19"}   # single-threaded zstd on 300-540 MB inputs: only the -T0 variants are run

def timed(argv, timeout=TIMEOUT):
    tf = tempfile.NamedTemporaryFile(delete=False, dir=RESULTS, prefix="time-", suffix=".txt"); tf.close()
    full = ["/usr/bin/time", "-f", "%e %M", "-o", tf.name] + argv
    proc = subprocess.Popen(full, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True)
    try:
        _, se = proc.communicate(timeout=timeout)
        rc, err = proc.returncode, se.decode(errors="replace")[-300:]
    except subprocess.TimeoutExpired:
        # kill the whole session (/usr/bin/time + the encoder); never `pkill -f <name>`,
        # which once matched an unrelated process whose argv contained "bsdiff".
        os.killpg(proc.pid, signal.SIGKILL); proc.wait()
        rc, err = -999, f"TIMEOUT after {timeout}s"
    try:
        el, rss = open(tf.name).read().split()[-2:]; el, rss = float(el), int(rss)
    except Exception:
        el, rss = float("nan"), 0
    os.unlink(tf.name)
    return el, rss, rc, err

def run_case(label, method, enc_t, dec_t, old, new, reps):
    patch = os.path.join(PATCHES, f"{label}.{method}.patch")
    out = os.path.join(PATCHES, f"{label}.{method}.out")
    sub = lambda t: [a.format(patch=patch, out=out) for a in t]
    r = dict(label=label, method=method, old_bytes=os.path.getsize(old), new_bytes=os.path.getsize(new),
             enc_cmd=" ".join(sub(enc_t)), dec_cmd=" ".join(sub(dec_t)), reps=reps)
    encs, decs, encrss, decrss = [], [], [], []
    t0 = time.time()
    for _ in range(reps):
        el, rss, rc, err = timed(sub(enc_t))
        if rc != 0:
            r["error"] = f"encode rc={rc}: {err.strip()}"; return r
        encs.append(el); encrss.append(rss)
    r["patch_bytes"] = os.path.getsize(patch)
    for _ in range(reps):
        el, rss, rc, err = timed(sub(dec_t))
        if rc != 0:
            r["error"] = f"decode rc={rc}: {err.strip()}"; return r
        decs.append(el); decrss.append(rss)
    ok = subprocess.run(["cmp", "-s", out, new]).returncode == 0
    r.update(enc_s=min(encs), enc_s_all=encs, enc_rss_kb=max(encrss), dec_s=min(decs), dec_rss_kb=max(decrss), verified=ok, wall_s=time.time() - t0)
    if os.path.exists(out): os.unlink(out)
    return r

def default_pairs():
    P = []
    spec = [("prometheus", "3.13.1", "3.13.2", "prometheus"), ("prometheus", "3.13.2", "3.14.0", "prometheus"),
            ("kube-apiserver", "1.36.3", "1.36.4", "kube-apiserver"), ("terraform", "1.15.8", "1.15.9", "terraform"),
            ("cockroach", "26.2.4", "26.2.5", "cockroach"), ("vault", "2.0.3", "2.0.4", "vault")]
    for proj, a, b, bn in spec:
        o, n = os.path.join(CORPUS, proj, a, bn), os.path.join(CORPUS, proj, b, bn)
        if os.path.exists(o) and os.path.exists(n):
            P.append((f"{proj}-{a}-{b}", o, n))
        if os.path.exists(o + ".stripped") and os.path.exists(n + ".stripped"):
            P.append((f"{proj}-{a}-{b}-stripped", o + ".stripped", n + ".stripped"))
    for a, b in (("v1", "v5"), ("v4", "v5")):
        P.append((f"F2.{a}-{b}", os.path.join(BIN, f"{a}-F2"), os.path.join(BIN, f"{b}-F2")))
    return P

def fmt_row(r):
    if "error" in r:
        return f"| {r['label']} | {r['method']} | ERROR | | | | | {r['error'][:80]} |"
    return (f"| {r['label']} | {r['method']} | {r['patch_bytes']:,} | {r['enc_s']:.2f} | {r['enc_rss_kb']/1024:.0f} | "
            f"{r['dec_s']:.2f} | {r['dec_rss_kb']/1024:.0f} | {r['verified']} |")

def main():
    workers, reps, only, pairs, big_only, small_only, outname = 4, 3, None, None, False, False, "scale.json"
    a = sys.argv[1:]
    while a:
        k = a.pop(0)
        if k == "--workers": workers = int(a.pop(0))
        elif k == "--reps": reps = int(a.pop(0))
        elif k == "--only": only = a.pop(0).split(",")
        elif k == "--pairs": pairs = [(p.split("=")[0], *p.split("=")[1].split(":")) for p in a.pop(0).split(",")]
        elif k == "--big-only": big_only = True
        elif k == "--small-only": small_only = True
        elif k == "--out": outname = a.pop(0)
    os.makedirs(PATCHES, exist_ok=True); os.makedirs(RESULTS, exist_ok=True)
    pairs = pairs or default_pairs()
    small_jobs, big_jobs, mt_jobs = [], [], []
    for label, old, new in pairs:
        big = max(os.path.getsize(old), os.path.getsize(new)) > BIG
        if (big_only and not big) or (small_only and big): continue
        for m, (e, d) in methods(old, new).items():
            if only and m not in only: continue
            if big and m in BIG_SKIP: continue
            rr = 1 if big else reps
            job = (label, m, e, d, old, new, rr)
            (big_jobs if big else (mt_jobs if m in MT else small_jobs)).append(job)
    results, t0 = [], time.time()
    def log(r):
        results.append(r)
        print(f"[{time.time()-t0:6.0f}s] {r['label']:<36} {r['method']:<16} {r.get('patch_bytes','ERR'):>12} enc={r.get('enc_s','-')} rss={r.get('enc_rss_kb',0)//1024}MB dec={r.get('dec_s','-')} ok={r.get('verified','-')} {r.get('error','')}", flush=True)
    print(f"{len(small_jobs)} parallel jobs ({workers} workers), {len(mt_jobs)} multithreaded jobs, {len(big_jobs)} big-pair jobs (serial)", flush=True)
    with ThreadPoolExecutor(max_workers=workers) as ex:
        for r in ex.map(lambda j: run_case(*j), small_jobs): log(r)
    for j in mt_jobs: log(run_case(*j))
    for j in big_jobs: log(run_case(*j))
    path = os.path.join(RESULTS, outname)
    prev = {}
    if os.path.exists(path):
        prev = {(x["label"], x["method"]): x for x in json.load(open(path))}
    for r in results: prev[(r["label"], r["method"])] = r
    json.dump(list(prev.values()), open(path, "w"), indent=1)
    print("\n| pair | method | patch B | enc s | enc RSS MB | apply s | apply RSS MB | verified |\n|---|---|---:|---:|---:|---:|---:|---|")
    for r in results: print(fmt_row(r))
    print(f"\nwrote {path} ({len(prev)} records)")

if __name__ == "__main__":
    main()

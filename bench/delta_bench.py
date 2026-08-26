#!/usr/bin/env python3
"""delta_bench.py [--workers N] [--reps 3] [--only METHOD,...] [--pairs v1:v2c,...] [--cfgs F1,F2]

Measures patch size, encode/apply wall time (min of reps) and peak RSS (max of
reps) for several delta encoders. Every apply output is cmp'd against NEW.
Writes markdown tables to stdout and JSON to out/results/delta.json.
"""
import json, os, shutil, subprocess, sys, tempfile, time
from concurrent.futures import ThreadPoolExecutor

BENCH = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(BENCH, "out", "bin")
PATCHES = os.path.join(BENCH, "out", "patches")
RESULTS = os.path.join(BENCH, "out", "results")
HD = os.path.join(BENCH, "out", "tools", "HDiffPatch")
HDIFFZ, HPATCHZ = os.path.join(HD, "hdiffz"), os.path.join(HD, "hpatchz")
TIMEOUT = 200

# name -> (encode argv template, decode argv template). {old} {new} {patch} {out}
METHODS = {
    "zstd-19":           (["zstd", "-q", "-f", "-19", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "zstd-19-T0":        (["zstd", "-q", "-f", "-19", "-T0", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "zstd-19-long27":    (["zstd", "-q", "-f", "-19", "--long=27", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "zstd-22-ultra-long31": (["zstd", "-q", "-f", "--ultra", "-22", "--long=31", "--memory=2048MB", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--long=31", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "zstd-3-long27":     (["zstd", "-q", "-f", "-3", "--long=27", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "zstd-9-long27":     (["zstd", "-q", "-f", "-9", "--long=27", "--patch-from={old}", "{new}", "-o", "{patch}"],
                          ["zstd", "-q", "-f", "-d", "--memory=2048MB", "--patch-from={old}", "{patch}", "-o", "{out}"]),
    "xdelta3-9-djw":     (["xdelta3", "-f", "-e", "-9", "-S", "djw", "-B", "268435456", "-W", "16777216", "-s", "{old}", "{new}", "{patch}"],
                          ["xdelta3", "-f", "-d", "-B", "268435456", "-s", "{old}", "{patch}", "{out}"]),
    "xdelta3-9-lzma":    (["xdelta3", "-f", "-e", "-9", "-S", "lzma", "-B", "268435456", "-W", "16777216", "-s", "{old}", "{new}", "{patch}"],
                          ["xdelta3", "-f", "-d", "-B", "268435456", "-s", "{old}", "{patch}", "{out}"]),
    "bsdiff":            (["bsdiff", "{old}", "{new}", "{patch}"],
                          ["bspatch", "{old}", "{out}", "{patch}"]),
    "hdiffz-m6-zstd21":  ([HDIFFZ, "-m-6", "-SD", "-d", "-f", "-p-1", "-c-zstd-21-24", "{old}", "{new}", "{patch}"],
                          [HPATCHZ, "-f", "-p-1", "{old}", "{patch}", "{out}"]),
    "hdiffz-s64-zstd21": ([HDIFFZ, "-s-64", "-SD", "-d", "-f", "-p-1", "-c-zstd-21-24", "{old}", "{new}", "{patch}"],
                          [HPATCHZ, "-f", "-p-1", "{old}", "{patch}", "{out}"]),
    "hdiffz-m6-lzma2":   ([HDIFFZ, "-m-6", "-SD", "-d", "-f", "-p-1", "-c-lzma2-9-16m", "{old}", "{new}", "{patch}"],
                          [HPATCHZ, "-f", "-p-1", "{old}", "{patch}", "{out}"]),
    "hdiffz-m6-zstd21-p8": ([HDIFFZ, "-m-6", "-SD", "-d", "-f", "-p-8", "-c-zstd-21-24", "{old}", "{new}", "{patch}"],
                          [HPATCHZ, "-f", "-p-8", "{old}", "{patch}", "{out}"]),
}
BASELINES = {  # full-download baselines: compress NEW only
    "full-zstd-19":        (["zstd", "-q", "-f", "-19", "{new}", "-o", "{patch}"], ["zstd", "-q", "-f", "-d", "--memory=2048MB", "{patch}", "-o", "{out}"]),
    "full-zstd-19-long31": (["zstd", "-q", "-f", "-19", "--long=31", "--memory=2048MB", "{new}", "-o", "{patch}"], ["zstd", "-q", "-f", "-d", "--long=31", "--memory=2048MB", "{patch}", "-o", "{out}"]),
    "full-xz-9":           (["xz", "-9", "-k", "-f", "-c", "{new}"], ["xz", "-d", "-c", "{patch}"]),
}
PAIRS = ["v1:v2s", "v1:v2l", "v1:v2c", "v1:v2p", "v1:v3", "v2c:v3", "v3:v4", "v1:v4"]

def timed(argv, stdout_path=None, timeout=TIMEOUT):
    """Run argv under /usr/bin/time; return (elapsed_s, max_rss_kb, returncode, stderr)."""
    tf = tempfile.NamedTemporaryFile(delete=False, dir=RESULTS, prefix="time-", suffix=".txt")
    tf.close()
    full = ["/usr/bin/time", "-f", "%e %M", "-o", tf.name] + argv
    out = open(stdout_path, "wb") if stdout_path else subprocess.DEVNULL
    try:
        p = subprocess.run(full, stdout=out, stderr=subprocess.PIPE, timeout=timeout)
        rc, err = p.returncode, p.stderr.decode(errors="replace")[-400:]
    except subprocess.TimeoutExpired:
        rc, err = -999, f"TIMEOUT after {timeout}s"
    finally:
        if stdout_path:
            out.close()
    try:
        el, rss = open(tf.name).read().split()[-2:]
        el, rss = float(el), int(rss)
    except Exception:
        el, rss = float("nan"), 0
    os.unlink(tf.name)
    return el, rss, rc, err

def run_case(label, method, enc_t, dec_t, old, new, reps):
    patch = os.path.join(PATCHES, f"{label}.{method}.patch")
    out = os.path.join(PATCHES, f"{label}.{method}.out")
    to_stdout = method.startswith("full-xz")
    sub = lambda t: [a.format(old=old, new=new, patch=patch, out=out) for a in t]
    r = dict(label=label, method=method, enc_cmd=" ".join(sub(enc_t)), dec_cmd=" ".join(sub(dec_t)))
    encs, decs, encrss, decrss = [], [], [], []
    for _ in range(reps):
        el, rss, rc, err = timed(sub(enc_t), patch if to_stdout else None)
        if rc != 0:
            r["error"] = f"encode rc={rc}: {err.strip()}"
            return r
        encs.append(el); encrss.append(rss)
    r["patch_bytes"] = os.path.getsize(patch)
    for _ in range(reps):
        el, rss, rc, err = timed(sub(dec_t), out if to_stdout else None)
        if rc != 0:
            r["error"] = f"decode rc={rc}: {err.strip()}"
            return r
        decs.append(el); decrss.append(rss)
    ok = subprocess.run(["cmp", "-s", out, new]).returncode == 0
    r.update(enc_s=min(encs), enc_rss_kb=max(encrss), dec_s=min(decs), dec_rss_kb=max(decrss), verified=ok)
    if os.path.exists(out):
        os.unlink(out)
    return r

def main():
    workers, reps, only, pairs, cfgs = 6, 3, None, PAIRS, ["F1", "F2"]
    a = sys.argv[1:]
    while a:
        k = a.pop(0)
        if k == "--workers": workers = int(a.pop(0))
        elif k == "--reps": reps = int(a.pop(0))
        elif k == "--only": only = a.pop(0).split(",")
        elif k == "--pairs": pairs = a.pop(0).split(",")
        elif k == "--cfgs": cfgs = a.pop(0).split(",")
    os.makedirs(PATCHES, exist_ok=True); os.makedirs(RESULTS, exist_ok=True)
    jobs = []
    serial = []   # multithreaded methods run after, alone
    for cfg in cfgs:
        for pr in pairs:
            o, n = pr.split(":")
            old, new = os.path.join(BIN, f"{o}-{cfg}"), os.path.join(BIN, f"{n}-{cfg}")
            for m, (e, d) in METHODS.items():
                if only and m not in only: continue
                (serial if ("T0" in m or "p8" in m) else jobs).append((f"{cfg}.{o}-{n}", m, e, d, old, new))
        for v in sorted({x for pr in pairs for x in pr.split(":")} - {"v1"} if pairs != PAIRS else {"v2s", "v2l", "v2c", "v2p", "v3", "v4"}):
            new = os.path.join(BIN, f"{v}-{cfg}")
            for m, (e, d) in BASELINES.items():
                if only and m not in only: continue
                jobs.append((f"{cfg}.{v}", m, e, d, new, new))
    if "F2" in cfgs and (not only or set(only) & {"zstd-19", "bsdiff"}):
        # PIE and mismatched-flags cases
        for m in ("zstd-19", "zstd-19-long27", "bsdiff", "xdelta3-9-djw", "hdiffz-m6-zstd21"):
            if only and m not in only: continue
            e, d = METHODS[m]
            jobs.append(("F2pie.v1-v2c", m, e, d, os.path.join(BIN, "v1-F2pie"), os.path.join(BIN, "v2c-F2pie")))
        for m in ("zstd-19", "bsdiff", "zstd-19-long27"):
            if only and m not in only: continue
            e, d = METHODS[m]
            jobs.append(("mismatch.v1F1-v2cF2", m, e, d, os.path.join(BIN, "v1-F1"), os.path.join(BIN, "v2c-F2")))
    results = []
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=workers) as ex:
        for r in ex.map(lambda j: run_case(*j, reps), jobs):
            results.append(r)
            print(f"[{time.time()-t0:6.0f}s] {r['label']:<22} {r['method']:<22} {r.get('patch_bytes','ERR'):>12} enc={r.get('enc_s','-')} dec={r.get('dec_s','-')} ok={r.get('verified','-')} {r.get('error','')}", flush=True)
    for j in serial:
        r = run_case(*j, reps)
        results.append(r)
        print(f"[{time.time()-t0:6.0f}s] {r['label']:<22} {r['method']:<22} {r.get('patch_bytes','ERR'):>12} enc={r.get('enc_s','-')} dec={r.get('dec_s','-')} ok={r.get('verified','-')} {r.get('error','')}", flush=True)
    path = os.path.join(RESULTS, "delta.json")
    prev = []
    if os.path.exists(path) and only:
        prev = [x for x in json.load(open(path)) if x["method"] not in only]
    json.dump(prev + results, open(path, "w"), indent=1)
    print(f"\nwrote {path}")

if __name__ == "__main__":
    main()

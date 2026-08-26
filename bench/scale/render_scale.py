#!/usr/bin/env python3
"""render_scale.py — markdown tables from out/results/{scale-small,scale-big,churn_scale}.json:
patch sizes, encode/apply time + RSS per pair, per-tool scaling (MB/s, RAM/input byte, log-log
fit of encode time vs input size) and the 1 GB extrapolation."""
import json, math, os
BENCH = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
R = os.path.join(BENCH, "out", "results")
ORDER = ["F2.v1-v5", "F2.v4-v5", "kube-apiserver-1.36.3-1.36.4", "prometheus-3.13.1-3.13.2-stripped", "prometheus-3.13.2-3.14.0-stripped",
         "terraform-1.15.8-1.15.9", "prometheus-3.13.1-3.13.2", "prometheus-3.13.2-3.14.0", "cockroach-26.2.4-26.2.5-stripped",
         "cockroach-26.2.4-26.2.5", "vault-2.0.3-2.0.4-stripped", "vault-2.0.3-2.0.4"]
METHODS = ["bsdiff", "hdiffz-m6", "hdiffz-m6-p8", "hdiffz-s64", "zstd-19", "zstd-19-T0", "xdelta3-9-lzma", "full-zstd-19", "full-zstd-19-T0"]

def load():
    d = {}
    for f in ("scale-small.json", "scale-big.json", "scale.json"):
        p = os.path.join(R, f)
        if os.path.exists(p):
            for r in json.load(open(p)): d[(r["label"], r["method"])] = r
    return d

def fit(xs, ys):
    """log-log least squares: y = a * x^b ; returns a, b, r2"""
    lx, ly = [math.log(x) for x in xs], [math.log(y) for y in ys]
    n = len(xs); mx, my = sum(lx) / n, sum(ly) / n
    sxx = sum((x - mx) ** 2 for x in lx); sxy = sum((x - mx) * (y - my) for x, y in zip(lx, ly))
    b = sxy / sxx if sxx else float("nan"); a = math.exp(my - b * mx)
    ss_res = sum((y - (math.log(a) + b * x)) ** 2 for x, y in zip(lx, ly)); ss_tot = sum((y - my) ** 2 for y in ly)
    return a, b, (1 - ss_res / ss_tot) if ss_tot else float("nan")

def main():
    d = load(); churn = json.load(open(os.path.join(R, "churn_scale.json")))
    labels = [l for l in ORDER if any((l, m) in d for m in METHODS)]
    print("### Patch size (bytes) per pair and method\n")
    print("| pair | NEW size | " + " | ".join(METHODS) + " |\n|---|---:|" + "---:|" * len(METHODS))
    for l in labels:
        row = [f"{d[(l, m)]['patch_bytes']:,}" if (l, m) in d and "patch_bytes" in d[(l, m)] else ("ERR" if (l, m) in d else "-") for m in METHODS]
        new = next(d[(l, m)]["new_bytes"] for m in METHODS if (l, m) in d)
        print(f"| {l} | {new:,} | " + " | ".join(row) + " |")
    print("\n### Patch size as % of the full `zstd -19 -T0` download\n")
    print("| pair | full B | " + " | ".join(m for m in METHODS if not m.startswith("full")) + " |\n|---|---:|" + "---:|" * 7)
    for l in labels:
        f = d.get((l, "full-zstd-19-T0")) or d.get((l, "full-zstd-19"))
        if not f or "patch_bytes" not in f: continue
        print(f"| {l} | {f['patch_bytes']:,} | " + " | ".join(f"{100*d[(l,m)]['patch_bytes']/f['patch_bytes']:.1f}%" if (l, m) in d and "patch_bytes" in d[(l, m)] else "-" for m in METHODS if not m.startswith("full")) + " |")
    print("\n### Encode / apply wall time (s, min of reps) and peak RSS (MB, max of reps)\n")
    print("| pair | method | patch B | enc s | enc RSS MB | apply s | apply RSS MB | reps | verified |\n|---|---|---:|---:|---:|---:|---:|---:|---|")
    for l in labels:
        for m in METHODS:
            r = d.get((l, m))
            if not r: continue
            if "error" in r: print(f"| {l} | {m} | ERROR | | | | | {r['reps']} | {r['error'][:90]} |"); continue
            print(f"| {l} | {m} | {r['patch_bytes']:,} | {r['enc_s']:.2f} | {r['enc_rss_kb']/1024:.0f} | {r['dec_s']:.2f} | {r['dec_rss_kb']/1024:.0f} | {r['reps']} | {r['verified']} |")
    print("\n### Scaling per tool: encode throughput (NEW MB / encode s), encoder RSS per input byte (RSS / OLD size), apply throughput\n")
    print("| tool | pairs | MB/s min-median-max | RSS/byte min-median-max | apply MB/s median | apply RSS/byte median | fit t = a*n^b: b | R2 | t(1 GB) from fit | RSS(1 GB) = median ratio * 1 GB |\n|---|---:|---|---|---:|---:|---:|---:|---:|---:|")
    for m in METHODS:
        rs = [d[(l, m)] for l in labels if (l, m) in d and "patch_bytes" in d[(l, m)]]
        if len(rs) < 2: continue
        mbs = sorted(r["new_bytes"] / 1e6 / r["enc_s"] for r in rs); rat = sorted(r["enc_rss_kb"] * 1024 / r["old_bytes"] for r in rs)
        amb = sorted(r["new_bytes"] / 1e6 / max(r["dec_s"], 0.005) for r in rs); arat = sorted(r["dec_rss_kb"] * 1024 / r["old_bytes"] for r in rs)
        a, b, r2 = fit([r["old_bytes"] for r in rs], [r["enc_s"] for r in rs])
        t1g = a * (1e9) ** b; med = lambda v: v[len(v) // 2]
        print(f"| {m} | {len(rs)} | {mbs[0]:.1f} - {med(mbs):.1f} - {mbs[-1]:.1f} | {rat[0]:.2f} - {med(rat):.2f} - {rat[-1]:.2f} | {med(amb):.0f} | {med(arat):.2f} | {b:.2f} | {r2:.2f} | {t1g:.0f} s | {med(rat)*1e9/1e9:.2f} GB |")
    print("\n### Compact: patch MB / encode s / encoder RSS MB per pair (reps: 3, or 1 for inputs > 200 MB)\n")
    cm = ["bsdiff", "hdiffz-m6", "hdiffz-m6-p8", "hdiffz-s64", "zstd-19-T0", "xdelta3-9-lzma", "full-zstd-19-T0"]
    print("| pair (NEW MB) | " + " | ".join(cm) + " |\n|---|" + "---|" * len(cm))
    for l in labels:
        new = next(d[(l, m)]["new_bytes"] for m in METHODS if (l, m) in d)
        cells = []
        for m in cm:
            r = d.get((l, m))
            cells.append("timeout 840 s" if r and "error" in r else (f"{r['patch_bytes']/1e6:.2f} / {r['enc_s']:.0f} s / {r['enc_rss_kb']/1024:.0f}" if r else "-"))
        print(f"| {l} ({new/1e6:.0f}) | " + " | ".join(cells) + " |")
    print("\n### 1 GB extrapolation from the largest measured input per tool (linear in size for time and RSS, plus the log-log fit for time)\n")
    print("| tool | largest input | enc s | enc RSS MB | apply s | apply RSS MB | 1 GB enc s (linear / fit) | 1 GB enc RSS GB (linear) | 1 GB apply RSS GB |\n|---|---|---:|---:|---:|---:|---:|---:|---:|")
    for m in METHODS:
        rs = [d[(l, m)] for l in labels if (l, m) in d and "patch_bytes" in d[(l, m)]]
        if not rs: continue
        big = max(rs, key=lambda r: r["old_bytes"]); n = big["old_bytes"]
        a, b, _ = fit([r["old_bytes"] for r in rs], [r["enc_s"] for r in rs])
        print(f"| {m} | {big['label']} ({n/1e6:.0f} MB) | {big['enc_s']:.0f} | {big['enc_rss_kb']/1024:.0f} | {big['dec_s']:.2f} | {big['dec_rss_kb']/1024:.0f} | {big['enc_s']*1e9/n:.0f} / {a*1e9**b:.0f} | {big['enc_rss_kb']*1024/n:.1f} | {big['dec_rss_kb']*1024/n:.1f} |")
    print("\n### Churn summary (section-aligned byte comparison, bench/scale/churn_scale.py)\n")
    print("| pair | old B | new B | differing % | runs | runs (gap<=8) | median run | largest unchanged | in unchanged runs >=64K | >=256K | >=1M | .text % | .rodata % | .gopclntab % | .debug_* % (bytes) |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|")
    for l in ORDER:
        c = churn.get(l)
        if not c: continue
        sec = {s["name"]: s for s in c["sections"]}
        dbg = [s for s in c["sections"] if s["name"].startswith(".debug_")]
        dbgs = f"{100*sum(s['diff'] for s in dbg)/sum(s['size'] for s in dbg):.1f}% ({sum(s['diff'] for s in dbg):,})" if dbg else "-"
        g = lambda n: f"{sec[n]['pct']:.1f}%" if n in sec else "-"
        cov = c["unchanged_cov"]
        print(f"| {l} | {c['old_bytes']:,} | {c['new_bytes']:,} | {c['pct']:.1f}% | {c['runs']:,} | {c['runs8']:,} | {c['median_run']} B | {c['largest_unchanged']:,} | {100*cov['65536']:.1f}% | {100*cov['262144']:.1f}% | {100*cov['1048576']:.1f}% | {g('.text')} | {g('.rodata')} | {g('.gopclntab')} | {dbgs} |")

if __name__ == "__main__":
    main()

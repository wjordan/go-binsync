#!/usr/bin/env python3
"""churn_scale.py [label=OLD:NEW ...]  — per-section byte churn for big binaries.

Reuses bench/analyze_diff.py (sections(), runs_of(), merged_run_count()) in section-aligned
mode without disassembly. Prints a markdown table per pair and writes
bench/out/results/churn_scale.json.
"""
import json, os, sys, time
import numpy as np
BENCH = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, BENCH)
from analyze_diff import sections, runs_of, merged_run_count
from delta_scale import default_pairs
KEEP = (".text", ".rodata", ".typelink", ".itablink", ".gopclntab", ".go.buildinfo", ".noptrdata", ".data",
        ".symtab", ".strtab", ".debug_info", ".debug_line", ".debug_loclists", ".debug_rnglists", ".debug_addr",
        ".debug_abbrev", ".debug_str", ".debug_frame", ".debug_gdb_scripts", ".debug_aranges", ".debug_ranges",
        ".rela", ".rela.plt", ".rela.dyn", ".data.rel.ro", ".dynsym", ".dynstr", ".eh_frame", ".plt", ".go.fipsinfo")

def churn(old_p, new_p):
    old = np.fromfile(old_p, dtype=np.uint8); new = np.fromfile(new_p, dtype=np.uint8)
    so, sn = sections(old_p), sections(new_p)
    byname = {s["name"]: s for s in sn}
    diff = np.zeros(old.size, dtype=bool); covered = np.zeros(old.size, dtype=bool)
    secs = []
    for s in so:
        if s["type"] == "NOBITS" or s["size"] == 0: continue
        covered[s["off"]:s["off"] + s["size"]] = True
        t = byname.get(s["name"])
        if t is None:
            diff[s["off"]:s["off"] + s["size"]] = True
        else:
            n = min(s["size"], t["size"])
            diff[s["off"]:s["off"] + n] = old[s["off"]:s["off"] + n] != new[t["off"]:t["off"] + n]
            if s["size"] > n: diff[s["off"] + n:s["off"] + s["size"]] = True
        d = diff[s["off"]:s["off"] + s["size"]]; st, ln = runs_of(d); db = int(d.sum())
        secs.append(dict(name=s["name"], size=s["size"], new_size=(t["size"] if t else None), diff=db,
                         pct=100 * db / s["size"], runs=int(st.size), runs8=merged_run_count(st, ln, 8),
                         compressed=False))
    n = min(old.size, new.size); raw = np.zeros(old.size, dtype=bool); raw[:n] = old[:n] != new[:n]
    diff |= raw & ~covered
    total = int(diff.sum()); ds, dl = runs_of(diff); us, ul = runs_of(~diff)
    cov = {str(thr): int(ul[ul >= thr].sum()) / old.size for thr in (4096, 65536, 262144, 1 << 20, 4 << 20)}
    return dict(old_bytes=int(old.size), new_bytes=int(new.size), diff_bytes=total, pct=100 * total / old.size,
                runs=int(ds.size), runs8=merged_run_count(ds, dl, 8), median_run=(int(np.median(dl)) if dl.size else 0),
                largest_unchanged=int(ul.max()) if ul.size else 0, unchanged_cov=cov, sections=secs)

def main():
    pairs = [(p.split("=")[0], *p.split("=")[1].split(":")) for p in sys.argv[1:]] or default_pairs()
    out = {}
    for label, o, n in pairs:
        t0 = time.time(); r = churn(o, n); r["seconds"] = time.time() - t0; out[label] = r
        print(f"\n## {label}: old={r['old_bytes']:,} new={r['new_bytes']:,} differing={r['diff_bytes']:,} ({r['pct']:.2f}%) "
              f"runs={r['runs']:,} (gap<=8: {r['runs8']:,}) median run={r['median_run']} B largest unchanged={r['largest_unchanged']:,} "
              f"unchanged>=64K {100*r['unchanged_cov']['65536']:.1f}% >=256K {100*r['unchanged_cov']['262144']:.1f}% >=1M {100*r['unchanged_cov']['1048576']:.1f}%  [{r['seconds']:.1f}s]")
        print("| section | old size | new size | differing | % | runs | runs (gap<=8) |\n|---|---:|---:|---:|---:|---:|---:|")
        for s in r["sections"]:
            if s["name"] in KEEP or s["diff"] > 0 and s["size"] >= 4096:
                print(f"| {s['name']} | {s['size']:,} | {s['new_size'] if s['new_size'] is not None else 'gone':,} | {s['diff']:,} | {s['pct']:.2f}% | {s['runs']:,} | {s['runs8']:,} |")
        sys.stdout.flush()
    os.makedirs(os.path.join(BENCH, "out", "results"), exist_ok=True)
    json.dump(out, open(os.path.join(BENCH, "out", "results", "churn_scale.json"), "w"), indent=1)

if __name__ == "__main__":
    main()

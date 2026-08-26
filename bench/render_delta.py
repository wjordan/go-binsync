#!/usr/bin/env python3
"""render_delta.py — markdown tables from out/results/delta.json."""
import json, os, sys
BENCH = os.path.dirname(os.path.abspath(__file__))
rows = json.load(open(os.path.join(BENCH, "out", "results", "delta.json")))
by = {(r["label"], r["method"]): r for r in rows}
methods = ["zstd-19", "zstd-19-long27", "zstd-22-ultra-long31", "zstd-3-long27", "zstd-9-long27", "xdelta3-9-djw", "xdelta3-9-lzma", "bsdiff", "hdiffz-m6-zstd21", "hdiffz-m6-lzma2", "hdiffz-s64-zstd21"]
pairs = ["v1-v2s", "v1-v2l", "v1-v2c", "v1-v2p", "v1-v3", "v2c-v3", "v3-v4", "v1-v4"]
def cell(r, k, f="{:,}"):
    if r is None: return "n/a"
    if "error" in r: return "ERR"
    return f.format(r[k])
for cfg in ("F1", "F2"):
    print(f"\n#### Patch size (bytes), {cfg}\n")
    print("| pair | " + " | ".join(methods) + " |"); print("|---|" + "---:|" * len(methods))
    for p in pairs:
        print(f"| {p} | " + " | ".join(cell(by.get((f"{cfg}.{p}", m)), "patch_bytes") for m in methods) + " |")
    print(f"\n#### Full-download baselines, {cfg} (bytes; encode s; decode s)\n")
    print("| variant | zstd -19 | zstd -19 --long=31 | xz -9 |"); print("|---|---:|---:|---:|")
    for v in ("v2s", "v2l", "v2c", "v2p", "v3", "v4"):
        print(f"| {v} | " + " | ".join(f"{cell(by.get((f'{cfg}.{v}', m)), 'patch_bytes')} ({cell(by.get((f'{cfg}.{v}', m)), 'enc_s', '{:.2f}')} s / {cell(by.get((f'{cfg}.{v}', m)), 'dec_s', '{:.2f}')} s)" for m in ("full-zstd-19", "full-zstd-19-long31", "full-xz-9")) + " |")
    for p in ("v1-v2c", "v1-v3"):
        print(f"\n#### Encode/apply time and peak RSS, {cfg} {p} (min of 3 wall; max RSS)\n")
        print("| method | patch bytes | encode s | encode RSS MB | apply s | apply RSS MB | verified |"); print("|---|---:|---:|---:|---:|---:|---|")
        for m in methods + ["zstd-19-T0", "hdiffz-m6-zstd21-p8"]:
            r = by.get((f"{cfg}.{p}", m))
            if r is None: continue
            if "error" in r: print(f"| {m} | ERR: {r['error'][:80]} | | | | | |"); continue
            print(f"| {m} | {r['patch_bytes']:,} | {r['enc_s']:.2f} | {r['enc_rss_kb']/1024:.0f} | {r['dec_s']:.2f} | {r['dec_rss_kb']/1024:.0f} | {r['verified']} |")
print("\n#### PIE and mismatched-flags cases\n")
print("| case | method | patch bytes | encode s | apply s | verified |"); print("|---|---|---:|---:|---:|---|")
for r in rows:
    if r["label"].startswith(("F2pie", "mismatch")):
        print(f"| {r['label']} | {r['method']} | {cell(r,'patch_bytes')} | {cell(r,'enc_s','{:.2f}')} | {cell(r,'dec_s','{:.2f}')} | {r.get('verified', r.get('error','')[:60])} |")
print("\n#### Chain vs direct (bytes): v1->v2c + v2c->v3 + v3->v4 vs v1->v4\n")
print("| cfg | method | v1->v2c | v2c->v3 | v3->v4 | chain sum | direct v1->v4 | chain/direct |"); print("|---|---|---:|---:|---:|---:|---:|---:|")
for cfg in ("F1", "F2"):
    for m in ("bsdiff", "zstd-19", "hdiffz-m6-zstd21", "xdelta3-9-lzma"):
        a, b, c, d = (by.get((f"{cfg}.{p}", m), {}).get("patch_bytes") for p in ("v1-v2c", "v2c-v3", "v3-v4", "v1-v4"))
        if None in (a, b, c, d): continue
        print(f"| {cfg} | {m} | {a:,} | {b:,} | {c:,} | {a+b+c:,} | {d:,} | {(a+b+c)/d:.2f} |")
bad = [r for r in rows if r.get("verified") is False or "error" in r]
print(f"\nfailures/unverified: {len(bad)}")
for r in bad: print(f"  {r['label']} {r['method']}: {r.get('error','apply output != NEW')}")

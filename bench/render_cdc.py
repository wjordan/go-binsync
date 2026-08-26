#!/usr/bin/env python3
"""render_cdc.py — markdown tables from out/results/cdc_desync.json and cdc_sim.json."""
import json, os
BENCH = os.path.dirname(os.path.abspath(__file__))
R = os.path.join(BENCH, "out", "results")
d = json.load(open(os.path.join(R, "cdc_desync.json")))
print("| chunking | pair | chunks in NEW | new chunks (not in OLD) | new raw bytes | new compressed bytes (store) | index bytes | extract --seed s | verified |")
print("|---|---|---:|---:|---:|---:|---:|---:|---|")
for p in d["pairs"]:
    print(f"| {p['cfg']} | {p['old']}->{p['new']} | {p['chunks_new_total']} | {p['unique_chunks_not_in_old']} ({100*p['unique_chunks_not_in_old']/p['chunks_new_total']:.0f}%) | {p['raw_bytes']:,} | {p['compressed_bytes']:,} | {p['index_bytes']:,} | {p['extract_seed_s']:.2f} | {p['verified']} |")
mk = [m for m in d["make"] if m["variant"] in ("v1", "v2c")]
print("\n`desync make` (whole file, store write included): " + "; ".join(f"{m['cfg']} {m['variant']}: {m['chunks']} chunks, {m['make_s']:.2f} s, {m['make_rss_kb']//1024} MB RSS" for m in mk))
s = json.load(open(os.path.join(R, "cdc_sim.json")))
print("\n| scheme | pair | chunks in NEW | new chunks | new raw bytes | zstd -19 each (sum) | zstd -19 concatenated | zstd -19 each + 1 MiB dict | dict penalty recovered |")
print("|---|---|---:|---:|---:|---:|---:|---:|---:|")
for r in s:
    pen = r["zstd19_each"] - r["zstd19_concat"]
    rec = (r["zstd19_each"] - r["zstd19_each_dict"]) / pen * 100 if pen > 0 and r["zstd19_each_dict"] else float("nan")
    print(f"| {r['scheme']} | {r['old']}->{r['new']} | {r['chunks_total']} | {r['chunks_new']} ({100*r['chunks_new']/r['chunks_total']:.0f}%) | {r['raw_bytes']:,} | {r['zstd19_each']:,} | {r['zstd19_concat']:,} | {r['zstd19_each_dict']:,} | {rec:.0f}% |")

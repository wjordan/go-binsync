#!/usr/bin/env python3
"""model.py — transfer-time model over measured results (Step 6).

time = (RTT + TTFB) * max(1, objects/parallelism) + bytes/bandwidth + encode_s + apply_s
Inputs: out/results/delta.json, out/results/cdc_desync.json. Output: markdown tables.
"""
import json, math, os

BENCH = os.path.dirname(os.path.abspath(__file__))
R = os.path.join(BENCH, "out", "results")
LINKS = [("100 Mbit/s, 100 ms RTT", 100e6, 0.100), ("20 Mbit/s, 200 ms RTT", 20e6, 0.200)]
TTFB, PAR = 0.020, 8
PAIRS = [("v1", "v2c"), ("v1", "v3")]
DELTA_METHODS = ["zstd-19", "zstd-22-ultra-long31", "zstd-3-long27", "xdelta3-9-lzma", "bsdiff", "hdiffz-m6-zstd21", "hdiffz-s64-zstd21"]

def main():
    delta = {(r["label"], r["method"]): r for r in json.load(open(os.path.join(R, "delta.json")))}
    cdc = json.load(open(os.path.join(R, "cdc_desync.json")))
    make_t = {(m["cfg"], m["variant"]): m["make_s"] for m in cdc["make"]}
    rows = []  # (pair, method, bytes, objects, enc, apply)
    for o, n in PAIRS:
        lab = f"F2.{o}-{n}"
        for m in ("full-zstd-19", "full-xz-9"):
            r = delta.get((f"F2.{n}", m))
            if r and "patch_bytes" in r:
                rows.append(((o, n), m + " (1 object)", r["patch_bytes"], 1, r["enc_s"], r["dec_s"]))
        for m in DELTA_METHODS:
            r = delta.get((lab, m))
            if r and "patch_bytes" in r:
                rows.append(((o, n), m + " (1 object)", r["patch_bytes"], 1, r["enc_s"], r["dec_s"]))
        for p in cdc["pairs"]:
            if (p["old"], p["new"]) == (o, n):
                objs = p["unique_chunks_not_in_old"] + 1
                rows.append(((o, n), f"desync CDC {p['cfg']} ({objs} objects)", p["compressed_bytes"] + p["index_bytes"], objs, make_t[(p["cfg"], n)], p["extract_seed_s"]))
    for name, bw, rtt in LINKS:
        print(f"\n### {name} (TTFB {int(TTFB*1000)} ms/object, {PAR} parallel fetches)\n")
        print("| pair | method | bytes | objects | latency s | transfer s | encode s | apply s | total s |")
        print("|---|---|---:|---:|---:|---:|---:|---:|---:|")
        for (o, n), m, b, objs, enc, app in rows:
            lat = (rtt + TTFB) * max(1, objs / PAR)
            xfer = b * 8 / bw
            print(f"| {o}->{n} | {m} | {b:,} | {objs} | {lat:.2f} | {xfer:.2f} | {enc:.2f} | {app:.2f} | {lat+xfer+enc+app:.2f} |")

if __name__ == "__main__":
    main()

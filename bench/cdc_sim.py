#!/usr/bin/env python3
"""cdc_sim.py — FastCDC (normalisation level 2, deterministic gear table) and
fixed-size chunking simulation over the F2 variant binaries.

Per pair reports: new-chunk count and raw bytes; zstd -19 of new chunks
individually (sum); zstd -19 of new chunks concatenated; zstd -19 of new
chunks individually with a 1 MiB dictionary trained (zstd --train) on OLD's chunks.

Gear hash: h_i = sum_{k<64} gear[b_{i-k}] << k (mod 2^64), i.e. a pure 64-byte
sliding window (not reset at chunk starts as in the reference FastCDC), which
lets it be vectorised with numpy. Masks use hash bits 40.. so all 64 window
bytes contribute. Cut rule (FastCDC NC=2): no cut before min; in [min, avg)
require (h & maskS)==0 with bits=log2(avg)+2; in [avg, max) require
(h & maskL)==0 with bits=log2(avg)-2; forced cut at max. min=avg/4, max=avg*4.
Writes JSON to out/results/cdc_sim.json.
"""
import hashlib, json, os, shutil, subprocess, sys, tempfile
import numpy as np

BENCH = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(BENCH, "out", "bin")
WORK = os.path.join(BENCH, "out", "cdcsim")
RESULTS = os.path.join(BENCH, "out", "results")
PAIRS = [("v1", "v2s"), ("v1", "v2l"), ("v1", "v2c"), ("v1", "v2p"), ("v1", "v3"), ("v3", "v4"), ("v1", "v4")]
AVGS = [4096, 16384, 65536, 262144]
CFG = "F2"
GEAR = np.random.default_rng(0x5eed).integers(0, 2**64, size=256, dtype=np.uint64)

def gear_hash(data):
    g = GEAR[data]
    h = np.zeros(len(data), dtype=np.uint64)
    for k in range(64):
        if k == 0:
            h += g
        else:
            h[k:] += g[:-k] << np.uint64(k)
    return h

def fastcdc(data, avg):
    mn, mx = avg // 4, avg * 4
    bits = int(np.log2(avg))
    h = gear_hash(data)
    maskS = np.uint64(((1 << (bits + 2)) - 1) << 40)
    maskL = np.uint64(((1 << (bits - 2)) - 1) << 40)
    candS = np.flatnonzero((h & maskS) == 0)
    candL = np.flatnonzero((h & maskL) == 0)
    cuts, p, n = [], 0, len(data)
    while p < n:
        if n - p <= mn:
            cuts.append(n); break
        lo, hi = p + mn, min(p + avg, n)
        i = np.searchsorted(candS, lo)
        if i < len(candS) and candS[i] < hi:
            p = int(candS[i]) + 1; cuts.append(p); continue
        lo, hi = max(p + avg, p + mn), min(p + mx, n)
        i = np.searchsorted(candL, lo)
        if i < len(candL) and candL[i] < hi:
            p = int(candL[i]) + 1; cuts.append(p); continue
        p = hi; cuts.append(p)
    return cuts

def fixed(data, size):
    return list(range(size, len(data), size)) + [len(data)]

def chunk_list(data, cuts):
    out, start = [], 0
    for c in cuts:
        b = data[start:c].tobytes(); out.append((hashlib.sha256(b).digest(), b)); start = c
    return out

def zstd_size_each(blobs, dict_path=None):
    """Compress each blob with zstd -19 in one batch invocation; return total .zst bytes."""
    if not blobs: return 0
    d = tempfile.mkdtemp(dir=WORK)
    names = []
    for i, b in enumerate(blobs):
        p = os.path.join(d, f"{i}"); open(p, "wb").write(b); names.append(p)
    cmd = ["zstd", "-q", "-19", "--no-progress"] + (["-D", dict_path] if dict_path else [])
    for i in range(0, len(names), 500):
        subprocess.run(cmd + names[i:i+500], check=True, timeout=600)
    tot = sum(os.path.getsize(p + ".zst") for p in names)
    shutil.rmtree(d)
    return tot

def zstd_size_concat(blobs):
    if not blobs: return 0
    p = os.path.join(WORK, "concat.bin"); open(p, "wb").write(b"".join(blobs))
    subprocess.run(["zstd", "-q", "-f", "-19", p, "-o", p + ".zst"], check=True, timeout=600)
    return os.path.getsize(p + ".zst")

def train_dict(blobs, path):
    d = tempfile.mkdtemp(dir=WORK)
    for i, b in enumerate(blobs):
        open(os.path.join(d, f"{i}"), "wb").write(b)
    p = subprocess.run(["zstd", "-q", "--train", "-r", d, "--maxdict=1MB", "-o", path], capture_output=True, text=True, timeout=600)
    shutil.rmtree(d)
    return p.returncode == 0, p.stderr[-200:]

def main():
    os.makedirs(WORK, exist_ok=True); os.makedirs(RESULTS, exist_ok=True)
    data = {v: np.fromfile(os.path.join(BIN, f"{v}-{CFG}"), dtype=np.uint8) for v in {x for pr in PAIRS for x in pr}}
    chunked = {}   # (variant, scheme) -> list of (hash, bytes)
    schemes = [("fastcdc", a) for a in AVGS] + [("fixed", 65536)]
    for v, arr in data.items():
        for kind, sz in schemes:
            cuts = fastcdc(arr, sz) if kind == "fastcdc" else fixed(arr, sz)
            chunked[(v, kind, sz)] = chunk_list(arr, cuts)
            lens = np.diff([0] + cuts)
            print(f"[chunk] {v} {kind}-{sz//1024}K: {len(cuts)} chunks, mean {lens.mean():.0f} B, min {lens.min()}, max {lens.max()}", flush=True)
    results = []
    dicts = {}
    for kind, sz in schemes:
        for o, n in PAIRS:
            oc, nc = chunked[(o, kind, sz)], chunked[(n, kind, sz)]
            oset = {h for h, _ in oc}
            new = {h: b for h, b in nc if h not in oset}
            blobs = list(new.values())
            raw = sum(len(b) for b in blobs)
            each = zstd_size_each(blobs)
            concat = zstd_size_concat(blobs)
            key = (o, kind, sz)
            if key not in dicts:
                dp = os.path.join(WORK, f"dict-{o}-{kind}-{sz}.zdict")
                ok, err = train_dict([b for _, b in oc], dp)
                dicts[key] = dp if ok else None
                if not ok: print(f"  dict train failed for {key}: {err}")
            withdict = zstd_size_each(blobs, dicts[key]) if dicts[key] else None
            r = dict(scheme=f"{kind}-{sz//1024}K", old=o, new=n, chunks_total=len(nc), chunks_new=len(blobs),
                     raw_bytes=raw, zstd19_each=each, zstd19_concat=concat, zstd19_each_dict=withdict,
                     dict_bytes=os.path.getsize(dicts[key]) if dicts[key] else None)
            results.append(r)
            print(f"[pair] {kind}-{sz//1024}K {o}->{n}: {len(nc)} chunks, {len(blobs)} new ({100*len(blobs)/len(nc):.1f}%), raw {raw:,}, zstd each {each:,}, concat {concat:,}, each+dict {withdict}", flush=True)
    json.dump(results, open(os.path.join(RESULTS, "cdc_sim.json"), "w"), indent=1)

if __name__ == "__main__":
    main()

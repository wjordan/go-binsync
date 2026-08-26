#!/usr/bin/env python3
"""cdc_desync.py — real-tool CDC measurements with desync (and one casync data point).

For each chunk config and each variant (F2 build), run `desync make` into a
per-config store, then for each (old,new) pair count the chunks of NEW absent
from OLD, their raw bytes and their compressed-on-disk bytes (sum of the
.cacnk chunk files in the store, which desync stores zstd-compressed).
Also times `desync extract --seed OLD` reconstruction of NEW.
Writes markdown to stdout and JSON to out/results/cdc_desync.json.
"""
import json, os, shutil, subprocess, sys, time

BENCH = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.join(BENCH, "out", "bin")
DS = os.path.join(BENCH, "out", "desync")
RESULTS = os.path.join(BENCH, "out", "results")
DESYNC = os.path.expanduser("~/go/bin/desync")
CONFIGS = ["4:16:64", "16:64:256", "64:256:1024"]
VARIANTS = ["v1", "v2s", "v2l", "v2c", "v2p", "v3", "v4"]
PAIRS = [("v1", "v2s"), ("v1", "v2l"), ("v1", "v2c"), ("v1", "v2p"), ("v1", "v3"), ("v2c", "v3"), ("v3", "v4"), ("v1", "v4")]
CFG = "F2"

def sh(argv, timeout=180, **kw):
    return subprocess.run(argv, capture_output=True, text=True, timeout=timeout, **kw)

def timed(argv, timeout=180):
    t0 = time.time()
    p = subprocess.run(["/usr/bin/time", "-f", "%e %M"] + argv, capture_output=True, text=True, timeout=timeout)
    el, rss = p.stderr.strip().splitlines()[-1].split()[-2:]
    return float(el), int(rss), p

def chunks_of(index):
    """desync list-chunks prints one chunk id per line in index order."""
    p = sh([DESYNC, "list-chunks", index])
    return [l.strip() for l in p.stdout.splitlines() if l.strip()]

def chunk_sizes(path, cfg):
    """desync chunk prints 'start length id' triples."""
    p = sh([DESYNC, "chunk", "-m", cfg, path])
    sizes = {}
    for l in p.stdout.splitlines():
        parts = l.split()
        if len(parts) >= 3:
            sizes[parts[2]] = int(parts[1])
    return sizes

def store_size(store, cid):
    return os.path.getsize(os.path.join(store, cid[:4], cid + ".cacnk"))

def main():
    os.makedirs(RESULTS, exist_ok=True)
    results = {"make": [], "pairs": [], "casync": None}
    for cfg in CONFIGS:
        tag = cfg.replace(":", "-")
        store = os.path.join(DS, f"store-{tag}")
        shutil.rmtree(store, ignore_errors=True); os.makedirs(store)
        idx = {}
        for v in VARIANTS:
            f = os.path.join(BIN, f"{v}-{CFG}")
            idx[v] = os.path.join(DS, f"{v}-{tag}.caibx")
            el, rss, p = timed([DESYNC, "make", "-s", store, "-m", cfg, idx[v], f])
            if p.returncode != 0:
                print(f"desync make failed: {p.stderr[-300:]}"); sys.exit(1)
            n = len(chunks_of(idx[v]))
            results["make"].append(dict(cfg=cfg, variant=v, chunks=n, make_s=el, make_rss_kb=rss))
            print(f"[make {cfg}] {v}: {n} chunks, {el}s, {rss} KB", flush=True)
        store_total = sum(os.path.getsize(os.path.join(dp, fn)) for dp, _, fns in os.walk(store) for fn in fns)
        sizes = {v: chunk_sizes(os.path.join(BIN, f"{v}-{CFG}"), cfg) for v in VARIANTS}
        for o, n in PAIRS:
            oc, nc = chunks_of(idx[o]), chunks_of(idx[n])
            oset = set(oc)
            new_ids = [c for c in nc if c not in oset]
            uniq_new = list(dict.fromkeys(new_ids))
            raw = sum(sizes[n][c] for c in uniq_new)
            comp = sum(store_size(store, c) for c in uniq_new)
            out = os.path.join(DS, f"extract-{tag}-{o}-{n}.bin")
            el, rss, p = timed([DESYNC, "extract", "-s", store, "--seed", f"{idx[o]}:{os.path.join(BIN, o+'-'+CFG)}", idx[n], out])
            ok = p.returncode == 0 and subprocess.run(["cmp", "-s", out, os.path.join(BIN, f"{n}-{CFG}")]).returncode == 0
            if os.path.exists(out): os.unlink(out)
            r = dict(cfg=cfg, old=o, new=n, chunks_new_total=len(nc), chunks_old_total=len(oc), chunk_refs_not_in_old=len(new_ids),
                     unique_chunks_not_in_old=len(uniq_new), raw_bytes=raw, compressed_bytes=comp,
                     extract_seed_s=el, extract_rss_kb=rss, verified=ok, store_total_bytes=store_total,
                     index_bytes=os.path.getsize(idx[n]))
            results["pairs"].append(r)
            print(f"[pair {cfg}] {o}->{n}: {len(nc)} chunks, {len(uniq_new)} new ({100*len(uniq_new)/len(nc):.1f}%), raw {raw:,} B, compressed {comp:,} B, extract(seed) {el}s ok={ok}", flush=True)
    # casync one data point
    cs = os.path.join(DS, "casync-store"); shutil.rmtree(cs, ignore_errors=True); os.makedirs(DS, exist_ok=True)
    try:
        el1, _, p1 = timed(["casync", "make", "--store=" + cs, os.path.join(DS, "casync-v1.caibx"), os.path.join(BIN, f"v1-{CFG}")])
        n1 = sum(len(fns) for _, _, fns in os.walk(cs)); b1 = sum(os.path.getsize(os.path.join(dp, fn)) for dp, _, fns in os.walk(cs) for fn in fns)
        el2, _, p2 = timed(["casync", "make", "--store=" + cs, os.path.join(DS, "casync-v2c.caibx"), os.path.join(BIN, f"v2c-{CFG}")])
        n2 = sum(len(fns) for _, _, fns in os.walk(cs)); b2 = sum(os.path.getsize(os.path.join(dp, fn)) for dp, _, fns in os.walk(cs) for fn in fns)
        results["casync"] = dict(v1_chunks=n1, v1_store_bytes=b1, v2c_new_chunks=n2 - n1, v2c_new_compressed_bytes=b2 - b1, make_v1_s=el1, make_v2c_s=el2,
                                 rc=(p1.returncode, p2.returncode), err=(p1.stderr[-200:] + p2.stderr[-200:]).strip())
        print(f"[casync default] v1: {n1} chunks {b1:,} B store; v2c adds {n2-n1} chunks, {b2-b1:,} B compressed; make {el1}s/{el2}s")
    except Exception as e:
        results["casync"] = dict(error=str(e)); print(f"casync failed: {e}")
    json.dump(results, open(os.path.join(RESULTS, "cdc_desync.json"), "w"), indent=1)

if __name__ == "__main__":
    main()

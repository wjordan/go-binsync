#!/usr/bin/env python3
"""model_scale.py [--cc cubic] — transfer-time model over MEASURED netem throughput.

Inputs: out/results/scale-small.json, scale-big.json (patch bytes, encode/apply time),
out/results/netem.json (measured single-connection and 8-way times for 4 object sizes per
profile). transfer(bytes) is piecewise-linear interpolation through the measured points
(0 B -> connect+TTFB of the smallest object, then the measured medians), extrapolated
beyond the largest measured object with the slope of the last segment (= its average
rate). total = transfer + encode + apply. Prints markdown.
"""
import json, os, sys
BENCH = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
R = os.path.join(BENCH, "out", "results")
PAIRS = ["prometheus-3.13.1-3.13.2-stripped", "prometheus-3.13.2-3.14.0-stripped", "kube-apiserver-1.36.3-1.36.4",
         "terraform-1.15.8-1.15.9", "cockroach-26.2.4-26.2.5-stripped", "vault-2.0.3-2.0.4-stripped", "F2.v1-v5", "F2.v4-v5"]
METHODS = [("full-zstd-19-T0", "full download"), ("hdiffz-m6-p8", "hdiffz -m-6 -p-8"), ("hdiffz-m6", "hdiffz -m-6"),
           ("zstd-19-T0", "zstd -19 -T0 --patch-from"), ("bsdiff", "bsdiff"), ("xdelta3-9-lzma", "xdelta3 -9 lzma")]
TLS_RTT = 2  # note: TLS 1.3 adds 1 RTT, TLS 1.2 adds 2; the netem runs are plain HTTP

def load_scale():
    d = {}
    for f in ("scale-small.json", "scale-big.json", "scale.json"):
        p = os.path.join(R, f)
        if os.path.exists(p):
            for r in json.load(open(p)):
                if "patch_bytes" in r: d[(r["label"], r["method"])] = r
    return d

class Profile:
    def __init__(self, recs):
        self.name = recs[0]["profile"]; self.mbit, self.rtt, self.loss = recs[0]["mbit"], recs[0]["rtt_ms"], recs[0]["loss"]
        pts = sorted((r["bytes"], r["single_med_s"]) for r in recs if r.get("single_med_s"))
        sm = next(r for r in recs if "ttfb_med_s" in r)
        self.overhead = sm["ttfb_med_s"]            # connect + request + first byte, fresh connection
        self.cond304 = sm["cond304_med_s"]
        self.pts = [(0, self.overhead)] + pts
        self.par8 = sorted((r["bytes"], r["par8_med_s"]) for r in recs if r.get("par8_med_s"))
        self.par4 = sorted((r["bytes"], r["par4_med_s"]) for r in recs if r.get("par4_med_s"))
        self.timeouts = [r["object"] + (" (not measured)" if r.get("not_measured") else " (timeout)") for r in recs if r.get("single_med_s") is None]
        self.maxb = self.pts[-1][0]
    @staticmethod
    def interp(pts, b):
        if len(pts) < 2: return float("nan")
        for (x0, y0), (x1, y1) in zip(pts, pts[1:]):
            if b <= x1: return y0 + (y1 - y0) * (b - x0) / (x1 - x0)
        (x0, y0), (x1, y1) = pts[-2], pts[-1]
        return y1 + (y1 - y0) * (b - x1) / (x1 - x0)
    def t(self, b): return self.interp(self.pts, b)
    def t8(self, b):
        if b < 4 << 20 or not self.par8: return self.t(b)
        if len(self.par8) == 1: return b * self.par8[0][1] / self.par8[0][0]   # one point: scale proportionally
        return self.interp(self.par8, b)
    def rate_tail(self):  # average Mbit/s of the last measured segment
        (x0, y0), (x1, y1) = self.pts[-2], self.pts[-1]; return (x1 - x0) * 8 / (y1 - y0) / 1e6
    def size_for_time(self, secs):
        lo, hi = 0, 1 << 34
        for _ in range(80):
            mid = (lo + hi) / 2
            if self.t(mid) - self.overhead < secs: lo = mid
            else: hi = mid
        return lo

def main():
    cc = "cubic"
    a = sys.argv[1:]
    while a:
        k = a.pop(0)
        if k == "--cc": cc = a.pop(0)
    scale = load_scale()
    net = [r for r in json.load(open(os.path.join(R, "netem.json"))) if r["cc"] == cc]
    profs = {}
    for r in net: profs.setdefault(r["profile"], []).append(r)
    P = {k: Profile(v) for k, v in sorted(profs.items())}
    print(f"### Measured transfer curve per profile (cc={cc}): seconds for a single-connection GET, from netem medians\n")
    print("| profile | link | fresh-conn overhead s (connect+TTFB) | 304 s | " + " | ".join(f"t({s})" for s in ("64 KB", "1 MB", "8 MB", "30 MB", "100 MB", "300 MB")) + " | tail rate Mbit/s | 8-way t(30 MB) | timeouts |")
    print("|---|---|---:|---:|" + "---:|" * 6 + "---:|---:|---|")
    for k, p in P.items():
        sizes = (64 << 10, 1 << 20, 8 << 20, 30 << 20, 100 << 20, 300 << 20)
        print(f"| {k} | {p.mbit} Mbit/{p.rtt} ms/{p.loss:g}% | {p.overhead:.2f} | {p.cond304:.2f} | " + " | ".join(f"{p.t(s):.1f}" + ("*" if s > p.maxb else "") for s in sizes) +
              f" | {p.rate_tail():.2f} | {p.t8(30<<20):.1f} | {', '.join(p.timeouts) or '-'} |")
    print("\n(* = beyond the largest measured object for that profile: linear extrapolation at the tail rate, i.e. model, not measurement. Profile D's largest measured object is 8.4 MB.)\n")

    print("### Transfer + encode + apply per pair and method (seconds); enc = single-thread unless the method is multithreaded\n")
    print("| pair | method | patch B | enc s | apply s | " + " | ".join(f"{k}: xfer / total" for k in P) + " | D 8-way xfer |")
    print("|---|---|---:|---:|---:|" + "---:|" * len(P) + "---:|")
    for lab in PAIRS:
        for m, mname in METHODS:
            r = scale.get((lab, m))
            if not r: continue
            b, enc, app = r["patch_bytes"], r["enc_s"], r["dec_s"]
            cells = " | ".join(f"{p.t(b):.1f} / {p.t(b)+enc+app:.1f}" for p in P.values())
            d = P.get("D")
            print(f"| {lab} | {mname} | {b:,} | {enc:.1f} | {app:.2f} | {cells} | {d.t8(b) if d else float('nan'):.1f} |")

    print("\n### Resume cost when the connection drops at 50 % (bytes re-downloaded; extra seconds vs. no drop)\n")
    print("| pair | method | patch B | no Range: re-download B | " + " | ".join(f"{k} no-Range extra s / Range extra s" for k in P) + " |")
    print("|---|---|---:|---:|" + "---:|" * len(P))
    for lab in PAIRS:
        for m, mname in METHODS:
            r = scale.get((lab, m))
            if not r or m not in ("full-zstd-19-T0", "hdiffz-m6-p8", "zstd-19-T0", "bsdiff"): continue
            b = r["patch_bytes"]
            cells = " | ".join(f"{p.t(b):.1f} / {p.t(b/2) - (p.t(b) - p.t(b/2)) + p.overhead*0:.1f}".replace("/ ", "/ ") for p in P.values())
            # no Range: the whole object is fetched again -> extra = t(b) (a fresh connection incl. overhead)
            # Range:    the remaining half is fetched on a fresh connection -> extra = t(b/2) - [time the 2nd half would have taken in-stream]
            cells = " | ".join(f"{p.t(b):.1f} / {max(0.0, p.t(b/2) - (p.t(b) - p.t(b/2))):.1f}" for p in P.values())
            print(f"| {lab} | {mname} | {b:,} | {b:,} (Range: {b//2:,} remaining, 0 wasted) | {cells} |")

    print("\n### When does a second HTTP request stop mattering? (patch as a separate object vs inline in the pointer)\n")
    print("| profile | extra request cost s (fresh conn, plain HTTP) | +TLS 1.3 (1 RTT) s | size where transfer = 1x cost | = 10x cost (overhead < 10 %) | 304 poll s |")
    print("|---|---:|---:|---:|---:|---:|")
    for k, p in P.items():
        c = p.overhead
        print(f"| {k} | {c:.2f} | {c + p.rtt/1000:.2f} | {p.size_for_time(c)/1024:,.0f} KB | {p.size_for_time(10*c)/1024/1024:,.1f} MB | {p.cond304:.2f} |")

if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""analyze_diff.py OLD NEW [--samples N] [--no-disasm]

Byte-level diff of two ELF binaries: total differing bytes, differing runs,
per-section table, unchanged-run coverage, and (for .text) a categorisation of
what changed at the instruction level using objdump.
"""
import bisect, re, subprocess, sys
import numpy as np

SECTIONS_OF_INTEREST = [".text", ".rodata", ".typelink", ".itablink", ".gopclntab",
    ".go.buildinfo", ".noptrdata", ".data", ".rela.dyn", ".rela.plt", ".symtab", ".strtab",
    ".note.go.buildid", ".dynsym", ".dynstr", ".go.fipsinfo", ".go.module", ".shstrtab"]

def sections(path):
    out = subprocess.check_output(["readelf", "-S", "-W", path], text=True)
    secs = []
    for line in out.splitlines():
        m = re.match(r"\s*\[\s*(\d+)\]\s+(\S+)\s+(\S+)\s+([0-9a-f]+)\s+([0-9a-f]+)\s+([0-9a-f]+)", line)
        if not m or m.group(3) == "NULL":
            continue
        secs.append(dict(idx=int(m.group(1)), name=m.group(2), type=m.group(3),
                         addr=int(m.group(4), 16), off=int(m.group(5), 16), size=int(m.group(6), 16)))
    return secs

def runs_of(mask):
    """Return (starts, lengths) of True runs in a boolean array."""
    if mask.size == 0:
        return np.array([], dtype=np.int64), np.array([], dtype=np.int64)
    padded = np.concatenate(([False], mask, [False]))
    edges = np.flatnonzero(padded[1:] != padded[:-1])
    starts, ends = edges[0::2], edges[1::2]
    return starts, ends - starts

def merged_run_count(starts, lengths, gap):
    if starts.size == 0:
        return 0
    ends = starts + lengths
    return 1 + int(np.count_nonzero(starts[1:] - ends[:-1] > gap))

def fmt(n):
    return f"{n:,}"

def objdump(path):
    """address -> (mnemonic, operands) for .text, plus sorted address list."""
    out = subprocess.run(["objdump", "-d", "-j", ".text", "--no-show-raw-insn", path],
                         capture_output=True, text=True).stdout
    ins = {}
    for line in out.splitlines():
        m = re.match(r"\s*([0-9a-f]+):\t(\S+)\s*(.*)$", line)
        if m:
            ins[int(m.group(1), 16)] = (m.group(2), m.group(3).split("#")[0].strip())
    return ins, sorted(ins)

HEXRE = re.compile(r"0x[0-9a-f]+|\b[0-9a-f]{5,}\b")
def mask_addrs(s):
    return HEXRE.sub("ADDR", s)

def categorise(vaddr, old_ins, old_addrs, new_ins, new_addrs):
    i = bisect.bisect_right(old_addrs, vaddr) - 1
    if i < 0:
        return "unmapped", None, None
    oa = old_addrs[i]
    o = old_ins[oa]
    n = new_ins.get(oa)
    if n is None:
        # instruction boundary shifted: look for the same instruction nearby
        j = bisect.bisect_right(new_addrs, vaddr) - 1
        n2 = new_ins[new_addrs[j]] if j >= 0 else None
        return "insn-boundary-shifted", o, n2
    if o == n:
        # this byte run starts inside an instruction whose text is identical; the
        # difference must be in a following instruction (rare) or raw-only.
        return "text-identical(?)", o, n
    if o[0] == n[0]:
        if "(%rip)" in o[1] or o[0].startswith(("call", "jmp", "j", "lea")) or mask_addrs(o[1]) == mask_addrs(n[1]):
            return "same-insn-displacement/target-changed", o, n
        return "same-insn-immediate-changed", o, n
    # different mnemonic at same address: shifted code? try to find o at vaddr+delta
    for d in (16, 32, 48, 64, 96, 128, 160, 192, 256, 512, -16, -32, -64):
        n3 = new_ins.get(oa + d)
        if n3 and n3[0] == o[0] and mask_addrs(n3[1]) == mask_addrs(o[1]):
            return f"code-shifted(delta={d:+d})", o, n3
    return "different-instruction", o, n

def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    old_p, new_p = args[0], args[1]
    nsamples = 20
    for a in sys.argv[1:]:
        if a.startswith("--samples="):
            nsamples = int(a.split("=")[1])
    do_disasm = "--no-disasm" not in sys.argv
    old = np.fromfile(old_p, dtype=np.uint8)
    new = np.fromfile(new_p, dtype=np.uint8)
    so, sn = sections(old_p), sections(new_p)
    same_layout = (old.size == new.size) and all(
        (a["name"], a["off"], a["size"]) == (b["name"], b["off"], b["size"]) for a, b in zip(so, sn))
    print(f"## {old_p.split('/')[-1]} -> {new_p.split('/')[-1]}")
    print(f"old={fmt(old.size)} B new={fmt(new.size)} B; section layout identical: {same_layout}")
    mode = "raw file offsets" if same_layout else "section-aligned (each section compared from its own start)"
    print(f"comparison mode: {mode}")

    if same_layout:
        diff = old != new
    else:
        # build a section-aligned diff mask over OLD's file layout
        diff = np.zeros(old.size, dtype=bool)
        byname = {s["name"]: s for s in sn}
        for s in so:
            if s["type"] == "NOBITS" or s["size"] == 0:
                continue
            t = byname.get(s["name"])
            if t is None:
                diff[s["off"]:s["off"] + s["size"]] = True
                continue
            n = min(s["size"], t["size"])
            diff[s["off"]:s["off"] + n] = old[s["off"]:s["off"] + n] != new[t["off"]:t["off"] + n]
            if s["size"] > n:
                diff[s["off"] + n:s["off"] + s["size"]] = True
        # bytes outside any section (ELF header, program headers, section header table)
        covered = np.zeros(old.size, dtype=bool)
        for s in so:
            if s["type"] != "NOBITS":
                covered[s["off"]:s["off"] + s["size"]] = True
        n = min(old.size, new.size)
        raw = np.zeros(old.size, dtype=bool); raw[:n] = old[:n] != new[:n]
        diff |= raw & ~covered

    total = int(diff.sum())
    dstarts, dlens = runs_of(diff)
    print(f"\ntotal differing bytes: {fmt(total)} ({100*total/old.size:.3f}% of old)")
    print(f"differing runs (byte-exact): {fmt(dstarts.size)}; merged with gap<=8: {fmt(merged_run_count(dstarts, dlens, 8))}; gap<=64: {fmt(merged_run_count(dstarts, dlens, 64))}")
    if dlens.size:
        print(f"differing run length: median {int(np.median(dlens))} B, mean {dlens.mean():.1f} B, max {fmt(int(dlens.max()))} B")

    ustarts, ulens = runs_of(~diff)
    print(f"largest contiguous unchanged region: {fmt(int(ulens.max()) if ulens.size else 0)} B")
    for thr in (4096, 16384, 65536, 262144, 1 << 20):
        cov = int(ulens[ulens >= thr].sum())
        print(f"  fraction of file in unchanged runs >= {thr//1024:>4} KiB: {100*cov/old.size:6.2f}%  ({fmt(cov)} B)")

    print("\n| section | size | differing bytes | % differing | runs (byte-exact) | runs (gap<=8) |")
    print("|---|---:|---:|---:|---:|---:|")
    for s in so:
        if s["type"] == "NOBITS" or s["size"] == 0:
            continue
        d = diff[s["off"]:s["off"] + s["size"]]
        if s["name"] not in SECTIONS_OF_INTEREST and s["size"] < 4096 and not d.any():
            continue
        st, ln = runs_of(d)
        db = int(d.sum())
        print(f"| {s['name']} | {fmt(s['size'])} | {fmt(db)} | {100*db/s['size']:.3f}% | {fmt(st.size)} | {fmt(merged_run_count(st, ln, 8))} |")

    text = next((s for s in so if s["name"] == ".text"), None)
    if not do_disasm or text is None:
        return
    d = diff[text["off"]:text["off"] + text["size"]]
    tstarts, tlens = runs_of(d)
    if tstarts.size == 0:
        print("\n.text: no differing bytes")
        return
    old_ins, old_addrs = objdump(old_p)
    new_ins, new_addrs = objdump(new_p)
    cats = {}
    for st in tstarts:
        c, _, _ = categorise(text["addr"] + int(st), old_ins, old_addrs, new_ins, new_addrs)
        cats[c] = cats.get(c, 0) + 1
    print(f"\n.text differing runs categorised by instruction at run start ({fmt(tstarts.size)} runs):")
    for c, n in sorted(cats.items(), key=lambda kv: -kv[1]):
        print(f"  {n:>8,}  {c}")
    print(f"\n{nsamples} sampled .text differing runs (evenly spaced):")
    idxs = np.linspace(0, tstarts.size - 1, min(nsamples, tstarts.size)).astype(int)
    for k in idxs:
        st, ln = int(tstarts[k]), int(tlens[k])
        va = text["addr"] + st
        c, o, n = categorise(va, old_ins, old_addrs, new_ins, new_addrs)
        ob = old[text["off"] + st:text["off"] + st + min(ln, 8)].tobytes().hex()
        nb = new[text["off"] + st:text["off"] + st + min(ln, 8)].tobytes().hex()
        print(f"  0x{va:x} len={ln:<3} old={ob:<16} new={nb:<16} [{c}]")
        print(f"      old: {o[0]} {o[1]}" if o else "      old: ?")
        print(f"      new: {n[0]} {n[1]}" if n else "      new: ?")
    # a few via objdump --start-address as a cross-check
    print("\nobjdump --start-address cross-check for 3 samples:")
    for k in idxs[:: max(1, len(idxs) // 3)][:3]:
        va = text["addr"] + int(tstarts[k])
        i = bisect.bisect_right(old_addrs, va) - 1
        start = old_addrs[max(i - 1, 0)]
        for p in (old_p, new_p):
            out = subprocess.run(["objdump", "-d", "-j", ".text", f"--start-address=0x{start:x}", f"--stop-address=0x{start+24:x}", p],
                                 capture_output=True, text=True).stdout
            lines = [l for l in out.splitlines() if re.match(r"\s*[0-9a-f]+:", l)]
            print(f"  {p.split('/')[-1]}:")
            for l in lines[:4]:
                print("    " + l.strip())

if __name__ == "__main__":
    main()

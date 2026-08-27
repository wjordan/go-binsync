# Go-aware predict-then-correct delta transform: prototype results

Research notes for go-binsync. Scope: a decoder-side *prediction* of the new Go
binary from the old one plus a small transmitted layout table, followed by a
generic delta (bsdiff / hdiffz / `zstd --patch-from`) that corrects the
prediction. The numbers of interest are the residual correction sizes against
the plain baseline deltas.

Everything here comes from the prototype in `bench/gotransform/` (Go,
`golang.org/x/arch v0.20.0` for x86 decoding) and its logs in
`bench/out/logs/06-gt-*.log`. No new measurements were taken for this write-up;
where a cell has no log line it says "not measured". Background:
`go-binary-layout.md` §1–2 (why a one-line change rewrites 13 % of the file) and
`benchmark-local.md` §2–3 (baseline encoders on the same synthetic pairs).

Inputs:

* Synthetic pairs: the `bench/testsrv` F2 binaries (`-trimpath -ldflags="-s -w"`,
  go1.26.4, 29,561,097 B, 32,838 functions). Variants (benchmark-local.md §1):
  `v2s` same-length string edit; `v2l` +3-byte string; `v2c` one added
  statement in `main.helloHandler` (512 → 704 B); `v2p` new function in
  `internal/util`; `v3` = v2c+v2p; `v4` = v3 + a one-line change in
  `internal/third`.
* Real corpus (adjacent releases, all linux/amd64, `-s -w` or stripped copies):
  kube-apiserver 1.36.3 → 1.36.4 (go1.26.5, 88.4 MB), prometheus 3.13.1 → 3.13.2
  (go1.26.5, 97.6 MB) and 3.13.2 → 3.14.0 (go1.26.5 → go1.26.6, 102.5 MB),
  terraform 1.15.8 → 1.15.9 (go1.25.10, 117.8 MB), cockroach 26.2.4 → 26.2.5
  (go1.25.5, cgo/C++, 230.3 MB stripped), vault 2.0.3 → 2.0.4 (go1.26.4 →
  go1.26.5, 393.8 MB stripped). Corpus runs used `-enc bh` (bsdiff + hdiffz;
  zstd columns are 0 = not run); vault used `-enc h` only, and bsdiff was
  killed on vault's .text in the sectdiff run.

## 1. Summary of findings

*Update: §10 (Go 1.27 pass) supersedes the headline numbers below — the pipeline
now transmits everything it uses, runs end-to-end (encode → patch → decode,
hash-verified), and regenerates type-descriptor offsets: prometheus
3.13.1→3.13.2 built with Go 1.27 is 2,691,644 → 237,585 B (11.3×, positional
stage 2) / 206,902 B (13.0×, hdiffz stage 2); the Go 1.26 official pair
2,714,204 → 184,388 B (14.7×).*

*Update 2: §11 (round 4) supersedes §10's headline: with the pointer-target
consensus and the regenerated pc tables / go:func.*, prometheus/1.27 is
2,691,644 → 111,552 B (24.1×; 94,470 B / 28.5× with hdiffz stage 2), the
encoder runs in 2.1 s instead of 12.6 s, and the synthetic v3→v4 gcbits
defect is closed (6,486 → 578 B).*

1. **The whole-file pipeline cuts the one-line-change patch by 12–38x on the
   synthetic pairs** (`06-gt-whole.log`): v1→v2c bsdiff 60,478 → 3,244 B
   (18.6x), hdiffz 75,305 → 3,226 B (23.3x), zstd 213,124 → 11,221 B (19.0x);
   v1→v2l 24,322 → 642 B (37.9x; hdiffz 32,899 → 333 B, 98.8x); v1→v4 (three
   packages touched) 66,541 → 5,247 B (12.7x); v3→v4 29,439 → 1,756 B (16.8x).
   The v2s pair (nothing moves) gets 0.7x: the two-stage framing has a fixed
   ~170 B overhead that a 347 B patch cannot amortise.
2. **On a real patch release it is 5.7x** (`06-gt-corpus-kube-apiserver-*-whole.log`):
   kube-apiserver 1.36.3→1.36.4 bsdiff 2,060,250 → 358,607 B (layout 1,106 +
   stage 1 66,126 + stage 2 291,375), hdiffz 2,366,695 → 410,589 B (5.8x).
3. **.text prediction is the strongest stage.** Synthetic v2c: .text baseline
   bsdiff 39,961 → 659 B (61x), the residual being exactly the 639 changed bytes
   of `main.helloHandler` plus three functions x86asm cannot fully decode
   (`06-gt-predict.log`). Corpus: kube-apiserver 1,016,241 → 32,465 B (31x),
   cockroach 2,849,784 → 217,482 B (13x), prometheus 3.13.2 1,098,950 → 149,309 B
   (7.4x), vault hdiffz 3,887,504 → 719,144 B (5.4x), terraform 2,092,232 →
   577,078 B (3.6x), prometheus 3.14.0 2,539,721 → 1,063,983 B (2.4x)
   (`06-gt-corpus-*-predict.log`).
4. **Relocation without a data map is worth only ~2x; the content map is what
   makes it 30–60x.** kube: P1 (section-identity map) 503,417 B, P2 (content
   map) 32,465 B; v2c: P1 33,633 B, P2 659 B. Most `.text` churn is RIP-relative
   `lea`/`mov` into `.rodata` (110,611 of 700,753 references in v1; 410,551 of
   2.14 M in kube), so predicting where `.rodata` symbols moved is the core.
5. **What remains in .text on the corpus is real change plus `.bss`**:
   prometheus 3.13.2's 225,534 residual bytes are 55,865 changed-function
   content + 12,194 new functions + **150,760 mispredicted `.bss/.noptrbss`
   references**; terraform 277,319 and vault 693,201 bss bytes. The shift-table
   mechanism (`shifttab.go`) that fixes exactly this was added after the corpus
   binary was built and therefore never ran on the corpus; on the synthetic
   pairs bss did not move (0 breakpoints), so it was never exercised with
   non-zero content. The mispredict dumps show a single dominant constant shift
   (+16 for 141,557 of prometheus' samples, +16 for 82,331 of cockroach's), i.e.
   a one-breakpoint table.
6. **The 32-byte function alignment, names, sizes and order all survive `-s -w`**:
   every corpus function sits on a 32-byte boundary (105,507/105,507 kube;
   500,917/500,917 vault); only the synthetic binary's 14 cgo C functions
   (`_cgo_*`, `x_cgo_*`) are unaligned. Go functions cover 100.00 % of `.text`
   except cockroach (97.24 %: 2.84 MB of C/C++ after `go:textfipsend`).
7. **Name matching is nearly total on patch releases**: kube 105,482 exact + 24
   content matches, 49 unmatched-new; vault 500,852 exact + 1 normalised + 23
   content, 619 unmatched-new; cockroach needs the closure-renumbering
   normaliser for 125 functions (`sql.init.makePostgresBoolGetStringValFn.func1143
   → .func1162`). The minor release prometheus 3.14.0 has 6,326 unmatched-new
   and 606 unmatched-old (`06-gt-corpus-*-inventory.log`).
8. **The layout table is tiny**: 30 B zstd for the synthetic pairs (23 B when no
   size changes), 1,106 B kube, 1,097 B prometheus 3.13.2, 2,702 B cockroach,
   9,028 B vault, 10,598 B terraform, 29,538 B prometheus 3.14.0.
9. **pclntab regeneration works and is exact on Go 1.26 synthetic pairs**:
   `findfunctab` regenerates byte-identically for every binary; the V4
   prediction leaves 12–15 differing bytes of 10.54 MB and costs 1,979 B
   (v2c, incl. the transmitted pctab/go:func.* deltas) vs 14,259 B baseline
   (7.2x). On the corpus V4 gives kube 82,334 vs 584,968 (7.1x), prometheus
   3.13.2 101,774 vs 1,036,118 (10.2x), vault hdiffz 675,811 vs 4,124,406
   (6.1x), prometheus 3.14.0 790,888 vs 2,414,157 (3.1x)
   (`06-gt-corpus-*-pclnpredict.log`).
10. **The Go 1.25 layout is not handled**: for terraform and cockroach the
    pclntab self-prediction (old → old) already differs by 33.0 MB / 68.6 MB and
    the predicted length is wrong, so their pclnpredict rows are invalid
    (cockroach V4 is *worse* than baseline: 1,749,107 vs 1,220,585 B). Cause
    not diagnosed.
11. **Data sections are the weakest stage on the corpus.** Re-laying `.rodata`
    blocks through the content map and rewriting absolute pointers gets
    kube `.rodata` 340,444 → 236,250 B (1.4x), prometheus 3.13.2 443,232 →
    256,723 B, terraform 1,407,168 → 1,172,460 B, but `.data` 8,394 → 1,108 B
    (kube) and 45,623 → 14,252 B (terraform) (`06-gt-corpus-*-datapredict.log`).
    After the whole pipeline, kube's stage-2 residual is dominated by
    `.gopclntab` (6,675,145 differing bytes, cheap: a 64-byte length slip) and
    `.rodata` (873,817 bytes in 464,969 runs, expensive: ~236 KB of the 291 KB
    stage 2).
12. **Sectioning costs nothing**: SUM(per-section deltas) / WHOLE-FILE is
    0.99–1.02 for every pair except v2s (1.73, fixed overhead), so per-section
    accounting is a valid decomposition of the baseline
    (`06-gt-sectdiff.log`, `06-gt-corpus-*-sectdiff.log`).
13. **Cost**: x86asm decodes 17–29 MB/s single-thread (0.54 s for 14.8 MB of
    `.text`, 6.0 s for vault's 159 MB); relocation with 24 goroutines takes
    0.17 s (synthetic) to 1.6 s (cockroach/vault); the content-map build takes
    0.30 s (3.5 MB `.rodata`) to 13.1 s (vault, 95 MB). The generic delta on the
    residual dominates (bsdiff 2.8 s on the 14.8 MB `.text`, 65 s on
    cockroach's 106 MB). Memory: not measured.
14. **Layout-exactness**: the `.text` and whole-file predictions have exactly
    the new length by construction; the pclntab prediction matched the new
    length in 7/7 synthetic pairs and 0/4 valid corpus pairs (off by −64, +128,
    +688, +4,384 B). Where the prediction is layout-exact, stage 2 is a sparse
    positional correction: v2c 2,127 differing bytes in 787 runs, v2l 179 in
    16, v3→v4 312 in 117 (`06-gt-whole.log`) — encodable without a suffix array.
15. **Honest accounting gaps**: the `.rodata/.noptrdata/.data` content maps
    and the new section table are oracle inputs to the prototype (built from
    the actual new section, not transmitted); a transmitted piecewise map
    would cost roughly one varint pair per shift change (3–8 for the synthetic
    pairs, 1,507 + 13 + 13 for kube, 6,124 + 211 + 1 for cockroach).

## 2. Inventory: what survives `-s -w`

`inventory.go` / `elfbin.go` parse the ELF, locate `moduledata` (the `.go.module`
section on Go 1.26; on ≤ 1.25 by scanning `.noptrdata`/`.data` for the
`pcHeader` pointer followed by the funcnametab slice), then read the Go 1.20+
pclntab (`magic 0xfffffff1`): `pcHeader` offsets, `functab` (pairs of
`entryOff, funcOff` relative to `moduledata.text`), each `_func` record's
`nameOff`, and `funcnametab`. Function *end* = next entry (last one: `maxpc`).
Nothing needs `.symtab`.

Synthetic v1-F2 (`06-gt-inventory-v1-v2c.log`): 32,838 functions, 1,910 files;
`.gopclntab` 10,541,954 B = funcnametab 2,084,432 + cutab 76,096 + filetab
84,536 + pctab 3,396,048 + functab+_func 3,111,808 + go:func.* 1,716,784 +
findfunctab 72,178 (the last two live *inside* `.gopclntab` on Go 1.26; on 1.25
they are in `.rodata` at `moduledata.gofunc`/`findfunctab`). Sum of function
sizes = 14,781,825 B = 100.00 % of `.text` (16-byte gap after
`go:textfipsend`). 32,824 entries on a 32-byte boundary, 14 not (all cgo C
functions at the start of `.text`). x86asm decode failures: 1,869 bytes in 85
functions, all hand-written assembly with instruction forms x/arch does not
know (`chacha20poly1305.chacha20Poly1305Seal`=247, `Open`=181,
`bigmod.addMulVVW2048`=154, `sha512.blockAVX2`=114, `sha256.blockAVX2`=95,
`runtime.asyncPreempt`=36). 34 names occur twice (ABI0/ABIInternal pairs such as
`runtime.newstack`, `syscall.Syscall6`).

Name-stability categories (per binary, `nameCategories` in `match.go`):

| binary | funcs | closure `.funcN` | `.deferwrapN` | `.gowrapN` | generic `[go.shape…]` | `-fm` | `type:.eq/.hash` | linker/cgo/rt0 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| testsrv v1-F2 | 32,838 | 2,335 | 2,254 | 66 | 650 | 257 | 1,218 | 516 |
| kube-apiserver 1.36.3 | 105,507 | 10,267 | 1,845 | 222 | 1,665 | 569 | 2,710 | 941 |
| prometheus 3.13.1 | 113,780 | 6,388 | 2,257 | 173 | 3,768 | 467 | 4,689 | 915 |
| terraform 1.15.8 | 143,531 | 7,666 | 3,713 | 236 | 2,256 | 719 | 4,933 | 492 |
| cockroach 26.2.4 | 254,756 | 24,358 | 7,130 | 270 | 4,884 | 1,155 | 7,927 | 3,276 |
| vault 2.0.3 | 500,917 | 43,846 | 9,588 | 473 | 14,638 | 2,160 | 13,387 | 68 |

Table: `06-gt-inventory-v1-v2c.log`, `06-gt-corpus-*-inventory.log`.

Matching (`matchFuncs`): (1) exact name, duplicates paired in order; (2)
normalised name (`.funcN`/`.deferwrapN`/`.gowrapN` numbers collapsed to `#`);
(3) same size and content hash with every PC-relative field zeroed
(`contentHash`, so a function whose only difference is displacements hashes
equal). Match statistics per pair:

| pair | old → new funcs | exact | normalised | content | unmatched new | unmatched old | size changed | moved | byte-identical | content-changed |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| v1→v2s | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 0 | 0 | not logged | not logged |
| v1→v2l | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 0 | 0 | 26,706 | 1 |
| v1→v2c | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 1 (512→704) | 768 | 23,858 | 4 |
| v1→v2p | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 1 (512→640) | 768 | 31,493 | 3 |
| v1→v3 | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 1 (512→832) | 768 | 23,822 | 4 |
| v1→v4 | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 1 (512→832) | 768 | 23,822 | 4 |
| v3→v4 | 32,838 → 32,838 | 32,838 | 0 | 0 | 0 | 0 | 0 | 0 | 25,797 | 1 |
| kube-apiserver | 105,507 → 105,555 | 105,482 | 0 | 24 | 49 | 1 | 32 | 90,292 | 20,049 | 88 |
| prometheus 3.13.1→3.13.2 | 113,780 → 113,817 | 113,762 | 0 | 5 | 50 | 13 | 28 | 111,856 | 22,741 | 80 |
| prometheus 3.13.2→3.14.0 | 113,817 → 119,537 | 113,154 | 1 | 56 | 6,326 | 606 | 845 | 112,636 | 19,833 | 2,400 |
| terraform | 143,531 → 144,133 | 143,363 | 9 | 92 | 669 | 67 | 1,586 | 141,749 | 27,336 | 4,415 |
| cockroach | 254,756 → 254,817 | 254,603 | 125 | 23 | 66 | 5 | 364 | 218,362 | 54,539 | 685 |
| vault | 500,917 → 501,495 | 500,852 | 1 | 23 | 619 | 41 | 322 | 499,451 | 105,640 | 1,265 |

Table: `06-gt-inventory-*.log`, `06-gt-corpus-*-inventory.log`. "moved" = entry
address differs; "byte-identical" = same bytes at the new position (the rest
differ only in displacements unless "content-changed").

Observations: `v2p` (a new function in `internal/util`) shows 0 unmatched
because the prototype counts pclntab entries and the linker inlined
`util.Extra` (no new `_func`). Content matches are mostly renames
(`x/net/http2.ConfigureServer → configureServer`, grpc
`isTransportResponseFrame → isThrottled`); on prometheus 3.14.0 they include
spurious pairs of unrelated trivial functions with identical bodies
(`strfmt.ISODuration.MarshalBSON ← libopenapi orderedmap.MarshalYAML`), which is
harmless for prediction (identical bytes) but means the "content" bucket is not
a rename detector. Both the moved count (90–99 % of functions on every corpus
pair) and the content-changed count (88 of 105,507 for kube) quantify the
problem: nearly everything moves, almost nothing changes.

## 3. Where the baseline bytes live

Per-section deltas (each section as its own old/new pair) vs the whole file
(`sectdiff.go`; bsdiff / hdiffz / zstd -19 --long=27 --patch-from):

| pair | .text | .gopclntab | .rodata | .data | .noptrdata | typelink+itablink | SUM(sections) | WHOLE FILE |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| v1→v2s | 0 / 0 / 0 | 0 | 170 / 49 / 486 | 0 | 0 | 0 | 601 / 213 / 631 | 347 / 163 / 3,684 |
| v1→v2l | 22,152 / 30,880 / 49,003 | 0 | 1,229 / 1,214 / 3,255 | 807 / 796 / 1,919 | 0 | 0 | 24,819 / 33,116 / 54,379 | 24,322 / 32,899 / 55,648 |
| v1→v2c | 39,961 / 50,999 / 90,285 | 14,259 / 16,358 / 86,139 | 4,211 / 5,203 / 32,431 | 1,120 / 1,797 / 4,093 | 211 / 142 / 191 | 0 | 61,081 / 75,030 / 213,647 | 60,478 / 75,305 / 213,124 |
| v1→v2p | 9,327 / 11,183 / 19,173 | 4,541 / 4,876 / 16,329 | 2,096 / 2,931 / 4,846 | 215 / 127 / 112 | 212 / 143 / 184 | 0 | 17,703 / 19,797 / 41,150 | 16,141 / 19,230 / 41,040 |
| v1→v3 | 41,221 / 51,761 / 91,765 | 16,464 / 19,523 / 96,240 | 4,975 / 6,042 / 32,890 | 1,169 / 1,829 / 4,099 | 206 / 133 / 190 | 0 | 65,384 / 79,842 / 225,710 | 64,902 / 79,879 / 224,693 |
| v1→v4 | 43,203 / 53,799 / 94,709 | 16,152 / 19,174 / 95,915 | 4,887 / 5,650 / 33,024 | 1,235 / 1,895 / 4,157 | 206 / 133 / 190 | 0 | 67,041 / 81,205 / 228,521 | 66,541 / 80,796 / 228,024 |
| v3→v4 | 25,788 / 35,205 / 61,048 | 852 / 831 / 2,194 | 1,955 / 2,709 / 7,382 | 776 / 740 / 2,674 | 0 | 0 | 30,003 / 39,711 / 73,500 | 29,439 / 39,230 / 73,074 |

Table: `06-gt-sectdiff.log` (bsdiff / hdiffz / zstd, bytes). The remaining
~1.5 KB per pair is `.note.*` build IDs, `.plt`, `.got.plt`, `.dynamic`,
`.go.module`, `.go.fipsinfo` (each 150–240 B under bsdiff because of its fixed
header). Whole-file timings: bsdiff 5.8–7.0 s, hdiffz 0.1–1.9 s, zstd 11–16 s.

| pair (file size) | .text | .gopclntab | .rodata | .data | .noptrdata | typelink+itablink | SUM(sections) | WHOLE FILE (bsdiff s / hdiffz s) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| kube-apiserver (88,436,898) | 1,016,241 / 1,075,802 | 584,968 / 763,836 | 340,444 / 428,194 | 8,394 / 13,709 | 3,759 / 4,077 | 77,416 / 78,400 | 2,032,405 / 2,364,601 | 2,060,250 / 2,366,695 (64.0 / 25.3) |
| prometheus 3.13.1→3.13.2 (97,577,304) | 1,098,950 / 1,129,463 | 1,036,118 / 938,925 | 443,232 / 553,655 | 7,357 / 10,056 | 1,421 / 1,847 | 86,364 / 96,583 | 2,674,919 / 2,731,386 | 2,714,204 / 2,732,978 (66.4 / 20.4) |
| prometheus 3.13.2→3.14.0 (102,514,648) | 2,539,721 / 2,229,326 | 2,414,157 / 1,873,106 | 4,632,707 / 4,352,496 | 31,395 / 27,951 | 11,994 / 10,558 | 105,571 / 103,815 | 9,741,345 / 8,602,271 | 9,808,003 / 8,599,007 (113.2 / 41.2) |
| terraform (117,838,008) | 2,092,232 / 1,999,646 | 1,768,362 / 1,574,266 | 1,407,168 / 1,231,306 | 45,623 / 31,203 | 5,636 / 5,900 | 104,996 / 112,450 | 5,426,459 / 4,956,586 | 5,427,575 / 4,918,933 (112.9 / 32.4) |
| cockroach (230,309,064) | 2,849,784 / 2,963,663 | 1,220,585 / 1,749,621 | 1,247,881 / 1,473,398 | 44,012 / 74,773 | 2,997,947 / 2,981,548 | 181,281 / 174,279 | 8,550,602 / 9,428,872 | 8,588,309 / 9,455,199 (177.7 / 61.8) |
| vault (393,769,576) | killed / 3,887,504 | 4,889,537 / 4,124,406 | 2,674,593 / 3,192,050 | 108,233 / 83,087 | 6,603 / 6,443 | 524,460 / 471,763 | 8,205,778* / 7,879,372* | killed / 11,752,681 (244.4 / 112.3) |

Table: `06-gt-corpus-*-sectdiff.log` (bsdiff / hdiffz, bytes). \*vault's SUM
excludes `.text` (the row is dropped when bsdiff fails); adding hdiffz's .text
gives 11,766,876 ≈ WHOLE. cockroach also has `.dynsym` 2,530, `.eh_frame`
3,677, `.data.rel.ro` 390 (bsdiff).

Where the bytes are: on patch releases `.text` is 41–49 % of the baseline,
`.gopclntab` 14–52 %, `.rodata` 15–26 %; on the minor release (prometheus
3.14.0) `.rodata` is 48 % (4.6 MB, new type descriptors and strings). cockroach
is the outlier with 3.0 MB in `.noptrdata` (6.9 MB of its 8.1 MB differ at the
same offset; not investigated — the prediction did nothing for it). SUM/WHOLE
is 0.99–1.02 everywhere, so a section-wise transform loses no cross-section
matches.

## 4. .text prediction

### 4.1 Method (`predict.go`, `datamap.go`, `shifttab.go`)

The decoder rebuilds the new `.text` from the old one and the layout table:

1. **Layout table** (`encodeLayout`, `match.go`): for each new function, in
   address order, one op byte — `0` "same name as the next expected old
   function" (implicit), `1` + varint "explicit old index", `2` + length + name
   "new function" — followed by a zigzag varint of the size delta against the
   old function. New entry addresses follow from the 32-byte alignment and the
   sizes. Raw 65,677 B for 32,838 functions, zstd −19 → 30 B (v2c); kube
   214,004 B → 1,106 B.
2. **Relocation** (`relocate`): every matched function's old bytes are decoded
   with `x86asm.Decode` (64-bit). For each instruction with a PC-relative field
   (`inst.PCRel` = field width 1/2/4, `inst.PCRelOff` = its offset), the old
   target = oldPC + len + disp is mapped to a new address and the field
   re-encoded for the new PC; if the new displacement does not fit the field it
   is left (`no-fit` class, never observed). Decode failures skip one byte
   (1,788–16,000 bytes per corpus binary). Truncation/growth pads with `INT3`;
   unmatched (new) functions are left as `INT3`.
3. **Address map** (`mapper.mapAddr`): targets inside the *same* function move
   with it (`text-self`); targets in another matched function move with that
   function's new entry (`text-matched`); targets in unmatched old functions or
   outside the image are left unchanged. Targets in `.rodata`, `.noptrdata`,
   `.data` go through a per-section **content map** ("datamap"); `.bss` /
   `.noptrbss` through a **shift table**; anything else (`.plt`, `.got`) is
   section-relative identity.
4. **Content map** (`buildDataMapFuzzy`, block 16): every 16-byte-aligned block
   of the old section is looked up (at any alignment, hash index over every
   position of the new section, chain ≤ 256) and the candidate whose shift is
   closest to the previous block's shift wins; a shift change must be confirmed
   by the next block (otherwise duplicated content drags its neighbours);
   unmatched blocks inherit the previous shift; blocks with > 256 candidates
   are resolved in a backward pass (next block's shift, then 0). **Pointer
   masking**: before matching, every 8-byte-aligned qword whose value lies in
   the image's address range is zeroed on both sides (`maskPointers`), so
   absolute pointers (which change whenever their target moves) do not defeat
   matching. **Alignment constraint**: a block that held such a pointer may
   only take a shift that is a multiple of 8, because pointer-holding symbols
   are pointer-aligned and cannot move by a non-multiple. The effect of these
   two fixes is visible by comparing the earlier run (`06-gt-predict-v1-v2c.log`,
   block 32, no masking: `.rodata` 81,657 of 110,233 blocks matched, 27,106
   unmatched, P2 = 1,354 B with 351 mispredicted `.data` bytes) with the final
   one (220,173 of 220,466 matched, 293 unmatched, P2 = 659 B, 0 mispredicted).
5. **Shift table** (`deriveShiftTables`): `.bss`/`.noptrbss` have no content, so
   the *encoder* (which sees both binaries) walks unchanged-content matched
   functions, compares the targets of their 4-byte PC-relative references into
   those sections in old and new, and emits breakpoints `(offset, delta)` where
   the delta changes; transmitted as varints and added to the layout cost.
   Rationale: bss symbols are size-sorted, so one insertion shifts everything
   after it by one constant.

Three variants are measured: P0 = functions copied to their new positions with
no relocation; P1 = relocation with section-identity maps; P2 = relocation with
content maps (+ bss shift on the synthetic build).

### 4.2 Results

| pair | baseline bsdiff / hdiffz / zstd (rawdiff, runs) | P1 | P2 bsdiff / hdiffz / zstd (rawdiff, runs) | changed content | new fn | mispredicted refs | predict s |
|---|---:|---:|---:|---:|---:|---|---:|
| v1→v2l | 22,152 / 30,880 / 49,003 (20,315; 20,002) | = baseline | 179 / 65 / 1,636 (6; 5) | 3 | 0 | rodata 3 | 0.16 |
| v1→v2c | 39,961 / 50,999 / 90,285 (460,280; 46,603) | 33,633 / 44,323 / 75,320 | 659 / 481 / 1,999 (639; 29) | 639 | 0 | none | 0.17 |
| v1→v2p | 9,327 / 11,183 / 19,173 (427,932; 16,365) | 2,653 / 3,184 / 4,227 | 1,103 / 1,057 / 2,433 (670; 191) | 461 | 0 | rodata 209 | 0.19 |
| v1→v3 | 41,221 / 51,761 / 91,765 (462,455; 46,402) | 34,882 / 45,508 / 76,014 | 1,374 / 1,330 / 2,670 (988; 195) | 780 | 0 | rodata 208 | 0.18 |
| v1→v4 | 43,203 / 53,799 / 94,709 (463,023; 46,402) | 36,907 / 47,594 / 78,990 | 1,500 / 1,422 / 2,774 (1,047; 206) | 780 | 0 | rodata 267 | 0.47 |
| v3→v4 | 25,788 / 35,205 / 61,048 (25,288; 24,686) | = baseline | 475 / 487 / 1,957 (128; 79) | 3 | 0 | rodata 125 | 0.21 |

Table: `06-gt-predict.log` (P0 equals the baseline within 0.2 % everywhere:
moving functions without fixing displacements buys nothing). Reference census
for v1 (identical for P1/P2): text-self 320,896, text-matched 204,608, rodata
110,611, noptrdata 3,956, data 12,303, bss/noptrbss 47,994, plt 70,
other-section 40, outside-image 275; 3,664,769 instructions, 1,869 decode
failures. Decode throughput 17.2–27.5 MB/s single-thread; datamap build
0.30–0.65 s.

| pair | baseline bsdiff / hdiffz (rawdiff; runs) | P1 | P2 bsdiff / hdiffz (rawdiff; runs) | changed content | new fn | mispredicted: rodata / data / bss | decode MB/s (fails) | datamap s | predict s |
|---|---:|---:|---:|---:|---:|---|---:|---:|---:|
| kube-apiserver | 1,016,241 / 1,075,802 (34,764,961; 1,096,485) | 503,417 / 548,773 | 32,465 / 36,925 (54,851; 9,731) | 28,697 | 16,913 | 9,135 / 87 / 0 (+text 17) | 29.0 (1,788) | 1.22 | 0.48 |
| prometheus 3.13.1→3.13.2 | 1,098,950 / 1,129,463 (39,993,386; 1,179,851) | 548,180 / 572,486 | 149,309 / 177,788 (225,534; 150,097) | 55,865 | 12,194 | 6,683 / 30 / 150,760 | 27.8 (1,796) | 2.60 | 0.41 |
| prometheus 3.13.2→3.14.0 | 2,539,721 / 2,229,326 (41,580,817; 1,144,209) | 1,673,565 / 1,485,627 | 1,063,983 / 882,894 (3,026,687; 313,982) | 1,636,364 | 1,143,691 | 20,947 / 5,070 / 220,579 | 26.6 (1,795) | 3.09 | 0.38 |
| terraform | 2,092,232 / 1,999,646 (53,169,950; 1,742,200) | 1,280,026 / 1,308,346 | 577,078 / 579,605 (2,042,726; 290,988) | 1,519,602 | 232,688 | 7,214 / 5,840 / 277,319 | 28.6 (7,880) | 3.19 | 0.65 |
| cockroach | 2,849,784 / 2,963,663 (88,164,290; 2,997,745) | 1,279,546 / 1,345,246 | 217,482 / 240,638 (814,677; 147,560) | 613,943 | 34,209 | 3,364 / 0 / 88,016 (+75,125 outside functions) | 29.0 (2,347) | 6.40 | 1.58 |
| vault (hdiffz only) | – / 3,887,504 (150,221,698; 4,410,556) | – / 2,496,519 | – / 719,144 (1,550,958; 668,727) | 521,669 | 249,939 | 77,925 / 8,035 / 693,201 | 26.4 (16,000) | 13.1 | 1.50 |

Table: `06-gt-corpus-*-predict.log`. Reference census, kube: text-self 868,833,
text-matched 613,558, text-unmatched 3, rodata 410,551, noptrdata 1,341, data
56,088, bss/noptrbss 187,634, other-section 37, outside-image 275 (10,585,312
instructions). vault: text-self 3,298,435, text-matched 2,679,098, rodata
1,634,082, data 149,139, bss/noptrbss 613,336. Baseline bsdiff times: kube
20.1 s, prometheus 23.6–27.6 s, terraform 48.2 s, cockroach 65.3 s; hdiffz
7.0–30.9 s.

Reading the residual breakdown: on kube, 45,610 of the 54,851 residual bytes
are genuinely changed or new code and only 9,241 are mispredicted references —
the prediction is within 1.4x of the "delta of just the changed functions"
lower bound (22,532 B bsdiff). On prometheus 3.13.2, terraform, cockroach and
vault the single largest misprediction class is `.bss/.noptrbss` (150,760 /
277,319 / 88,016 / 693,201 bytes): the corpus binary (`gotransform-corpus`,
built 11:49) predates `shifttab.go` (11:57), so P2 there is "content-map" only
(the log line says so) and bss references were mapped section-identity. The
`-dump 10` output shows the fix is cheap: `internal/cpu.processOptions` MOVs
into `.bss+33560` land at `+16` in prometheus (histogram: `16:141557`),
`.bss+73688 → +16` in cockroach (`16:82331`), `+192` in vault, `+112` in
terraform, `+9088` in prometheus 3.14.0 — one or two breakpoints each. On the
synthetic pairs the derived tables have 0 breakpoints from 8,988 (`.bss`) and
39,004 (`.noptrbss`) samples, i.e. bss never moved there, so the mechanism is
implemented but unvalidated on non-trivial input.

`.rodata` mispredictions on the synthetic pairs (209–267 bytes) are all in
functions such as `runtime.printanycustomtype` (six LEAs to `.rodata+3186552`,
predicted +0/+8, actual +8/+192): blocks that are unmatched (`matched=false`)
because their content changed (inline-tree `nameOff`s) and inherit the wrong
neighbouring shift. kube's 9,135 come from a run of `internal/cpu.doinit`,
`runtime.chansend`… LEAs into `.rodata+11.78 M` predicted `+5440` (previous
block's shift) with actual `−16` relative to that.

The "changed functions" list contains a systematic artefact: `math.archExp`,
`crypto/internal/fips140/sha256.blockAVX2`, `sha512.blockAVX2`,
`chacha20poly1305.*`, `expandAVX512_*`, `internal/runtime/gc/scan.init` appear
as content-changed on every pair, with 2–6 residual bytes each. These are
exactly the functions in the decode-failure list: an undecodable instruction
(EVEX/AVX-512 forms unknown to x/arch v0.20.0) is hashed byte-by-byte, so a
PC-relative displacement inside it is neither masked by `contentHash` nor
relocated. Inference, not verified per instruction.

**Invertibility check** (relocating NEW → OLD and comparing with OLD): v2c: 9 of
32,838 matched functions differ, 4 with changed content; residual bsdiff 547 B
(forward: 659). kube: 2,868 differ, 88 changed; residual 19,826 / 29,432 B
(forward 32,465 / 36,925). The unchanged-content mismatches are `main.jsonHandler,
zipHandler, regexHandler, hashHandler, main.main` (v2c), `runtime.(*mheap).sysAlloc,
runtime.sysMapOS, runtime.markroot, …` (v2p/v3/v4), and on every corpus pair
`internal/cpu.Initialize, internal/cpu.processOptions, internal/cpu.doinit,
internal/cpu.Name, internal/runtime/exithook.Run, internal/bytealg.init.0,
cmpbody, countbody, memeqbody, …`. Explanation (inference from the dump lines,
which name the same functions with `.bss` targets): these functions reference
`internal/cpu.X86` feature flags and other `.bss` symbols — `cmpbody`,
`countbody`, `memeqbody` are assembly bodies that read the feature flags — and
the reverse shift table/content map does not cover the inserted/removed
symbol, so the reverse relocation mispredicts them; the synthetic `main.*`
cases are references to the new strings that have no counterpart in v1. The
check confirms the relocation itself is symmetric (no drift from
re-encoding), which is what a chained or bidirectional update needs.

## 5. pclntab prediction (`pclnpredict.go`)

The `.gopclntab` layout (Go 1.20+ format, `runtime/symtab.go` `pcHeader`): a
72-byte header (magic, minLC, ptrSize, nfunc, nfiles, five table offsets),
then `funcnametab` (NUL-terminated names), `cutab` (uint32 file indices per
compilation unit), `filetab` (file names), `pctab` (varint-encoded pc-value
tables shared by all functions), `functab` (nfunc+1 pairs of `entryOff,
funcOff` followed by the `_func` records, each 44 bytes + 4·npcdata + 4·nfuncdata,
8-byte aligned), and on Go 1.26 `go:func.*` (funcdata: stack maps, inline
trees, 16-aligned) and `findfunctab` (4-aligned). Everything is an *offset*
from `moduledata.text` or from a table start, which is why the section carries
no relocations and why a 32-byte function growth changes 858,149 runs of it
(v2c) yet all the information needed to rewrite those runs is in the layout
table.

What the prototype regenerates:

* `functab[i].entryOff` and `_func.entryOff` from the new layout;
  `functab[i].funcOff` from the rebuilt record stream (`rnd(size, 8)`); the
  sentinel `functab[nfunc] = end of last function`; `pcHeader.nfunc` and the
  five table offsets.
* `_func.nameOff`: either the old value (V1), the offset in a rebuilt
  `funcnametab` (V2), or a lookup of the function's name in the *new*
  `funcnametab` (V4).
* `_func.pcsp/pcfile/pcln` (record offsets 16/20/24) and the `pcdata[]` array
  (offset 44+): re-based through a fuzzy content map (block 16, ≤ 4 differing
  bytes) old pctab → new pctab (V3/V4). `funcdata[]` (after pcdata): re-based
  through the same kind of map for `go:func.*`. `cuOffset` (record offset 32)
  through a map of `cutab` (V4).
* Records for new functions: zero-filled with the modal (npcdata, nfuncdata)
  of the old binary, funcdata = `^0`, cuOffset = previous record's.
* `findfunctab` from scratch (`genFindfunctab`: 4,096-byte buckets, 16
  256-byte sub-buckets with a uint32 base and byte deltas, ported from
  `cmd/link/internal/ld/pcln.go findfunctab`); byte-identical to the linker's
  for all 18 binaries (e.g. kube 206,576 B, vault 777,094 B).
* `funcnametab` rebuild (V2, `nameSegments`): models the linker's `walkFuncs`
  order — a function's name is emitted when first seen, followed by its
  not-yet-seen inlined callees — as a sequence of segments per "starter"
  function, moved as blocks into the new order. The earlier build had this
  wrong (`06-gt-pclnpredict-v1-v2c-b.log`: self-rebuild differed in 1,950,837
  bytes); the final build self-predicts every Go 1.26 binary with 0 differing
  bytes under all four variants.

The four variants, cumulative: **V1** relayout only (entryOff/funcOff
regenerated, old `nameOff`, old pctab/funcdata offsets, new names appended);
**V2** = V1 + funcnametab rebuilt in layout order; **V3** = V2 + the *new*
`pctab` and `go:func.*` are transmitted first (their deltas are counted in the
total) and old offsets are re-based through content maps; **V4** = new
funcnametab/cutab/filetab/pctab/go:func.* all transmitted first (stage 1),
`nameOff` by name lookup, everything else re-based.

| pair | baseline | V1 | V2 | V3 residual (pred len ok?) | V4 residual (pred len ok?) | stage-1 blob deltas | **V4 total** |
|---|---:|---:|---:|---:|---:|---:|---:|
| v1→v2l | 143 / 39 / 1,289 | = | = | = | = (yes) | 0 | 143 / 39 / 1,289 |
| v1→v2c | 14,259 / 16,358 / 86,139 | 13,271 / 15,236 / 82,788 | = V1 | 206 / 74 / 1,327 (yes) | 206 / 74 / 1,327 (yes) | pctab 336 + go:func 1,437 | 1,979 / 1,803 / 6,188 |
| v1→v2p | 4,541 / 4,876 / 16,329 | 3,482 / 3,855 / 13,076 | = V1 | 606 / 549 / 2,784 (−32) | 199 / 78 / 1,318 (yes) | fnt 206 + pctab 372 + go:func 1,679 | 2,456 / 2,023 / 6,091 |
| v1→v3 | 16,464 / 19,523 / 96,240 | 15,176 / 17,851 / 89,780 | = V1 | 599 / 546 / 2,781 (−32) | 203 / 73 / 1,314 (yes) | 206 + 422 + 2,633 | 3,464 / 3,188 / 9,127 |
| v1→v4 | 16,152 / 19,174 / 95,915 | 14,881 / 17,485 / 89,725 | = V1 | 599 / 546 / 2,781 (−32) | 203 / 73 / 1,314 (yes) | 206 + 422 + 2,336 | 3,167 / 2,865 / 9,137 |
| v3→v4 | 852 / 831 / 2,194 | = | = | 147 / 39 / 1,289 (yes) | 147 / 39 / 1,289 (yes) | go:func 853 | 1,000 / 870 / 2,387 |

Table: `06-gt-pclnpredict.log` (bsdiff / hdiffz / zstd, bytes). V1 on its own
barely helps (7 %): re-basing `entryOff` fixes the functab pairs but every
`_func` still holds stale pctab/funcdata offsets, and those are the bulk of the
858 K runs. V3/V4 residuals are 12–15 differing bytes (in `functab+_func`) out
of 10.54 MB; the 143–206 B is bsdiff's fixed cost. The v2c V4 total of 1,979 B
is 7.2x below baseline, and the 1,437 B `go:func.*` delta (48,778 differing
bytes in 17,232 runs: funcdata offsets that themselves point into the shifted
inline trees) is now the largest piece — the next thing to regenerate.

| pair | baseline bsdiff / hdiffz | V1 | V2 | V3 residual | V4 residual (pred len − new) | stage-1 blob deltas (fnt+cutab+filetab+pctab+go:func) | **V4 total** (factor) |
|---|---:|---:|---:|---:|---:|---:|---:|
| kube-apiserver | 584,968 / 763,836 | 291,426 / 331,563 | 302,594 / 347,081 | 51,156 / 56,272 | 15,508 / 16,041 (−64) | 1,715+13,915+561+18,627+32,008 = 66,826 | 82,334 / 84,716 (7.1x / 9.0x) |
| prometheus 3.13.1→3.13.2 | 1,036,118 / 938,925 | 407,213 / 458,964 | 403,207 / 451,874 | 53,504 / 67,043 | 11,643 / 14,982 (+128) | 1,398+16,161+523+13,986+58,063 = 90,131 | 101,774 / 108,661 (10.2x / 8.6x) |
| prometheus 3.13.2→3.14.0 | 2,414,157 / 1,873,106 | 1,386,963 / 1,219,249 | 1,321,640 / 1,197,006 | 283,096 / 268,139 | 193,717 / 174,524 (+4,384) | 36,404+34,017+9,665+291,378+225,707 = 597,171 | 790,888 / 702,530 (3.1x / 2.7x) |
| vault (hdiffz) | – / 4,124,406 | – / 2,207,116 | – / 2,163,436 | – / 517,972 | – / 294,131 (+688) | 8,356+91,681+3,317+114,482+163,844 = 381,680 | – / 675,811 (6.1x) |
| terraform (Go 1.25, invalid) | 1,768,362 / 1,574,266 | 927,603 / 920,123 | 911,304 / 902,980 | 233,983 / 639,297 | 155,994 / 548,637 (+1,348,108) | 618,649 | 774,643 / 996,321 |
| cockroach (Go 1.25, invalid) | 1,220,585 / 1,749,621 | 2,242,812 / 1,786,800 | 2,211,300 / 1,751,786 | 1,480,940 / 946,275 | 1,454,413 / 904,525 (+3,995,908) | 294,694 | 1,749,107 / 1,257,995 |

Table: `06-gt-corpus-*-pclnpredict.log`. On the Go 1.26 corpus pairs V4's
residual is 3–10 % of the total; the transmitted blobs dominate, and inside
them `go:func.*` (32–226 KB) and `pctab` (14–291 KB) are the big ones —
`funcnametab` costs 1.4–1.7 KB on a patch release even though 7.5–9.4 MB of it
differ at the same offset. None of the corpus V4 predictions has the exact new
length (−64 to +4,384 B); the likely cause is the modal-guess record size for
the 49–6,326 new functions (inference), and the consequence is that
`functab+_func` differs in 4.9 M (kube) to 32.5 M (vault) bytes at equal
offsets — cheap for bsdiff (15.5 KB) but fatal for a positional correction.
The Go 1.25 rows are reported for completeness only: `self-prediction self:
len 75621372 vs 71625832, differing bytes=68624203` (cockroach) and `len
37320340 vs 35983552` (terraform) mean the record parsing/regeneration does not
reproduce the 1.25 `functab` (the prototype notes that 1.25's
`moduledata.cutab` length is in bytes rather than elements, so more layout
details probably differ); `findfunctab` regeneration is nevertheless identical
on 1.25.

## 6. Data sections (`datapredict.go`)

For `.rodata`, `.noptrdata`, `.data`: old blocks are re-laid through the content
map, then every 8-byte-aligned qword pointing into the image is rewritten
through the same `mapAddr` used for code (function map for code pointers,
content maps for data pointers, shift table for bss).

| pair | section (size) | qwords into image | re-targeted ok / mispredicted / unchanged | baseline bsdiff / hdiffz | re-laid only | re-laid + pointer rewrite (rawdiff; runs) |
|---|---|---:|---|---:|---:|---:|
| v1→v2c | .rodata (3,527,449) | 110,307 (text 15,931, rodata 94,315) | 29,110 / 1 / 81,196 | 4,211 / 5,203 | 4,188 / 5,206 | 481 / 634 (1,052; 585) |
| v1→v2c | .data (178,258) | 8,526 (rodata 5,215, bss 1,788) | 2,899 / 0 / 5,627 | 1,120 / 1,797 | 1,120 / 1,797 | 142 / 35 (0; 0) |
| v1→v2c | .noptrdata (454,273) | 658 | 32 / 0 / 626 | 211 / 142 | 211 / 142 | 142 / 35 (0; 0) |
| v1→v2l | .rodata | 110,307 | 1,885 / 0 / 108,422 | 1,229 / 1,214 | 1,266 / 1,239 | 243 / 86 (41; 5) |
| v3→v4 | .rodata | 110,307 | 4,712 / 26 / 105,586 | 1,955 / 2,709 | 1,963 / 2,850 | 316 / 218 (50; 29) |
| kube-apiserver | .rodata (12,649,465) | 317,988 (text 38,780, rodata 279,027) | 274,623 / 32,877 / 10,912 | 340,444 / 428,194 | 403,420 / 455,958 | 236,250 / 266,207 (873,817; 464,969) |
| kube-apiserver | .noptrdata (579,329) | 2,454 | 1,265 / 963 / 365 | 3,759 / 4,077 | 3,736 / 15,184 | 1,757 / 12,395 (28,721; 2,768) |
| kube-apiserver | .data (287,730) | 14,868 (rodata 8,988, bss 3,564) | 13,720 / 1,141 / 91 | 8,394 / 13,709 | 8,104 / 14,162 | 1,108 / 1,599 (1,442; 1,175) |
| prometheus 3.13.1→3.13.2 | .rodata (19,352,697) | 425,849 | 412,374 / 12,183 / 1,719 | 443,232 / 553,655 | 488,492 / 572,043 | 256,723 / 329,395 (945,175; 438,366) |
| prometheus 3.13.1→3.13.2 | .data (345,650) | 17,026 | 12,861 / 4,164 / 92 | 7,357 / 10,056 | 7,465 / 9,987 | 1,251 / 1,863 (4,374; 4,167) |
| prometheus 3.13.2→3.14.0 | .rodata (19,354,777) | 425,917 | 359,926 / 65,429 / 758 | 4,632,707 / 4,352,496 | 4,571,059 / 4,323,249 | 4,124,373 / 3,936,045 (5,350,617; 681,611) |
| terraform | .rodata (22,668,857) | 456,166 | 423,995 / 30,268 / 6,043 | 1,407,168 / 1,231,306 | 1,513,158 / 1,334,180 | 1,172,460 / 975,920 (2,586,632; 940,092) |
| terraform | .noptrdata (1,743,361) | 2,396 | 1,191 / 1,192 / 233 | 5,636 / 5,900 | 26,639 / 25,325 | 23,492 / 22,122 (47,045; 2,725) |
| terraform | .data (616,618) | 29,203 | 18,252 / 10,951 / 107 | 45,623 / 31,203 | 39,699 / 31,669 | 14,252 / 14,203 (32,573; 15,463) |
| cockroach | .rodata (41,409,512) | 816,743 | 761,517 / 31,858 / 30,015 | 1,247,881 / 1,473,398 | 1,344,302 / 1,556,817 | killed / 1,028,031 (2,824,008; 1,250,336) |
| cockroach | .noptrdata (8,128,097) | 6,842 | 2,822 / 3,002 / 3,926 | 2,997,947 / 2,981,548 | 3,000,713 / 2,983,357 | 3,003,549 / 2,984,753 (2,971,274; 16,651) |
| cockroach | .data (2,433,736) | 122,749 | 121,173 / 1,551 / 98 | 44,012 / 74,773 | 39,892 / 55,365 | 1,449 / 1,616 (2,661; 1,567) |
| vault (hdiffz) | .rodata (94,880,697) | 2,031,941 | 1,674,611 / 354,929 / 5,134 | – / 3,192,050 | – / 3,022,647 | – / 1,982,236 (5,869,122; 2,495,529) |
| vault (hdiffz) | .data (1,754,978) | 99,028 | 58,042 / 40,985 / 116 | – / 83,087 | – / 69,629 | – / 6,859 (47,856; 41,658) |

Table: `06-gt-datapredict.log`, `06-gt-corpus-*-datapredict.log` (bsdiff / hdiffz,
bytes; "unchanged" = the qword has the same value in new, "mispredicted" = the
rewritten value differs from new).

`.data` (mostly pointer tables: 5,215 of 8,526 image pointers in the synthetic
binary point into `.rodata`, 1,788 into bss) is solved: 0 differing bytes on
the synthetic pairs, 8x on kube. `.rodata` is not: the block re-lay alone makes
things slightly *worse* than the baseline (kube 340 KB → 403 KB) because 24 %
of the blocks (192,205 of 790,592) are unmatched — new/changed type
descriptors, strings, inline metadata — and inherit shifts that put wrong
content at the right place; the pointer rewrite then recovers 274,623 of
317,988 pointers and lands at 1.4x. What the prototype does not touch in
`.rodata` are the *32-bit offsets* embedded in type descriptors (`nameOff`,
`typeOff`, method tables, relative to `runtime.types`), which behave exactly
like the `.typelink` entries and shift whenever `.rodata` is re-laid; on the
whole-file run these leave `.typelink` with 66,546 of 119,464 bytes wrong on
kube. On the minor release (prometheus 3.14.0: 403,503 unmatched blocks,
65,429 mispredicted pointers) the section is simply new content, 4.1 MB either
way. terraform `.noptrdata` regresses (5,636 → 23,492 B): the map's 15,250
unmatched blocks (of 108,961) scatter content that was better left in place —
a fallback "use the identity map when the section-level residual is worse" is
needed.

## 7. Whole-file pipeline (`whole.go`)

Decoder-side order, and what is transmitted:

1. **Layout table** (function names/sizes/order as in §4.1, plus the bss shift
   tables), zstd-compressed.
2. **Stage 1**: one generic delta from the concatenation of the *old* pclntab
   blob tables (funcnametab, cutab, filetab, pctab, and go:func.* on 1.26) to
   the new ones — 7,357,984 B (synthetic) / 22,152,664 B (kube) of blobs. The
   decoder needs these before it can re-base the `_func` offsets, and content
   maps over them (old blob → new blob) are derivable from the delta's copy
   stream.
3. **Prediction of the whole new file**: start from a copy of the old file
   (headers, gaps); for each allocated section of the new binary write
   `predictText` (`.text`), `predictPcln` V4 (`.gopclntab`),
   `predictDataSection` (`.rodata`, `.noptrdata`, `.data`), `.typelink` int32
   offsets re-mapped through the `.rodata` content map, `.itablink` absolute
   pointers through `mapAddr`, and the old bytes for everything else
   (`.plt`, `.got.plt`, `.dynamic`, notes, …).
4. **Stage 2**: one generic delta from the predicted file to the new file.
   Total = layout + stage 1 + stage 2.

| pair | baseline bsdiff / hdiffz / zstd | layout | stage 1 | stage 2 (rawdiff; runs) | **total** (factor) |
|---|---:|---:|---:|---:|---:|
| v1→v2s | 347 / 163 / 3,684 | 23 | 147 / 39 / 980 | 347 / 163 / 3,684 (103; 8) | 517 / 225 / 4,687 (0.7x / 0.7x / 0.8x) |
| v1→v2l | 24,322 / 32,899 / 55,648 | 23 | 147 / 39 / 980 | 472 / 271 / 3,780 (179; 16) | 642 / 333 / 4,783 (37.9x / 98.8x / 11.6x) |
| v1→v2c | 60,478 / 75,305 / 213,124 | 30 | 1,625 / 1,678 / 5,364 | 1,589 / 1,518 / 5,827 (2,127; 787) | 3,244 / 3,226 / 11,221 (18.6x / 23.3x / 19.0x) |
| v1→v2p | 16,141 / 19,230 / 41,040 | 30 | 1,964 / 1,862 / 4,807 | 2,131 / 2,155 / 6,216 (2,025; 979) | 4,125 / 4,047 / 11,053 (3.9x / 4.8x / 3.7x) |
| v1→v3 | 64,902 / 79,879 / 224,693 | 30 | 2,949 / 3,030 / 8,042 | 2,406 / 2,503 / 6,691 (2,674; 981) | 5,385 / 5,563 / 14,763 (12.1x / 14.4x / 15.2x) |
| v1→v4 | 66,541 / 80,796 / 228,024 | 30 | 2,663 / 2,704 / 7,993 | 2,554 / 2,733 / 6,931 (2,773; 1,013) | 5,247 / 5,467 / 14,954 (12.7x / 14.8x / 15.2x) |
| v3→v4 | 29,439 / 39,230 / 73,074 | 23 | 855 / 841 / 1,885 | 878 / 793 / 4,214 (312; 117) | 1,756 / 1,657 / 6,122 (16.8x / 23.7x / 11.9x) |
| kube-apiserver | 2,060,250 / 2,366,695 / – | 1,106 | 66,126 / 68,633 / – | 291,375 / 340,850 / – (7,709,875; 2,594,655) | 358,607 / 410,589 (5.7x / 5.8x) |

Table: `06-gt-whole.log`, `06-gt-corpus-kube-apiserver-1.36.3-1.36.4-whole.log`.
Stage-1 rawdiff for v2c is 989,142 (the blobs slide) but its delta is 1.6 KB;
for v2l/v2s the blobs are identical and stage 1 is pure overhead (147 B).

Stage-2 residual by section (differing bytes at the same offset, runs):

* v1→v2c: `.rodata` 1,052 (585), `.text` 639 (29), `.plt` 81 (46),
  `.note.go.buildid` 80, `.got.plt` 76 (44), `.go.fipsinfo` 35, `.note.gnu.build-id`
  20, `.go.module` 17 (13), `.gopclntab` 13 (8), `.dynamic` 12 (7).
* v1→v2l: build IDs 100, `.rodata` 41 (5), `.go.fipsinfo` 32, `.text` 6 (5).
* v1→v4: `.rodata` 1,256 (627), `.text` 1,047 (206), `.plt` 92, `.got.plt` 88,
  build IDs 97, `.go.fipsinfo` 35, `.go.module` 28, `.dynamic` 14, `.gopclntab`
  12, `.data` 5.
* kube-apiserver: `.gopclntab` 6,675,145 (2,085,322), `.rodata` 873,817
  (464,969), `.typelink` 66,546 (26,836), `.text` 54,851 (9,731), `.noptrdata`
  28,721 (2,768), `.itablink` 6,687 (3,120), `.data` 1,442 (1,175),
  `.go.module` 105, build IDs 98, `.go.buildinfo` 58, `.go.fipsinfo` 54.

So after the transform the synthetic residual is the changed code, the build
IDs, and the PLT/GOT (whose entries shift with `.text` and are not relocated —
81 + 76 B, trivially fixable), plus ~1 KB of `.rodata` unmatched blocks. On
kube the residual bytes are in `.gopclntab` (a 64-byte length slip, see §5,
~15 KB of patch) and `.rodata` (~236 KB of the 291 KB stage 2, see §6).

### 7.5 Second corpus pass: whole-file results on every pair

A later pass of the prototype (`bench/out/logs/06-gt-corpus2-*-whole.log`,
run log `06-gt-corpus3-run.log`) added the `.bss/.noptrbss` shift tables to
the whole-file pipeline and a Go 1.25 pclntab layout path (the `entryOff`
difference that broke terraform and cockroach in §5), then ran `whole` on
every corpus pair. The vault run did not complete (the log ends after the
shift tables). Totals = layout + bss shift tables + stage 1 + stage 2:

| pair (layout) | baseline bsdiff | Go-aware bsdiff | factor | hdiffz baseline → Go-aware | layout + shifts | stage 1 | stage 2 |
|---|---:|---:|---:|---:|---:|---:|---:|
| kube-apiserver 1.36.3→1.36.4 (go1.26) | 2,060,250 | 358,607 | 5.7x | 2,366,695 → 410,589 (5.8x) | 1,106 + 0 | 66,126 | 291,375 |
| prometheus 3.13.1→3.13.2 (go1.26) | 2,714,204 | 393,964 | 6.9x | 2,732,978 → 486,853 (5.6x) | 1,097 + 40 | 89,649 | 303,178 |
| terraform 1.15.8→1.15.9 (go1.25) | 5,427,575 | 1,990,549 | 2.7x | 4,918,933 → 2,165,164 (2.3x) | 10,598 + 191 | 243,020 | 1,736,740 |
| cockroach 26.2.4→26.2.5 (go1.25) | 8,588,309 | 4,190,756 | 2.0x | 9,455,199 → 4,544,272 (2.1x) | 2,702 + 23 | 65,371 | 4,122,660 |
| prometheus 3.13.2→3.14.0 minor (go1.26) | (bsdiff not run) | 5,899,984 | — | 8,599,007 → 5,416,050 (1.6x) | 29,538 + 130 | 607,472 (hdiffz 532,047) | 5,262,844 (hdiffz 4,854,335) |
| vault 2.0.3→2.0.4 (go1.26) | 12,108,972 | not measured | — | — | 9,028 + 136 | not measured | not measured |

Shift tables are tiny (3–112 breakpoints, 23–191 B zstd) and mostly one
constant step (+8/+16), as predicted from the mispredict dumps in §4. Stage-2
differing bytes by section (raw, before the delta) show where the remaining
patch bytes come from — no longer `.text`:

| pair | .text | .rodata | .gopclntab | .typelink | other |
|---|---:|---:|---:|---:|---|
| kube-apiserver | 54,851 (9,731 runs) | 873,817 (464,969 runs) | 6,675,145 (2.09 M runs) | 66,546 | .itablink 6,687; .noptrdata 28,721 |
| prometheus 3.13.2 | 74,305 (7,508) | 945,175 (438,366) | 7,914,534 (2.00 M) | 77,076 | .itablink 8,505 |
| terraform | 1,760,301 (91,082) | 2,586,633 (940,092) | 8,100,407 (2.00 M) | 110,106 | .noptrdata 47,043 |
| cockroach | 726,256 (64,320) | 2,824,008 (1.25 M) | 4,181,274 (952 K) | 132,309 | .dynsym 13,005 (cgo) |
| prometheus 3.14.0 | 2,792,483 (162,871) | 5,350,617 (681,611) | 10,288,434 (2.70 M) | 118,844 | .go.buildinfo 15,852 |

Reading: on the two Go 1.26 patch releases the pipeline is at 5.7–6.9x and
`.text` contributes < 75 KB; `.rodata` (type descriptors and other
offset-bearing structures that the content map cannot re-target) and the pc
tables inside `.gopclntab` are the remaining residual. Terraform's 1.76 MB of
`.text` residual is genuine change (a vendored-dependency update, see
`benchmark-scale.md` §1) rather than misprediction. The Go 1.25 pairs reach
only 2.0–2.7x: their pclntab regeneration is newer and less complete, which
is consistent with per-version layout handling being a first-class
requirement rather than a corner case.

## 8. Projection and recommendation

Expected patch sizes with the transform (bsdiff unless stated; full download
of the synthetic binary is 8,432,157 B at zstd −19, `06-model.log`):

| scenario | baseline | with transform | basis |
|---|---:|---:|---|
| (i) one-line code change (v1→v2c) | 60,478 | 3,244 (18.6x) | measured, `06-gt-whole.log` |
| (i') string/constant edit that slides `.rodata` (v1→v2l) | 24,322 | 642 (37.9x) | measured |
| (ii) synthetic multi-package (v1→v4) | 66,541 | 5,247 (12.7x) | measured |
| (iii) real patch release, kube-apiserver 1.36.3→1.36.4 | 2,060,250 | 358,607 (5.7x) | measured, `…-whole.log` |
| (iii) prometheus 3.13.1→3.13.2 | 2,714,204 | ≈ 510–600 K (4.5–5.3x) | **estimate**: .text 149,309 + pclntab 101,774 + .rodata 256,723 + .data 1,251 + .noptrdata 812 + layout 1,097 (+ typelink/itablink ≤ 86,364 unpredicted) |
| (iii) vault 2.0.3→2.0.4 (hdiffz) | 11,752,681 | ≈ 3.4–3.9 M (3.0–3.5x) | **estimate**: 719,144 + 675,811 + 1,982,236 + 6,859 + 3,883 + 9,028 (+ ≤ 471,763) |
| (iii) terraform 1.15.8→1.15.9 | 5,427,575 | ≈ 2.6–3.6 M (1.5–2.1x) | **estimate**: .text 577,078 + pclntab 774,643 (Go 1.25 path invalid; baseline 1,768,362 as upper bound) + .rodata 1,172,460 + .data 14,252 + .noptrdata 5,636 (identity fallback) + layout 10,598 (+ ≤ 104,996) |
| (iii) cockroach 26.2.4→26.2.5 | 8,588,309 | ≈ 5.3–5.5 M (1.6x) | **estimate**: .text 217,482 + pclntab 1,220,585 (unhandled) + .rodata ≈ 0.87 M (hdiffz ratio 0.70 applied) + .noptrdata 2,997,947 (no gain) + .data 1,449 + layout 2,702 (+ ≤ 181,281) |
| (iv) minor release, prometheus 3.13.2→3.14.0 | 9,808,003 | ≈ 6.0–6.1 M (1.6x) | **estimate**: 1,063,983 + 790,888 + 4,124,373 + 6,634 + 9,662 + 29,538 (+ ≤ 105,571); content-dominated: 1.64 MB changed + 1.14 MB new function bytes, 4.1 MB of new `.rodata` |

The estimates add per-stage results that were measured independently; the
kube case (where both are available) shows the sum (≈ 355 K) agrees with the
whole-file run (358,607) to 1 %, and the whole-file stage-2 section residuals
match the per-stage rawdiffs exactly (`.rodata` 873,817; `.gopclntab`
6,675,153 vs 6,675,145), so the addition is legitimate, but the bss fix and
Go 1.25 pclntab would move terraform/cockroach/vault noticeably.

**Encoder/decoder cost.** Both sides run the same prediction: pclntab parse +
matching (not timed separately; the whole `inventory` command including
zstd of the layout ran inside the 3-minute kube job), x86asm decode of the old
`.text` at 17–29 MB/s single-thread (0.54–0.86 s for 14.8 MB, 1.5 s kube, 6.0 s
vault), relocation 0.17 s (synthetic) to 1.6 s (cockroach) with 24 goroutines,
content maps 0.30 s (synthetic) to 13.1 s (vault). The encoder then runs the
generic delta on the residual — the dominant cost today (bsdiff 2.8 s on the
14.8 MB `.text`, 20 s kube, 65 s cockroach; hdiffz 0.5 / 7.0 / 20.6 s) — but
where the prediction is layout-exact (the `.text` stage always; the whole file
on 7/7 synthetic pairs, 0/4 corpus pairs because of the pclntab length slip)
stage 2 could be a positional patch list: v2c needs 787 runs / 2,127 bytes,
v2l 16 / 179, v3→v4 117 / 312 — no suffix array, O(n) compare, and the decoder
never needs the generic delta's memory. Memory of the prototype: not measured;
the content-map index is a Go map over every byte position of the new section
(95 M entries for vault's `.rodata`), which is the obvious thing to replace
(index block-aligned positions, or reuse the delta's copy stream as the map).

**Safety argument.** Prediction is a compression context, not a correctness
mechanism: whatever the predictor gets wrong, stage 2 corrects, so a bad
prediction costs bytes (P0 ≈ baseline everywhere), never a wrong output. The
one real hazard is encoder/decoder divergence — different prototype versions,
x/arch versions, or float/hash-order nondeterminism would make the decoder
apply the patch to a different base. Mitigation: the patch must carry a hash
of the predicted base (and of the stage-1 output) and the predictor must be
versioned; the invertibility results (§4.2) show the relocation is
deterministic and symmetric, which is what makes that check cheap.

**Portability and fragility.**

* arm64: the prototype is x86-only (`x86asm`, `PCRel`/`PCRelOff`). arm64 needs
  `ADRP` (21-bit page-relative immediate, split immhi/immlo) paired with the
  following `ADD`/`LDR` imm12 (Go emits the pair for every `R_ADDRARM64`), and
  `B`/`BL` imm26 (±128 MB) for calls; the map logic is unchanged, only the
  field decode/re-encode differs, and `golang.org/x/arch/arm64/arm64asm` exists.
  Not measured.
* Go-version dependence: the prototype hard-codes the Go 1.20+ pclntab magic
  (`internal/abi/symtab.go` `Go120PCLnTabMagic = 0xfffffff1`), the `pcHeader`
  field offsets (`runtime/symtab.go`: nfunc @8, nfiles @16, the five table
  offsets @32..64), the `_func` record (`runtime/runtime2.go`: entryOff @0,
  nameOff @4, pcsp/pcfile/pcln @16/20/24, npcdata @28, cuOffset @32, nfuncdata
  @43, then `pcdata[npcdata]`, `funcdata[nfuncdata]` uint32s), the `moduledata`
  field order (`runtime/symtab.go`: pcHeader, funcnametab, cutab, filetab,
  pctab, pclntable, ftab, findfunctab, minpc/maxpc, text/etext, …, types/etypes,
  rodata, gofunc, epclntab (1.26), textsectmap, typelinks, itablinks), the
  `findfunctab` bucket constants (`internal/abi` `FuncTabBucketSize` = 4096,
  16 sub-buckets; `cmd/link/internal/ld/pcln.go findfunctab`), the funcnametab
  emission order (`pcln.go walkFuncs`), the table alignments, `funcAlign = 32`
  (`cmd/link/internal/amd64/l.go`) and the `.go.module` section (1.26; ≤ 1.25
  keeps `moduledata` in `.noptrdata`). The Go 1.25 pclntab regeneration already
  fails; every Go release must be regression-tested against the self-prediction
  check (old → old must be byte-exact), and the transform should fall back to
  the plain delta when it fails.

**What the prototype did not do**, with the residual it accounts for on
kube-apiserver:

* Type-descriptor internals: 32-bit `nameOff`/`typeOff`/method offsets inside
  `.rodata` and the sorted `.typelink` list are not regenerated (only 8-byte
  absolute pointers are) — `.rodata` 873,817 residual bytes (≈ 236 KB of
  patch), `.typelink` 66,546, `.itablink` 6,687.
* New-function records in pclntab use a modal (npcdata, nfuncdata) guess, so
  the corpus predictions are 64–4,384 B off in length — 6.68 M differing
  `.gopclntab` bytes, ≈ 15.5 KB of patch, and no positional correction.
* `go:func.*`/`pctab` are transmitted, not regenerated: 32,008 + 18,627 B on
  kube, 1,437 + 336 B on v2c.
* bss shift tables never ran on the corpus (150,760 / 277,319 / 693,201 /
  88,016 mispredicted `.text` bytes on prometheus / terraform / vault /
  cockroach; 0 on kube) and were never non-trivial on the synthetic pairs.
* New functions are left as `INT3` (16,913 B kube; 1,143,691 B prometheus
  3.14.0) — the generic delta must find their content itself.
* `.plt`/`.got.plt` are not relocated (81 + 76 B on v2c).
* Non-Go code (cockroach: 2,839,766 B of C/C++ after the last Go function,
  75,125 residual bytes) and cockroach's 3.0 MB `.noptrdata` change are
  untouched.
* The data-section content maps and the new section table are oracle inputs
  (built from the actual new sections inside the encoder-side prototype, cost
  0 B); a transmitted piecewise map costs ≈ shift changes × a few bytes (kube:
  1,507 + 13 + 13 changes ≈ 10 KB; v2c: 3).
* End-to-end apply was not exercised (`verifyBspatch` exists in `measure.go`
  but `whole` does not call it); the patch sizes are those of real
  bsdiff/hdiffz/zstd runs over the predicted file.

**Recommendation.** Build the `.text` stage first (layout table + relocation
+ transmitted `.rodata` shift map + bss shift table): it is the simplest, has
the best ratio (31x on kube, 60x on v2c), and its prediction is layout-exact,
so its correction can be positional. Add pclntab V4 second (7–10x on Go 1.26
patch releases) after fixing the new-function record sizes so the prediction
is length-exact. Leave `.rodata` to the generic delta until type-descriptor
offsets are regenerated; on minor releases nothing here beats content growth.
Gate everything behind the self-prediction check and a hash of the predicted
base, and keep the plain delta as the fallback path.

## 9. Reproduce

```
cd bench/gotransform && go build -o gotransform .        # go 1.26, x/arch v0.20.0
./gotransform inventory   OLD [NEW]                       # §2
./gotransform sectdiff    OLD NEW                         # §3 (runs bsdiff/hdiffz/zstd per section)
./gotransform predict     [-block 16] [-datatol 0] [-mask] [-enc bhz] [-dump N] OLD NEW   # §4
./gotransform pclnpredict [-gfblock 16] [-gftol 4] [-enc bhz] OLD NEW                     # §5
./gotransform datapredict [-block 16] [-enc bhz] OLD NEW  # §6
./gotransform whole       [-block 16] [-enc bhz] OLD NEW  # §7
```

Synthetic pairs: `../out/bin/{v1,v2s,v2l,v2c,v2p,v3,v4}-F2` (built by
`bench/build.sh`). Corpus: `run_corpus.sh` (Go 1.26 pairs, `-enc bh`, vault
`-enc h`), `run_corpus125.sh` (terraform, cockroach), `run_corpus_whole.sh`
(the `whole` runs; only kube-apiserver completed). Tools: system `bsdiff`,
`zstd -19 --long=27 --patch-from`, `hdiffz -m-6 -SD -d -f -p-1 -c-zstd-21-24`
from `bench/out/tools/HDiffPatch`. Scratch files go to `$GOTRANSFORM_TMP`.
Logs: `bench/out/logs/06-gt-{inventory,sectdiff,predict,pclnpredict,datapredict,whole}*.log`
and `06-gt-corpus-<pair>-<cmd>.log`; run timings in `06-gt-corpus-run.log`
(kube 3.0 min, prometheus 4.5–5.0 min, vault 9.8 min) and
`06-gt-corpus125-run.log` (terraform 5.6 min, cockroach 9.7 min).

References (Go 1.26.4 tree at `/usr/local/go/src`): `runtime/symtab.go`
(`pcHeader`, `moduledata`, `funcInfo`), `runtime/runtime2.go` (`_func`),
`internal/abi/symtab.go` (pclntab magic, `FuncTabBucketSize`, `MINFUNC`),
`cmd/link/internal/ld/pcln.go` (`walkFuncs`, `findfunctab`, table alignments,
inline-tree records), `cmd/link/internal/amd64/l.go` (`funcAlign`),
`debug/gosym/pclntab.go` (independent reader of the same format);
`golang.org/x/arch/x86/x86asm` (`Inst.PCRel`, `Inst.PCRelOff`). Related notes:
`docs/research/go-binary-layout.md`, `docs/research/benchmark-local.md`.

## 10. Go 1.27 pass

Scope of this pass (logs `bench/out/logs/08-gt127-*.log`; binaries built by
`bench/build.sh` with `BENCH_OUT_BIN=bench/out/bin127` under go1.27.0, and
prometheus v3.13.1/v3.13.2 built from source with go1.27.0 into
`bench/out/corpus127/prometheus/` — `08-gt127-corpus-versions.log` has
`go version -m` of every output): make the prototype handle Go 1.27, make the
measured numbers honest (nothing oracle, decoder implemented), and take the
cheap part of the "next 2–3×" (type-descriptor offsets). Go 1.27 is the only
fully supported layout from here on; 1.26 still works because it cost nothing;
1.25 is untouched (§5/§7.5 caveats stand).

### 10.1 Go 1.27 format findings

Read from the 1.27 tree (`runtime/symtab.go`, `runtime/type.go`,
`runtime/iface.go`, `internal/abi/type.go`, `cmd/link/internal/ld/{data,symtab}.go`)
and confirmed on the binaries:

1. **`moduledata` changed**: `types, typedesclen, etypes, itaboffset, itabsize`
   replace `types, etypes`, and the `typelinks []int32` / `itablinks []*itab`
   slices are gone (`.typelink` and `.itablink` sections no longer exist).
   Offsets (bytes): types 296, typedesclen 304, etypes 312, itaboffset 320,
   itabsize 328, rodata 336, gofunc 344, epclntab 352, textsectmap 360. The
   runtime rebuilds the typelink list lazily by walking descriptors from
   `types+8` for `typedesclen` bytes with `abi.Type.DescriptorSize` (linker:
   typelink-flagged descriptors are laid out first, sorted by type string) and
   the itabs from `types+itaboffset` for `itabsize` bytes (`itab.Size()`).
   The prototype detects the version from `.go.buildinfo` (`debug/buildinfo`)
   or the presence of `.go.type`, and checks `epclntab == end of .gopclntab`
   after parsing.
2. **Type descriptors moved out of `.rodata`** into a new relro section
   `.go.type` (STYPE symbols: descriptors, itabs, `type:.namedata.*`), with a
   sibling `.go.func` (SGOFUNC: `go:funcdesc` / `·f` closure descriptors).
   Synthetic v1: `.rodata` 3,527,449 → 787,129 B, `.go.type` 2,710,760 B;
   prometheus: `.rodata` 3.7 MB, `.go.type` 11.8 MB. `nameOff`/`typeOff` stay
   relative to `moduledata.types` (= `.go.type` start); some name data
   referenced by `nameOff` lives outside `.go.type` (in `.noptrdata`).
3. **`abi.MapType` grew by 24 bytes** (136 vs 112: `KeysOff/KeyStride/ElemsOff/
   ElemStride` replace `SlotSize`); every other descriptor struct, `_func`,
   `pcHeader`, the pclntab magic (0xfffffff1), `findfunctab` and `funcAlign`
   are unchanged (`pcln.go` diff is empty).
4. **`go:func.*` alignment inside `.gopclntab` is data-dependent**: 16 on
   every 1.26 build, 8 on the 1.27 builds (offset 8846552 ≡ 8 mod 16), so the
   old hard-coded `place(gofunc, 16)` made the 1.27 self-prediction 8 bytes
   too long (`08-gt127-pclnpredict-v1-v2c-pre.log`: 1,009,107 differing bytes).
   Fixed by transmitting the seven table offsets (≈ 30 B, see 10.2).
5. **The generic-delta baseline is worse on 1.27**: the one-line change
   v1→v2c is bsdiff 150,475 B on 1.27 vs 60,478 B on 1.26 (hdiffz 176,929 vs
   75,305): the sorted `.go.type` section is full of `textOff` fields that all
   change when one function grows. The Go-aware patch does not care (3,144 B).

After these changes the self-prediction (old → old) is byte-exact on every
1.27 and 1.26 binary tried: v1-F2 (both toolchains, `08-gt127-selfcheck-v1.log`)
and prometheus 3.13.1/go1.27 (`08-gt127-selfcheck-prometheus127-fixed.log`,
0 differing bytes; the earlier 54 bytes were `_func.nameOff` for functions
whose name string has an inlined-only twin in `funcnametab`, now resolved by
mapping the old `nameOff` through an old→new `funcnametab` content map).

### 10.2 What changed in the prototype (honest accounting, end to end)

New files `layout.go`, `codec.go`, `typedesc.go`, `codec_test.go` (tests run
in 3 ms); `whole.go`, `pclnpredict.go`, `datamap.go`, `elfbin.go` reworked.

* **Everything the decoder uses is transmitted and counted.** The `Layout`
  blob (varints, delta-coded against the old binary, zstd −19) carries: new
  file length, section table (name/addr/off/size/NOBITS as deltas vs the old
  section of the same name), the moduledata values the predictors read, the
  function layout table (§4.1), `nfiles`, the pclntab length and its seven
  table offsets, the `(npcdata, nfuncdata)` of every new-function record and
  of every matched record whose shape changed (20 on prometheus), the bss
  shift tables, and the `.rodata/.go.type/.go.func/.noptrdata/.data` content
  maps as run-length `(count, Δshift)` pairs (prometheus/1.27: `.go.type`
  1,979 shift changes → 6,002 B raw). Layout cost: 140–225 B synthetic,
  4,390–5,728 B corpus (was "0 B, oracle" for the maps and section table).
* **Length-exact pclntab**: with the transmitted table offsets and record
  shapes the predicted `.gopclntab` has exactly the new length on every pair
  (previously 0/4 corpus pairs), which is what makes stage 2 positional.
* **Decoder implemented** (`decode OLD PATCH NEW`): parses the patch, checks
  sha256(old), applies stage 1 with the external tool (hpatchz/bspatch/zstd),
  builds a *skeleton* new binary from the layout + stage-1 blobs, runs the same
  `predictWhole` as the encoder, checks sha256(predicted base) — the
  encoder/decoder-divergence guard of §8 — applies stage 2, checks
  sha256(new). The encoder itself predicts from the decoded layout and the
  stage-1 output, never from `new`, so nothing can leak.
* **Positional stage 2**: runs of differing bytes as `uvarint(gap)
  uvarint(len) bytes` (runs < 4 bytes apart merged), zstd −19; `-s2 h|b|z`
  keep the generic-delta stage 2 for comparison.
* **Bounded content-map index**: the per-position Go map (95 M entries on
  vault) is replaced by a sampled index (1 in 8 windows by rolling hash,
  flat sorted array, 16 B/entry); a block that fails at the previous shift
  looks up the sampled windows in the next 64 bytes and verifies every
  candidate against its own bytes, so matching is still exact and at any
  alignment. Windows with ≥ 256 occurrences are skipped (repetitive content
  is resolved in the backward pass as before).
* **`.plt` relocated** as code (rip-relative into `.got.plt`), and `.got.plt`,
  `.got`, `.dynamic`, `.rela*`, `.dynsym`, `.go.module`, `.go.fipsinfo` get the
  absolute-pointer rewrite; `.go.type`/`.go.func` get content maps.
* **Type-descriptor offsets regenerated** (`typedesc.go`, 330 lines): walk the
  typelinked descriptors (1.27: `[types+8, types+typedesclen)` with a port of
  `DescriptorSize`; 1.26: `.typelink`) and the itabs, follow every `*Type`
  pointer and `typeOff` to reach the non-typelinked descriptors (prometheus:
  62,516 descriptors, 5,337 itabs), and re-target `Type.Str/PtrToThis`,
  `UncommonType.PkgPath`, `Method.{Name,Mtyp,Ifn,Tfn}`, `Imethod.{Name,Typ}`
  and the trailing pkgPath `nameOff` of names (197,353 nameOff + 114,379
  typeOff + 170,045 textOff + 2,155 pkgPath rewritten on prometheus/1.27).
  Nothing is transmitted; the map for the type section is the one already
  sent. `-types=false` disables it for A/B.

### 10.3 Results (all end to end: encode → patch file → decode → byte-identical)

Totals are the patch file size (header 111–114 B + layout + stage 1 + stage 2).
Stage 1 is hdiffz in every Go-aware row; "positional" and "hdiffz s2" differ
only in stage 2. Baselines are `bsdiff` and `hdiffz -m-6 -SD -d -p-8
-c-zstd-21-24` on the whole files. Synthetic rows: `08-gt127-run-syn127t.log`;
prometheus/1.27: `08-gt127-run-realf.log` (final binary); the two official
1.26 pairs: `08-gt127-run-realt.log`.

| pair (Go) | bsdiff | hdiffz -p-8 | Go-aware positional: total = layout + stage 1 + stage 2 | Go-aware hdiffz s2 | factor (bsdiff → positional / → hdiffz s2) |
|---|---:|---:|---|---:|---|
| v1→v2s (1.27) | 351 | 163 | 425 = 140 + 39 + 135 | 453 | 0.8× / 0.8× |
| v1→v2l (1.27) | 24,874 | 33,713 | 525 = 152 + 39 + 223 | 556 | 47.4× / 44.7× |
| v1→v2c (1.27) | 150,475 | 176,929 | 3,144 = 207 + 1,283 + 1,542 | 2,862 | 47.9× / 52.6× |
| v1→v2p (1.27) | 109,940 | 130,642 | 3,064 = 200 + 1,069 + 1,683 | 3,088 | 35.9× / 35.6× |
| v1→v3 (1.27) | 142,634 | 170,105 | 5,026 = 218 + 2,180 + 2,516 | 4,918 | 28.4× / 29.0× |
| v1→v4 (1.27) | 145,205 | 171,760 | 5,828 = 225 + 2,587 + 2,904 | 5,818 | 24.9× / 25.0× |
| v3→v4 (1.27) | 30,196 | 40,523 | 6,486 = 153 + 810 + 5,411 | 7,194 | 4.7× / 4.2× (see 10.5) |
| prometheus 3.13.1→3.13.2, **built with go1.27.0** | 2,691,644 | 2,719,152 | **237,585** = 5,252 + 100,949 + 131,270 | **206,902** | **11.3× / 13.0×** |
| prometheus 3.13.1→3.13.2, official go1.26.5 (stripped) | 2,714,204 | 2,730,887 | 291,214 = 4,390 + 97,374 + 189,336 | 184,388 | 9.3× / 14.7× |
| kube-apiserver 1.36.3→1.36.4, official go1.26.5 | 2,060,250 | 2,367,788 | 292,972 = 5,728 + 68,461 + 218,669 | 183,956 | 7.0× / 11.2× |

With bsdiff for both stages (`-s1 b -s2 b`) the corpus totals are 190,871
(prometheus/1.27), 171,993 (prometheus/1.26) and 162,427 (kube-apiserver) at
1.5–2× the encode time. For comparison, §7.5's oracle numbers were 393,964
(prometheus/1.26) and 358,607 (kube): the honest, transmitted, decoded
pipeline is *smaller* than the oracle one because of the type-descriptor
rewrite. Its effect in isolation (`08-gt127-run-real.log`, same binary
without it): prometheus/1.27 positional 960,168 → 237,585, hdiffz s2 523,786
→ 206,902; prometheus/1.26 1,040,304 → 291,214 / 486,308 → 184,388; kube
884,210 → 292,972 / 410,332 → 183,956 (`08-gt127-types-prometheus127-{off,on}.log`
show the `.go.type` residual going 1,190,177 → 154,635 bytes).

**Encode / decode cost** (`/usr/bin/time`, wall seconds / peak RSS MB; the
RSS is the largest process in the tree — the encoder runs its hdiffz/zstd
children sequentially, so this is the true peak, not a sum; encode/decode run
with GOGC=50):

| pair | bsdiff (bspatch) | hdiffz -p-8 (hpatchz) | Go-aware encode, positional | Go-aware encode, hdiffz s2 | Go-aware decode, positional | Go-aware decode, hdiffz s2 |
|---|---|---|---|---|---|---|
| v1→v2c (1.27, 30 MB) | 5.98 s / 258 MB (0.10 s / 61 MB) | 0.75 s / 105 MB (0.01 s / 11 MB) | 1.68 s / 230 MB | 1.78 s / 231 MB | 0.28 s / 205 MB | 0.32 s / 182 MB |
| prometheus/1.27 (94 MB) | 42.7 s / 805 MB (0.49 s / 187 MB) | 7.4 s / 388 MB (0.07 s / 25 MB) | 12.6 s / 698 MB | 13.1 s / 695 MB | 1.04 s / 591 MB | 1.15 s / 575 MB |
| prometheus/1.26 (98 MB) | 44.4 s / 838 MB (0.50 s / 194 MB) | 7.0 s / 397 MB (0.09 s / 25 MB) | 39.3 s / 722 MB | 39.9 s / 703 MB | 0.93 s / 634 MB | 1.08 s / 705 MB |
| kube-apiserver/1.26 (88 MB) | 45.6 s / 758 MB (0.42 s / 174 MB) | 6.1 s / 377 MB (0.06 s / 22 MB) | 30.5 s / 636 MB | 30.6 s / 633 MB | 5.04 s / 581 MB | 5.05 s / 623 MB |

Reading: the encoder is 3–4× faster than bsdiff and holds about as much
memory (three whole binaries plus maps); 10.7 of the 12.6 s on prometheus/1.27
is the content-map build (`08-gt127-types-prometheus127-on.log` timestamps),
and the 1.26 pairs are slower because their 19 MB `.rodata` maps take longer
than 1.27's split `.go.type`/`.rodata`. Decode is 1 s and needs no generic
delta on the positional path — but it still holds old + predicted + tables
(≈ 6× the file), which is the obvious next thing to bound.

### 10.4 Residual by section after this pass

Stage-2 differing bytes (runs) of the positional path:

| pair | .text | .go.type / .rodata | .gopclntab | .typelink + .itablink | other |
|---|---:|---:|---:|---:|---|
| prometheus/1.27 | 64,548 (6,074) | .go.type 154,635 (62,078); .rodata 6,083 (2,212) | 7,169 (3,663) | — (sections gone) | .go.func 2,568; .data 1,552; .noptrdata 647 |
| prometheus/1.26 | 74,809 (7,614) | .rodata 70,916 (17,005) | 23,917 (9,343) | 77,162 + 8,505 | .data 1,064; .noptrdata 674 |
| kube-apiserver/1.26 | 55,186 (10,141) | .rodata 145,173 (50,191) | 15,312 (6,800) | 66,573 + 6,687 | .noptrdata 28,759 |
| v1→v2c (1.27) | 966 (298) | .go.type 5 (4); .rodata 31 (14) | 7 (7) | — | build IDs 100; .go.module 12 |

What remains, in patch bytes on prometheus/1.27 (237,585 total): **stage 1 =
100,949 B** (the transmitted new `funcnametab`/`cutab`/`filetab`/`pctab`/
`go:func.*`, i.e. the pc tables — now the single largest item, 42 %), the
`.go.type` residual (~ 80 KB of the 131 KB stage 2: 113,914 of 735,529 blocks
are unmatched because a changed 4-byte offset inside a 16-byte block defeats
exact matching; a fuzzy tolerance makes it worse — `08-gt127-types-prometheus127-tol{4,8}.log`
— because wrong shifts get confirmed), and `.text` 64,548 bytes of genuinely
changed code. On the 1.26 pairs `.typelink` (66–77 KB of int32 offsets that
the sorted-by-string insertion shifts) is left unregenerated because it no
longer exists in 1.27.

### 10.5 Not done, and known defects

* **v3→v4 on 1.27 is only 4.7×** (6,486 B): 10,486 one-byte differences in
  `.go.type` are the `GCData` pointers of pointer types that all target one
  `runtime.gcbits.*` symbol in `.rodata`, which the `.rodata` content map
  places 5 bytes off — gcbits content is short and repetitive, every window is
  ambiguous, and the backward pass keeps shift 0. Diagnosed
  (`08-gt127-run-syn127t.log`, dump analysis in the session), not fixed; a
  pointer-target consensus (many pointers agreeing on one shift) would resolve
  it and is the next map improvement.
* **pctab / go:func.* are still transmitted, not regenerated** (100,949 B of
  237,585 on prometheus/1.27). Regenerating them means re-encoding pc-value
  tables against the new function sizes; not attempted.
* **Only prometheus was rebuilt with 1.27**; kube-apiserver, vault, terraform,
  cockroach were not (the two official 1.26 pairs are the cross-check). The
  Go 1.25 path was not touched.
* **Decoder memory** is ≈ 600 MB for a 94 MB binary (old + predicted + fake
  pclntab + maps); streaming section-by-section would cut it to ≈ 2× the file.
* The synthetic pass (`syn127t`) ran with a binary that predates the
  `nameOff` fix and the `mapAddr`-based `nameOff` resolution; both leave the
  synthetic pairs unchanged (v1→v2c 3,144 B, v3→v4 6,486 B, v1→v1 0 differing
  bytes with the final binary). The prometheus/1.27 row is from the final
  binary (`realf`); the two 1.26 rows from the preceding one (`realt`, ≤ 100 B
  of `nameOff` residual difference, cf. 7,223 → 7,169 `.gopclntab` bytes on
  prometheus/1.27).
* kube-apiserver decodes in 5 s vs 1 s for prometheus (both paths); not
  profiled — most likely the 1.26 type walker over its larger `.rodata`.
* `.go.module` (moduledata) is pointer-rewritten but its slice lengths are
  left to stage 2 (52 B); build IDs (100 B) are inherent.
* arm64, PIE, cgo-heavy binaries: unchanged from §8.

### 10.6 Reproduce

```
cd bench/gotransform && go build -o gotransform .              # go1.27.0
./gotransform encode [-s1 h|b|z] [-s2 p|h|b|z] [-types=false] OLD NEW PATCH
./gotransform decode OLD PATCH NEW                            # sha256-verified
./gotransform whole OLD NEW                                    # measurement only
go test ./...                                                 # 3 ms
GT=./gotransform ./run_corpus127.sh NAME OLD NEW [OLD NEW ...] # baselines + encode/decode under /usr/bin/time
python3 summarize127.py ../out/logs/08-gt127-run-NAME.log      # the tables above
```

Synthetic 1.27 corpus: `BENCH_OUT_BIN=$PWD/out/bin127 GOTOOLCHAIN=go1.27.0 bench/build.sh`.
prometheus/1.27: `go build -trimpath -ldflags="-s -w -buildid=" -buildvcs=false ./cmd/prometheus`
at tags v3.13.1 / v3.13.2 (`08-gt127-build-prometheus-*.log`: 45 s, 3.0 GB RSS
each).

## 11. Round 4: encoder time, gcbits consensus, pc tables

Scope: Go 1.27 amd64 ELF only; prometheus 3.13.1→3.13.2 (both built with
go1.27.0) and the seven synthetic `bin127` pairs. Everything below is end to
end (encode → patch file → decode → `cmp` byte-exact) under `/usr/bin/time -v`;
all timings are single runs on the same machine.

**Headline** (positional stage 2 unless stated; details in 11.3):
prometheus/1.27 237,585 → **111,552 B** (bsdiff 2,691,644: 11.3× → **24.1×**;
94,470 B with an hdiffz stage 2 = 28.5×; 91,425 B with bsdiff for both stages
= 29.4×). Encoder 12.6 s → **2.1 s** wall (bsdiff 39 s, hdiffz -p-8 6.5 s) at
632 MB RSS; decoder 1.04 → 0.98 s. Synthetic v3→v4 6,486 → **578 B** (bsdiff
30,196, 52×), v1→v2c 4,559 → 2,207 B (bsdiff 150,475, 68×; 1,930 B / 78× with
hdiffz stage 2).

### 11.1 What changed (file / function level)

*Starting point, honestly.* The working tree at the start of this round did
**not** reproduce the §10 numbers: building it gave 245,428 B / 5.7 s on
prometheus against the logged 237,585 B / 12.3 s of the `gotransform-127f`
binary (`09-enctime-prometheus127.log`, both runs side by side). `go tool nm`
showed the same functions; a parameter sweep (`09-mapparams-prometheus127.log`)
showed the logged binary behaved like a longer hash chain (`maxchain≈4096`)
in the `.rodata` content map (`.gopclntab` residual 7,169 B is reproduced only
then). Rather than reproduce the drift, the index was redesigned (below); the
"before" column in 11.3 is the logged §10 result.

**A. Encoder time** (`datamap.go`, `shifttab.go`, `whole.go`, `main.go`):

* `deriveShiftTables` (`shifttab.go`) — was one single-threaded x86asm pass per
  matched function pair *plus* a second decode inside the comparison; 62 % of
  the "before" profile (`x86asm.decode1` 5.03 s flat of 8.16 s samples,
  `09-enctime-prometheus127.log`). Rewritten: 24 workers, one decode pass per
  equal-size matched function, non-displacement bytes compared directly,
  per-worker sample lists merged and sorted by (offset, delta). Same output.
* Content-map index (`datamap.go`: `buildWinIndex`, `lookup`) — the sampled
  index (`sampleBits`) plus a Go map of chains was replaced by a dense index:
  one `idxEnt{h uint32, p uint32}` per 16-byte window position, bucketed by
  the top 16 hash bits with a two-pass parallel histogram placement (24
  workers, no temporary per-worker slices), entries sorted by (h, p) within a
  bucket. Lookup is a binary search for the hash run in its bucket and then
  the nearest candidate positions to `q + prev` (the previous block's shift),
  so a 2,048-deep chain of repetitive content no longer walks — it goes
  `Ambiguous`. `buildDataMapFuzzy` is sharded (24 shards seeded with shift 0,
  each re-run from the true incoming shift until the first block agrees, which
  makes the parallel result identical to the sequential one) and keeps a
  `step(i, prev)` closure with a rolling hash (`h = h*rollBase + c + 1`) over
  the lookahead. `DataMap` gained `Ambiguous []bool` and `reliable(i)`.
* Prediction in place and in parallel (`whole.go: predictWholeStats`,
  `predictDataSection(..., dst)`, `predict.go: predictText(..., out)`): each
  section is predicted by its own goroutine directly into its slice of the
  output file, no per-section temporaries.
* Memory (`main.go`): `GOGC=50` and `debug.SetMemoryLimit(640 MiB)` unless
  `GOGC`/`GOMEMLIMIT` are set. Without the limit the parallel build peaked at
  848–868 MB RSS; with it 632–651 MB (encode / decode) on prometheus.
* Flags added for the experiments (`codec.go`): `-cpuprofile`, `-samplebits`,
  `-lookahead`, `-maxchain`, `-ovmin`, `-ovrounds`, `-typeresid N`.

**B. gcbits / pointer-target consensus** (`whole.go`, `layout.go`,
`predict.go`, `typedesc.go`):

* `collectVotes` walks every absolute pointer in the data sections whose own
  block is `reliable` and every type-descriptor field (`rewriteTypeOffsets`
  gained a `vote` callback: name/pkgPath `nameOff`, `typeOff`, text offsets)
  and records (old target, predicted new target). `majorities` reduces the
  sorted votes to one winner per old target; `deriveOverrides` then (round 1)
  fixes *unmatched* target blocks with the majority-implied shift and
  forward-fills, and (round 2) emits an `AddrOverride{Old, New}` for every
  target with ≥ 2 votes, > half agreeing, whose map prediction differs. The
  table is transmitted in the layout (`encodeOverrides` / `decodeOverrides`:
  gap-of-old, delta-of-(new−old) varints) and consulted by `mapAddr` before
  the maps (`mapper.overrides`).
* Prometheus: 492,298 pointers + 494,946 fields → 321,488 distinct targets,
  47,008 mispredicted by the maps, 255 block fixes, 2,681 overrides (20,725
  votes). Cost 5,539 B of layout (5,732 → 11,271 B), gain 30,363 B of stage 2
  (110,059 → 79,696 B): net −26,454 B (`09-c-ablation-prometheus127.log`).
  Round 2 changes nothing on prometheus (identical output at `-ovrounds 1`),
  so the default of 2 rounds costs ≈ 0.2 s for no measured benefit; kept
  because the block fixes of round 1 do change the map the votes were
  counted on, and only one binary pair was checked.
* `residual.go: typeResidual` (flag `-typeresid`) classifies the remaining
  `.go.type` residual by descriptor region and block status
  (`09-typeresid-prometheus127.log`) — used for task D, see 11.5.

**C. pc tables / go:func.* regeneration** (`blobs.go` new, `layout.go`,
`pclnpredict.go`, `codec.go`):

Stage 1 is now two generic deltas instead of one:

* stage 1a: old `funcnametab|filetab` → new (as before, 1,484 B on prometheus);
* stage 1b: **predicted** `cutab|pctab|go:func.*` → new, where the prediction
  is `predictBlobs(old, skeleton, match, mapper)` emulating
  `cmd/link/internal/ld/pcln.go` from the old tables in the *new* function
  order: `cutab` re-mapped by file name through the new `filetab` (73,253
  entries, 71,508 mapped, the rest are new files → `^0`); `pctab` = one zero
  byte then per new function (its old counterpart's) pcsp, pcfile, pcln,
  pcdata[k≠2], then pcdata[2] (pcinline), each self-delimiting
  (`pcTableLen`), deduplicated by content (241,951 tables); `go:func.*` = the
  old funcdata symbols (77,276) sorted by alignment class — stable, first-use
  order within a class — with the relocations re-resolved: inline-tree
  `nameOff` through a `funcnametab` content map with name verification
  (`nameOffMap`, 183,592 entries), stack-object `gcdata` and wrapinfo through
  `mapAddr`. The class rule that reproduces the linker's order is: stack
  objects 8, pointer maps / inline trees / wrapinfo 4, the varint streams
  (opendefer, arginfo, argliveinfo) 1, *except* the argument maps of assembly
  functions which are 8 (they sit before the first break in first-use order
  of the old blob, which is how they are recognised). Self-check
  (`blobcheck`, old → old): cutab 0 differing bytes, pctab and go:func.*
  exact up to trailing alignment padding (`09-blobcheck.log`), which is then
  padded to the layout's table lengths.
* The decoder builds the skeleton with a zeroed fake pclntab
  (`skeletonBin(old, layout)`), applies 1a into it (`fillSkeleton`), runs
  `predictBlobs`, applies 1b onto the prediction, and `predictPcln` re-bases
  every `_func` pctab/funcdata offset old → predicted (`blobPred.PcOff /
  GfOff`, exact) → new (content map of two nearly identical blobs), instead of
  old → new through a content map of two very different blobs.
  `.gopclntab` stage-2 residual 7,169 → 4,549 B as a side effect.
* Patch format `GTP2`: header, layout, S1a, S1b, S2 (`codec.go`).
* Where the remaining 18,985 B of stage 1b are
  (`09-c-s1b-breakdown-prometheus127.log`, hdiffz of each table separately):
  pctab 14,367 B (changed functions' pc tables — inherent unless pc-value
  tables are re-derived from the text change), go:func.* 3,410 B, cutab
  1,466 B. Before: go:func.* 61,654 + cutab 21,800 + pctab 14,669 B
  (`09-pctab-regions-prometheus127.log`). So the win is almost entirely the
  funcdata blob and cutab; pctab was already handled well by hdiffz because
  its content moves as whole tables.

**Tests** (`codec_test.go`): `TestMajorities`, `TestOverridesRoundTrip`
(incl. truncated stream), `TestPcTableLen`, `TestFillSkeleton`,
`TestDenseIndexLookup`; `go test ./...` 17 ms (`09-gotest.log`).

### 11.2 Results: before (§10, logged binary) → after (this round)

prometheus 3.13.1 → 3.13.2, go1.27.0, 93.74 → 93.77 MB
(`09-run-prometheus127.log`; before: `08-gt127-run-realf.log` /
`08-gt127-run-realt.log`):

| variant | patch B before | after | vs bsdiff | encode s / MB before | after | decode s / MB before | after |
|---|---|---|---|---|---|---|---|
| bsdiff | 2,691,644 | 2,691,644 | 1.0× | 39 / 805 | — | 0.47 / 187 | — |
| hdiffz -m-6 -p-8 | 2,719,152 | 2,719,152 | 0.99× | 6.5 / 388 | — | 0.07 / 25 | — |
| go-aware, positional s2 | 237,585 | **111,552** | **24.1×** | 12.6 / 698 | **2.07 / 632** | 1.04 / 591 | 0.98 / 651 |
| go-aware, hdiffz s2 | 206,902 | **94,470** | **28.5×** | 13.1 / 716 | 2.36 / 633 | 1.01 / 578 | 1.05 / 631 |
| go-aware, bsdiff s1+s2 | 190,971 | **91,425** | **29.4×** | 38.1 / 825 | 24.5 / 805 | 1.41 / 641 | 1.30 / 619 |

Patch composition, positional: header 116 + layout 11,271 + stage 1a 1,484 +
stage 1b 18,985 + stage 2 79,696 (before: 114 + 5,252 + 100,949 + 131,357).
Stage-2 residual by section (differing bytes, before in parentheses):
`.text` 62,555 in 4,935 runs (64,548 in 6,074 — the 2 KB gained are
mispredicted data references fixed by the overrides; the rest is the real
code change), `.go.type` 45,170 (154,635), `.gopclntab` 4,549 (7,169),
`.rodata` 3,782 (6,083), `.go.func` 2,568 (2,568), `.data` 744 (1,552),
`.noptrdata` 468 (647), `.go.buildinfo` 215 (215), `.go.module` 36 (52).

Synthetic pairs (`bin127`, 29.99 MB; `09-run-syn127.log`, before
`08-gt127-run-syn127.log`; positional / hdiffz stage 2):

| pair | bsdiff | hdiffz | before pos / hd | after pos / hd | after vs bsdiff |
|---|---|---|---|---|---|
| v1→v2s (rebuild) | 351 | 163 | 425 / 453 | 466 / 494 | 0.75× |
| v1→v2l (literal) | 24,874 | 33,713 | 525 / 556 | 566 / 597 | 44× |
| v1→v2c (one line) | 150,475 | 176,929 | 4,559 / 3,304 | **2,207 / 1,930** | 68× / 78× |
| v1→v2p (+package) | 109,940 | 130,642 | 4,443 / 3,555 | **1,684 / 1,622** | 65× |
| v1→v3 | 142,634 | 170,105 | 6,461 / 5,348 | **2,702 / 2,506** | 53× |
| v1→v4 | 145,205 | 171,760 | 7,264 / 6,249 | **2,733 / 2,545** | 53× |
| v3→v4 (gcbits case) | 30,196 | 40,523 | 6,486 / 7,194 | **578 / 613** | 52× |

Encode wall 1.8 → 0.6 s per pair (bsdiff 4.7–5.7 s), decode 0.3 s, RSS
≈ 320 → ≈ 270 MB. The two trivial pairs got 41 B *worse*: the second stage-1
delta has a 39 B hdiffz floor even when nothing changed. The gcbits defect of
§10.5 is closed: v3→v4 stage 2 5,411 → 228 B; the consensus alone took it to
1,310 B (`09-gcbits-syn127.log`), the pc-table regeneration the rest.

### 11.3 Encoder time breakdown (prometheus, wall-clock laps)

| step | before (`gotransform-127f`, 12.19 s) | after (2.03 s) |
|---|---|---|
| load + match | 0.14 | 0.14 |
| content maps (.rodata .go.type .go.func .noptrdata .data) | 0.65 | 0.45 |
| shift tables (x86 decode of matched functions) | 10.2 | 0.14 |
| overrides (votes, 2 rounds) | — | 0.40 |
| layout | 0.01 | 0.01 |
| stage 1 (hdiffz) | 0.43 | 0.06 + 0.30 (1a + predictBlobs/1b) |
| prediction (relocate .text, data sections, pclntab) | 0.68 | 0.42 |
| stage 2 (positional + zstd -19) | 0.12 | 0.10 |

CPU profile after (`09-c-profile-prometheus127.log`, 10.48 s of samples in
2.13 s wall, 491 %): `x86asm.decode1` 38.5 % cum (now split over 24 workers
in `relocateBytes` 23 % and `deriveShiftTables` 21 % — the text is decoded
twice; caching the first decode would save ≈ 0.3 s wall), index build
(`buildWinIndex` histogram + per-bucket `pdqsort`) ≈ 30 %, lookups 5 %,
sha256 1 %. Before: `x86asm.decode1` 62 % flat, single-threaded.

### 11.4 What did not work, and why

* **Single-vote overrides** (`-ovmin 1`): patch 227 KB vs 194 KB at ≥ 2 votes
  and a *larger* `.text` residual — a lone pointer whose own block was
  mis-mapped votes for a wrong target and the override then corrupts every
  reference to it. Majority with ≥ 2 votes and > half is the floor.
* **Reproducing the drifted §10 binary's map parameters**: `-maxchain 4096`
  reproduces its `.rodata` map but costs 10 s; the dense nearest-position
  index gives a better map (patch 218,131 B before overrides) at 0.45 s, so
  the old behaviour was not restored.
* **Sampled index at 0 sample bits** with the old chain walker: > 120 s (chain
  iteration through a `seen` map per candidate). Fixed by design, not tuning.
* **Fully parallel prediction without a memory limit**: 848–868 MB RSS,
  above the ~700 MB budget; the no-temporary index build, in-place section
  prediction and the 640 MiB soft limit brought it to 632–651 MB. The limit
  is a soft one: RSS is set by the live set (old + new + fake pclntab +
  index), not by GC slack.
* **go:func.* by size divisibility**: classifying pointer maps as 8-aligned
  when `size % 8 == 0` (2.67 M differing bytes) — the linker's class is the
  symbol's, not the size's; only the assembly `args_stackmap` symbols are
  8-aligned among the pointer maps.
* **pctab content dedup**: emulated, never triggers (0) — the compiler's
  content-addressable symbols already deduplicate identical tables, so every
  old offset is unique content.
* **Task D, unmatched `.go.type` blocks**: the consensus pass cut the
  `.go.type` residual 154,635 → 45,170 B. What is left
  (`09-typeresid-prometheus127.log`): 31.6 KB is inserted/changed descriptor
  content with no old counterpart (uncovered blocks — genuinely new types and
  methods), 7.9 KB wrong offset rewrites inside matched descriptors, 4.5 KB
  names; the "unmapped typeOff" counts are the `-1` unreachable-type
  sentinel, i.e. benign. No anchor pass beyond the consensus was attempted.

### 11.5 Remaining defects and not done

* `.text` 62,555 B (56 % of the positional stage 2) is the actual code change
  plus 1,876 undecodable instructions; nothing in this round touched it.
* `pctab` 14,367 B is per-function pc-value table change for changed
  functions; regenerating pcsp/pcline from the new text would need the
  compiler's information, not the linker's.
* The override table is 5.5 KB of the 11.3 KB layout; a cheaper encoding
  (group by section, delta against the map's own prediction) was not tried.
* `ovRounds = 2` is unproven (no change on prometheus at 1 round).
* Only prometheus was measured for the real-release case; the 1.26 pairs of
  §10 were not re-run on this binary (the pclntab emulation targets 1.27's
  linker order and was only self-checked on 1.27 binaries).
* Decoder memory ≈ 650 MB for a 94 MB binary, unchanged in structure.
* Experimental flags (`-samplebits`, `-lookahead`, `-maxchain`, `-ovmin`,
  `-ovrounds`, `-typeresid`) are still in the binary.

### 11.6 Reproduce

```
cd bench/gotransform && go build -o gotransform-09 .        # go1.27.0
timeout 30 go test ./...                                    # 17 ms
./gotransform-09 blobcheck OLD [OLD ...]                    # pc-table / go:func.* self-check (old -> old)
LOGPREFIX=09-run GT=./gotransform-09 ./run_corpus127.sh syn127 \
  ../out/bin127/v1-F2 ../out/bin127/v2s-F2 ../out/bin127/v1-F2 ../out/bin127/v2l-F2 \
  ../out/bin127/v1-F2 ../out/bin127/v2c-F2 ../out/bin127/v1-F2 ../out/bin127/v2p-F2 \
  ../out/bin127/v1-F2 ../out/bin127/v3-F2  ../out/bin127/v1-F2 ../out/bin127/v4-F2 \
  ../out/bin127/v3-F2 ../out/bin127/v4-F2
LOGPREFIX=09-run GT=./gotransform-09 ./run_corpus127.sh prometheus127 \
  ../out/corpus127/prometheus/3.13.1/prometheus ../out/corpus127/prometheus/3.13.2/prometheus
python3 summarize127.py ../out/logs/09-run-syn127.log ../out/logs/09-run-prometheus127.log
./gotransform-09 encode -cpuprofile enc.prof OLD NEW PATCH && go tool pprof -top gotransform-09 enc.prof
./gotransform-09 encode -ovrounds 0 OLD NEW PATCH            # override ablation
GOTRANSFORM_DUMP=dir ./gotransform-09 encode OLD NEW PATCH    # writes s1b-{0,1,2}.{pred,new} for per-table hdiffz
```

Logs: `09-run-syn127.log`, `09-run-prometheus127.log`, `09-c-prom.log`,
`09-c-profile-prometheus127.log`, `09-c-ablation-prometheus127.log`,
`09-c-s1b-breakdown-prometheus127.log`, `09-blobcheck.log`, `09-gotest.log`,
`09-enctime-prometheus127.log`, `09-mapparams-prometheus127.log`,
`09-gcbits-syn127.log`, `09-gcbits-prometheus127.log`,
`09-typeresid-prometheus127.log`, `09-pctab-regions-prometheus127.log`.

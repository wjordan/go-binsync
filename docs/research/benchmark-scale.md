# Scale benchmark: real release deltas, 100 MB-540 MB binaries, lossy WAN

Measured on 2026-08-26 on the same 24-core Linux 6.17 x86-64 box (91 GB RAM) as
`benchmark-local.md`, whose assumptions (one-line change, 29.6 MB binary, 100 Mbit/100 ms
link) this document re-grounds against (a) real adjacent releases of prometheus,
kube-apiserver, terraform, cockroach and vault, (b) 88-538 MB binaries, (c) a real
netem-emulated WAN at 5-100 Mbit/s, 100-300 ms RTT, 0-2 % loss. Scripts: `bench/scale/`;
downloaded corpus: `bench/out/corpus/` (index: `bench/out/corpus/README.md`); logs:
`bench/out/logs/07-*.log`; JSON: `bench/out/results/{scale-small,scale-big,churn_scale,netem}.json`.

## Key findings

* **Real releases move almost everything.** Every adjacent release in the corpus (kube-apiserver 1.36.3->1.36.4, prometheus 3.13.1->3.13.2, terraform 1.15.8->1.15.9, cockroach 26.2.4->26.2.5, vault 2.0.3->2.0.4) rewrites 79-87 % of the stripped binary's bytes in 5.8-24 M byte-runs of median 3-4 B (`.text` 82-96 %, `.gopclntab` 79-87 %, `.rodata` 65-80 %); 0-2 % of the file survives in unchanged runs >= 64 KiB (the one-line synthetic v2c: 13.4 % / 21.6 %). Even prometheus 3.13.2 (11 commits, 4 Go files, +136/-36 lines) does this. Chunk/CDC stores are ruled out for real Go releases.
* **Best patch = 11-21 % of a full download for patch releases, 35-40 % for a minor release** (bsdiff/hdiffz -m-6 vs `zstd -19` of NEW): kube 2.06 MB vs 19.1 MB (10.8 %), prometheus 2.71 vs 23.8 MB (11.4 %), cockroach stripped 8.59 vs 58.4 MB (14.7 %), vault stripped 11.75 vs 67.7 MB (17.4 %, across a Go 1.26.4->1.26.5 bump), terraform 4.92 vs 25.5 MB (19.3 %), prometheus 3.13.2->3.14.0 8.6 vs 24.3 MB (35 %). The old synthetic gave 0.7 %.
* **Unstripped release binaries (prometheus, vault, cockroach ship them) cannot be delta'd usefully:** best patch 56-71 % of the full download because 24.6-77.7 MB of zlib-compressed `.debug_*` is 99 % rewritten. Stripping prometheus turns a 27.9 MB patch into 2.7 MB.
* **bsdiff and hdiffz -m-6 tie on size** (within +-12 % on every pair, 5 wins each); `zstd --patch-from` is 2.3-3.4x larger, xdelta3 -9 lzma 3.1-4.4x, hdiffz -s-64 (streaming) 3.0-4.4x.
* **Encode time is the differentiator at scale:** at 393-537 MB, hdiffz -m-6 -p-8 46 s (single-thread 161 s), `zstd -19 -T0` 36-38 s, xdelta3 55-58 s, hdiffz -s-64 71-83 s, bsdiff 267 s at 393 MB and **> 840 s (timeout) at 537 MB**. bsdiff scales as n^1.43 (3.1 MB/s at 30 MB -> 0.7 MB/s at 326 MB), hdiffz as n^1.2-1.3, zstd/xdelta3 ~linearly.
* **Encoder memory per input byte:** bsdiff 9.0, xdelta3 (`-B` = file size) 8.9, zstd -19 --long 4.5-5.7, hdiffz -m-6 3.2-4.6, hdiffz -s-64 flat ~220 MB. Apply: hpatchz flat 27 MB; bspatch and `zstd -d --patch-from` 1.9 B/B (1.03 GB to apply a vault patch) - the client-side number.
* **1 GB extrapolation** (linear from 537 MB / log-log fit): hdiffz -m-6 -p-8 86-130 s and 3.3 GB; hdiffz -s-64 155-200 s and < 0.5 GB; `zstd -19 -T0` ~70 s and 4.7 GB (+2 GB to apply); xdelta3 107-149 s and 9.4 GB; bsdiff 680-1,785 s (11-30 min) and 9 GB; full `zstd -19 -T0` ~43 s.
* **Synthetic multi-package release (v1->v5):** 70 % of bytes differ; bsdiff 470 KB (7.8x v2c's 60 KB), hdiffz 573 KB, zstd 1.76 MB, xdelta3 2.72 MB = 5.6-32 % of the 8.43 MB full download; v4->v5 costs the same as v1->v5.
* **netem, no loss (cubic):** a 60 KB GET on a fresh connection costs 4 RTT (0.40 s at 100 ms, 0.81 s at 200 ms); a 304 poll 2 RTT (0.20 / 0.40 / 0.60 s); 25.5 MB takes 3.2 s at 100 Mbit/100 ms (64 Mbit/s effective, slow start) and 12.3 s at 20 Mbit/200 ms (16.6 Mbit/s). 4/8-way ranged fetches gain only 10-20 %.
* **netem with loss (cubic):** at 20 Mbit/200 ms/1 % a single connection gets 1.12-1.18 Mbit/s (5.7 % of the link, 1.6x the Mathis bound of 0.71 Mbit/s): 8.4 MB takes 57 s, 25.5 MB 180 s; 8 parallel ranged connections reach 8-9 Mbit/s (7-7.7x faster). At 5 Mbit/300 ms/2 %: 0.51 Mbit/s single (Mathis 0.33), 8.4 MB in 132 s, 8-way 27 s (4.9x); 25.5 MB single-connection was not measured (run killed, section 9; model: ~500 s, above the 600 s curl limit either way).
* **Transfer model on the measured curves:** at 20 Mbit/200 ms a kube 1.36.4 update is full 20.6 s vs hdiffz -p-8 9.7 s (2.6 s transfer + 7 s encode) vs bsdiff 52.6 s; at 5 Mbit/300 ms/2 % full 317 s vs hdiffz 40 s vs bsdiff 77 s; vault stripped on the same link: full 1,119 s vs hdiffz -p-8 223 s. Parallel Range fetches shrink the full download to 61-215 s on that link but a 2-12 MB patch on one connection still wins.
* **Resume:** without Range support a drop at 50 % re-downloads the whole object: +10-30 s (B) / +130-480 s (C) / +300-1,100 s (D) for a 19-68 MB full download vs +2.5-6.5 / 10-81 / 27-186 s for the hdiffz patch; with Range the extra cost is one fresh-connection overhead (0.4-1.6 s).
* **A second HTTP request for the patch stops mattering (< 10 % overhead) above 13 MB (A), 6.4 MB (B), 0.8 MB (C), 0.5 MB (D)** and equals the patch's own transfer time only below 39-59 KB. Real patches are 2-12 MB: inlining the patch in the pointer object never pays; a keep-alive or HTTP/2 second request costs 1 RTT.
* **Not measured:** the BBR pass and profile D's 25 MB fetch (the netem run was killed by the delta harness's `pkill -f bsdiff`, section 9); single-threaded zstd on the > 200 MB pairs (by design).

## 0. Setup

Same machine and methodology as `benchmark-local.md`: 24-core Linux 6.17 x86-64, 91 GB RAM,
Go 1.26.4, zstd 1.5.7, xdelta3 3.2.0, bsdiff 4.3, HDiffPatch v5.1.3 (`bench/out/tools/HDiffPatch`),
curl, iproute2/tc + `sch_netem`, `ethtool`. Wall time = `/usr/bin/time -f "%e %M"`, min of 3
runs (1 run for inputs > 200 MB), RSS = max; every patch apply is `cmp`'d against NEW; every
command has a timeout < 15 min (840 s). Scripts: `bench/scale/`; corpus: `bench/out/corpus/`
(index in `bench/out/corpus/README.md`); logs: `bench/out/logs/07-*.log`; JSON:
`bench/out/results/{scale-small,scale-big,churn_scale,netem}.json`.

## 1. Corpus: adjacent official releases

`bench/scale/fetch_corpus.sh` downloads the official linux/amd64 release binaries (11 files,
2.3 GB, 16 s). Metadata from `go version -m`, `readelf -h/-S/-d` (`bench/scale/corpus_info.sh`):

| project | versions (old -> new) | size old / new | Go old / new | ELF | trimpath | cgo | symbols / DWARF | stripped copy (`strip --strip-all`) |
|---|---|---:|---|---|---|---|---|---:|
| prometheus | 3.13.1 -> 3.13.2 (patch) | 135,857,317 / 135,899,968 | go1.26.5 / go1.26.5 | EXEC | no | 0 | .symtab + 8 `.debug_*` (SHF_COMPRESSED) | 97,552,696 / 97,577,304 |
| prometheus | 3.13.2 -> 3.14.0 (**minor**) | 135,899,968 / 142,095,522 | go1.26.5 / **go1.26.6** | EXEC | no | 0 | same | 97,577,304 / 102,514,648 |
| kube-apiserver | 1.36.3 -> 1.36.4 (patch) | 88,387,746 / 88,436,898 | go1.26.5 / go1.26.5 | EXEC | yes | 0 | stripped (`-s -w`) | - |
| terraform | 1.15.8 -> 1.15.9 (patch) | 117,289,144 / 117,838,008 | go1.25.10 / go1.25.10 | EXEC | yes | 0 | stripped | - |
| cockroach | 26.2.4 -> 26.2.5 (patch) | 326,021,464 / 326,116,488 | go1.25.5 / go1.25.5 | EXEC | no buildinfo (Bazel) | **yes** (libc, libm, libresolv ... NEEDED) | .symtab + 13 `.debug_*` (compressed) | 230,243,816 / 230,309,064 |
| vault | 2.0.3 -> 2.0.4 (patch) | 536,903,029 / 537,754,780 | **go1.26.4 / go1.26.5** | EXEC | no | 0 | .symtab + 8 `.debug_*` (compressed) | 393,175,304 / 393,769,576 |

All are non-PIE `EXEC`; prometheus/vault carry `-X ...version` ldflags only (no `-s -w`);
kube-apiserver and terraform are the only vendors that ship stripped binaries. Two of the six
pairs cross a Go toolchain bump (vault 2.0.3->2.0.4 is a *patch* release built with a newer
Go; prometheus 3.14.0 as well) - a realistic worst case that is measured as-is.

Source-level size of each delta (`bench/scale/gh_compare.sh`, GitHub compare API; the API caps
the file list at 300, so the counts marked "diff" come from the unified diff):

| repo | tags | commits | files changed | +lines / -lines | .go files | notes |
|---|---|---:|---:|---:|---:|---|
| prometheus | v3.13.1..v3.13.2 | 11 | 13 | +136 / -36 | 4 | tiny patch release: 4 Go files |
| prometheus | v3.13.2..v3.14.0 | 359 | 265 | +11,929 / -2,151 (diff: +16,549 / -4,365) | 158 | minor release |
| kubernetes | v1.36.3..v1.36.4 | 18 | 320 (diff) | +22,070 / -83,210 (diff) | 234 | mostly a vendored-dependency removal |
| terraform | v1.15.8..v1.15.9 | 8 | 37 | +889 / -724 | 2 | 2 Go files + docs/tests |
| vault | v2.0.3..v2.0.4 | 200 | 413 (diff) | +21,102 / -2,368 (diff) | 208 | |
| cockroach | v26.2.4..v26.2.5 | 240 | 284 | +12,817 / -1,433 | 167 | |

## 2. Where do real releases change? (byte churn)

`bench/scale/churn_scale.py` reuses `bench/analyze_diff.py` (section-aligned comparison:
every section compared from its own start; no disassembly). "differing" therefore counts a
byte that merely moved as changed - it measures what a *fixed-offset* scheme (rsync-style
blocks, fixed chunks) sees; delta encoders that search for moved matches do far better
(section 3), but the unchanged-run coverage columns are exactly what CDC/chunk stores get.

| pair | old B | new B | differing | runs (byte-exact) | median run | largest unchanged | file in unchanged runs >= 64 KiB / >= 256 KiB / >= 1 MiB | .text | .rodata | .gopclntab | .debug_* |
|---|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|
| F2 v1->v2c (old synthetic, from benchmark-local) | 29,561,097 | 29,561,097 | 13.4 % | 964,933 | 3 B | 5,521,513 | 21.9 / 21.6 / - % | 3.1 % | 12.4 % | 29.1 % | - |
| F2 v1->v5 (new synthetic, section 5) | 29,561,097 | 29,561,097 | 70.0 % | 1,952,657 | 3 B | 413,546 | 3.9 / 2.8 / 0.0 % | 73.1 % | 49.8 % | 76.6 % | - |
| F2 v4->v5 | 29,561,097 | 29,561,097 | 70.2 % | 1,950,124 | 3 B | 413,546 | 3.9 / 2.8 / 0.0 % | 73.1 % | 49.7 % | 77.1 % | - |
| kube-apiserver 1.36.3->1.36.4 | 88,387,746 | 88,436,898 | 78.6 % | 5,766,047 | 3 B | 903,339 | 1.2 / 1.0 / 0.0 % | 82.2 % | 69.7 % | 78.8 % | - |
| prometheus 3.13.1->3.13.2 stripped | 97,552,696 | 97,577,304 | 84.3 % | 6,238,411 | 3 B | 334,055 | 0.6 / 0.3 / 0.0 % | 94.3 % | 73.4 % | 80.7 % | - |
| prometheus 3.13.2->3.14.0 stripped (minor) | 97,577,304 | 102,514,648 | 87.9 % | 5,728,508 | 3 B | 63,257 | 0.0 / 0.0 / 0.0 % | 95.6 % | 80.4 % | 83.3 % | - |
| terraform 1.15.8->1.15.9 | 117,289,144 | 117,838,008 | 86.8 % | 7,576,102 | 3 B | 66,194 | 0.1 / 0.0 / 0.0 % | 94.4 % | 69.4 % | 86.6 % | - |
| prometheus 3.13.1->3.13.2 (unstripped) | 135,857,317 | 135,899,968 | 86.4 % | 7,094,198 | 3 B | 334,055 | 0.6 / 0.2 / 0.0 % | 94.3 % | 73.4 % | 80.7 % | 99.5 % (24.6 MB) |
| cockroach 26.2.4->26.2.5 stripped | 230,243,816 | 230,309,064 | 79.7 % | 14,226,447 | 3 B | 2,440,673 | 1.1 / 1.1 / 1.1 % | 83.5 % | 64.8 % | 85.0 % | - |
| cockroach 26.2.4->26.2.5 (unstripped) | 326,021,464 | 326,116,488 | 82.3 % | 16,322,945 | 3 B | 2,440,673 | 1.8 / 1.7 / 1.4 % | 83.5 % | 64.8 % | 85.0 % | 98.9 % (57.9 MB) |
| vault 2.0.3->2.0.4 stripped | 393,175,304 | 393,769,576 | 85.4 % | 24,116,426 | 4 B | 701,409 | 0.2 / 0.2 / 0.0 % | 94.4 % | 74.1 % | 83.9 % | - |
| vault 2.0.3->2.0.4 (unstripped) | 536,903,029 | 537,754,780 | 86.9 % | 27,905,054 | 4 B | 701,409 | 0.1 / 0.1 / 0.0 % | 94.4 % | 74.1 % | 83.9 % | 99.6 % (77.7 MB) |

* Even the smallest real release (prometheus 3.13.2: 11 commits, 4 Go files, +136/-36
  lines) rewrites 84 % of the stripped binary in 6.2 M byte-runs of median 3 B: any function
  that grows shifts every later function in `.text` (94 %) and every rel32/`lea`
  displacement and pclntab offset behind it (`.gopclntab` 81 %, `.rodata` 73 %). Releases
  differ in *how much* is really new, not in how much moves: the median run is 3-4 B in
  every pair. `.typelink`/`.itablink` (32-bit offsets) move 30-72 %; `.noptrdata`/`.data`
  1-93 %; `.go.buildinfo` differs only in the version strings.
* The fraction of the file that survives in unchanged runs >= 64 KiB is 0-2 % in every real
  pair (21.6 % for the old one-line synthetic). This closes the CDC question from
  `benchmark-local.md` section 4: for real Go releases a chunk store re-uploads essentially
  the whole binary regardless of chunk size.
* Unstripped binaries add 24.6-77.7 MB of `.debug_*` that is 98.9-99.6 % rewritten (the Go
  linker zlib-compresses DWARF; a one-byte address shift changes the entire stream) plus a
  `.strtab` that is 88-94 % "moved". `.symtab` itself only changes 20-23 %.
* cockroach (cgo, Bazel) also carries `.dynsym`/`.rela.plt`/`.eh_frame` (0.1-0.3 MB, 5-10 %
  changed) - negligible.

## 3. Delta encoders on the corpus

`bench/scale/delta_scale.py` (same tool set and commands as `benchmark-local.md` section 3;
`zstd --patch-from` gets `--long=L, L = max(27, ceil(log2(size)))` and the matching
`--memory` on decode; `xdelta3 -B <filesize>`; per-command timeout 840 s). Pairs <= 200 MB:
min of 3 runs, 4 encoders in parallel (single-threaded ones), the `-T0`/`-p-8` runs
afterwards; pairs > 200 MB: 1 run, serial, and only bsdiff / hdiffz (both modes) /
`zstd -19 -T0` / xdelta3 / full `zstd -19 -T0` as specified. The `-T0`/`-p-8` phase of the
small suite overlapped the big suite, so those timings carry some contention noise (e.g.
`zstd -19 -T0` 42 s vs 33 s single-threaded on one pair). All 99 completed cases applied
byte-identically to NEW (`cmp`); 1 case timed out (vault unstripped bsdiff, see section 9).

### Patch size (bytes)

| pair | NEW size | bsdiff | hdiffz -m-6 zstd | hdiffz -m-6 -p-8 | hdiffz -s-64 (stream) | zstd -19 --patch-from (1T = T0) | xdelta3 -9 lzma | full zstd -19 (1T = T0) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| F2 v1->v5 | 29,561,097 | **470,031** | 572,895 | 573,285 | 3,445,605 | 1,758,395 | 2,721,791 | 8,433,714 |
| F2 v4->v5 | 29,561,097 | **470,743** | 578,505 | 579,001 | 3,445,613 | 1,755,495 | 2,723,865 | 8,433,714 |
| kube-apiserver 1.36.3->1.36.4 | 88,436,898 | **2,060,250** | 2,366,695 | 2,367,788 | 10,388,211 | 7,078,385 | 10,044,293 | 19,072,059 |
| prometheus 3.13.1->3.13.2 stripped | 97,577,304 | **2,714,204** | 2,732,978 | 2,730,887 | 11,390,472 | 8,279,163 | 11,256,241 | 23,808,116 |
| prometheus 3.13.2->3.14.0 stripped | 102,514,648 | 9,808,003 | **8,599,007** | 8,593,347 | 16,225,396 | 13,425,596 | 17,343,600 | 24,326,673 |
| terraform 1.15.8->1.15.9 | 117,838,008 | 5,427,575 | **4,918,933** | 4,913,545 | 14,826,573 | 11,320,635 | 15,065,548 | 25,547,524 |
| prometheus 3.13.1->3.13.2 | 135,899,968 | 28,108,743 | **27,879,860** | 27,879,870 | 36,544,395 | 33,438,259 | 37,570,665 | 49,598,993 |
| prometheus 3.13.2->3.14.0 | 142,095,522 | 36,068,700 | **34,677,348** | 34,665,529 | 42,108,809 | 39,287,096 | 42,730,327 | 50,819,527 |
| cockroach 26.2.4->26.2.5 stripped | 230,309,064 | **8,588,309** | 9,455,199 | 9,457,347 | 29,454,522 | 23,782,446 | 29,435,656 | 58,377,272 |
| cockroach 26.2.4->26.2.5 | 326,116,488 | **67,624,518** | 67,792,784 | 67,827,098 | 88,406,171 | 82,710,339 | 86,462,021 | 119,699,162 |
| vault 2.0.3->2.0.4 stripped | 393,769,576 | 12,108,972 | **11,752,681** | 11,787,185 | 40,451,947 | 36,507,981 | 44,822,809 | 67,677,354 |
| vault 2.0.3->2.0.4 | 537,754,780 | timeout | **91,132,633** | 91,148,620 | 119,765,773 | 115,826,792 | 126,749,351 | 149,329,950 |

Patch as % of the full `zstd -19` download (the number that matters for the transfer model):

| pair | full B | bsdiff | hdiffz -m-6 | hdiffz -s-64 | zstd -19 | xdelta3 |
|---|---:|---:|---:|---:|---:|---:|
| F2 v1->v5 | 8,433,714 | 5.6 % | 6.8 % | 40.9 % | 20.8 % | 32.3 % |
| kube-apiserver 1.36.3->1.36.4 | 19,072,059 | 10.8 % | 12.4 % | 54.5 % | 37.1 % | 52.7 % |
| prometheus 3.13.1->3.13.2 stripped | 23,808,116 | 11.4 % | 11.5 % | 47.8 % | 34.8 % | 47.3 % |
| prometheus 3.13.2->3.14.0 stripped (minor) | 24,326,673 | 40.3 % | 35.3 % | 66.7 % | 55.2 % | 71.3 % |
| terraform 1.15.8->1.15.9 | 25,547,524 | 21.2 % | 19.3 % | 58.0 % | 44.3 % | 59.0 % |
| cockroach 26.2.4->26.2.5 stripped | 58,377,272 | 14.7 % | 16.2 % | 50.5 % | 40.7 % | 50.4 % |
| vault 2.0.3->2.0.4 stripped (Go 1.26.4 -> 1.26.5) | 67,677,354 | 17.9 % | 17.4 % | 59.8 % | 53.9 % | 66.2 % |
| prometheus 3.13.1->3.13.2 unstripped | 49,598,993 | 56.7 % | 56.2 % | 73.7 % | 67.4 % | 75.7 % |
| prometheus 3.13.2->3.14.0 unstripped | 50,819,527 | 71.0 % | 68.2 % | 82.9 % | 77.3 % | 84.1 % |
| cockroach 26.2.4->26.2.5 unstripped | 119,699,162 | 56.5 % | 56.6 % | 73.9 % | 69.1 % | 72.2 % |
| vault 2.0.3->2.0.4 unstripped | 149,329,950 | - | 61.0 % | 80.2 % | 77.6 % | 84.9 % |

* Real patch releases of *stripped* binaries cost 11-21 % of a full download with
  bsdiff/hdiffz (5-9x saving), not the 0.7 % (140x) of the old one-line synthetic; a minor
  release costs 35-40 % (2.5-2.8x). The vault pair, which crosses a Go toolchain bump
  (1.26.4 -> 1.26.5), is not worse than the same-toolchain pairs (17 %).
* bsdiff and hdiffz -m-6 are within +-12 % of each other on every pair (each wins 5 of 10);
  `zstd --patch-from` is 2.3-3.4x larger than hdiffz on stripped binaries (it was 2.8x on
  v2c), xdelta3 3.1-4.4x, hdiffz -s-64 (streaming) 3.0-4.4x - the streaming mode's penalty
  grows from 12x-but-tiny on v2c to "half a full download" on real releases.
* Unstripped release binaries (prometheus, vault and cockroach ship them this way): the best
  patch is 56-71 % of the full download; stripping prometheus turns a 27.9 MB patch into
  2.7 MB (10x). A delta scheme must ship stripped binaries (or strip `.debug_*`/`.symtab`
  itself and treat them as a separate, non-delta object).

### Encode / apply time and memory

Compact view: patch MB / encode s / encoder peak RSS MB (min wall of 3 runs, 1 run above 200 MB; full rows incl. apply time/RSS in `bench/out/logs/07-render-scale.log`):

| pair (NEW MB) | bsdiff | hdiffz -m-6 | hdiffz -m-6 -p-8 | hdiffz -s-64 | zstd -19 -T0 | xdelta3 -9 lzma | full zstd -19 -T0 |
|---|---|---|---|---|---|---|---|
| F2 v1->v5 (30) | 0.47 / 10 s / 255 | 0.57 / 3 s / 126 | 0.57 / 2 s / 129 | 3.45 / 3 s / 211 | 1.76 / 15 s / 160 | 2.72 / 3 s / 316 | 8.43 / 8 s / 121 |
| kube-apiserver (88) | 2.06 / 50 s / 760 | 2.37 / 15 s / 373 | 2.37 / 7 s / 378 | 10.39 / 13 s / 218 | 7.08 / 29 s / 409 | 10.04 / 10 s / 757 | 19.07 / 11 s / 349 |
| prometheus patch, stripped (98) | 2.71 / 54 s / 839 | 2.73 / 16 s / 392 | 2.73 / 7 s / 398 | 11.39 / 15 s / 217 | 8.28 / 32 s / 428 | 11.26 / 10 s / 766 | 23.81 / 12 s / 363 |
| prometheus minor, stripped (103) | 9.81 / 81 s / 839 | 8.60 / 20 s / 428 | 8.59 / 11 s / 440 | 16.23 / 17 s / 218 | 13.43 / 42 s / 438 | 17.34 / 13 s / 770 | 24.33 / 12 s / 450 |
| terraform (118) | 5.43 / 85 s / 1008 | 4.92 / 20 s / 502 | 4.91 / 10 s / 510 | 14.83 / 19 s / 218 | 11.32 / 32 s / 471 | 15.07 / 15 s / 787 | 25.55 / 11 s / 465 |
| prometheus patch, unstripped (136) | 28.11 / 160 s / 1167 | 27.88 / 35 s / 435 | 27.88 / 18 s / 442 | 36.54 / 21 s / 217 | 33.44 / 33 s / 670 | 37.57 / 18 s / 1342 | 49.60 / 11 s / 585 |
| prometheus minor, unstripped (142) | 36.07 / 193 s / 1168 | 34.68 / 39 s / 479 | 34.67 / 17 s / 491 | 42.11 / 22 s / 218 | 39.29 / 31 s / 682 | 42.73 / 17 s / 1339 | 50.82 / 11 s / 592 |
| cockroach stripped (230) | 8.59 / 150 s / 1977 | 9.46 / 43 s / 901 | 9.46 / 26 s / 909 | 29.45 / 37 s / 221 | 23.78 / 40 s / 927 | 29.44 / 40 s / 1407 | 58.38 / 18 s / 845 |
| cockroach unstripped (326) | 67.62 / 447 s / 2799 | 67.79 / 103 s / 1007 | 67.83 / 30 s / 1018 | 88.41 / 53 s / 221 | 82.71 / 47 s / 1375 | 86.46 / 48 s / 2547 | 119.70 / 18 s / 1237 |
| vault stripped (394) | 12.11 / 267 s / 3376 | 11.75 / 70 s / 1529 | 11.79 / 36 s / 1538 | 40.45 / 71 s / 225 | 36.51 / 36 s / 1548 | 44.82 / 55 s / 2587 | 67.68 / 18 s / 1414 |
| vault unstripped (538) | timeout 840 s | 91.13 / 161 s / 1696 | 91.15 / 46 s / 1709 | 119.77 / 83 s / 224 | 115.83 / 38 s / 2398 | 126.75 / 58 s / 4797 | 149.33 / 23 s / 1595 |

Apply (patch) side, largest inputs: hpatchz 0.2-0.45 s at a flat 27 MB RSS for every size;
bspatch 1.2-3.7 s at 1.9 B RAM per OLD byte (762 MB at 393 MB); `zstd -d --patch-from`
0.3-0.7 s at 1.9 B/B (1.03 GB at 537 MB - OLD is the dictionary and must be resident);
xdelta3 1.0-2.2 s at 1.1 B/B. Single-thread `zstd -19 --patch-from` (only run <= 142 MB)
takes 30-41 s there, the same as `-T0`: with `--patch-from` zstd's multithreading buys
nothing until inputs are far larger than the job split.

## 4. Scaling and the 1 GB extrapolation

Encode throughput = NEW MB / encode s; RSS ratio = encoder peak RSS / OLD size; the fit is
least-squares in log-log space over all measured pairs (11-12 per tool, 30-537 MB).

| tool | pairs | encode MB/s min - median - max | RSS/input byte min - median - max | apply MB/s (median) | fit t = a * n^b: b | R^2 |
|---|---:|---|---|---:|---:|---:|
| bsdiff | 11 | 0.7 - 1.5 - 3.1 | 9.00 - 9.01 - 9.04 | 164 | 1.43 | 0.92 |
| hdiffz -m-6 | 12 | 3.2 - 5.6 - 9.9 | 3.24 - 4.22 - 4.60 | 1406 | 1.33 | 0.96 |
| hdiffz -m-6 -p-8 | 12 | 7.7 - 11.7 - 19.7 | 3.27 - 4.27 - 4.73 | 1536 | 1.19 | 0.96 |
| hdiffz -s-64 | 12 | 5.6 - 6.4 - 9.4 | 0.44 - 1.95 - 7.50 (flat ~220 MB) | 853 | 1.14 | 0.99 |
| zstd -19 (1T) | 8 | 1.6 - 3.1 - 4.6 | 4.21 - 5.17 - 5.67 | 693 | 0.44 (*) | 0.87 |
| zstd -19 -T0 | 12 | 2.0 - 4.1 - 14.3 | 4.13 - 4.71 - 5.68 | 710 | 0.35 (*) | 0.70 |
| xdelta3 -9 lzma -B size | 12 | 5.8 - 8.5 - 10.5 | 6.41 - 8.98 - 11.19 | 233 | 1.09 | 0.98 |
| full zstd -19 (1T) | 8 | 3.7 - 9.3 - 11.9 | 3.89 - 4.15 - 4.83 | 1220 | 0.30 (*) | 0.94 |
| full zstd -19 -T0 | 12 | 3.8 - 12.0 - 23.3 | 3.12 - 4.15 - 4.83 | 1309 | 0.39 (*) | 0.93 |

(*) zstd's sub-linear exponents are an artefact of the 29.6 MB synthetic pair, where
`zstd -19` is anomalously slow per byte (15-18 s = 1.7 MB/s; its match finder spends its
budget on the 70 % of shifted bytes), and of `-T0` scaling with core count at large sizes;
from 88 MB up zstd is ~linear at 3-4.6 MB/s single-threaded. bsdiff's 1.43 and hdiffz's
1.19-1.33 are real super-linear behaviour (suffix sort / match search cache misses): bsdiff
drops from 3.1 MB/s at 30 MB to 0.7 MB/s at 326 MB.

Extrapolation to a 1 GB binary (two estimators: linear from the largest measured input,
and the log-log fit; RSS linear in size from the largest input). These are extrapolations,
not measurements - the 537 MB vault unstripped bsdiff already exceeded the 840 s limit.

| tool | largest measured (OLD MB) | enc s | enc RSS MB | 1 GB encode s (linear / fit) | 1 GB encode RSS | 1 GB apply RSS |
|---|---|---:|---:|---:|---:|---:|
| bsdiff | vault stripped (393) | 267 | 3,376 | 680 / 1,785 (11-30 min) | 9.0 GB | 2.0 GB |
| hdiffz -m-6 | vault (537) | 161 | 1,696 | 300 / 371 | 3.3 GB (4.2 GB at the median ratio) | 27 MB |
| hdiffz -m-6 -p-8 | vault (537) | 46 | 1,709 | 86 / 130 | 3.3 GB | 27 MB |
| hdiffz -s-64 | vault (537) | 83 | 224 | 155 / 200 | ~0.25-0.4 GB (flat) | 28 MB |
| zstd -19 -T0 --long=30 | vault (537) | 38 | 2,398 | 70 (linear; fit unreliable) | 4.7 GB | 2.0 GB (`--memory` >= OLD) |
| zstd -19 single-thread | prometheus (136) | 31 | 682 | ~230 (linear at 4.4 MB/s) | 5.3 GB | 2.0 GB |
| xdelta3 -9 lzma -B 1G | vault (537) | 58 | 4,797 | 107 / 149 | 9.4 GB (bounded by `-B`; smaller -B = bigger patch) | 1.1 GB |
| full zstd -19 -T0 | vault (537) | 23 | 1,595 | 43 / 28 | 3.1 GB | 13 MB |

Practical reading: at 1 GB the only encoders that finish in "CI-step" time are
`hdiffz -m-6 -p-8` (1.5-2 min, ~3.3 GB), `zstd -19 -T0 --patch-from` (~1 min, ~4.7 GB, but a
2.5-3x larger patch) and `hdiffz -s-64` (~3 min, < 0.5 GB, 3-4x larger patch); bsdiff needs
9 GB and 10-30 min. On the *client*, hpatchz is the only applier whose memory does not
scale with the binary.

## 5. Synthetic multi-package release (v5)

`bench/gen.py v5` = v4 plus: three more edits in `handlers.go` (a second header, a slug in
the greeting, a new `fourthHandler`), a new route `/fourth` and a new JSON/XML-encoded
struct field `person.Email` in `main.go`, a new package `internal/fourth` (uses
`encoding/base64`, `hash/crc32`, `sort`; called from the new handler), a rewritten
`util.Repeat` plus new `util.Slug`, a new `third.Tag` (pulls `fmt` into package third), and
two string constants changed to different lengths (`greeting` +6 B, third's prefix +4 B).
Built as F2 (`-trimpath -ldflags="-s -w"`, plus `-buildvcs=false` - see section 9) it is
exactly 29,561,097 B like v1..v4.

| pair | differing bytes | bsdiff | hdiffz -m-6 | hdiffz -s-64 | zstd -19 | xdelta3 | full zstd -19 |
|---|---:|---:|---:|---:|---:|---:|---:|
| v1->v2c (one line, from benchmark-local) | 13.4 % | 60,478 | 75,305 | 910,116 | 213,124 | 343,972 | 8,432,157 |
| v1->v4 (one line + one func + one line) | ~25 % | 66,541 | 80,796 | 922,457 | 228,024 | 357,316 | 8,432,105 |
| **v1->v5** (multi-package) | 70.0 % | **470,031** (7.8x v2c) | 572,895 (7.6x) | 3,445,605 (3.8x) | 1,758,395 (8.3x) | 2,721,791 (7.9x) | 8,433,714 |
| **v4->v5** | 70.2 % | 470,743 | 578,505 | 3,445,613 | 1,755,495 | 2,723,865 | 8,433,714 |

Encode: bsdiff 9.5 s, hdiffz 3.1 s (1.5 s `-p-8`), zstd 18.5 s (14.9 s `-T0`), xdelta3 3.4 s.
v4->v5 costs the same as v1->v5: once a release touches several packages the accumulated
history behind it is irrelevant, and the patch is 5.6-6.8 % of a full download rather than
0.7 % - the same order as the real kube/prometheus patch releases (11-12 %). The remaining
gap to the real releases is that v5 touches 4 tiny packages of the bench module while a
real patch release also bumps vendored dependencies (kube 1.36.4: 320 files).

## 6. Real WAN: netem measurements

`bench/scale/netem.py`: namespaces `bs-srv`/`bs-cli` on a veth pair, `netem delay RTT/2 rate R
[loss p] limit max(1000, 2 BDP)` on **both** ends, TSO/GSO/GRO off, Go static server
(`bench/scale/netsrv`, Range + ETag/If-Modified-Since) in the server namespace, curl in the
client namespace, plain HTTP, cubic (host default). Objects: the 60,478 B bsdiff patch of
v1->v2c, the 1,758,395 B `zstd -19 --patch-from` patch of v1->v5, the 8,432,157 B `zstd -19`
full v2c binary, and the 25,547,524 B `zstd -19` terraform 1.15.9 binary. Single-connection
GET and (objects >= 4 MB) 4-way / 8-way parallel `-r` range fetches (one process each,
concatenation `cmp`'d), 3 runs each, median; a measurement over 150 s was not repeated
(n=1). `--max-time 600`. Conditional GET = `curl -z <file>` -> 304.

| profile | link | object | single s (n) | Mbit/s | 4-way s | 8-way s | 8-way Mbit/s | connect / TTFB / 304 ms |
|---|---|---|---:|---:|---:|---:|---:|---|
| A | 100 Mbit, 100 ms, 0 % | 60 KB | 0.40 (3) | 1.20 | | | | 100 / 201 / 200 |
| A | | 1.76 MB | 1.13 (3) | 12.5 | | | | |
| A | | 8.43 MB | 1.76 (3) | 38.3 | 1.43 | 1.26 | 53.7 | |
| A | | 25.5 MB | 3.19 (3) | 64.0 | 2.97 | 2.87 | 71.2 | |
| B | 20 Mbit, 200 ms, 0 % | 60 KB | 0.81 (3) | 0.60 | | | | 200 / 401 / 401 |
| B | | 1.76 MB | 2.34 (3) | 6.0 | | | | |
| B | | 8.43 MB | 5.14 (3) | 13.1 | 5.53 | 4.28 | 15.8 | |
| B | | 25.5 MB | 12.29 (3) | 16.6 | 11.63 | 11.38 | 18.0 | |
| C | 20 Mbit, 200 ms, 1 % each way | 60 KB | 1.01 (3) | 0.48 | | | | 200 / 401 / 401 |
| C | | 1.76 MB | 8.06 (3) | 1.75 | | | | |
| C | | 8.43 MB | 57.24 (3) | 1.18 | 14.83 | 7.41 | 9.10 | |
| C | | 25.5 MB | 179.9 (1) | 1.14 | 47.90 | 25.64 | 7.97 | |
| D | 5 Mbit, 300 ms, 2 % each way | 60 KB | 1.52 (3) | 0.32 | | | | 300 / 602 / 602 |
| D | | 1.76 MB | 22.41 (3) | 0.63 | | | | |
| D | | 8.43 MB | 131.5 (3) | 0.51 | 40.60 | 26.81 | 2.52 | |
| D | | 25.5 MB | not measured | | not measured | not measured | | |

* Latency structure (no loss): a fresh-connection GET of a small object costs 2 RTT before
  the first byte (connect + request) and slow start adds ~1 RTT per doubling from 10 MSS:
  60 KB = 4 RTT, 1.76 MB = 11 RTT at 100 ms (1.13 s), 25.5 MB reaches only 64 % of a 100 Mbit
  link because slow start covers most of a 3 s transfer. The 304 poll is exactly 2 RTT on a
  fresh connection (1 RTT on keep-alive); TLS 1.3 would add 1 RTT to each fresh connection.
* Parallel range fetches without loss buy 10-20 % (they only shorten slow start; the link is
  the bottleneck).
* With loss, cubic's single-connection throughput collapses to 1.12-1.18 Mbit/s on the 20 Mbit
  link and 0.49-0.51 Mbit/s on the 5 Mbit link, independent of object size beyond ~1 MB.
  Mathis (`MSS/RTT * 1.22/sqrt(p)`, MSS 1448): 0.71 Mbit/s for C, 0.33 Mbit/s for D - the
  measurements are 1.5-1.6x above the Reno bound (cubic's faster growth) but the model's
  *shape* is right: rate ~ 1/(RTT sqrt p), not the link rate. N parallel connections scale
  almost linearly (8-way: 7.0-7.7x on C, 4.9x on D, where 8 flows already need 2.5 of the
  5 Mbit). Under these profiles the link is < 10 % utilised by one flow; anything the client
  can do to fetch in parallel (Range on one object, or several objects) is worth 5-8x, and a
  BBR sender would change this picture entirely (not measured, section 9).
* A 25 MB full download on profile C takes 180 s single-connection (26 s 8-way); on D the
  8.4 MB object alone took 132 s, so the 25 MB single fetch would have needed ~500 s (model).

## 7. Updated transfer model

`bench/scale/model_scale.py`: `transfer(bytes)` is a piecewise-linear interpolation through
the **measured** single-connection medians of section 6 (0 B -> connect+TTFB, 60 KB, 1.76 MB,
8.4 MB, 25.5 MB) per profile, extrapolated beyond the largest measured object at the tail
rate (marked *; profile D's largest measured object is 8.4 MB, so its 25 MB+ cells are
model). `total = transfer + encode + apply`; encode is the measured single-thread time
except for `-p-8`/`-T0` rows. 8-way = parallel Range fetch of one object (measured 8-way
curve; proportional for D). Full table (6 pairs x 6 methods): `bench/out/logs/07-model.log`.

| profile | overhead s (fresh conn) | 304 s | t(64 KB) | t(1 MB) | t(8 MB) | t(30 MB) | t(100 MB) | tail Mbit/s | 8-way t(30 MB) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| A 100 Mbit/100 ms/0 % | 0.20 | 0.20 | 0.4 | 0.8 | 1.8 | 3.7* | 9.8* | 95.5 | 3.4 |
| B 20 Mbit/200 ms/0 % | 0.40 | 0.40 | 0.8 | 1.7 | 5.1 | 14.8* | 45.5* | 19.1 | 13.8 |
| C 20 Mbit/200 ms/1 % | 0.40 | 0.40 | 1.0 | 5.1 | 56.9 | 222* | 748* | 1.12 | 31.9 |
| D 5 Mbit/300 ms/2 % | 0.60 | 0.60 | 1.6 | 13.7 | 131 | 508* | 1,708* | 0.49 | 100* |

Transfer / total seconds (total includes encode + apply); "8-way" = transfer only:

| pair | method | patch B | enc s | A xfer / total | B xfer / total | C xfer / total | D xfer / total | D 8-way xfer |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| kube-apiserver 1.36.3->1.36.4 | full download (zstd -19 -T0) | 19,072,059 | 11.0 | 2.7 / 13.7 | 9.6 / 20.6 | 133 / 145 | 306* / 317* | 61* |
| | hdiffz -m-6 -p-8 | 2,367,788 | 7.0 | 1.2 / 8.3 | 2.6 / 9.7 | 12.6 / 19.6 | 32.4 / 39.5 | 32.4 |
| | bsdiff | 2,060,250 | 49.6 | 1.2 / 51.2 | 2.5 / 52.6 | 10.3 / 60.4 | 27.3 / 77.4 | 27.3 |
| | zstd -19 -T0 --patch-from | 7,078,385 | 28.6 | 1.6 / 30.3 | 4.6 / 33.2 | 47.3 / 75.9 | 109 / 138 | 22.5 |
| prometheus 3.13.1->3.13.2 stripped | full download | 23,808,116 | 12.4 | 3.0 / 15.5 | 11.6 / 24.0 | 167 / 180 | 383* / 395* | 76* |
| | hdiffz -m-6 -p-8 | 2,730,887 | 7.5 | 1.2 / 8.8 | 2.8 / 10.3 | 15.2 / 22.8 | 38.3 / 45.9 | 38.3 |
| | bsdiff | 2,714,204 | 54.5 | 1.2 / 56.2 | 2.7 / 57.7 | 15.1 / 70.1 | 38.0 / 93.0 | 38.0 |
| prometheus 3.13.2->3.14.0 stripped (minor) | full download | 24,326,673 | 11.9 | 3.1 / 15.1 | 11.8 / 23.8 | 171 / 183 | 391* / 404* | 77* |
| | hdiffz -m-6 -p-8 | 8,593,347 | 10.7 | 1.8 / 12.6 | 5.2 / 16.0 | 58.4 / 69.2 | 134* / 145* | 27.3 |
| | bsdiff | 9,808,003 | 80.9 | 1.9 / 83.8 | 5.7 / 87.7 | 67.1 / 149 | 154* / 236* | 31.2 |
| cockroach 26.2.4->26.2.5 stripped | full download | 58,377,272 | 17.6 | 5.9* / 23.7 | 26.0* / 43.8 | 415* / 433* | 948* / 966* | 186* |
| | hdiffz -m-6 -p-8 | 9,457,347 | 26.0 | 1.8 / 28.0 | 5.6 / 31.7 | 64.6 / 90.7 | 148* / 174* | 30.1 |
| | bsdiff | 8,588,309 | 150.2 | 1.8 / 153 | 5.2 / 157 | 58.4 / 210 | 134* / 286* | 27.3 |
| vault 2.0.3->2.0.4 stripped | full download | 67,677,354 | 18.4 | 6.7* / 25.3 | 29.9* / 48.5 | 482* / 500* | 1,100* / 1,119* | 215* |
| | hdiffz -m-6 -p-8 | 11,787,185 | 36.0 | 2.0 / 38.3 | 6.5 / 42.8 | 81.3 / 118 | 186* / 223* | 37.5 |
| | bsdiff | 12,108,972 | 267.2 | 2.1 / 271 | 6.7 / 276 | 83.6 / 353 | 192* / 461* | 38.5 |
| F2 v1->v5 | full download | 8,433,714 | 7.7 | 1.8 / 9.5 | 5.1 / 12.9 | 57.2 / 65.0 | 132 / 139 | 26.8 |
| | hdiffz -m-6 -p-8 | 573,285 | 1.5 | 0.6 / 2.1 | 1.3 / 2.8 | 3.1 / 4.6 | 7.8 / 9.3 | 7.8 |
| | bsdiff | 470,031 | 9.5 | 0.6 / 10.3 | 1.2 / 10.9 | 2.7 / 12.4 | 6.6 / 16.3 | 6.6 |
| | zstd -19 -T0 --patch-from | 1,758,395 | 14.9 | 1.1 / 16.1 | 2.3 / 17.3 | 8.1 / 23.0 | 22.4 / 37.4 | 22.4 |

* On A and B every option is encode-dominated: hdiffz -p-8 is the fastest end-to-end on
  every pair (8-43 s) and the full download is second (14-49 s); bsdiff's 50-270 s encode
  makes it the slowest even though its patch is the smallest.
* On C and D the patch size dominates: the full download of a 19-68 MB object takes
  133-482 s (C) and 306-1,100 s (D, model) on one connection; the 2-12 MB hdiffz patch
  13-81 s / 32-186 s. An 8-way parallel full download (61-215 s on D) still loses to the
  single-connection patch, and a parallel *patch* fetch (27-38 s on D for 2-12 MB) is the
  best case overall.
* Encode time can be amortised (one encode per release serves every client); the transfer
  numbers cannot. With that lens, on D the ordering is bsdiff (smallest) ~ hdiffz > zstd
  (2.5-3x more bytes) > full, and the 1 GB extrapolation says only hdiffz/zstd encoders stay
  in the minutes range.

Resume cost (connection drops at 50 % of the object; extra seconds vs. no drop):

| object | no Range: bytes re-fetched | A / B / C / D extra s without Range | with Range (0 wasted bytes) |
|---|---:|---|---|
| kube full download 19.1 MB | 19,072,059 | 2.7 / 9.6 / 133 / 306 | 1.1 / 1.6 / ~0 / ~0 (+1 fresh-connection overhead 0.2-0.6 s) |
| kube hdiffz patch 2.37 MB | 2,367,788 | 1.2 / 2.6 / 12.6 / 32.4 | 0.6 / 1.1 / ~0 / ~0 |
| vault stripped full download 67.7 MB | 67,677,354 | 6.7 / 29.9 / 482 / 1,100 | 1.1 / 1.6 / ~0 / ~0 |
| vault stripped hdiffz patch 11.8 MB | 11,787,185 | 2.0 / 6.5 / 81 / 186 | 1.0 / 1.6 / ~0 / ~0 |

Without Range the resume cost is a whole second transfer; with Range it is the
fresh-connection overhead plus a second slow start (~1-1.6 s on A/B; on the lossy profiles
the stream never leaves the loss-limited regime, so restarting costs nothing measurable).

When does a second HTTP request (patch as its own object instead of inline in the pointer)
stop mattering? Extra request cost on a fresh connection = measured connect+TTFB:

| profile | extra request s (plain HTTP) | + TLS 1.3 | patch size where transfer = 1x the extra request | where the extra request is < 10 % | 304 poll s |
|---|---:|---:|---:|---:|---:|
| A | 0.20 | 0.30 | 59 KB | 13.1 MB | 0.20 |
| B | 0.40 | 0.60 | 58 KB | 6.4 MB | 0.40 |
| C | 0.40 | 0.60 | 39 KB | 0.8 MB | 0.40 |
| D | 0.60 | 0.90 | 39 KB | 0.5 MB | 0.60 |

With keep-alive/HTTP/2 the extra request is 1 RTT (0.1-0.3 s) instead. Real-release patches
(2-12 MB) are 40-200x above the 1x threshold on every profile: a separate patch object costs
nothing noticeable; only the old sub-100 KB one-line patches would have benefited from
inlining.

## 8. What changed vs the earlier assumptions

| assumption in `benchmark-local.md` | what the corpus / netem show |
|---|---|
| A release changes ~13 % of the bytes (one line, v2c); CDC keeps 22 % of the file in >= 64 KiB runs | 79-87 % of bytes move; 0-2 % survives in >= 64 KiB runs; even the smallest real release (4 Go files) behaves like this |
| Patches are 60-75 KB = 0.7 % of a full download (140x saving) | 2-12 MB = 11-21 % (5-9x) for patch releases, 35-40 % for a minor release; the synthetic v5 is 5.6-6.8 % |
| Encode is 1-6 s; bsdiff is viable | At 100-140 MB bsdiff takes 50-190 s and 0.8-1.2 GB; at 393 MB 267 s / 3.4 GB; at 537 MB it exceeds 14 min. hdiffz -p-8 is 7-46 s across the range |
| zstd --patch-from is a reasonable pure-Go-friendly fallback (2.8x bsdiff) | Still 2.3-3.4x hdiffz/bsdiff, but now that means 5-25 MB more per update; it also needs OLD resident on the client (1.9 B/B) |
| Unstripped binaries are a corner case | Three of five vendors ship unstripped; their deltas are 56-71 % of a full download. Stripping must be part of the pipeline |
| Transfer = bytes/bandwidth + RTT; 100 Mbit/100 ms and 20 Mbit/200 ms without loss | A fresh-connection small GET is 4 RTT and a 304 is 2 RTT; with 1-2 % loss a cubic connection gets 1.1 / 0.5 Mbit/s regardless of the 20 / 5 Mbit link (1.5-1.6x the Mathis bound), and 8 parallel ranges recover 5-8x |
| The patch can be inlined in the pointer object to save a round trip | The second request is < 10 % overhead above 0.5-13 MB; real patches are 2-12 MB, so it does not matter |
| Apply memory is negligible | hpatchz: 27 MB flat; bspatch / zstd -d --patch-from: 1.9x OLD (0.8-1 GB for vault/cockroach) |

## 9. Failures, skips and caveats

* **vault 2.0.3->2.0.4 unstripped bsdiff (537 MB) timed out at 840 s** (it needed ~9 GB and,
  extrapolating cockroach's 447 s at 326 MB with exponent 1.43, ~900 s). Not retried; the
  stripped pair (393 MB) completed in 267 s.
* Single-threaded `zstd -19 --patch-from` and `zstd -19` were not run on the > 200 MB pairs
  (only `-T0`), as specified; their single-thread rate is ~linear at 3-4.6 MB/s from 88 MB up.
* The `v1..v4` bench binaries had been built before `bench/` was under git; `go build` now
  stamps `vcs.revision/vcs.time/vcs.modified` into `.go.buildinfo` (+128 B in `.rodata`),
  which shifted everything and broke bit-reproducibility against the stored files.
  `bench/scale/build_v5.sh` therefore builds with `-buildvcs=false`, which reproduces the
  stored v4-F2 byte-for-byte (verified) and was used for v5.
* cockroach is a Bazel build without Go buildinfo (`go version -m` gives only `go1.25.5`):
  CGO is inferred from the dynamic `NEEDED` libc entries.
* The GitHub compare API caps file lists at 300; kube and vault counts come from the unified
  diff (marked). Nothing was cloned.
* `-T0`/`-p-8` timings from the small suite overlapped the big suite's single-threaded runs
  (up to ~2 cores busy); treat them as +-20 %.
* **The netem run was killed at 1,541 s (profile D, before the 25 MB object) by the delta
  harness**: on the vault bsdiff timeout it ran `pkill -f bsdiff`, which also matched
  netem.py's own command line (`--obj bsdiff-60k=...`). Profile D's 60 KB / 1.76 MB / 8.4 MB
  results were recovered from the log (`from_log` records in `netem.json`); profile D's
  25 MB object and the whole **BBR pass were not measured**. The harness now kills the timed-out
  process group instead (`os.killpg`). Namespaces were removed manually afterwards.
* netem: the same `loss p%` is applied on both veth ends, so ACKs are lost with the same
  probability as data. Queue `limit` = max(1000, 2 x BDP) packets. TSO/GSO/GRO were
  disabled on both ends so netem sees real 1500-B packets (with offloads on, one "loss"
  drops a 64 KB super-packet). Plain HTTP; TLS 1.3 would add 1 RTT to every fresh
  connection (TLS 1.2: 2 RTT). Client and server are on the same kernel, so this measures
  TCP congestion-control behaviour, not a real Internet path.

## 10. Naive comparisons on the Go 1.27 pairs

Whole-file compression, a content-defined-chunk store and the exact-match delta
coders, measured on the two Go 1.27 pairs the Go-aware codec is headlined on
(`go-aware-transform.md` §10), so the whole ladder is on one toolchain. Single
runs, one thread unless noted (`nice -n 19`, `/usr/bin/time`). The chunk-store
figure is the bytes *added* to a `desync` store (zstd-compressed chunks) by
indexing the new binary after the old one — what a chunk-based fetch would
transfer.

| Technique | one-line change, 30 MB (v1-F2 → v2c-F2) | prometheus 3.13.1 → 3.13.2, 94 MB |
|---|---:|---:|
| whole file, `xz -9` | 7,917,680 (6.5 s, 331 MB) | 18,441,736 (20.9 s, 683 MB) |
| whole file, `zstd -19` | 8,663,959 (6.0 s, 124 MB) | 20,579,646 (21.1 s, 191 MB; 9.4 s with `-T0`) |
| chunk store, `desync` 16:64:256 KiB | 8,023,962 (93 % of the archive) | 25,703,807 (125 %) |
| chunk store, `desync` 4:16:64 KiB | 8,204,624 | 25,899,127 |
| `xdelta3 -e -9 -S none -B 128MiB` | 1,925,369 (1.5 s, 240 MB) | 15,157,924 (7.6 s, 697 MB) |
| `zstd -19 --long=27 --patch-from` | 538,493 (12.2 s, 163 MB) | 8,479,550 (27.0 s, 343 MB) |
| `hdiffz -m-6 -SD -d -p-8 -c-zstd-21-24` | 176,929 (0.7 s, 106 MB) | 2,719,152 (7.4 s, 388 MB) |
| `bsdiff` | 150,475 (5.4 s, 265 MB) | 2,691,644 (39.3 s, 825 MB) |
| Go-aware codec (`go-aware-transform.md` §11) | 2,207 (0.7 s, 286 MB) | 111,552 (2.1 s, 654 MB) |

Apply cost on the prometheus pair: `zstd -d` 0.07 s / 13 MB, `zstd --patch-from`
decode 0.12 s / 187 MB, `xdelta3 -d` 0.19 s / 111 MB, `bspatch` 0.47 s / 192 MB,
`hpatchz` 0.08 s / 25 MB, Go-aware decoder 1.0 s / 636 MB.

Two things differ from the Go 1.26 measurements in `benchmark-local.md`. The
exact-match coders are much worse on 1.27 (one-liner: `--patch-from` 213 KB →
538 KB, xdelta3 344 KB → 1.9 MB) because the new sorted `.go.type` section is
dense with code offsets that all shift; the approximate matchers grow 2.5× for
the same reason. And the chunk store on the 94 MB pair re-sends *more* than a
fresh archive: no chunk survives, and 16–256 KiB chunks compress worse alone
than one 94 MB stream. Logs: `bench/out/logs/09-naive-prometheus127.log`,
`09-naive-oneliner127.log`.

## Reproduce

```
bench/scale/fetch_corpus.sh                    # 11 binaries -> bench/out/corpus/ (2.3 GB, ~20 s)
bench/scale/corpus_info.sh                     # corpus/README.md, sections.txt, goversion.txt, stripped copies
bench/scale/gh_compare.sh                      # source-level delta sizes via gh api (07-gh-compare.log)
bench/scale/build_v5.sh                        # v5-F2 (+ v4-F2 round-trip check)
python3 bench/scale/churn_scale.py             # section 2 (results/churn_scale.json, 07-churn.log)
python3 bench/scale/delta_scale.py --small-only --workers 4 --out scale-small.json   # ~40 min
python3 bench/scale/delta_scale.py --big-only --out scale-big.json                   # ~50 min
python3 bench/scale/render_scale.py            # sections 3-4 tables (07-render-scale.log)
python3 bench/scale/netem.py --obj bsdiff-60k=... --obj patch-1m=... --obj full-8m=... --obj blob-25m=... --cc cubic,bbr   # ~70 min, needs sudo
python3 bench/scale/model_scale.py [--cc bbr]  # section 7
```

`bench/scale/netsrv/` is the Range/304-capable Go static server used inside the server
namespace (`bench/out/tools/netsrv`). Logs: `bench/out/logs/07-{fetch,corpus-info,gh-compare,build-v5,churn,delta-small,delta-big,render-scale,netem,model}.log`.

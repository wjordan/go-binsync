# Local benchmark: incremental updates of a large Go web-server binary

Measured on 2026-08-26 on a 24-core Linux x86-64 box (Linux 6.17, 91 GB RAM).
Everything here is reproducible with `bench/run.sh` (see the last section);
raw logs are in `bench/out/logs/`, JSON results in `bench/out/results/`.

## Key findings

* Binary: F2 (`-trimpath -ldflags="-s -w"`) is 29.56 MB (EXEC, .text 14.8 MB, .gopclntab 10.5 MB, .rodata 3.5 MB); F1 is 42.16 MB, F2pie 32.48 MB. Builds are bit-for-bit reproducible (F1 and F2). Full download at `zstd -19`: 8.43 MB (F2), 18.1 MB (F1); `xz -9`: 7.71 MB (F2).
* A single added statement (v1->v2c, F2) changes 3.97 MB = 13.4% of the bytes in 964,933 byte-exact runs (median 3 B): .gopclntab 3.07 MB (29% of the section, 858 K runs of shifted 32-bit offsets), .rodata 439 KB, .text 460 KB. Only 21.6% of the file survives in unchanged runs >= 256 KiB. Adding a function in another package (v2p) rewrites 58% of .gopclntab; v3 = 61%.
* In .text, 69-100% of the differing runs are 1-byte changes in the displacement of an unchanged `lea`/`mov` instruction (RIP-relative operand into .rodata shifted by +8 for v2c, +3 for v2l); only the ~370 KB of code after the grown function is actually shifted (+192 B). E8/E9 call targets barely change, so the x86 BCJ filter changes patch sizes by < 2% (no gain).
* Same-length string change (v2s) touches 103 bytes (5 in .rodata + build ID); a +3-byte string (v2l) touches nothing in .gopclntab but slides .rodata and rewrites 20,002 `lea` displacements.
* Unstripped F1 binaries are hopeless for deltas: the Go linker zlib-compresses DWARF (`SHF_COMPRESSED`), so v1->v2c rewrites 99.5% of `.debug_*` (~8.7 MB) and every encoder produces an 8.4-8.9 MB patch (vs 60-460 KB for F2). PIE (F2pie) behaves like F2 (+`.rela` 0.8% changed): bsdiff 63.8 KB, zstd 233 KB.
* Delta patch sizes, F2 v1->v2c: bsdiff 60,478 B (6.0 s encode, 255 MB); hdiffz -m-6 zstd 75,305 B (1.17 s, 80 MB; 0.47 s with -p-8); hdiffz lzma2 70,932 B; zstd -19 --patch-from 213,124 B (15.7 s single-thread, 9.8 s -T0; `--long=27` and `--ultra -22 --long=31` change nothing/+0.5%); xdelta3 -9 lzma 343,972 B (0.93 s), djw 464,283 B; zstd -3 --long=27 1.71 MB (0.08 s). v1->v3: bsdiff 64.9 KB, hdiffz 79.9 KB, zstd 224.7 KB. All 252 measured cases (encode + apply, min of 3 runs each) applied byte-identically to NEW. Apply is 0.01-0.10 s for every tool.
* Mismatched base (v1 built with F1, target v2c with F2): zstd 489,779 B (2.3x), bsdiff 161,114 B (2.7x) - degraded but still 17-52x smaller than a full download. Chaining v1->v2c->v3->v4 costs 1.72x (bsdiff, zstd) to 1.81x (xdelta3) the direct v1->v4 patch.
* Content-defined chunking does not help for code changes: with desync at 16:64:256 KiB, v1->v2c makes 273 of 386 chunks new (7.7 MB compressed = 92% of the full download); at 4:16:64 KiB 1,175 of 1,746 chunks (7.7 MB); at 64:256:1024 KiB 78 of 102 (7.9 MB). Only v2s (2 new chunks, 8-194 KB) and v2p (23-55% of chunks) benefit. casync default chunking: 273 new chunks, 7.6 MB. The FastCDC simulation agrees (62-78% of chunks new for v2c at every size; fixed 64 KiB blocks 79%).
* Per-chunk compression penalty (simulation, v1->v2c): compressing new chunks individually costs +55% at 4 KiB (7.30 vs 4.69 MB concatenated), +29% at 16 KiB, +16% at 64 KiB, +9% at 256 KiB; a 1 MiB dictionary trained on OLD's chunks recovers 53-75% of that penalty (4-16 KiB) and 40% for fixed 64 KiB blocks.
* Pure Go: `index/suffixarray` over the 29.6 MB binary takes 2.33 s and 148 MB RSS; gabstv/go-bsdiff produces a 69.6 KB patch in 9.0 s (553 MB RSS); klauspost zstd with OLD as a raw dictionary reaches 261 KB in 0.83-1.25 s but only at `SpeedBestCompression` (the other levels ignore the dictionary: 10.0-10.4 MB). SHA-256 2.56 GB/s, BLAKE3 9.66 GB/s (1.27 GB/s in 64 KiB calls), single goroutine.
* Transfer model, 100 Mbit/s / 100 ms RTT, v1->v2c: hdiffz 1.3 s, xdelta3-lzma 1.1 s, bsdiff 6.3 s, zstd-19 15.9 s (all encode-dominated), full download 8.6 s (7.8 s of that is `zstd -19` encode), desync 16:64:256 4.9 s (274 objects), desync 4:16:64 18.4 s. At 20 Mbit/s / 200 ms: hdiffz 1.4 s, full download 11.4 s, desync 4:16:64 35.6 s.

## 0. Tools and versions

| tool | version | notes |
|---|---|---|
| go | go1.26.4 linux/amd64 | /usr/local/go/bin/go |
| zstd | v1.5.7 | /usr/bin/zstd |
| xdelta3 | 3.2.0 | `~/.local/bin/xdelta3` (first on PATH; used by the harness). Debian package xdelta3 3.0.11-dfsg-1.2 was also installed by apt at /usr/bin |
| bsdiff/bspatch | 4.3-23 (Debian) | classic bzip2 patches |
| HDiffPatch hdiffz/hpatchz | v5.1.3, commit 3b9dca7 (2026-07-31) | built from source in `bench/out/tools/HDiffPatch` with `make LDEF=0 MD5=0 XXH=0 BZIP2=0 VCD=0 BSD=0` (plain `make` fails without the sibling `../lzma ../zstd ../zlib ../libdeflate ../bzip2 ...` source trees; lzma, zstd and zlib were cloned from sisong/*). A pre-existing `~/.local/bin/hdiffz` v5.1.3 was not used |
| desync | v1.1.0 (github.com/folbricht/desync) | `~/go/bin/desync` (`--version` flag does not exist; version from `go version -m`) |
| casync | 2 | /usr/bin/casync |
| xz | 5.8.1 | |
| GNU objdump/readelf | binutils 2.45 | |
| python3 / numpy | 3.13.7 / 2.2.4 | |
| Go libs (bench/goprobe) | klauspost/compress v1.19.2, lukechampine.com/blake3 v1.4.1, gabstv/go-bsdiff v1.0.5 | |

## 1. The test binary

`bench/testsrv` (module `binsync-bench`, go 1.26) is a chi router whose handlers
really use net/http, encoding/json, encoding/xml, crypto/tls+x509, compress/gzip,
archive/zip, text/template, html/template, net/http/pprof, log/slog, regexp,
math/big, image/png, database/sql (with modernc.org/sqlite), net/rpc, go/parser,
go/types, plus prometheus client_golang/promhttp, google.golang.org/grpc
(+health, reflection), go.etcd.io/bbolt and aws-sdk-go-v2 (config + s3).
`bench/gen.py VARIANT` writes `handlers.go`, `internal/util/util.go` and
`internal/third/third.go`; each variant is one source-level change:

| variant | change |
|---|---|
| v1 | baseline |
| v2s | `"hello world"` -> `"hello there"` (same length) |
| v2l | `"hello world"` -> `"hello world!!!"` (+3 bytes) |
| v2c | one line added to the handler: `w.Header().Set("X-Foo", "bar")` |
| v2p | new exported `util.Extra` in package internal/util, called from the handler |
| v3 | v2c + v2p |
| v4 | v3 + a one-line change in internal/third |

Build commands (from `bench/build.sh`, run in `bench/testsrv`; CGO enabled by default, so the
binaries are dynamically linked against libc through `net`/`os/user`):

```
F1:    go build -o OUT .
F2:    go build -trimpath -ldflags="-s -w" -o OUT .
F3:    go build -trimpath -ldflags="-s -w -buildid=" -buildvcs=false -o OUT .
F2pie: go build -buildmode=pie -trimpath -ldflags="-s -w" -o OUT .
```

| config | v1 size | ELF type | notes |
|---|---:|---|---|
| F1 | 42,164,054 B | EXEC | .symtab 998 KB, .strtab 2.27 MB, DWARF ~9.3 MB, all `.debug_*` sections carry the SHF_COMPRESSED (`C`) flag |
| F2 | 29,561,097 B | EXEC | every variant v1..v4 has exactly this size (section alignment absorbs the code growth) |
| F3 | 29,561,061 B | EXEC | F2 minus the 36-byte `.note.go.buildid` payload; v1->v2c differs from F2's v1->v2c by 100 bytes |
| F2pie | 32,477,486 B | DYN | adds `.rela` 2.9 MB and moves most of .rodata (3.5 MB -> 481 KB) into `.data.rel.ro` (3.0 MB) |

F2 section sizes: .text 14,781,841; .gopclntab 10,541,954; .rodata 3,527,449;
.noptrdata 454,273; .data 178,258; .typelink 38,272; .itablink 16,600; .go.buildinfo 4,128.

Reproducibility: building v1 twice into different output paths gave bit-identical
files for both F1 and F2 (`cmp` clean). Link time after the first build is 0.5-1.0 s
per variant (the 9.8 s F2pie entry in the log is the first PIE link, which recompiles
the standard library for PIE).

## 2. Where does the binary change?

`bench/analyze_diff.py OLD NEW` compares byte-for-byte. When the section layouts
(offset, size) of the two files are identical it compares raw file offsets ("raw");
otherwise every section is compared from its own start ("section-aligned"). In F2 all
variants have the same file size, but v2c/v2p/v3 grow `.text` by 192/128/320 bytes
so the later sections move and the section-aligned mode is used for them.

| pair | mode | differing bytes | % of file | runs (byte-exact) | runs (gap<=8 merged) | median run | largest unchanged run | in unchanged runs >=4K | >=16K | >=64K | >=256K |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| F2 v1->v2s | raw | 103 | 0.000% | 8 | 3 | 14 B | 17,555,420 | 99.99% | 99.99% | 99.99% | 99.99% |
| F2 v1->v2l | raw | 396,119 | 1.340% | 41,987 | 23,033 | 1 B | 10,614,303 | 69.51% | 59.39% | 53.80% | 53.46% |
| F2 v1->v2c | section-aligned | 3,970,589 | 13.432% | 964,933 | 76,899 | 3 B | 5,521,513 | 37.67% | 27.66% | 21.87% | 21.63% |
| F2 v1->v2p | section-aligned | 6,526,025 | 22.076% | 1,134,685 | 25,046 | 3 B | 2,879,547 | 65.36% | 58.83% | 45.12% | 28.87% |
| F2 v1->v3 | section-aligned | 7,297,638 | 24.687% | 1,173,797 | 71,008 | 3 B | 2,035,626 | 25.76% | 15.86% | 10.08% | 9.83% |
| F2 v3->v4 | raw | 430,675 | 1.457% | 52,993 | 31,856 | 1 B | 8,758,187 | 64.87% | 54.49% | 48.47% | 48.25% |
| F1 v1->v2c | section-aligned | 12,789,084 | 30.332% | 1,005,095 | 77,964 | 3 B | 5,559,482 | 35.18% | 28.01% | 23.88% | 23.50% |
| F2pie v1->v2c | section-aligned | 4,028,557 | 12.404% | 980,033 | 92,006 | 3 B | 5,523,443 | 37.33% | 29.43% | 23.48% | 19.69% |

Per-section differing bytes (size column is the F2 size, except `.data.rel.ro`/`.rela`
which are F2pie and `.symtab`/`.strtab`/`.debug_*` which are F1):

| section | size | F2 v1->v2s | F2 v1->v2l | F2 v1->v2c | F2 v1->v2p | F2 v1->v3 | F2 v3->v4 | F1 v1->v2c | F2pie v1->v2c |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| .text | 14,781,841 | 0 (0.000%) | 20,315 (0.137%) | 460,088 (3.113%) | 427,804 (2.894%) | 462,135 (3.126%) | 25,288 (0.171%) | 460,079 (3.112%) | 458,071 (3.098%) |
| .rodata | 3,527,449 | 5 (0.000%) | 374,537 (10.618%) | 438,873 (12.442%) | 2,837 (0.080%) | 439,194 (12.451%) | 402,784 (11.419%) | 438,882 (12.442%) | 401,875 (83.541%) |
| .data.rel.ro | 3,046,384 | - | - | - | - | - | - | - | 22,381 (0.735%) |
| .rela | 2,908,968 | - | - | - | - | - | - | - | 23,481 (0.807%) |
| .gopclntab | 10,541,954 | 0 (0.000%) | 0 (0.000%) | 3,067,956 (29.102%) | 6,094,753 (57.814%) | 6,392,450 (60.638%) | 689 (0.007%) | 3,067,955 (28.998%) | 3,120,079 (29.587%) |
| .typelink | 38,272 | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | - |
| .itablink | 16,600 | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | - |
| .go.buildinfo | 4,128 | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) | 0 (0.000%) |
| .note.go.buildid | 100 | 78 (78.000%) | 80 (80.000%) | 80 (80.000%) | 76 (76.000%) | 78 (78.000%) | 79 (79.000%) | 79 (79.000%) | 79 (79.000%) |
| .noptrdata | 454,273 | 0 (0.000%) | 0 (0.000%) | 57 (0.013%) | 51 (0.011%) | 64 (0.014%) | 0 (0.000%) | 57 (0.013%) | 55 (0.012%) |
| .data | 178,258 | 0 (0.000%) | 1,135 (0.637%) | 2,998 (1.682%) | 28 (0.016%) | 3,000 (1.683%) | 1,783 (1.000%) | 3,000 (1.683%) | 2,042 (1.146%) |
| .symtab | 997,896 | - | - | - | - | - | - | 1,579 (0.158%) | - |
| .strtab | 2,265,131 | - | - | - | - | - | - | 0 (0.000%) | - |
| .debug_info | 3,671,165 | - | - | - | - | - | - | 3,655,912 (99.585%) | - |
| .debug_line | 2,167,531 | - | - | - | - | - | - | 2,157,344 (99.530%) | - |
| .debug_loclists | 2,179,094 | - | - | - | - | - | - | 2,167,446 (99.465%) | - |

Observations:

* **v2s (same-length string)** changes 5 bytes of .rodata plus the 80-byte Go build ID
  (`.note.go.buildid`, which also feeds `.go.fipsinfo`) and nothing else.
* **v2l (+3-byte string)** touches no code, but the 3-byte growth of one .rodata string
  shifts everything behind it in .rodata (374 KB "differing" in raw-offset terms, i.e. a
  3-byte slide), and every RIP-relative `lea` in .text that points behind it gets a
  displacement +3: 20,002 single-byte runs in .text, 100% categorised as
  `same-insn-displacement/target-changed`. .gopclntab is untouched.
* **v2c (one statement)** grows `main.helloHandler` by 192 bytes (32-byte function
  alignment). Only the ~370 KB of .text after it (rest of package main, runtime/cgo
  trampolines, PLT) is shifted (`code-shifted(delta=+192)` and `insn-boundary-shifted`
  categories, ~9.7 K runs). The much larger effect is indirect: the new `"X-Foo"`/`"bar"`
  strings and inlined-call metadata grow .rodata by 8 bytes, so 32,361 RIP-relative
  `lea`/`mov` displacements across the *whole* .text change by +8 (sampled disassembly
  below), and **.gopclntab changes in 858,148 byte-exact runs (median 3 B), 3.07 MB =
  29% of the section**: the per-function tables hold 32-bit offsets into the shared
  pctab/funcname/funcdata areas and every offset behind the grown function moves.
* **v2p (new function in another package)** barely touches .rodata (2.8 KB) but the new
  `_func` entry and its tables shift 58% of .gopclntab (6.09 MB in 1.1 M runs). .text is
  shifted by +128 from `internal/util` onwards.
* **v3 = v2c+v2p**: .gopclntab 61% differing (6.39 MB), .text 3.1%, .rodata 12.4%;
  only 9.8% of the file is in unchanged runs >= 256 KiB.
* **v3->v4 (one line in a third package, no size change of any function)** looks like
  v2l: only displacements (24,686 runs, all `same-insn-displacement`) and a .rodata slide.
* **F1 (unstripped)**: the same v1->v2c change additionally rewrites 99.5% of
  `.debug_info`, `.debug_line`, `.debug_loclists`, `.debug_rnglists`, `.debug_addr`
  (~8.7 MB): they are zlib-compressed by the Go linker, so an address shift changes the
  whole compressed stream. `.symtab` changes 0.16%, `.strtab` 0%.
* **F2pie**: same picture as F2; `.rela` (2.9 MB of R_X86_64_RELATIVE entries) changes
  0.8% (23,481 runs, one per shifted target) and `.data.rel.ro` 0.7%.
* `.typelink`, `.itablink`, `.go.buildinfo`, `.noptrdata` are effectively unchanged in
  every pair.

Instruction-level categorisation of every differing run in .text (start instruction from
`objdump -d -j .text`, both binaries):

| pair (F2) | .text runs | displacement/target of same instruction | code shifted (same insn found at +delta) / boundary shifted | immediate changed | different instruction |
|---|---:|---:|---:|---:|---:|
| v1->v2l | 20,002 | 20,002 (100%) | 0 | 0 | 0 |
| v1->v2c | 46,602 | 32,361 (69%) | 5,148 + 4,838 | 4,253 | 2 |
| v1->v2p | 16,364 | 1,906 (12%) | 4,984 + 5,027 | 4,501 | 1 |
| v1->v3 | 46,401 | 32,503 (70%) | ~500 + 4,807 | 4,252 | 4,439 |
| v3->v4 | 24,686 | 24,686 (100%) | 0 | 0 | 0 |

("immediate changed" and "different instruction" occur only inside the shifted tail
where a same-address comparison is meaningless; in v1->v3 the tail is shifted by 320
bytes, outside the +-512 search.) Sampled disassembly (from `02-diff-F2-v1-v2c.log`):

```
0x401013  e8 98 8d e1 00  call 1219db0 <malloc@plt>   ->  e8 58 8e e1 00  call 1219e70   (PLT moved +192)
0xa425c4  48 8d 05 85 80 a7 00  lea 0xa78085(%rip),%rax  ->  lea 0xa7808d(%rip),%rax     (.rodata target +8)
0x105381e 48 8d 05 0b fd 4c 00  lea 0x4cfd0b(%rip),%rax  ->  lea 0x4cfd13(%rip),%rax     (.rodata target +8)
0x11c19a0 (in package main, after helloHandler): identical instruction stream found at +192
```
and from `02-diff-F2-v1-v2l.log`: `lea 0x10d10fc(%rip),%rbx -> lea 0x10d10ff(%rip),%rbx`
(+3) at 0x4031fe, and the same +3 at every one of the 20,002 runs.

No E8/E9 `call` displacements changed except in the shifted tail (calls into
package main and the PLT); the rel32 churn is almost entirely `lea` (0x8D) and
RIP-relative `mov` operands pointing into .rodata, which is why the x86 BCJ filter
(Section 5) does nothing here.

## 3. Delta encoders

Harness: `bench/delta_bench.py` (8 parallel workers for single-threaded methods; the
`-T0`/`-p-8` multithreaded variants ran alone afterwards). Wall time is the minimum of 3
runs and RSS the maximum of 3 (`/usr/bin/time -f "%e %M"`). Every apply output was
`cmp`'d against NEW.

Commands (`OLD NEW P OUT` substituted):

```
zstd-19:              zstd -19 --patch-from=OLD NEW -o P            | zstd -d --memory=2048MB --patch-from=OLD P -o OUT
zstd-19-long27:       zstd -19 --long=27 --patch-from=OLD NEW -o P
zstd-22-ultra-long31: zstd --ultra -22 --long=31 --memory=2048MB --patch-from=OLD NEW -o P | zstd -d --long=31 --memory=2048MB --patch-from=OLD P -o OUT
zstd-3/9-long27:      zstd -3|-9 --long=27 --patch-from=OLD NEW -o P
zstd-19-T0:           zstd -19 -T0 --patch-from=OLD NEW -o P
xdelta3-9-djw:        xdelta3 -e -9 -S djw -B 268435456 -W 16777216 -s OLD NEW P | xdelta3 -d -B 268435456 -s OLD P OUT
xdelta3-9-lzma:       same with -S lzma
bsdiff:               bsdiff OLD NEW P | bspatch OLD OUT P
hdiffz-m6-zstd21:     hdiffz -m-6 -SD -d -f -p-1 -c-zstd-21-24 OLD NEW P | hpatchz -f -p-1 OLD P OUT   (README "max compression")
hdiffz-s64-zstd21:    hdiffz -s-64 -SD -d -f -p-1 -c-zstd-21-24 OLD NEW P                              (README "fast/stream" mode)
hdiffz-m6-lzma2:      hdiffz -m-6 -SD -d -f -p-1 -c-lzma2-9-16m OLD NEW P
hdiffz-m6-zstd21-p8:  as hdiffz-m6-zstd21 with -p-8 (and hpatchz -p-8)
full-zstd-19:         zstd -19 NEW ;  full-zstd-19-long31: zstd -19 --long=31 --memory=2048MB NEW ;  full-xz-9: xz -9 -c NEW
```

#### Patch size (bytes), F1

| pair | zstd-19 | zstd-19-long27 | zstd-22-ultra-long31 | zstd-3-long27 | zstd-9-long27 | xdelta3-9-djw | xdelta3-9-lzma | bsdiff | hdiffz-m6-zstd21 | hdiffz-m6-lzma2 | hdiffz-s64-zstd21 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| v1-v2s | 5,168 | 5,168 | 5,168 | 15,761 | 7,881 | 361 | 422 | 351 | 163 | 164 | 177 |
| v1-v2l | 57,148 | 57,148 | 57,359 | 360,300 | 181,489 | 105,463 | 78,551 | 24,311 | 32,905 | 29,943 | 162,644 |
| v1-v2c | 8,537,327 | 8,537,327 | 8,512,313 | 10,178,901 | 9,735,082 | 8,866,404 | 8,731,806 | 8,510,856 | 8,411,387 | 8,404,532 | 9,403,211 |
| v1-v2p | 8,302,459 | 8,302,459 | 8,271,191 | 8,641,900 | 8,609,443 | 8,420,963 | 8,399,285 | 8,405,726 | 8,288,683 | 8,285,431 | 8,541,462 |
| v1-v3 | 8,615,448 | 8,615,448 | 8,577,471 | 10,293,531 | 9,851,369 | 8,965,050 | 8,825,595 | 8,577,882 | 8,472,858 | 8,464,941 | 9,515,734 |
| v2c-v3 | 8,522,176 | 8,522,176 | 8,489,496 | 9,492,715 | 9,364,315 | 8,723,136 | 8,669,682 | 8,568,535 | 8,449,307 | 8,445,708 | 9,086,914 |
| v3-v4 | 74,606 | 74,606 | 74,912 | 461,575 | 235,125 | 140,651 | 107,526 | 29,497 | 39,220 | 35,984 | 215,650 |
| v1-v4 | 8,618,083 | 8,618,083 | 8,579,455 | 10,294,144 | 9,851,100 | 8,966,064 | 8,827,605 | 8,579,491 | 8,473,567 | 8,465,681 | 9,516,785 |

#### Full-download baselines, F1 (bytes; encode s; decode s)

| variant | zstd -19 | zstd -19 --long=31 | xz -9 |
|---|---:|---:|---:|
| v2s | 18,100,040 (9.50 s / 0.04 s) | 17,935,099 (13.09 s / 0.04 s) | 17,208,704 (12.59 s / 0.26 s) |
| v2l | 18,100,121 (9.53 s / 0.03 s) | 17,934,602 (12.98 s / 0.05 s) | 17,205,272 (12.20 s / 0.25 s) |
| v2c | 18,105,781 (10.12 s / 0.04 s) | 17,938,545 (12.89 s / 0.04 s) | 17,209,172 (12.07 s / 0.25 s) |
| v2p | 18,100,630 (9.48 s / 0.04 s) | 17,934,833 (13.28 s / 0.05 s) | 17,208,820 (12.01 s / 0.25 s) |
| v3 | 18,101,258 (9.29 s / 0.03 s) | 17,940,154 (12.94 s / 0.05 s) | 17,206,820 (12.03 s / 0.26 s) |
| v4 | 18,101,337 (9.54 s / 0.03 s) | 17,940,761 (13.27 s / 0.05 s) | 17,205,056 (12.27 s / 0.25 s) |

#### Encode/apply time and peak RSS, F1 v1-v2c (min of 3 wall; max RSS)

| method | patch bytes | encode s | encode RSS MB | apply s | apply RSS MB | verified |
|---|---:|---:|---:|---:|---:|---|
| zstd-19 | 8,537,327 | 18.58 | 206 | 0.05 | 85 | True |
| zstd-19-long27 | 8,537,327 | 18.63 | 206 | 0.05 | 84 | True |
| zstd-22-ultra-long31 | 8,512,313 | 26.22 | 766 | 0.05 | 85 | True |
| zstd-3-long27 | 10,178,901 | 0.11 | 101 | 0.05 | 85 | True |
| zstd-9-long27 | 9,735,082 | 0.27 | 143 | 0.05 | 85 | True |
| xdelta3-9-djw | 8,866,404 | 1.81 | 451 | 0.08 | 71 | True |
| xdelta3-9-lzma | 8,731,806 | 2.55 | 462 | 0.10 | 77 | True |
| bsdiff | 8,510,856 | 28.02 | 363 | 0.43 | 89 | True |
| hdiffz-m6-zstd21 | 8,411,387 | 4.81 | 242 | 0.02 | 19 | True |
| hdiffz-m6-lzma2 | 8,404,532 | 5.15 | 148 | 0.06 | 19 | True |
| hdiffz-s64-zstd21 | 9,403,211 | 1.99 | 211 | 0.04 | 22 | True |
| zstd-19-T0 | 8,537,327 | 12.33 | 206 | 0.05 | 84 | True |
| hdiffz-m6-zstd21-p8 | 8,412,518 | 1.63 | 254 | 0.02 | 19 | True |

#### Patch size (bytes), F2

| pair | zstd-19 | zstd-19-long27 | zstd-22-ultra-long31 | zstd-3-long27 | zstd-9-long27 | xdelta3-9-djw | xdelta3-9-lzma | bsdiff | hdiffz-m6-zstd21 | hdiffz-m6-lzma2 | hdiffz-s64-zstd21 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| v1-v2s | 3,684 | 3,684 | 3,684 | 4,778 | 5,927 | 333 | 394 | 347 | 163 | 164 | 177 |
| v1-v2l | 55,648 | 55,648 | 55,862 | 380,051 | 182,999 | 106,331 | 79,044 | 24,322 | 32,899 | 29,940 | 162,526 |
| v1-v2c | 213,124 | 213,124 | 214,255 | 1,709,819 | 1,217,754 | 464,283 | 343,972 | 60,478 | 75,305 | 70,932 | 910,116 |
| v1-v2p | 41,040 | 41,040 | 40,911 | 159,302 | 129,264 | 81,170 | 70,356 | 16,141 | 19,230 | 18,183 | 96,698 |
| v1-v3 | 224,693 | 224,693 | 224,272 | 1,729,597 | 1,229,788 | 478,669 | 355,317 | 64,902 | 79,879 | 74,867 | 921,720 |
| v2c-v3 | 105,327 | 105,327 | 105,212 | 866,078 | 734,988 | 233,447 | 195,115 | 24,340 | 29,198 | 28,302 | 497,722 |
| v3-v4 | 73,074 | 73,074 | 73,439 | 490,995 | 238,108 | 141,268 | 107,783 | 29,439 | 39,230 | 36,002 | 215,608 |
| v1-v4 | 228,024 | 228,024 | 226,831 | 1,729,723 | 1,229,161 | 479,664 | 357,316 | 66,541 | 80,796 | 75,617 | 922,457 |

#### Full-download baselines, F2 (bytes; encode s; decode s)

| variant | zstd -19 | zstd -19 --long=31 | xz -9 |
|---|---:|---:|---:|
| v2s | 8,431,905 (7.93 s / 0.03 s) | 8,404,746 (8.68 s / 0.04 s) | 7,707,920 (7.92 s / 0.20 s) |
| v2l | 8,431,859 (7.81 s / 0.03 s) | 8,404,595 (8.35 s / 0.03 s) | 7,707,704 (7.75 s / 0.20 s) |
| v2c | 8,432,157 (7.78 s / 0.03 s) | 8,404,996 (8.65 s / 0.04 s) | 7,708,160 (7.74 s / 0.20 s) |
| v2p | 8,432,544 (7.94 s / 0.03 s) | 8,405,037 (8.72 s / 0.04 s) | 7,706,692 (7.64 s / 0.20 s) |
| v3 | 8,432,428 (8.20 s / 0.03 s) | 8,404,907 (8.85 s / 0.04 s) | 7,706,308 (8.17 s / 0.21 s) |
| v4 | 8,432,105 (8.22 s / 0.03 s) | 8,404,675 (8.98 s / 0.04 s) | 7,706,860 (8.19 s / 0.21 s) |

#### Encode/apply time and peak RSS, F2 v1-v2c (min of 3 wall; max RSS)

| method | patch bytes | encode s | encode RSS MB | apply s | apply RSS MB | verified |
|---|---:|---:|---:|---:|---:|---|
| zstd-19 | 213,124 | 15.71 | 158 | 0.04 | 59 | True |
| zstd-19-long27 | 213,124 | 15.72 | 158 | 0.04 | 59 | True |
| zstd-22-ultra-long31 | 214,255 | 22.19 | 718 | 0.04 | 59 | True |
| zstd-3-long27 | 1,709,819 | 0.08 | 67 | 0.04 | 60 | True |
| zstd-9-long27 | 1,217,754 | 0.23 | 107 | 0.04 | 60 | True |
| xdelta3-9-djw | 464,283 | 1.01 | 306 | 0.06 | 47 | True |
| xdelta3-9-lzma | 343,972 | 0.93 | 311 | 0.05 | 48 | True |
| bsdiff | 60,478 | 6.04 | 255 | 0.09 | 61 | True |
| hdiffz-m6-zstd21 | 75,305 | 1.17 | 80 | 0.01 | 11 | True |
| hdiffz-m6-lzma2 | 70,932 | 1.05 | 80 | 0.01 | 11 | True |
| hdiffz-s64-zstd21 | 910,116 | 0.81 | 74 | 0.02 | 14 | True |
| zstd-19-T0 | 213,124 | 9.79 | 158 | 0.03 | 59 | True |
| hdiffz-m6-zstd21-p8 | 75,339 | 0.47 | 82 | 0.01 | 11 | True |

#### Encode/apply time and peak RSS, F2 v1-v3 (min of 3 wall; max RSS)

| method | patch bytes | encode s | encode RSS MB | apply s | apply RSS MB | verified |
|---|---:|---:|---:|---:|---:|---|
| zstd-19 | 224,693 | 15.41 | 158 | 0.04 | 59 | True |
| zstd-19-long27 | 224,693 | 15.50 | 158 | 0.04 | 59 | True |
| zstd-22-ultra-long31 | 224,272 | 21.21 | 718 | 0.04 | 59 | True |
| zstd-3-long27 | 1,729,597 | 0.07 | 67 | 0.04 | 60 | True |
| zstd-9-long27 | 1,229,788 | 0.21 | 107 | 0.03 | 61 | True |
| xdelta3-9-djw | 478,669 | 0.92 | 306 | 0.06 | 47 | True |
| xdelta3-9-lzma | 355,317 | 0.94 | 311 | 0.06 | 48 | True |
| bsdiff | 64,902 | 6.12 | 255 | 0.10 | 61 | True |
| hdiffz-m6-zstd21 | 79,879 | 0.98 | 81 | 0.01 | 11 | True |
| hdiffz-m6-lzma2 | 74,867 | 1.22 | 81 | 0.02 | 11 | True |
| hdiffz-s64-zstd21 | 921,720 | 0.77 | 74 | 0.03 | 14 | True |
| zstd-19-T0 | 224,693 | 9.80 | 158 | 0.03 | 59 | True |
| hdiffz-m6-zstd21-p8 | 79,921 | 0.48 | 83 | 0.01 | 11 | True |

#### PIE and mismatched-flags cases

| case | method | patch bytes | encode s | apply s | verified |
|---|---|---:|---:|---:|---|
| F2pie.v1-v2c | zstd-19 | 233,470 | 16.30 | 0.04 | True |
| F2pie.v1-v2c | zstd-19-long27 | 233,470 | 14.53 | 0.04 | True |
| F2pie.v1-v2c | bsdiff | 63,826 | 6.70 | 0.10 | True |
| F2pie.v1-v2c | xdelta3-9-djw | 935,107 | 1.12 | 0.06 | True |
| F2pie.v1-v2c | hdiffz-m6-zstd21 | 79,127 | 0.98 | 0.02 | True |
| mismatch.v1F1-v2cF2 | zstd-19 | 489,779 | 14.07 | 0.04 | True |
| mismatch.v1F1-v2cF2 | bsdiff | 161,114 | 8.94 | 0.11 | True |
| mismatch.v1F1-v2cF2 | zstd-19-long27 | 489,779 | 13.84 | 0.04 | True |

#### Chain vs direct (bytes): v1->v2c + v2c->v3 + v3->v4 vs v1->v4

| cfg | method | v1->v2c | v2c->v3 | v3->v4 | chain sum | direct v1->v4 | chain/direct |
|---|---|---:|---:|---:|---:|---:|---:|
| F1 | bsdiff | 8,510,856 | 8,568,535 | 29,497 | 17,108,888 | 8,579,491 | 1.99 |
| F1 | zstd-19 | 8,537,327 | 8,522,176 | 74,606 | 17,134,109 | 8,618,083 | 1.99 |
| F1 | hdiffz-m6-zstd21 | 8,411,387 | 8,449,307 | 39,220 | 16,899,914 | 8,473,567 | 1.99 |
| F1 | xdelta3-9-lzma | 8,731,806 | 8,669,682 | 107,526 | 17,509,014 | 8,827,605 | 1.98 |
| F2 | bsdiff | 60,478 | 24,340 | 29,439 | 114,257 | 66,541 | 1.72 |
| F2 | zstd-19 | 213,124 | 105,327 | 73,074 | 391,525 | 228,024 | 1.72 |
| F2 | hdiffz-m6-zstd21 | 75,305 | 29,198 | 39,230 | 143,733 | 80,796 | 1.78 |
| F2 | xdelta3-9-lzma | 343,972 | 195,115 | 107,783 | 646,870 | 357,316 | 1.81 |

failures/unverified: 0

Notes on the delta results:

* `--long=27` produces byte-identical output to plain `-19` with `--patch-from` (zstd already
  sizes the window to cover the reference), and `--ultra -22 --long=31` is within +-0.5% at
  4.5x the memory (718 MB vs 158 MB) and 1.4x the time.
* zstd `--patch-from` at level 19 is the slowest encoder here (15-19 s single-threaded,
  9.8-13 s with `-T0`); levels 3/9 are fast (0.1-0.3 s) but produce 8x/5.7x larger patches.
* hdiffz `-m-6` with zstd or lzma2 is within 17-25% of bsdiff's size at 5x lower encode
  time and 3x lower memory; its streaming `-s-64` mode is 11-12x larger on these inputs.
* xdelta3's patches are 5.7-7.7x bsdiff's; its `-S djw` secondary compressor is worse than lzma.
* The zstd patch for the 103-byte v2s change is 3,684 B (frame overhead) vs 163 B (hdiffz).
* Encode wall times were measured with 8 concurrent single-threaded jobs on 24 cores;
  the `-T0`/`-p-8` rows ran alone.

## 4. Content-defined chunking

### desync (real tool, F2 binaries)

`desync make -s STORE -m min:avg:max IDX FILE` for every variant into one store per
chunk config, then per pair: chunks of NEW whose ID is not in OLD's index, their raw
bytes, and the on-disk size of those chunks in the store (desync stores each chunk as
zstd-compressed `.cacnk`, default level). `desync extract -s STORE --seed OLD.caibx:OLD
NEW.caibx OUT` reconstructs NEW using OLD as seed; the number of chunks it must fetch
from the store equals the "new" column (desync takes all other chunks from the seed).

| chunking | pair | chunks in NEW | new chunks (not in OLD) | new raw bytes | new compressed bytes (store) | index bytes | extract --seed s | verified |
|---|---|---:|---:|---:|---:|---:|---:|---|
| 4:16:64 | v1->v2s | 1762 | 2 (0%) | 16,793 | 8,399 | 70,584 | 0.04 | True |
| 4:16:64 | v1->v2l | 1751 | 719 (41%) | 12,897,466 | 5,130,931 | 70,144 | 0.04 | True |
| 4:16:64 | v1->v2c | 1746 | 1175 (67%) | 20,211,219 | 7,735,822 | 69,944 | 0.05 | True |
| 4:16:64 | v1->v2p | 1767 | 399 (23%) | 7,497,598 | 3,085,247 | 70,784 | 0.04 | True |
| 4:16:64 | v1->v3 | 1756 | 1190 (68%) | 20,329,276 | 7,764,431 | 70,344 | 0.05 | True |
| 4:16:64 | v2c->v3 | 1756 | 587 (33%) | 10,298,646 | 4,029,615 | 70,344 | 0.05 | True |
| 4:16:64 | v3->v4 | 1756 | 813 (46%) | 14,387,518 | 5,562,334 | 70,344 | 0.05 | True |
| 4:16:64 | v1->v4 | 1756 | 1189 (68%) | 20,322,873 | 7,774,962 | 70,344 | 0.05 | True |
| 16:64:256 | v1->v2s | 388 | 2 (1%) | 157,138 | 73,920 | 15,624 | 0.05 | True |
| 16:64:256 | v1->v2l | 383 | 182 (48%) | 13,592,077 | 4,916,997 | 15,424 | 0.05 | True |
| 16:64:256 | v1->v2c | 386 | 273 (71%) | 21,434,779 | 7,725,492 | 15,544 | 0.06 | True |
| 16:64:256 | v1->v2p | 384 | 148 (39%) | 12,251,731 | 4,832,916 | 15,464 | 0.04 | True |
| 16:64:256 | v1->v3 | 391 | 282 (72%) | 21,709,055 | 7,806,275 | 15,744 | 0.04 | True |
| 16:64:256 | v2c->v3 | 391 | 191 (49%) | 15,473,644 | 5,857,857 | 15,744 | 0.04 | True |
| 16:64:256 | v3->v4 | 391 | 206 (53%) | 15,476,296 | 5,415,736 | 15,744 | 0.05 | True |
| 16:64:256 | v1->v4 | 391 | 282 (72%) | 21,709,055 | 7,803,569 | 15,744 | 0.05 | True |
| 64:256:1024 | v1->v2s | 107 | 2 (2%) | 403,713 | 194,520 | 4,384 | 0.06 | True |
| 64:256:1024 | v1->v2l | 107 | 49 (46%) | 14,947,843 | 5,205,420 | 4,384 | 0.06 | True |
| 64:256:1024 | v1->v2c | 102 | 78 (76%) | 22,556,400 | 7,863,170 | 4,184 | 0.06 | True |
| 64:256:1024 | v1->v2p | 107 | 59 (55%) | 17,967,339 | 6,289,284 | 4,384 | 0.04 | True |
| 64:256:1024 | v1->v3 | 99 | 77 (78%) | 23,909,761 | 8,179,261 | 4,064 | 0.04 | True |
| 64:256:1024 | v2c->v3 | 99 | 65 (66%) | 20,622,051 | 7,109,631 | 4,064 | 0.05 | True |
| 64:256:1024 | v3->v4 | 102 | 59 (58%) | 17,501,057 | 5,893,849 | 4,184 | 0.06 | True |
| 64:256:1024 | v1->v4 | 102 | 80 (78%) | 23,909,761 | 8,210,792 | 4,184 | 0.05 | True |

`desync make` (whole file, store write included): 4:16:64 v1: 1762 chunks, 0.17 s, 103 MB RSS; 4:16:64 v2c: 1746 chunks, 0.12 s, 104 MB RSS; 16:64:256 v1: 388 chunks, 0.13 s, 121 MB RSS; 16:64:256 v2c: 386 chunks, 0.13 s, 132 MB RSS; 64:256:1024 v1: 107 chunks, 0.17 s, 228 MB RSS; 64:256:1024 v2c: 102 chunks, 0.17 s, 211 MB RSS

casync (`casync make` default chunking 16:64:256): v1 -> 388 chunks, 10,852,607 B in the
store; adding v2c stored 273 new chunks, 7,603,934 B compressed (make 0.19 s / 0.16 s).

### FastCDC / fixed-size simulation (`bench/cdc_sim.py`)

Gear hash with a deterministic 256-entry table (numpy RNG seed 0x5eed), normalisation
level 2 (min = avg/4, max = 4*avg; mask bits log2(avg)+2 below avg, log2(avg)-2 above),
64-byte sliding window that is *not* reset at chunk starts (a deviation from the
reference implementation that makes it vectorisable). Chunks are identified by SHA-256.
Compression columns: `zstd -19` of every new chunk as a separate file (sum); `zstd -19`
of all new chunks concatenated; per-chunk `zstd -19 -D dict` with a 1 MiB dictionary from
`zstd --train --maxdict=1MB` over all of OLD's chunks.

| scheme | pair | chunks in NEW | new chunks | new raw bytes | zstd -19 each (sum) | zstd -19 concatenated | zstd -19 each + 1 MiB dict | dict penalty recovered |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| fastcdc-4K | v1->v2s | 6248 | 2 (0%) | 12,375 | 5,350 | 5,534 | 4,984 | nan% |
| fastcdc-4K | v1->v2l | 6236 | 2327 (37%) | 11,180,048 | 4,900,702 | 2,873,743 | 3,728,036 | 58% |
| fastcdc-4K | v1->v2c | 6253 | 3862 (62%) | 18,365,507 | 7,299,920 | 4,694,318 | 5,905,647 | 54% |
| fastcdc-4K | v1->v2p | 6244 | 824 (13%) | 4,036,792 | 1,587,391 | 1,170,698 | 1,351,968 | 56% |
| fastcdc-4K | v1->v3 | 6254 | 3876 (62%) | 18,435,117 | 7,316,050 | 4,709,152 | 5,921,943 | 53% |
| fastcdc-4K | v3->v4 | 6256 | 2632 (42%) | 12,584,239 | 5,329,726 | 3,172,406 | 4,515,339 | 38% |
| fastcdc-4K | v1->v4 | 6256 | 3878 (62%) | 18,449,653 | 7,320,052 | 4,710,989 | 5,924,817 | 53% |
| fastcdc-16K | v1->v2s | 1556 | 2 (0%) | 42,826 | 17,770 | 18,459 | 15,918 | nan% |
| fastcdc-16K | v1->v2l | 1551 | 686 (44%) | 13,132,372 | 4,605,432 | 3,404,887 | 3,666,874 | 78% |
| fastcdc-16K | v1->v2c | 1544 | 1064 (69%) | 20,452,760 | 6,854,735 | 5,333,783 | 5,709,525 | 75% |
| fastcdc-16K | v1->v2p | 1547 | 382 (25%) | 7,471,973 | 2,692,011 | 2,182,863 | 2,276,929 | 82% |
| fastcdc-16K | v1->v3 | 1538 | 1062 (69%) | 20,522,464 | 6,867,038 | 5,348,463 | 5,722,757 | 75% |
| fastcdc-16K | v3->v4 | 1547 | 763 (49%) | 14,546,784 | 4,952,471 | 3,703,973 | 4,202,751 | 60% |
| fastcdc-16K | v1->v4 | 1547 | 1071 (69%) | 20,517,202 | 6,868,813 | 5,348,040 | 5,723,978 | 75% |
| fastcdc-64K | v1->v2s | 391 | 2 (1%) | 144,051 | 64,491 | 64,693 | 59,941 | nan% |
| fastcdc-64K | v1->v2l | 389 | 182 (47%) | 13,857,948 | 4,346,160 | 3,631,901 | 3,853,791 | 69% |
| fastcdc-64K | v1->v2c | 390 | 288 (74%) | 21,650,096 | 6,661,214 | 5,736,676 | 6,025,490 | 69% |
| fastcdc-64K | v1->v2p | 391 | 168 (43%) | 12,690,198 | 4,255,104 | 3,679,974 | 3,839,112 | 72% |
| fastcdc-64K | v1->v3 | 390 | 292 (75%) | 22,065,395 | 6,755,099 | 5,823,958 | 6,115,602 | 69% |
| fastcdc-64K | v3->v4 | 390 | 207 (53%) | 15,689,977 | 4,804,668 | 4,046,043 | 4,682,336 | 16% |
| fastcdc-64K | v1->v4 | 390 | 291 (75%) | 21,963,416 | 6,721,874 | 5,793,962 | 6,083,050 | 69% |
| fastcdc-256K | v1->v2s | 99 | 2 (2%) | 648,284 | 251,310 | 251,522 | 249,345 | nan% |
| fastcdc-256K | v1->v2l | 100 | 51 (51%) | 15,564,081 | 4,577,848 | 4,139,479 | 4,454,774 | 28% |
| fastcdc-256K | v1->v2c | 99 | 77 (78%) | 23,092,819 | 6,811,629 | 6,258,265 | 6,662,248 | 27% |
| fastcdc-256K | v1->v2p | 100 | 54 (54%) | 16,518,944 | 5,213,113 | 4,761,116 | 5,083,341 | 29% |
| fastcdc-256K | v1->v3 | 99 | 79 (80%) | 23,703,496 | 6,951,576 | 6,391,364 | 6,800,436 | 27% |
| fastcdc-256K | v3->v4 | 99 | 60 (61%) | 18,020,463 | 5,164,110 | 4,678,419 | 5,023,341 | 29% |
| fastcdc-256K | v1->v4 | 99 | 80 (81%) | 24,092,399 | 7,020,509 | 6,440,137 | 6,869,172 | 26% |
| fixed-64K | v1->v2s | 452 | 2 (0%) | 131,072 | 59,634 | 58,279 | 56,897 | 202% |
| fixed-64K | v1->v2l | 452 | 214 (47%) | 14,024,704 | 4,443,542 | 3,654,078 | 4,120,769 | 41% |
| fixed-64K | v1->v2c | 452 | 356 (79%) | 23,330,816 | 7,121,563 | 6,082,801 | 6,705,104 | 40% |
| fixed-64K | v1->v2p | 452 | 297 (66%) | 19,464,192 | 7,089,523 | 6,348,635 | 6,777,611 | 42% |
| fixed-64K | v1->v3 | 452 | 409 (90%) | 26,804,224 | 9,092,427 | 7,972,801 | 8,633,349 | 41% |
| fixed-64K | v3->v4 | 452 | 240 (53%) | 15,728,640 | 4,838,330 | 4,005,273 | 4,435,271 | 48% |
| fixed-64K | v1->v4 | 452 | 409 (90%) | 26,804,224 | 9,092,571 | 7,972,789 | 8,633,218 | 41% |

## 5. Pure-Go feasibility probes (`bench/goprobe/`, `bench/goprobe.sh`)

| probe | result |
|---|---|
| `index/suffixarray.New` over v1-F2 (29.6 MB) | 2.33 s build, +118 MB heap, 148 MB max RSS (F1 42.2 MB: 4.47 s, 209 MB RSS). This is the floor for a pure-Go bsdiff-style encoder using the stdlib suffix array (`sa/main.go`). |
| gabstv/go-bsdiff v1.0.5, v1->v2c F2 | patch 69,589 B (C bsdiff: 60,478 B), encode 9.00 s, apply 0.05 s, 553 MB max RSS, round-trip OK (`gobsdiff/main.go`) |
| klauspost/compress zstd v1.19.2 with OLD as raw dictionary (`WithEncoderDictRaw(1, old)` / `WithDecoderDictRaw`), v1->v2c F2 | `SpeedBestCompression`: 261,272 B in 0.83 s (64 MiB window), 1.05 s (128 MiB), 1.25 s (256 MiB); decode 0.01-0.02 s; round-trip OK. `SpeedBetterCompression`/`SpeedDefault` with the same raw dict: 10,052,326 / 10,419,150 B, i.e. worse than the 9,497,944 B no-dictionary result: only the `best` encoder actually finds matches inside a raw dictionary. CLI `zstd -19 --patch-from` on the same pair: 213,124 B (the library is 23% larger). Peak RSS of the probe process 1.3 GB (all nine configurations + inputs held in memory). The library can express patch-from semantics (raw content dict + window >= OLD size) but only at level `best`. |
| Hashing, single goroutine, v1-F2 | SHA-256 (crypto/sha256, SHA-NI) 2,558 MB/s; BLAKE3 (lukechampine.com/blake3, AVX-512 path) 9,659 MB/s on the whole 29.6 MB buffer; BLAKE3 over 64 KiB pieces 1,269 MB/s (per-call overhead dominates at that size) |
| x86 BCJ (E8/E9 rel32 -> absolute, LZMA-SDK `x86_Convert` port, applied inside `.text` only; `bcj/main.go`) | Round-trip dec(enc(v1)) == v1 verified. Rewrites 931,563 bytes of v1-F2. v1->v2c: zstd-19 213,124 -> 212,995 B, bsdiff 60,478 -> 61,509 B, xdelta3 464,283 -> 466,051 B, hdiffz-m6 75,305 -> 76,513 B. v1->v2l: zstd-19 55,648 -> 55,689 B, bsdiff 24,322 -> 24,322 B. Differing bytes v1->v2c 4,000,728 -> 3,999,654. Net effect: within +-2%, no benefit, because (Section 2) the rel32 churn is in RIP-relative `lea`/`mov` operands that point into .rodata, which BCJ does not transform, while E8 call targets are stable. |

## 6. Transfer-time model

`bench/model.py`: `time = (RTT + 20 ms TTFB) * max(1, objects/8) + bytes / bandwidth
+ encode_s + apply_s`, using the measured F2 numbers (subset of methods; the full set is in `bench/out/logs/06-model.log`) (single-threaded encode; desync
"encode" = `desync make` of NEW, "apply" = `desync extract --seed`). Objects for a delta
patch or a full download = 1; for CDC = new chunks + 1 index.

### 100 Mbit/s, 100 ms RTT (TTFB 20 ms/object, 8 parallel fetches)

| pair | method | bytes | objects | latency s | transfer s | encode s | apply s | total s |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| v1->v2c | full-zstd-19 (1 object) | 8,432,157 | 1 | 0.12 | 0.67 | 7.78 | 0.03 | 8.60 |
| v1->v2c | zstd-19 (1 object) | 213,124 | 1 | 0.12 | 0.02 | 15.71 | 0.04 | 15.89 |
| v1->v2c | xdelta3-9-lzma (1 object) | 343,972 | 1 | 0.12 | 0.03 | 0.93 | 0.05 | 1.13 |
| v1->v2c | bsdiff (1 object) | 60,478 | 1 | 0.12 | 0.00 | 6.04 | 0.09 | 6.25 |
| v1->v2c | hdiffz-m6-zstd21 (1 object) | 75,305 | 1 | 0.12 | 0.01 | 1.17 | 0.01 | 1.31 |
| v1->v2c | desync CDC 4:16:64 (1176 objects) | 7,805,766 | 1176 | 17.64 | 0.62 | 0.12 | 0.05 | 18.43 |
| v1->v2c | desync CDC 16:64:256 (274 objects) | 7,741,036 | 274 | 4.11 | 0.62 | 0.13 | 0.06 | 4.92 |
| v1->v2c | desync CDC 64:256:1024 (79 objects) | 7,867,354 | 79 | 1.19 | 0.63 | 0.17 | 0.06 | 2.04 |
| v1->v3 | full-zstd-19 (1 object) | 8,432,428 | 1 | 0.12 | 0.67 | 8.20 | 0.03 | 9.02 |
| v1->v3 | zstd-19 (1 object) | 224,693 | 1 | 0.12 | 0.02 | 15.41 | 0.04 | 15.59 |
| v1->v3 | xdelta3-9-lzma (1 object) | 355,317 | 1 | 0.12 | 0.03 | 0.94 | 0.06 | 1.15 |
| v1->v3 | bsdiff (1 object) | 64,902 | 1 | 0.12 | 0.01 | 6.12 | 0.10 | 6.35 |
| v1->v3 | hdiffz-m6-zstd21 (1 object) | 79,879 | 1 | 0.12 | 0.01 | 0.98 | 0.01 | 1.12 |
| v1->v3 | desync CDC 4:16:64 (1191 objects) | 7,834,775 | 1191 | 17.87 | 0.63 | 0.11 | 0.05 | 18.65 |
| v1->v3 | desync CDC 16:64:256 (283 objects) | 7,822,019 | 283 | 4.25 | 0.63 | 0.11 | 0.04 | 5.02 |
| v1->v3 | desync CDC 64:256:1024 (78 objects) | 8,183,325 | 78 | 1.17 | 0.65 | 0.16 | 0.04 | 2.02 |

### 20 Mbit/s, 200 ms RTT (TTFB 20 ms/object, 8 parallel fetches)

| pair | method | bytes | objects | latency s | transfer s | encode s | apply s | total s |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| v1->v2c | full-zstd-19 (1 object) | 8,432,157 | 1 | 0.22 | 3.37 | 7.78 | 0.03 | 11.40 |
| v1->v2c | zstd-19 (1 object) | 213,124 | 1 | 0.22 | 0.09 | 15.71 | 0.04 | 16.06 |
| v1->v2c | xdelta3-9-lzma (1 object) | 343,972 | 1 | 0.22 | 0.14 | 0.93 | 0.05 | 1.34 |
| v1->v2c | bsdiff (1 object) | 60,478 | 1 | 0.22 | 0.02 | 6.04 | 0.09 | 6.37 |
| v1->v2c | hdiffz-m6-zstd21 (1 object) | 75,305 | 1 | 0.22 | 0.03 | 1.17 | 0.01 | 1.43 |
| v1->v2c | desync CDC 4:16:64 (1176 objects) | 7,805,766 | 1176 | 32.34 | 3.12 | 0.12 | 0.05 | 35.63 |
| v1->v2c | desync CDC 16:64:256 (274 objects) | 7,741,036 | 274 | 7.54 | 3.10 | 0.13 | 0.06 | 10.82 |
| v1->v2c | desync CDC 64:256:1024 (79 objects) | 7,867,354 | 79 | 2.17 | 3.15 | 0.17 | 0.06 | 5.55 |
| v1->v3 | full-zstd-19 (1 object) | 8,432,428 | 1 | 0.22 | 3.37 | 8.20 | 0.03 | 11.82 |
| v1->v3 | zstd-19 (1 object) | 224,693 | 1 | 0.22 | 0.09 | 15.41 | 0.04 | 15.76 |
| v1->v3 | xdelta3-9-lzma (1 object) | 355,317 | 1 | 0.22 | 0.14 | 0.94 | 0.06 | 1.36 |
| v1->v3 | bsdiff (1 object) | 64,902 | 1 | 0.22 | 0.03 | 6.12 | 0.10 | 6.47 |
| v1->v3 | hdiffz-m6-zstd21 (1 object) | 79,879 | 1 | 0.22 | 0.03 | 0.98 | 0.01 | 1.24 |
| v1->v3 | desync CDC 4:16:64 (1191 objects) | 7,834,775 | 1191 | 32.75 | 3.13 | 0.11 | 0.05 | 36.05 |
| v1->v3 | desync CDC 16:64:256 (283 objects) | 7,822,019 | 283 | 7.78 | 3.13 | 0.11 | 0.04 | 11.06 |
| v1->v3 | desync CDC 64:256:1024 (78 objects) | 8,183,325 | 78 | 2.15 | 3.27 | 0.16 | 0.04 | 5.62 |

## Reproduce

```
bench/run.sh                 # all steps: tools build diff delta cdc probes model
bench/run.sh build diff      # subset
```

* `bench/build.sh` - builds v1..v4 x {F1,F2,F3} (+F2pie for v1/v2c) into `bench/out/bin`, checks reproducibility.
* `bench/gen.py VARIANT` - writes the variant sources into `bench/testsrv`.
* `bench/analyze_diff.py OLD NEW` - Section 2 (needs numpy, objdump, readelf).
* `bench/delta_bench.py` / `bench/render_delta.py` - Section 3 (`bench/out/results/delta.json`).
* `bench/cdc_desync.py`, `bench/cdc_sim.py` - Section 4 (`bench/out/results/cdc_*.json`).
* `bench/goprobe.sh` - Section 5 (`bench/out/logs/05-*.log`).
* `bench/model.py` - Section 6.

Logs: `bench/out/logs/00-*` tools, `01-build.log`, `02-diff-*.log`, `03-delta-bench.log`,
`04-cdc-*.log`, `05-*.log`, `06-model.log`. `bench/out/` is git-ignored.

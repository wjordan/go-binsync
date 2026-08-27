# Binary delta / patch encoders — research notes for go-binsync

Scope: updating a deployed Go web server binary (~30–100 MB, ELF, linux/amd64
assumed) on remote hosts over a high-latency link. Typical change is one line of
code or one string constant; skipped releases (vN → vN+k) must work. The source
side is a build box (CPU/RAM cheap); the target side applies the patch.

Every number below is taken from the cited source; where a claim is my
inference it is labelled as such. Numbers from different benchmarks are not
comparable to each other (different corpora, machines, versions).

## Summary of findings

- The "pointer problem" is the whole game. A one-line change shifts every
  address/branch displacement after it; copy/insert differs (xdelta, plain LZ)
  pay for each shifted pointer. Percival: "a one-line source code patch in a
  500kB executable could translate into a 50kB patch file" with copy/insert
  tools (https://www.daemonology.net/papers/bsdiff.pdf).
- bsdiff's fix is *approximate matching*: extend exact matches while ≥50% of
  bytes agree, emit bytewise differences (mostly zero, low-entropy) plus a
  small "extra" stream, then bzip2. On Percival's 19 Alpha binaries: bzip2
  35.5%, Xdelta 19.4%, .RTPatch 9.8%, Exediff 7.3%, BSDiff 8.6% of original
  size. On 97 FreeBSD 4.7 security-update binaries (36.4 MB total): xdelta
  3.29 MB, .RTPatch 750 KB, bsdiff 621 KB (58×). Cost: bsdiff needs
  max(17n, 9n+m) bytes and O((n+m) log n); bspatch needs n+m and O(n+m)
  (https://www.daemonology.net/bsdiff/).
- Disassembly-assisted patchers beat bsdiff by another ~1.3–10× *when they
  understand the format*. Courgette on a Chrome dev-channel update: full
  10,385,920 B, bsdiff 704,512 B, Courgette 78,848 B
  (https://www.chromium.org/developers/design-documents/software-updates-courgette/).
  Zucchini (Courgette's successor) on whole Firefox partial updates: 69.0→69.0.1
  mbsdiff 5.17 MB, Courgette 2.19 MB, Zucchini 1.94 MB; but Mozilla projected
  only ~10% gain on Linux/ELF and ~9.7% on Mach-O (raw fallback) vs ~33% on
  Windows PE (https://bugzilla.mozilla.org/show_bug.cgi?id=1632374).
- zstd `--patch-from` (v1.4.5+) is "dictionary compression with windowSize >
  srcSize", auto-enabling long-distance matching; Facebook's own claim: level
  19 patches comparable to bsdiff at ~7× the speed; level 1/3 >200×/>100×
  faster with patches "<0.5% of the original file size" on git-tarball corpora
  (https://github.com/facebook/zstd/releases/tag/v1.4.5,
  https://github.com/facebook/zstd/wiki/Zstandard-as-a-patching-engine). It has
  no approximate matching, so on executables it is competitive with, not
  better than, bsdiff.
- HDiffPatch's independent 20-case benchmark (Chrome, VS Code, JDK, Go
  distribution, Linux kernel source, etc.; R9-7945HX, Win11): bsdiff 8.17%
  (diff 2.5 MB/s, diff RAM 2773 MB, patch peak RAM 2313 MB, patch 146 MB/s);
  xdelta3 13.60% (6.7 MB/s, 409 MB; patch 85 MB, 154 MB/s); zstd --patch-from
  7.57% (5.5 MB/s, 2730 MB; patch peak 2306 MB, 1462 MB/s); hdiffz -m zstd
  6.74% (12 MB/s, 682 MB; patch 20–26 MB, 716 MB/s); hdiffz -BSD
  (bsdiff-compatible output) 7.74% at 14.3 MB/s
  (https://github.com/sisong/HDiffPatch/blob/master/README.md).
- Secondary compressor matters ~20%: same hdiffz delta with zlib 7.81%, zstd
  6.74%, lzma2 6.45% (same source). xz/lzma is what Firefox and Sparkle use;
  zstd decodes ~5–10× faster.
- Patch *application* is cheap for every candidate except memory: bspatch is
  O(n+m) and linear-scan; hpatchz streaming mode needs ~19–26 MB regardless of
  file size; zstd --patch-from decode needs the whole old file resident as the
  dictionary/window (peak 2.3 GB in the benchmark above on GB-scale inputs;
  for a 100 MB binary, on the order of a few hundred MB).
- Go-specific: no published work on delta-encoding Go binaries was found
  (searched: "Go binary delta", pclntab + bsdiff/zucchini, etc.). Relevant
  facts: pclntab is ~23–36% of binary size (CockroachDB:
  https://www.cockroachlabs.com/blog/go-file-size/) and is offset-encoded with
  no relocations (runtime/symtab.go: "The next field used to be textStart.
  This is no longer stored as it requires a relocation"); default linux/amd64
  builds are non-PIE (no `.rela.dyn`), so Zucchini's abs32 path (which reads
  `R_X86_64_RELATIVE` entries) has nothing to work with; Go 1.21+ builds are
  bit-reproducible given `-trimpath` and `CGO_ENABLED=0`
  (https://go.dev/blog/rebuild).
- Multi-version policy in the wild: Firefox ships partial MARs for "the
  previous four versions released (including point versions)"
  (https://firefox-source-docs.mozilla.org/toolkit/mozapps/update/docs/MarFiles.html);
  the 2012 decision that started it: partials for 14.0.1/13.0.1/12.0 covered
  ">75% of the userbase", each extra partial "up to 2 minutes" × 70+ locales
  (https://bugzilla.mozilla.org/show_bug.cgi?id=575317). Chrome's Omaha
  protocol keys diffs on a "differential fingerprint" (SHA-256 of the payload)
  and mandates full-package fallback if a diff fails
  (https://chromium.googlesource.com/chromium/src/+/main/docs/updater/protocol_3_1.md).
  Windows uses a hub: forward differential from RTM plus a reverse
  differential back to RTM, so one package serves any installed revision
  (https://learn.microsoft.com/en-us/windows/deployment/update/psfxwhitepaper).
- Pure-Go options are thin. `github.com/klauspost/compress/zstd` has
  `WithEncoderDictRaw`/`WithDecoderDictRaw` ("used as an initial history"), a
  window up to 512 MB, and dictionary support at all four speed levels, but it
  is hash-table matching with fixed table sizes and no LDM analogue
  (https://pkg.go.dev/github.com/klauspost/compress/zstd). `kr/binarydist`
  shells out to the `bzip2` binary for compression; `gabstv/go-bsdiff` is a
  pure-Go bsdiff 4 using `dsnet/compress` bzip2. xdelta3 and HDiffPatch exist
  for Go only as cgo wrappers or not at all; Zucchini lives inside the
  Chromium tree (C++, `base/` dependency), no port found.
- Replacing a running ELF: `open(O_WRONLY|O_TRUNC)` on an executing file gives
  ETXTBSY; `rename(2)` over it does not, because it swaps the directory entry
  and the old inode lives on until the process exits
  (https://lwn.net/Articles/866493/). Write temp in the same directory, fsync,
  rename, fsync the directory, verify the full-file hash before the rename.
- Every production system verifies the *result* (Firefox: size+CRC of the old
  file in the patch header, then post-patch check; Zucchini: CRC32 of old and
  new in `PatchHeader`; Chrome OS: re-read and hash the whole partition; Sparkle:
  checksum, fall back to full) and signs the patch or its manifest (Sparkle:
  `sparkle:edSignature` per `.delta`; MAR: up to 8 signatures in the header).

## 1. Generic delta encoders

### 1.1 zstd `--patch-from`

Mechanism (man page, https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md):
"effectively dictionary compression with some convenient parameter selection,
namely that windowSize > srcSize"; `--long` is activated automatically "if
chainLog < fileLog"; cannot be combined with `-D`; the CLI caps the dictionary
at 128 MiB unless `-M`/`--memory` raises it (v1.4.5 raised the hard maximum
from 32 MB to 2 GB); if windowLog > 27 the *decoder* must also be given
`--long=N` or `--memory=`. Tuning notes from the same page: up to level 15
`--single-thread` "marginally improves" ratio; above 15 single-thread *hurts*;
at level 19 set `--zstd=targetLength=4096` and a large `chainLog` for more
ratio at more time.

Library API (https://github.com/facebook/zstd/blob/dev/lib/zstd.h):
`ZSTD_CCtx_refPrefix()` references a "single-usage dictionary" that is
discarded at end of frame, "compatible with LDM", interpreted as raw content
(no dictionary parsing), buffer must outlive the compression;
`ZSTD_DCtx_refPrefix()` is the mirror. `ZSTD_c_enableLongDistanceMatching`
"enables default ZSTD_c_windowLog to 128 MB except when expressly set";
`ZSTD_c_windowLog` max is 31 on 64-bit; the streaming decoder refuses windows
above `ZSTD_d_windowLogMax` (default 27 = 128 MB) unless raised.

Facebook's numbers (release notes + wiki, corpora are *git tarballs*: zstd
31 MB, WordPress 273 MB, LLVM 1.66 GB, Linux ~1 GB): level 19 "comparable
patch sizes" to bsdiff at ~7× speed, levels 1/3 >200×/>100× faster with patches
"<0.5% of the original file size"; versus xdelta and SmartVersion, "patch sizes
... pretty comparable with Xdelta underperforming ... only slightly", zstd
fastest at extraction in all cases. The actual tables are images/Sheets, not
reproduced here.

Independent numbers (HDiffPatch benchmark, above): 7.57% vs bsdiff 8.17% and
hdiffz 6.74%; diff RAM 2730 MB and patch peak 2306 MB on the GB-scale cases.
Caveat for us: those corpora are tarballs with many unchanged files; nobody
has published a zstd-vs-bsdiff comparison on *single executables with a
one-line change*. Because zstd has no approximate-match mode, every shifted
rel32/offset breaks a match and costs a literal run plus a new sequence; bsdiff
encodes the same as a near-zero diff byte stream. Expect zstd to lose on that
workload; measure.

### 1.2 xdelta3 / VCDIFF / open-vcdiff

VCDIFF (RFC 3284, Korn/Vo) is a COPY/ADD/RUN format; xdelta3 is the reference
encoder. Source window `-B` defaults to 64 MB and the encoder keeps a "source
horizon" of half the buffer ahead of the input position, so "a source copy will
not be found if it lies more than half the source buffer size away from its
absolute position" (https://github.com/jmacd/xdelta/blob/wiki/TuningMemoryBudget.md);
the decoder needs `-B` bytes for the source plus `-W` (8 MB default). `-S`
selects secondary compression; the 3.1 man page lists `djw|fgk` (the `lzma`
option exists in builds with liblzma but is not in the man page I fetched).
In the HDiffPatch benchmark xdelta3 default is the worst of the diff tools
(13.60%); with a large `-B` it improves to 9.63% but diff RAM rises to 2.3 GB.
No approximate matching, so it inherits the pointer problem. Go: cgo wrappers
only (https://github.com/antmicro/go-xdelta,
https://github.com/nine-lives-later/go-xdelta). open-vcdiff is Google's
VCDIFF library (used by SDCH); same format, same limits.

### 1.3 bsdiff family

Algorithm (Percival 2003, https://www.daemonology.net/papers/bsdiff.pdf):
suffix-sort the old file (qsufsort, Larsson–Sadakane); scan the new file for
exact matches, keeping only matches with ≥8 bytes not explained by extending
the previous match; extend each match forward/backward while every suffix/
prefix of the extension matches ≥50% of bytes; emit (control, diff, extra)
streams; bzip2 each. "Pointer adjustments account for most of the differences
between versions, and the remainder of the diff bytes are mostly zero." Table 1
of the paper (Alpha binaries, sizes in bytes):

| Pair | New size | Xdelta | .RTPatch | Exediff | BSDiff |
|---|---|---|---|---|---|
| alto: extra printf | 466,944 | 50,613 | 7,524 | 6,237 | 6,299 |
| agrep 4.0→4.1 | 262,144 | 14,631 | 5,910 | 3,531 | 6,066 |
| gcc 2.8.0→2.8.1 | 2,899,968 | 549,250 | 140,284 | 76,072 | 121,371 |
| netscape 3.01→3.04 | 6,250,496 | 1,100,430 | 351,759 | 284,608 | 302,431 |
| apache 1.3.0→1.3.1 | 679,936 | 111,421 | 48,033 | 40,460 | 38,278 |
| average (19 pairs) | 100% | 19.4% | 9.8% | 7.3% | 8.6% |

The "extra printf" row is the closest analogue to go-binsync's typical change:
bsdiff 1.35% of the binary, a platform-specific disassembler only 1% better.
Percival's thesis claims a further ~20% via a more sophisticated algorithm
(https://www.daemonology.net/bsdiff/).

Costs: O((n+m) log n) time and max(17n, 9n+m)+O(1) bytes to diff; n+m to
patch. In the HDiffPatch benchmark bsdiff 4.3 is the slowest encoder (2.5 MB/s)
and bspatch is slow-ish (146 MB/s) because of bzip2.

Variants:
- Chromium `courgette/third_party/bsdiff`: qsufsort replaced by a modified
  libdivsufsort, a `search()` comparison fix (crbug 620867), varint encoding,
  Courgette streams instead of bzip2 — "the binary format is incompatible with
  the original"
  (https://chromium.googlesource.com/chromium/src/+/refs/tags/110.0.5481.1/courgette/third_party/bsdiff/README.chromium).
  Per chromium-dev (May 2023), Zucchini replaced Courgette for Chrome updates
  but "the BSDiff part of Courgette is still used by the component updater"
  (https://groups.google.com/a/chromium.org/g/chromium-dev/c/M-FQRn6baB0).
- AOSP `platform/external/bsdiff` (bsdiff with brotli, divsufsort; used by
  `update_engine` as `SOURCE_BSDIFF`/`BROTLI_BSDIFF`). I could not fetch its
  README from either googlesource or the GitHub mirror (404s); details are
  from the update_engine README
  (https://chromium.googlesource.com/aosp/platform/system/update_engine/+/HEAD/README.md).
- minibsdiff: bsdiff as a C library, "Control and data blocks in the patch
  output are not bzip2 compressed", same memory bounds, BSD-2
  (https://github.com/thoughtpolice/minibsdiff).
- bidiff (Rust, itch.io/divvun): "distantly derived from bsdiff", hash-table
  index with lock-free CAS insertion, rayon-parallel scan, chunked zstd
  output. Wine 4.18→4.19 on a Threadripper: bidiff 249 KiB (0.12%) in 0.42 s,
  patch 0.13 s; bsdiff 4.3 110 KiB (0.05%) in 3 min 8 s, patch 1.29 s
  (https://github.com/divvun/bidiff). 2.3× larger patches for ~450× faster
  diffs; the size loss is the price of hashing instead of suffix arrays.
- qbsdiff (Rust): bsdiff-4.x-compatible format, "would not generate exactly
  the same patch file as bsdiff" (https://github.com/hucsmn/qbsdiff).
- Go: see §6.

### 1.4 HDiffPatch

C/C++ library + CLI, MIT (https://github.com/sisong/HDiffPatch). Two matchers:
`-m` in-memory (suffix array via bundled libdivsufsort;
`libHDiffPatch/HDiff/private_diff/libdivsufsort`), RAM
"(newFileSize + oldFileSize×5–9) + O(1)"; `-s` streaming block match (default
64-byte blocks), RAM "O(oldFileSize×16/matchBlockSize + matchBlockSize×5×threads)"
and much faster but ~40% larger patches (9.58% vs 6.74% with zstd). `-block`
does a fast block pre-match. Output formats: native HDIFF13 (compressed),
`-SD` single-compressed-stream (HDIFFSF20, "only need one decompress buffer"),
`-WD` windowed (HDIFFW26, patch at 1.6–2.5 GB/s, multi-thread patch), `-BSD`
(bsdiff4-compatible), `-VCD` (RFC 3284, xdelta3/open-vcdiff-compatible).
Secondary compressors: zlib, libdeflate, bzip2/pbzip2, lzma/lzma2, zstd (and
brotli/lz4 in the build options). HPatchLite runs "on 1KB RAM devices".
Also ships `hsynz`, a zsync-like patch-from-any tool (14.95–20.05% in the
same benchmark, i.e. 2–3× larger than pairwise diffs).

Full benchmark rows worth keeping (20 cases, HDiffPatch 5.0.1, bsdiff 4.3,
xdelta3.1, zstd 1.5.7; "p8" = 8 threads):

| Program | Patch % | Diff RAM | Diff MB/s | Patch RAM (peak) | Patch MB/s |
|---|---|---|---|---|---|
| zstd --patch-from | 7.57 | 2730 M | 5.5 | 629 M (2306 M) | 1462 |
| xdelta3 | 13.60 | 409 M | 6.7 | 85 M (102 M) | 154 |
| xdelta3 -B (large) | 9.63 | 2282 M | 10.5 | 460 M (2071 M) | 256 |
| bsdiff | 8.17 | 2773 M | 2.5 | 637 M (2313 M) | 146 |
| hdiffz p1 -BSD | 7.74 | 676 M | 14.3 | 19 M | 164 |
| hdiffz sw p8 -VCD | 9.22 | 505 M | 52.0 | 32 M (36 M) | 522 |
| hdiffz p1 zlib | 7.81 | 676 M | 15.3 | 8 M (9 M) | 674 |
| hdiffz p1 lzma2 | 6.45 | 674 M | 12.0 | 20 M (25 M) | 480 |
| hdiffz p1 zstd | 6.74 | 682 M | 12.0 | 20 M (26 M) | 716 |
| hdiffz s p1 zstd | 9.58 | 195 M | 17.5 | 21 M (26 M) | 979 |
| hdiffz WD p8 zstd | 6.82 | 970 M | 20.6 | 24 M (30 M) | 2529 |
| hsynz p8 zstd | 14.95 | 3349 M | 10.1 | 24 M (34 M) | 410 |

Note the corpus includes `go_1.19.3.linux-amd64.tar ← 1.19.2` (468 MB, the
Go distribution with its compiled tools), but only aggregate numbers are
published. The self-reported nature of the table is a caveat; detools'
independent Python-tarball run agrees in direction (hdiffpatch+lzma 2.4 M vs
bsdiff+lzma 3.5 M, 13.7 s vs 24.3 s, https://github.com/eerimoq/detools).

### 1.5 Others

- librsync/rdiff: block signatures (rolling weak checksum + BLAKE2 strong),
  block-granular matching, no approximate matching; designed for remote sync
  where the server does not have the new file (https://librsync.github.io/).
  Wrong tool here: both files are on the build box.
- zsync: rsync's rolling checksum moved client-side; a precomputed control
  file (block checksums) is served statically and the client fetches missing
  blocks by HTTP Range — a true "patch from any old version" scheme with zero
  per-pair server work (https://zsync.moria.org.uk/paper/ch02.html). casync/
  desync (Go!) is the content-defined-chunking descendant: chunks 16 KB min /
  64 KB avg / 256 KB max, zstd-compressed, existing local files usable as
  seeds (https://github.com/folbricht/desync). Block/chunk granularity means a
  shifted binary re-downloads every chunk after the change point; hsynz's
  2–3× penalty above is the realistic cost.
- Puffin (Chrome OS / Android OTA): deterministically "puffs" deflate streams
  into a diffable form, bsdiffs, and re-"huffs" on the client, because in
  deflate "modifying even a single byte ... can cause the entire deflate stream
  to change drastically"
  (https://android.googlesource.com/platform/external/puffin/+/refs/heads/main/README.md).
  Same idea as Google's archive-patcher (File-by-File v1 for APK/JAR:
  uncompress changed entries, bsdiff, recompress with recorded settings;
  archived June 2024, https://github.com/google/archive-patcher). Irrelevant
  unless go-binsync ever ships compressed bundles; a lesson nonetheless: never
  diff compressed bytes.
- Microsoft PatchAPI / MSDelta: file-type "Transforms" preprocess the source
  "to make it more similar to the target at the byte level"; PatchAPI has
  transforms for i386 PE, MSDelta for i386/ia64/amd64; with PE-aware
  transforms "the size of the delta can typically be made 50–70% smaller";
  supports deltas against up to 255 source files, ignore/retain ranges, and
  "normalized signatures" to pick a source
  (https://learn.microsoft.com/en-us/previous-versions/bb417345(v=msdn.10)).
  Windows quality updates then layer the forward/reverse differential hub on
  top (https://learn.microsoft.com/en-us/windows/deployment/update/psfxwhitepaper).
- Academic LZ-style delta encoders: vcdiff/vdelta (Korn & Vo), zdelta (zlib
  modified to copy from a reference; Trendafilov/Memon/Suel TR-CIS-2002-02,
  https://cse.engineering.nyu.edu/tr/tr-cis-2002-02.pdf), Ddelta (2.5–8×
  faster encoding than xdelta/zdelta with similar ratio,
  https://www.sciencedirect.com/science/article/abs/pii/S0166531614000790),
  Edelta (word-enlarging, HotStorage'15,
  https://www.usenix.org/system/files/conference/hotstorage15/hotstorage15-xia.pdf),
  Gdelta (Gear rolling hash + array index + sampling,
  https://dl.acm.org/doi/10.1145/3664817). All target backup/dedup throughput,
  not executables; none does approximate matching. Motta/Gustafson/Chen,
  "Differential Compression of Executable Code" (DCC 2007, Bitfone) is the
  mobile-firmware take with a low-complexity decoder; only the abstract was
  reachable (https://ieeexplore.ieee.org/document/4148749/).
- Steam (SteamPipe): 1 MB chunks, chunk-level reuse against the previous
  build, plus "binary deltas" for modified chunks; guidance is to keep pack
  files uncompressed and avoid reordering assets
  (https://partner.steamgames.com/doc/sdk/uploading). deltarpm: bsdiff-based,
  needs "about three to four times the size of the rpm's uncompressed payload"
  (https://manpages.debian.org/testing/deltarpm/makedeltarpm.8.en.html);
  debdelta offers `--delta-algo xdelta|xdelta-bzip|xdelta3|bsdiff`
  (https://manpages.org/debdelta).

## 2. Executable-aware ("disassembly-assisted") patchers

### 2.1 Exediff (Baker, Manber, Muth 1999)

https://robert.muth.org/Papers/1999-exediff.pdf. Two ideas: *pre-matching*
(transform instruction streams lossily — keep opcode and admin registers,
collapse other registers/immediates to sign — then LCS-align) and *value
recovery* (predict each register/immediate/pointer in the upgrade from the
alignment via a fixed chain of heuristics: MatchValue, TranslateAddress,
EqualValue, CloseValue; store only mispredictions). Data-segment 8-byte
pointers are classified as text/data/other pointers before matching. Results:
"we usually beat bindiff by a factor of 2 to 5"; e.g. gcc cc1 2.8.0→2.8.1
gzipped delta 76,313 B vs bindiff.gz 847,457 B (9.0%). DEC Alpha only. This
is the conceptual ancestor of Courgette/Zucchini's "predict the secondary
changes, transmit only primary changes".

### 2.2 Courgette (Google, 2009)

Design doc: https://www.chromium.org/developers/design-documents/software-updates-courgette/.
Disassemble x86 into (pointer targets, non-pointer bytes, instruction
sequence); addresses go into a symbol table and pointers become indexes;
*adjust* the new symbol table ordering to maximize common substrings with the
old one; bsdiff the transformed streams; the client disassembles its own old
binary, bspatches, reassembles. Numbers: Chrome 190.1→190.4 full 10,385,920 B,
bsdiff 704,512 B, Courgette 78,848 B; "converting to assembly form alone
improved bsdiff results by roughly 30%". Generation is slow and memory-heavy;
Courgette had a history of security bugs and is now replaced.

### 2.3 Zucchini (Chromium, current)

README: https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md.
Core abstraction is a *reference* = (location, body, target, type); types are
grouped in *pools* sharing a target space. Two kinds: *abs32* (from relocation
tables, "storing semantic information (like RVA)") and *rel32* (embedded in
branch instructions, found by architecture-specific finders). *Equivalences*
(src_offset, dst_offset, length) map similar regions; targets in matched
regions get shared integer *labels*; the *encoded image* replaces reference
bytes by label-based encodings so the raw diff is mostly noise-free. Patch =
`PatchHeader` (magic, versions, old/new sizes and CRC32s) + per-element
equivalence list, raw deltas, reference deltas (signed target jumps), extra
targets per pool, all varint/delta-coded; an *ensemble* patch carries several
elements (PE/ELF/DEX/ZTF) with a raw fallback for unrecognized bytes.
Supported: PE (x86/x64/ARM), ELF (x86/x64/AArch32/AArch64 —
`disassembler_elf.h` reads `R_386_RELATIVE`/`R_X86_64_RELATIVE`/`R_ARM_RELATIVE`/
`R_AARCH64_RELATIVE` for abs32 and scans code sections for rel32), DEX. Costs:
"15–20 min for Chrome, using 1 GB of RAM" to generate; application memory is
the design constraint. v2.0 (2023-04) fixed a sort non-determinism that made
patches fail across builds — a warning about how brittle format-aware pipelines
are. Per chromium-dev: "Zucchini isn't significantly faster than Courgette" to
generate; advantages are "compressed patch size, patch-application memory, and
modernity"; `-raw` mode is faster and larger
(https://groups.google.com/a/chromium.org/g/chromium-dev/c/-8JPrR7GQOg).

Firefox adoption (bug 1632374) gives the only cross-tool numbers on real
whole-product updates:

| Firefox partial | mbsdiff | Courgette | Zucchini |
|---|---|---|---|
| 68.0.2→69.0 | 27.33 MB | 23.83 MB | 21.18 MB |
| 69.0→69.0.1 | 5.17 MB | 2.19 MB | 1.94 MB |
| 69.0.3→70.0 | 18.42 MB | 14.19 MB | 12.52 MB |
| 70.0→70.0.1 | 5.54 MB | 2.46 MB | 2.15 MB |

Projected gain vs mbsdiff: Windows ~33%, Linux ~10%, macOS ~9.7% (Mach-O is
unsupported → raw fallback). "Patch application takes about twice as long as
mbsdiff." Firefox's partial generation now uses zucchini and picks "the
smaller of the patch or the full file" per file
(https://firefox-source-docs.mozilla.org/taskcluster/partials.html). The
Linux number is the one that matters for us: on ELF the disassembly gain over
bsdiff was an order of magnitude smaller than on PE. My reading: Firefox's
Linux builds are PIE/shared objects, so abs32 is covered; the gap vs Windows
is presumably the PE relocation/RVA structure Zucchini models well. For a
non-PIE Go binary the abs32 path vanishes entirely (see §2.5).

### 2.4 Other format-aware systems, briefly

- MSDelta/PatchAPI transforms (§1.5): 50–70% smaller on PE, symbol files
  (.pdb) can be supplied to improve matching.
- detools (Python/C) has "experimental data format aware" modes for
  arm-cortex-m4 / aarch64 (normalizing branch encodings before diff); no
  numbers are published for them (https://github.com/eerimoq/detools).
- Sparkle (macOS): BinaryDelta v1 used bsdiff+bzip2 in a xar container; v3
  (Sparkle 2.1) is a custom container with lzma; each `.delta` is separately
  EdDSA-signed and the updater "falls back to downloading and installing the
  regular full update" on any failure
  (https://sparkle-project.org/documentation/delta-updates/).
- Chrome OS update_engine: block-level ops (ZERO, SOURCE_COPY, REPLACE_XZ,
  SOURCE_BSDIFF, PUFFDIFF, ...) against a source partition assumed
  "bit-by-bit equal to the original", whole-partition hash after apply.

### 2.5 Go binaries specifically

Nothing published on delta-encoding Go binaries was found; the points below
are from Go toolchain sources and my inference, and should be measured on
real go-binsync inputs.

- Layout mechanics of a one-line change (inference from cmd/link behaviour):
  functions are laid out in `.text` in the linker's symbol order, so growing
  one function shifts every later function; every `CALL rel32`/`JMP rel32`
  that crosses the change point changes by a constant; every RIP-relative LEA
  to `.rodata`/`.data` from code before the change point changes by the same
  constant. A string-constant *length* change does the same to `.rodata`.
  This is exactly the distribution bsdiff's diff stream compresses well.
- pclntab: `runtime.pclntab` was 23–36% of CockroachDB binaries and 35% of a
  hello-world (https://www.cockroachlabs.com/blog/go-file-size/). It is a
  table of `uint32` offsets (`_func.entryOff` relative to text start; header
  fields are offsets from the header) with no relocations
  (https://github.com/golang/go/blob/master/src/runtime/symtab.go). A
  function-size change rewrites `entryOff` for every later function — again a
  constant-delta pattern, bsdiff-friendly but invisible to Zucchini (neither
  rel32 in code nor abs32 in a relocation table). Also `runtime.findfunctab`
  buckets and `pctab` for changed functions.
- Absolute pointers: with the default `-buildmode=exe` on linux/amd64 the
  binary is not PIE; itab/type/func-value pointers in data and `moduledata`
  are absolute and there is no `.rela.dyn`. With `-buildmode=pie` those become
  dynamic relocations (~60k in one report,
  https://github.com/golang/go/issues/36028) which Zucchini's ELF path *can*
  model. Go 1.7 switched type metadata to relative offsets ("typelinks" are
  int32 offsets), shrinking pointer-heavy data.
- Strip: `-ldflags=-s -w` drops the symbol table and DWARF
  (https://pkg.go.dev/cmd/link); DWARF is address-dense and would otherwise
  inflate every delta. `-trimpath` removes build paths; build ID
  (`-buildid`) changes with every input change and is tiny.
- Reproducibility: Go 1.21+ is "perfectly reproducible" given `-trimpath`,
  `CGO_ENABLED=0` and the same toolchain (https://go.dev/blog/rebuild). This
  is what makes "identify the old binary by content hash" robust across build
  boxes, and it lets go-binsync regenerate any historical version from source if
  a patch base is missing.
- Feasibility of a Go-aware transform: a Zucchini-style pass for Go would need
  (a) x64 rel32 finding (heuristic, Zucchini has it), (b) a pclntab-aware
  "reference" type turning `entryOff`/`pctab` offsets into labels, (c)
  absolute pointer harvesting without relocations (needs symbol table or
  `moduledata` walking). Courgette's own data point — "converting to assembly
  form alone improved bsdiff results by roughly 30%" — and Zucchini's ~10% on
  Firefox/Linux bound the upside at maybe 1.1–3× over bsdiff for large effort
  and real fragility (Zucchini's 2023 non-determinism bug). Not a first step.

## 3. Benchmark corpus (all concrete tables found)

1. Percival 2003, 19 DEC Alpha pairs + 97 FreeBSD security binaries — §1.3.
2. Exediff 1999, 20 Alpha pairs, gzip-compressed deltas — §2.1 (e.g. cc1:
   bindiff.gz 847,457 B vs Exediff 76,313 B; upgrade.gz 831,626 B).
3. Courgette design doc, one Chrome update — §2.2.
4. Mozilla bug 1632374, four Firefox partials, mbsdiff/Courgette/Zucchini — §2.3.
5. HDiffPatch README, 20 mixed pairs + 32 APKs, sizes/speeds/RAM — §1.4.
   APK table highlights (Kirin980 for ARM speeds): zstd --patch-from 53.15%,
   xdelta3 54.51%, bsdiff 53.84%, hdiffz zstd 53.04%, archive-patcher 31.65%
   (0.9 MB/s diff, 14 MB/s patch) — i.e. on compressed containers only
   deflate-aware tools help.
6. zstd wiki (git tarballs; images only): level-19 ≈ bsdiff size at ~7× speed.
7. bidiff README, Wine 4.18→4.19 — §1.3.
8. detools README, Python 3.7.3→3.8.1 tar (79→84 MB) and MicroPython
   604 k→615 k — §1.4 (bsdiff+lzma 3.5 M/662 M RSS/24.3 s vs hdiffpatch+lzma
   2.4 M/523 M/13.7 s; MicroPython bsdiff+lzma 71 K vs hdiffpatch+lzma 65 K).
9. Hydraulic "Deltas diffed" is a survey, not a benchmark; its one data point
   is an Electron one-line change → ~115 KB (Windows) / ~32 KB (macOS) delta
   (https://hydraulic.dev/blog/20-deltas-diffed.html).

Application-side cost summary: bspatch/hpatchz are linear-time; hpatchz
streaming needs ~20 MB; bspatch needs old+new in memory (n+m); zstd
`--patch-from` decode is the fastest (1.4 GB/s) but needs the old file resident
as the window; Zucchini apply ≈ 2× mbsdiff time.

## 4. Multi-version / skipped releases

Options and who uses them:

1. Patch from the previous version only, full otherwise (Chrome OS delta
   payloads; Chrome's Omaha diffs are keyed on an exact payload fingerprint
   and fall back to full). Simplest; useless for vN → vN+k.
2. Patches from each of the last K versions (Firefox: K=4 today; started at
   K=3 in 2012 covering >75% of users; cost "up to 2 minutes" per extra
   partial per locale). Sparkle: a list of `.delta`s with `sparkle:deltaFrom`.
   Cost is K diff runs per release and K stored patches.
3. Chained patches vN→vN+1→...→vN+k. Download = sum of step patches (small if
   each step is a one-liner); apply = k passes over a 100 MB file with a hash
   check between passes — trivial on a server. Nobody in the survey ships
   this to end users, but nothing forbids it for a fleet tool; the failure
   mode is a missing intermediate.
4. Hub (Windows PSF): forward differential base→new plus reverse differential
   new→base, kept on the client; any revision hydrates via base. One package
   for all clients; extra apply step; needs the base version on every host.
5. Patch-from-any via chunking (zsync/casync/desync, hsynz): zero per-pair
   work, but 2–3× larger transfers on shifted binaries (hsynz 14.95% vs
   hdiffz 6.74%).
6. On-demand pairwise generation keyed by the client's reported old-binary
   hash, cached. Nobody does this at browser scale (millions of clients), but
   go-binsync's source is a build box talking to a fleet; the encoder runs once
   per distinct (old, new) pair (hdiffz ~14–34 MB/s → a few seconds for
   100 MB; bsdiff 4.3 ~2.5 MB/s → ~40 s; bidiff-style hashing ~0.5 s). Given
   Go's reproducible builds the box can regenerate any old version if it lacks
   the artifact.

Tradeoff guidance from the sources: Firefox's data says most users are on the
previous version *except* around emergency releases (only 18% on the previous
version for a chemspill), which is exactly the skipped-release case. For a
fleet the distribution of old versions is known, so (6) dominates: it costs
one encode per distinct old hash and guarantees a direct patch.

## 5. Safety

- Verify before and after. Firefox's patch header carries the expected size
  and CRC of the *existing* file and the updater refuses on mismatch; Zucchini
  stores CRC32 of old and new; Chrome OS re-reads and hashes the whole
  partition after writing; Sparkle verifies a checksum and falls back to the
  full download. For go-binsync: SHA-256 of old (pre-check) and new (post-check)
  in the patch manifest; refuse to rename on mismatch.
- Fallback is mandatory in every surveyed protocol ("fall back to a full
  package if the differential patch fails to apply" — Omaha 3.1).
- Atomic replace on Linux: write to a temp file in the target directory,
  fsync it, `rename()` over the executable, fsync the directory. ETXTBSY only
  arises when opening an executing file for writing; `execve()` takes
  `deny_write_access()` on the main executable (the 5.15 change removed only
  the `MAP_DENYWRITE` mmap flag); `rename()` never touches the old inode, which
  survives until the last process exits (https://lwn.net/Articles/866493/).
  Keep temp and target on the same filesystem (cross-fs `mv` degrades to
  copy+unlink). Expect 2× disk during the swap; the old inode is still mapped.
- Decoder hardening: bspatch-style decoders take offsets/lengths from the
  patch and have had memory-safety bugs. FreeBSD-SA-16:25 (CVE-2014-9862):
  "The implementation of bspatch does not check for a negative value on
  numbers of bytes read from the diff and extra streams", letting a malicious
  patch write at arbitrary heap locations and run code as the invoking user
  (https://www.freebsd.org/security/advisories/FreeBSD-SA-16:25.bspatch.asc).
  Bounds-check every control triple against old/new sizes, and never apply an
  unauthenticated patch.
- Signing: sign a manifest (old hash, new hash, patch hash, sizes, version)
  rather than the raw patch, so the same signature covers verification of the
  result. Sparkle signs each `.delta` with EdDSA; MAR headers hold up to 8
  signatures; Omaha ships SHA-256 of both diff and full packages. Ed25519 over
  a canonical manifest is the obvious choice for go-binsync; go-update-style
  tooling (https://github.com/inconshreveable/go-update) already does
  SHA-256 + ECDSA verification plus rename-based replacement.

## 6. Implications for go-binsync

### Comparison matrix

Patch size is for shifted-executable workloads (bsdiff = 1.0×), other columns
from the sources above; "impl" is effort to integrate into a Go tool.

| Candidate | Patch size | Encode (100 MB) | Decode | Encoder RAM | Decoder RAM | Impl | Go availability |
|---|---|---|---|---|---|---|---|
| bsdiff 4.3 (bzip2) | 1.0× | ~40 s (2.5 MB/s) | ~150 MB/s | ~17n ≈ 1.7 GB | n+m ≈ 200 MB | low | pure Go: gabstv/go-bsdiff (dsnet bzip2), kr/binarydist (execs `bzip2`) |
| bsdiff core + zstd/xz streams | ~0.8–1.0× (lzma2 vs zlib −17% on hdiffz) | as above (suffix sort dominates) | 500 MB/s+ | as above | n+m | low–med | suffix sort from go-bsdiff; zstd via klauspost, xz via ulikunitz/xz |
| HDiffPatch -m + zstd/lzma2 | ~0.8× | ~3–8 s (12–34 MB/s) | 700 MB/s–2.5 GB/s | n×5–9 ≈ 0.7 GB | ~20 MB | med (cgo) | no Go binding found; C library, MIT |
| HDiffPatch -s (stream) | ~1.2× | ~6 s | ~1 GB/s | ~200 MB | ~20 MB | med (cgo) | same |
| zstd --patch-from -19 (C) | ~1.0× on tarballs, unknown on exes | ~20 s (5.5 MB/s) | 1.4 GB/s | ~2–3× old | old + window | low (cgo) | cgo bindings; refPrefix exposure unverified |
| klauspost zstd raw dict | unknown (no LDM, hash tables) | fast | fast | ~old + tables | old + window | low | pure Go, `WithEncoderDictRaw`/`WithDecoderDictRaw`, window ≤512 MB |
| xdelta3 (VCDIFF) | ~1.2–1.7× | ~10 s | ~150–250 MB/s | 0.4–2.3 GB | ~85–460 MB | med (cgo) | cgo wrappers only |
| bidiff-style hash diff | ~2.3× | <1 s | fast | ~few× old | small | med | none in Go (Rust) |
| Zucchini (ELF x64) | ~0.9× on Linux (Firefox data) | minutes | ~2× bspatch | ~1 GB | moderate | very high | C++ in Chromium tree; no port |
| Go-aware transform + bsdiff | 0.3–0.9× (speculative) | bsdiff + parse | bsdiff + parse | bsdiff | bsdiff | very high | would be new work |
| chunk sync (desync/hsynz) | ~2–3× | seconds | fast | small | small | low | desync is pure Go |

### Candid assessment

- bsdiff's approximate matching is the right primitive for a Go binary with a
  one-line change; every survey and the Firefox Linux number say the
  disassembly layer buys ~10–30% on ELF for enormous complexity. Do not start
  with Zucchini or a Go-aware transform; keep it as a measured later
  experiment, ideally targeting pclntab `entryOff` deltas which no existing
  tool models.
- The weak part of classic bsdiff is the container: bzip2, three separately
  compressed streams, no header hashes. The obvious go-binsync design is bsdiff's
  matcher (suffix array + 50% extension) with the control/diff/extra streams
  compressed by zstd (or xz if the ~5% size matters more than decode speed),
  an authenticated manifest, and hpatchz-style streaming apply so the target
  never holds more than a window of the old file. That is what HDiffPatch
  already is; the question is cgo (HDiffPatch, MIT) versus a pure-Go
  reimplementation of the matcher (go-bsdiff's qsufsort is a starting point;
  encode speed will be far below hdiffz's 14–34 MB/s but the source side can
  afford it).
- zstd `--patch-from` is the pragmatic fallback and the best *decoder*: fast,
  standard frames, one dependency. Whether klauspost's raw-dictionary mode gets
  anywhere near C zstd's LDM ratio on a 100 MB history is unmeasured; its
  encoders index with fixed-size hash tables (best mode: 2^22 long / 2^18
  short entries) and have no long-distance matcher, so expect missed matches.
  Interop with `zstd -d --patch-from` should hold (raw-content dictionaries
  carry no dictionary ID) but must be tested.
- Encoder cost is irrelevant at fleet scale: even bsdiff 4.3's ~40 s per
  100 MB pair is fine when patches are generated once per distinct old hash
  on the build box and cached (§4, option 6). What matters on the target is
  decoder memory and a linear-time apply — bspatch/hpatchz both qualify; zstd
  needs the old file resident.
- Skipped releases: generate direct (old-hash → new) patches on demand and
  keep a full-binary fallback; chained application is a valid optimization
  when intermediates exist, and the hub scheme is over-engineered for a fleet
  where the server knows every old hash.
- Measure before deciding. The literature has no one-line-change numbers for
  Go binaries. A 10-minute experiment — build vN and vN+1 of the real server
  with `-trimpath -ldflags=-s -w`, run `bsdiff`, `hdiffz -m -c-zstd-21`,
  `hdiffz -s`, `zstd -19 --patch-from`, `xdelta3 -9 -S lzma -B <size>`, plus
  a klauspost raw-dict prototype — will settle the size axis; the memory and
  Go-availability axes are already settled above.

## References

- Percival, "Naïve Differences of Executable Code" (2003): https://www.daemonology.net/papers/bsdiff.pdf ; tool page: https://www.daemonology.net/bsdiff/
- Baker, Manber, Muth, "Compressing Differences of Executable Code" (1999): https://robert.muth.org/Papers/1999-exediff.pdf
- Courgette design doc: https://www.chromium.org/developers/design-documents/software-updates-courgette/
- Zucchini README: https://chromium.googlesource.com/chromium/src/+/main/components/zucchini/README.md ; ELF disassembler: https://github.com/chromium/chromium/blob/main/components/zucchini/disassembler_elf.h
- chromium-dev, Zucchini vs Courgette: https://groups.google.com/a/chromium.org/g/chromium-dev/c/-8JPrR7GQOg ; deprecation: https://groups.google.com/a/chromium.org/g/chromium-dev/c/M-FQRn6baB0
- Chromium bsdiff README.chromium: https://chromium.googlesource.com/chromium/src/+/refs/tags/110.0.5481.1/courgette/third_party/bsdiff/README.chromium
- Mozilla bug 1632374 (Zucchini for partials): https://bugzilla.mozilla.org/show_bug.cgi?id=1632374 ; bug 575317 (partials for more than last release): https://bugzilla.mozilla.org/show_bug.cgi?id=575317
- Firefox MAR files: https://firefox-source-docs.mozilla.org/toolkit/mozapps/update/docs/MarFiles.html ; partial generation: https://firefox-source-docs.mozilla.org/taskcluster/partials.html
- zstd v1.4.5 release: https://github.com/facebook/zstd/releases/tag/v1.4.5 ; v1.4.7: https://github.com/facebook/zstd/releases/tag/v1.4.7 ; patching-engine wiki: https://github.com/facebook/zstd/wiki/Zstandard-as-a-patching-engine ; man page: https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md ; zstd.h: https://github.com/facebook/zstd/blob/dev/lib/zstd.h
- klauspost/compress zstd: https://pkg.go.dev/github.com/klauspost/compress/zstd ; encoder options: https://github.com/klauspost/compress/blob/master/zstd/encoder_options.go ; enc_best.go: https://github.com/klauspost/compress/blob/master/zstd/enc_best.go ; decoder options: https://github.com/klauspost/compress/blob/master/zstd/decoder_options.go
- xdelta3 CLI: https://github.com/jmacd/xdelta/blob/wiki/CommandLineSyntax.md ; memory tuning: https://github.com/jmacd/xdelta/blob/wiki/TuningMemoryBudget.md ; man page: https://github.com/jmacd/xdelta/blob/release3_1_apl/xdelta3/xdelta3.1 ; RFC 3284: https://www.rfc-editor.org/rfc/rfc3284
- HDiffPatch: https://github.com/sisong/HDiffPatch (README benchmark tables; `libHDiffPatch/HDiff/private_diff/libdivsufsort`)
- bidiff: https://github.com/divvun/bidiff ; qbsdiff: https://github.com/hucsmn/qbsdiff ; minibsdiff: https://github.com/thoughtpolice/minibsdiff
- Go bsdiff ports: https://github.com/gabstv/go-bsdiff ; https://pkg.go.dev/github.com/kr/binarydist (bzip2.go execs `bzip2 -c`) ; https://github.com/icedream/go-bsdiff (cgo) ; go-update: https://github.com/inconshreveable/go-update
- Go xdelta wrappers: https://github.com/antmicro/go-xdelta ; https://github.com/nine-lives-later/go-xdelta
- detools: https://github.com/eerimoq/detools
- Puffin: https://android.googlesource.com/platform/external/puffin/+/refs/heads/main/README.md ; archive-patcher: https://github.com/google/archive-patcher ; update_engine: https://chromium.googlesource.com/aosp/platform/system/update_engine/+/HEAD/README.md
- Omaha protocol 3.1 (diff/fallback): https://chromium.googlesource.com/chromium/src/+/main/docs/updater/protocol_3_1.md
- Microsoft Delta Compression APIs (PatchAPI/MSDelta): https://learn.microsoft.com/en-us/previous-versions/bb417345(v=msdn.10) ; forward/reverse differentials: https://learn.microsoft.com/en-us/windows/deployment/update/psfxwhitepaper
- Sparkle delta updates: https://sparkle-project.org/documentation/delta-updates/
- SteamPipe: https://partner.steamgames.com/doc/sdk/uploading ; deltarpm: https://manpages.debian.org/testing/deltarpm/makedeltarpm.8.en.html ; debdelta: https://manpages.org/debdelta
- librsync: https://librsync.github.io/ ; zsync theory: https://zsync.moria.org.uk/paper/ch02.html ; desync: https://github.com/folbricht/desync
- zdelta TR: https://cse.engineering.nyu.edu/tr/tr-cis-2002-02.pdf ; Ddelta: https://www.sciencedirect.com/science/article/abs/pii/S0166531614000790 ; Edelta: https://www.usenix.org/system/files/conference/hotstorage15/hotstorage15-xia.pdf ; Gdelta: https://dl.acm.org/doi/10.1145/3664817 ; Motta et al. DCC 2007: https://ieeexplore.ieee.org/document/4148749/ ; Suel, "Delta Compression Techniques": https://research.engineering.nyu.edu/~suel/papers/delta-chap.pdf
- Hydraulic, "Deltas diffed": https://hydraulic.dev/blog/20-deltas-diffed.html
- Go: reproducible builds: https://go.dev/blog/rebuild ; runtime/symtab.go: https://github.com/golang/go/blob/master/src/runtime/symtab.go ; cmd/link flags: https://pkg.go.dev/cmd/link ; pclntab size: https://www.cockroachlabs.com/blog/go-file-size/ , https://github.com/golang/go/issues/36313 ; PIE relocations: https://github.com/golang/go/issues/36028
- LWN, "The shrinking role of ETXTBSY": https://lwn.net/Articles/866493/
- FreeBSD-SA-16:25.bspatch (CVE-2014-9862): https://www.freebsd.org/security/advisories/FreeBSD-SA-16:25.bspatch.asc

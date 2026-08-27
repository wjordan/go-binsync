# Content-defined chunking + content-addressed storage for binary sync

Research notes for go-binsync. Scope: can CDC/CAS (casync/desync/bita/Xet-style
"chunk the file, fetch only missing chunks by hash") efficiently update a
30-100 MB Go server binary over a high-latency link when the typical change is
one line of code or one string constant? Sources are cited inline; numbers are
from primary sources or from a small controlled experiment run for this document
(section 8). Where a number could not be found, that is stated.

Date: 2026-08-26.

---

## 0. Summary of findings

1. **CDC does badly on compiled binaries when anything shifts.** In the
   experiment below (25.6 MB VictoriaMetrics binary, stripped), a +15-byte
   string constant invalidated **57%** of 64 KiB FastCDC chunks (49% at 4 KiB);
   one extra `if` statement invalidated **68%** (60% at 4 KiB). Only a
   *same-length* string edit behaves well (2 chunks, 0.4% of the file). The
   churn is in `.rodata` (38-47% of bytes positionally changed), `.gopclntab`
   (59% on a code change) and, if present, compressed DWARF (`.debug_info`
   99.5%). Delta encoders on the same pairs: bsdiff 28 KB / 66 KB, `zstd
   --patch-from` 67 KB / 223 KB, vs. 6.0 MB for the full zstd-19 binary. That is
   a **~100x gap in favour of delta encoding** for this workload.
2. The literature says the same thing from the other side: Percival's bsdiff
   paper reports that naive copy/insert diffs of executables are far larger
   than the source change because "any source code modification will usually
   cause changes throughout an executable file"; on 97 FreeBSD security-patched
   binaries (36.4 MB) bzip2 gave 2.7x, Xdelta 11.0x, bsdiff 58.3x reduction
   ([bsdiff paper](https://www.daemonology.net/papers/bsdiff.pdf)). Chromium's
   Courgette doc cites ~500,000 internal pointers in Chrome and 10.4 MB full /
   704 KB bsdiff / 79 KB Courgette for one dev-channel update
   ([Courgette](https://www.chromium.org/developers/design-documents/software-updates-courgette/)).
3. The only CDC-on-executables figure found in the wild is from the Bazel
   REAPI blob-split proposal: **75% chunk reuse for a 300 MB executable with
   debug info** vs 96-98% for ~800 MB filesystem images
   ([remote-apis PR #282](https://github.com/bazelbuild/remote-apis/pull/282)).
   BuildBuddy reports 85% of written bytes deduplicated for blobs >2 MiB with
   ~512 KiB chunks, across mixed link/bundle/archive outputs, 20-40% overall
   ([BuildBuddy](https://www.buildbuddy.io/blog/content-defined-chunking/)).
   No paper quantifying chunk-level dedup ratios specifically for ELF/PE
   binaries across versions was found; Rugix's embedded-image benchmark is the
   closest (casync-style 64 KiB ≈ 0.40-0.75 of the LZMA'd full image for
   monthly→annual updates vs Xdelta ≈ 0.20-0.60; both >1.0 on major upgrades)
   ([Rugix](https://rugix.org/blog/efficient-delta-updates/)).
4. Algorithm choice matters much less than chunk size. FastCDC (Gear hash +
   normalized chunking) is ~10x faster than Rabin at nearly the same dedup
   ratio ([Xia et al. ATC'16](https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf)),
   and a 2024 independent evaluation of nine CDC algorithms concludes Rabin /
   Buzhash / Gear are "unbeaten in terms of deduplication" with Gear+NC and AE
   the practical picks ([arXiv 2409.06066](https://arxiv.org/abs/2409.06066)).
   Gear scalar ≈ 599 MiB/s, Rabin ≈ 202 MiB/s, SIMD Gear ≈ 941 MiB/s in that
   study; Go FastCDC implementations reach 1.3-9 GB/s.
5. Fixed-size chunking is catastrophically worse than CDC on shifted data
   (TPDS'20 Table 4: TAR 15.8% vs 46.7%, RDB 16.4% vs 92.6% dedup), but on
   *executables* CDC is closer to fixed-size than to delta: aligned 4 KiB
   blocks lost 86-93% of pages in the experiment; CDC 4 KiB lost 49-60%.
6. rsync-style matching (fixed blocks on the old side, rolling search on the
   new side) beats CDC at the same granularity because it needs no boundary
   agreement: 26% unmatched at 512 B blocks vs 49% for 4 KiB CDC on the
   +string case. It still leaves 26-42% of a Go binary unmatched, because the
   bytes really do change (relocations), not just move.
7. Chunk-size defaults cluster at 64 KiB avg for distribution tools (casync,
   desync 16:64:256 KiB, bita 16 KiB/64 KiB/16 MiB, Xet 8/64/128 KiB) and at
   1-4 MiB for backup tools (restic 512 KiB-8 MiB, borg 2 MiB target, kopia
   4 MiB buzhash); BuildBuddy uses ~512 KiB; bup uses 8 KiB with a fanout tree.
8. Per-chunk compression penalty (zstd -19) measured here: **+5.6% to +23%**
   bytes vs compressing the concatenation of the missing chunks (23% at 16 KiB
   chunks, 14% at 64 KiB on the rodata-shift case; 9.5%/5.6% on the code-change
   case). A 110 KB zstd dictionary trained on the old binary's chunks recovered
   about half of the loss. zstd's own manual: a dictionary gains ~10% on a
   64 KB input but ~500% on <1 KB inputs
   ([zstd.1](https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md)).
   zchunk's dictionary approach has the caveat that changing the dictionary
   invalidates every prior chunk.
9. Missing chunks are *clustered*: the code-change case at 64 KiB had 136
   missing chunks in only **9 contiguous runs**; at 16 KiB, 518 chunks in 15
   runs. A single archive + HTTP range requests with adjacent-chunk coalescing
   (bita's design) needs ~10 requests; per-chunk objects need 136-518.
10. Latency arithmetic: AWS documents small-object S3 latency of "roughly
    100-200 milliseconds" and 5,500 GET/s per prefix
    ([AWS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html));
    S3 and GCS accept **one byte range per request**
    ([bdon.org](https://bdon.org/2025/01/13/multiple-http-ranges)). 500 chunk
    GETs at 10-way concurrency and 100 ms each is ~5 s of pure latency; 10
    coalesced ranges is ~0.2 s. GET pricing is $0.0004/1000 — irrelevant.
11. Nobody serious serves one object per chunk to latency-sensitive clients:
    Xet packs ~1024 chunks into ≤64 MiB "xorbs" and fetches byte ranges inside
    them; git/restic use packs; bita/zchunk/zsync use ranges into one archive;
    casync's own author criticises OSTree's per-file fetch as "an explosion of
    HTTP GET requests" while shipping per-chunk `.cacnk` files
    ([casync blog](https://0pointer.net/blog/casync-a-tool-for-distributing-file-system-images.html)).
12. Verification designs: casync/desync hash every chunk (SHA-512/256) and the
    index lists chunk ids; bita verifies each chunk (blake2) *and* carries a
    whole-source checksum; zchunk carries header, whole-data, and per-chunk
    checksums; Xet uses a keyed-BLAKE3 Merkle tree (chunk→xorb→file); Bao
    provides verified byte-range streaming on BLAKE3 with ~6% overhead.
13. Post-dedup delta compression (DARE, Finesse, Ddelta/Edelta/Gdelta) adds
    ~2x reduction on backup corpora on top of chunk dedup, with Gear-based
    delta encoders 2.5-25x faster than Xdelta/Zdelta at similar or slightly
    lower ratios. For a two-version binary update this collapses to "delta the
    new file against the old file", which is what bsdiff/xdelta/zstd already
    do; the resemblance-detection machinery is only needed when there is no
    obvious base.
14. Go-specific: default `go build` embeds *compressed* DWARF, which
    avalanches entirely on any code change (bsdiff 4.9 MB unstripped vs 66 KB
    stripped for the same one-line change). Ship `-ldflags="-s -w"` and pin
    `-buildid=` (the build id alone is ~80 of the 98 bytes that differ in the
    same-length-edit case).
15. Bottom line for go-binsync: CDC/CAS alone will move roughly half the binary
    per release for the stated workload. It is still useful as (a) the storage
    substrate that makes an old version available as a *base* without keeping
    every version, (b) a fallback when no similar base exists, and (c) a way to
    resume/verify. The bandwidth win must come from delta encoding against the
    deployed version, and the latency win from a manifest + few coalesced
    range requests.

---

## 1. Algorithms

### 1.1 Rolling-hash chunkers (BSW family)

- **Rabin fingerprinting / LBFS.** LBFS (Muthitacharoen, Chen, Mazières 2001)
  introduced content-defined boundaries: a 48-byte window Rabin fingerprint,
  cut when `fp mod D == r`, min 2 KiB / avg 8 KiB / max 64 KiB. FastCDC's
  authors measured the open-source Rabin CDC at ~200 MB/s and note LBFS'
  2 KiB minimum ([Xia ATC'16](https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf)).
  restic keeps Rabin: 64-byte window, a random irreducible degree-53
  polynomial per repository, min 512 KiB, max 8 MiB, 20-bit mask → 1 MiB average
  ([restic/chunker](https://pkg.go.dev/github.com/restic/chunker),
  [restic blog](https://restic.net/blog/2015-09-12/restic-foundation1-cdc/)).
- **Buzhash** (cyclic polynomial, XOR/rotate of a 256-entry table). casync uses
  a 48-byte window and cuts when `h mod discriminator == discriminator-1`, where
  the discriminator is derived from the average with a correction because
  actual chunks come out ~1.32x larger than the nominal average; defaults avg
  64 KiB, min avg/4, max avg*4
  ([cachunker.h](https://raw.githubusercontent.com/systemd/casync/main/src/cachunker.h),
  [cachunker.c](https://raw.githubusercontent.com/systemd/casync/main/src/cachunker.c)).
  borg: `buzhash,19,23,21,4095` = 512 KiB min, 8 MiB max, 2 MiB target,
  4095-byte window ([borg internals](https://borgbackup.readthedocs.io/en/stable/internals/data-structures.html)).
  kopia defaults to `DYNAMIC-4M-BUZHASH`
  ([kopia splitter](https://pkg.go.dev/github.com/kopia/kopia/repo/splitter)).
  zchunk chunks either at caller-defined boundaries or automatically with
  buzhash ([jdieter, "What is zchunk?"](https://www.jdieter.net/posts/2018/05/31/what-is-zchunk/)).
- **Gear** (`fp = (fp << 1) + G[b]`, one shift + one add + one lookup per byte)
  was introduced for Ddelta (2014) and is the basis of FastCDC. Its weakness:
  with a 13-bit mask the effective window is only 13 bytes, which cost 5-8%
  dedup on the TAR dataset vs Rabin ([ATC'16 Table 3](https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf)).
- **FastCDC (2016).** Three techniques: (1) zero-padded mask so the judgement
  covers ~48 bytes again (`fp & Mask == 0`), (2) cut-point skipping (start
  hashing at MinSize; the paper measured a 2-15% dedup loss from skipping
  alone), (3) normalized chunking: a stricter mask (15 one-bits) before the
  target size and a looser one (11 bits) after it, around a 13-bit / 8 KiB
  target. Result: ~10x faster than open-source Rabin CDC, ~3x faster than
  Gear/AE, "nearly the same deduplication ratio" (ATC'16 Table 3, e.g. TAR at
  8 KiB: Rabin 47.58%, FastCDC 47.64%; VMA 38.23% vs 37.96%). Datasets: TAR
  19 GB, LNX 105 GB, WEB 36 GB, VMA 117 GB, VMB 1.9 TB, RDB 1.1 TB, SYN 1.4 TB —
  none is a corpus of executables.
- **FastCDC (TPDS 2020, not TOS).** Adds "rolling two bytes each time"
  (30-40% faster; ~12x Rabin overall), evaluates NC levels 1-3 and minimum
  sizes 2-8 KiB on seven datasets (~6 TB). Recommendation: **NC-2 with
  MinSize 8 KiB** (or 6 KiB for best ratio) at an 8 KiB target. Fixed-size
  chunking comparison (Table 4): TAR 15.77% vs Rabin 46.66%; RDB 16.39% vs
  92.57%; VMA 17.63% vs 36.70%; LNX 95.68% vs 96.30% (LNX is many small files,
  so fixed chunking barely matters)
  ([TPDS'20](https://csyhua.github.io/csyhua/hua-tpds2020-dedup.pdf)).
  Average generated chunk ≈ expected + min (MIN-4 KiB at 8 KiB → ~12 KiB).
  The Rust `fastcdc` crate notes the paper never published the Gear table or
  all masks, so implementations differ in exact cut points
  ([docs.rs/fastcdc](https://docs.rs/fastcdc/latest/fastcdc/)).

### 1.2 Extremum / interval chunkers

AE (asymmetric extremum), RAM, MII, PCI, BFBC. The 2024 independent study
(nine algorithms, five ~10 GiB datasets: RAND, LNX = Linux ISO images, PDF,
WEB, CODE = GCC/GDB/Emacs sources) reports at 2 KiB target on RAND:
FSC 2529, Gear64+SIMD 941, RAM 887, AE 734, Gear 599, Buzhash 338, PCI 308,
MII 256, Rabin 202 MiB/s. Dedup: BSW algorithms and AE are top and
comparable; RAM produces pathologically large chunks on low-entropy input;
MII/PCI "only viable for data synchronization"; NC up to level 3 has "only
marginal detrimental effects" on dedup while cutting variance; BFBC's claimed
gains did not reproduce ([arXiv 2409.06066](https://arxiv.org/abs/2409.06066)).
Chonkers (2025) offers provable chunk-size and edit-locality bounds
([arXiv 2509.11121](https://arxiv.org/abs/2509.11121)); SS-CDC/RapidCDC/QuickCDC
target parallel/faster chunking ([NetApp/SS-CDC](https://github.com/NetApp/SS-CDC)).

### 1.3 Rolling hash vs fixed-size: two different questions

There are two distinct designs, and the trade-off is *not* "CDC vs fixed":

| Design | Who chunks | What crosses the wire first | Insertion-tolerant? |
|---|---|---|---|
| rsync / librsync / zsync / RDC | receiver hashes *fixed* aligned blocks; sender does a byte-granular rolling search | block signatures (rsync, RDC: receiver→sender; zsync: precomputed `.zsync` file) | yes, via rolling search |
| CDC + CAS (casync, bita, Xet, restic) | both sides chunk independently with the same algorithm | a chunk index / manifest | yes, via boundary stability |

rsync needs no shared chunking parameters and matches at any byte offset, so
it wins on reuse at equal block size (see 8.2: 26% vs 49% unmatched). The
cost is one extra round trip (or a precomputed signature file, zsync style)
and that the *sender* must run the search — fine for go-binsync where the sender
is the release pipeline. CDC's advantage is that chunks are addressable across
files and versions without any pairwise computation, which is what makes a
CAS possible. Dropbox uses fixed 4 MB SHA-256 blocks for dedup plus rsync-style
delta inside blocks ([Dropbox](https://dropbox.tech/infrastructure/streaming-file-synchronization)).
Microsoft RDC chunks with the H3 rolling hash at local maxima within a horizon,
sends `(length, hash)` signatures, and applies itself recursively to the
signature file; it also finds similar seed files with 96 bits of metadata per
file ([MS-RDC overview](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdc/023c996d-4fb1-4107-8914-f142f0e3ba9a),
[MSR paper](https://www.microsoft.com/en-us/research/publication/optimizing-file-replication-over-limited-bandwidth-networks-using-remote-differential-compression/)).

### 1.4 Why executables are the worst case

CDC handles *insertions* (bytes move, content unchanged). A one-line change in
a compiled program does not primarily move bytes; it **rewrites** bytes
everywhere that encodes an address:

- Percival: "Adding or removing a small number of bytes of code or data will
  change the relative position of blocks of code, adjusting the displacement
  of relative branches...; any data located after the modified region will
  have a different address, causing data pointers to be modified throughout
  the file" — a one-line patch to a 500 kB executable could become a 50 kB
  naive patch. His key observation is that the diffs are *sparse and small*
  (addresses change in their low bytes, blocks shift by the same amount), which
  is why bsdiff's "approximate match + bytewise difference" works and why CDC's
  exact-match model cannot ([bsdiff paper](https://www.daemonology.net/papers/bsdiff.pdf)).
- Courgette/Zucchini go further by disassembling to separate pointer targets
  from code, because "it only takes a few source changes before almost all of
  these internal pointers have a different value"
  ([Courgette](https://www.chromium.org/developers/design-documents/software-updates-courgette/),
  [Zucchini](https://chromium.googlesource.com/chromium/src/+/HEAD/components/zucchini/)).
- debdelta observed that a compiler change (gcc 4.4→4.5) alone made Debian
  binaries "cannot be effectively delta-ed" ([debdelta](https://debdelta.debian.net/)).
- Go specifics measured in 8.3: a 128-byte `.text` growth changed 13.6% of
  `.text` bytes (RIP-relative displacements to shifted `.rodata`/`.data`),
  47% of `.rodata` (type descriptors, string headers, func pointers hold
  absolute addresses), 59% of `.gopclntab`, and 99.5% of the zlib-compressed
  DWARF sections.

Studies that *do* report high dedup on software corpora are file-level or
source-level: Docker Hub — 96.8% of files duplicate across 1.79 M layers /
167 TB uncompressed ([Zhao et al. CLUSTER'19](https://par.nsf.gov/biblio/10167826-large-scale-analysis-docker-hub-dataset);
[DupHunter ATC'20](https://www.usenix.org/conference/atc20/presentation/zhao));
FastCDC's LNX/TAR sets are *source* trees. BuildBuddy explicitly notes
"compressed formats like tar.gz archives and Docker image layers are often
less chunkable" ([BuildBuddy](https://www.buildbuddy.io/blog/content-defined-chunking/)).

---

## 2. Implementations and design choices

| Tool | Chunker (window) | min/avg/max | Hash | Compression | Index / metadata | Fetch model | Seeding |
|---|---|---|---|---|---|---|---|
| casync | buzhash (48 B) | 16/64/256 KiB | SHA-512/256 (or SHA-256) | zstd (xz, gzip) per chunk `.cacnk` | `.caibx/.caidx`: header (flags, min/avg/max) + 40 B per chunk (8 B offset + 32 B id) | one HTTP GET per chunk from `<store>/<4 hex>/<id>.cacnk` | `--seed=` files/dirs/devices; reflinks on btrfs/xfs |
| desync | same as casync; parallel chunking | 16/64/256 KiB | SHA-512/256 | zstd only | casync-compatible | S3/GCS/HTTP/SFTP/OCI stores, `-n 10` concurrent | null seed, self seed, file seeds + `.caibx`, cache store |
| bita | RollSum (64 B) or BuzHash (16 B) | 16 KiB / 64 KiB / 16 MiB | blake2 (64 B default) | brotli-6 default; zstd/lzma | protobuf dictionary: source size+checksum, chunker params, per-chunk (checksum, source_size, archive_size, archive_offset), rebuild_order | HTTP **range** into one archive; adjacent missing chunks coalesced into one request | local files and the output file itself, chunks copied in place |
| zchunk | manual or buzhash | n/a | SHA-1/256/512/512-128 (header, data, per chunk) | zstd per chunk, optional shared dictionary chunk | lead/preface/index/signatures in-file; index downloadable separately | HTTP ranges into one `.zck` | previous `.zck` |
| zsync | rsync blocks (fixed) | — | rsync weak+strong sums in `.zsync` | works on gzip via "rsync-friendly gzip" | `.zsync` control file | HTTP ranges (multi-range where supported) | local old file |
| rsync/librsync | fixed blocks, rolling search | 2 KiB default (rdiff) | rolling + MD4/blake2 | — | signature stream | bidirectional stream | old file on receiver |
| OSTree | per-file objects; static deltas via bsdiff between similar objects | — | SHA-256 | gzip `.filez`; delta parts | superblock + parts with a small bytecode | one GET per changed file, or one delta | local repo |
| restic | Rabin (64 B) | 512 KiB / 1 MiB / 8 MiB | SHA-256 | per blob, inside ~4 MiB+ packs | index files <8 MiB | pack range reads | — |
| borg | buzhash (4095 B) | 512 KiB / 2 MiB / 8 MiB | HMAC-SHA256 id | lz4/zstd/zlib/lzma post-dedup | segments | — | — |
| kopia | buzhash | `DYNAMIC-4M-BUZHASH` | — | per-content in packs | — | — | — |
| bup | rollsum (64 B) | ~8 KiB (13 bits), max 4x | SHA-1 git objects | zlib packs | fanout tree (4 bits/level) | git packs | — |
| Xet (HF) | gearhash mask `0xFFFF000000000000` | 8 / 64 / 128 KiB | keyed BLAKE3 Merkle | per chunk: none / LZ4 / byte-group+LZ4 | shards ≤64 MiB (file→terms, xorb→chunks) | ranges inside ≤64 MiB xorbs via presigned URLs | local shard cache + global dedup API |
| BuildBuddy / REAPI split-splice | FastCDC | ~512 KiB avg, blobs >2 MiB | REAPI digests | — | reconstruction metadata keyed by original digest | per-chunk CAS blobs | client's local CAS |
| Dropbox | fixed 4 MB | — | SHA-256 blocklist | + rsync delta | metaserver journal | per block | — |

Sources: [casync](https://github.com/systemd/casync), [casync blog](https://0pointer.net/blog/casync-a-tool-for-distributing-file-system-images.html),
[caformat.h](https://raw.githubusercontent.com/systemd/casync/main/src/caformat.h),
[desync](https://github.com/folbricht/desync), [desync local.go](https://raw.githubusercontent.com/folbricht/desync/master/local.go),
[bita](https://github.com/oll3/bita), [bita cli.rs](https://raw.githubusercontent.com/oll3/bita/master/src/cli.rs),
[bita chunk_dictionary.proto](https://raw.githubusercontent.com/oll3/bita/master/bitar/proto/chunk_dictionary.proto),
[zchunk](https://github.com/zchunk/zchunk), [zchunk_format.txt](https://raw.githubusercontent.com/zchunk/zchunk/main/zchunk_format.txt),
[zsync paper](http://zsync.moria.org.uk/paper/), [librsync rdiff](https://librsync.github.io/page_rdiff.html),
[OSTree formats](https://ostreedev.github.io/ostree/formats/), [restic chunker](https://pkg.go.dev/github.com/restic/chunker),
[restic references](https://restic.readthedocs.io/en/stable/100_references.html),
[borg](https://borgbackup.readthedocs.io/en/stable/internals/data-structures.html),
[kopia](https://pkg.go.dev/github.com/kopia/kopia/repo/splitter), [bup DESIGN](https://github.com/bup/bup/blob/main/DESIGN.md),
[Xet spec](https://jedisct1.github.io/draft-denis-xet/draft-denis-xet.html), [HF dedup spec](https://huggingface.co/docs/xet/en/deduplication),
[HF upload protocol](https://huggingface.co/docs/xet/upload-protocol), [BuildBuddy](https://www.buildbuddy.io/blog/content-defined-chunking/),
[REAPI PR #282](https://github.com/bazelbuild/remote-apis/pull/282), [Dropbox](https://dropbox.tech/infrastructure/streaming-file-synchronization).

Notes on the ones that matter most for go-binsync:

- **casync** was designed for OS images with explicit goals: bounded server
  space (no N^2 deltas), CDN-friendly object sizes, no history relationship
  between seed and target. The author is explicit that it "is not an attempt
  to minimize serialization and downloaded deltas to the extreme" and that
  smaller chunks "increase HTTP GET requests and server load". Chunks are
  sharded by the first four hex digits of the digest (desync does the same).
- **desync** adds S3/GCS/HTTP stores, a write-through cache store, and
  parallel chunking (splits the input into N parts and chunks each until the
  boundaries realign; "up to 10x faster", degraded on all-zero data). Its
  `--seed` and self-seed mechanism reuse an existing local file plus its
  `.caibx`; missing chunks fail over to remote stores. Verification is
  SHA-512/256 of the *uncompressed* chunk on read.
- **bita** is the design closest to what go-binsync needs on the fetch side: one
  archive file on any range-capable HTTP server, a dictionary that records
  each chunk's offset/size in both source and archive, the target file itself
  used as seed with chunks moved in place, and "all adjacent chunks are
  fetched with a single request". Default compression is brotli-6 (odd choice
  today; zstd is available), hash is blake2.
- **zchunk** is Fedora's answer for repo metadata: per-chunk zstd with an
  optional shared dictionary stored in the file, per-chunk checksums so a
  client can fetch only differing chunks via ranges. The dictionary must stay
  constant across revisions or "changing them invalidates all previous chunks".
- **Xet / xet-core** (Hugging Face) is the most recent large-scale CAS: 64 KiB
  gearhash chunks aggregated into ≤64 MiB / ≤8192-chunk xorbs (≈1024 chunks
  typical) precisely because per-chunk objects meant "millions of requests
  are generated on each upload and download"; files are reconstructed from
  "terms" `(xorb hash, chunk range)` and downloaded as byte ranges inside
  xorbs. Global dedup queries are sampled (chunk hash % 1024 == 0, roughly one
  query per 4 MB) and answered with HMAC-protected shards. It deliberately
  limits fragmentation (prefer runs ≥8 chunks / ≥1 MB in one xorb). Reported
  results are for ML weights, not code: gemma-2-9b-it-GGUF 191 GB → 97 GB
  stored; GPT-2 two versions 1.2 GB → 645 MB (53%), "+10%" estimated from
  compression ([From files to chunks](https://huggingface.co/blog/from-files-to-chunks),
  [From chunks to blocks](https://huggingface.co/blog/from-chunks-to-blocks)).
- **Bazel REAPI blob split/splice** (Buildbarn, BuildBuddy): server-side
  `SplitBlob` returns an ordered digest list; client fetches only missing
  chunks and concatenates. Reviewer debate settled on ~0.5 MB chunks as "a
  good trade-off between space savings and metadata overhead" after 8 KB was
  proposed; concern about extra round trips and needing client/server
  agreement on the chunker ([PR #282](https://github.com/bazelbuild/remote-apis/pull/282),
  [buildbarn/go-cdc](https://github.com/buildbarn/go-cdc)). Bazel exposes it as
  `--experimental_remote_cache_chunking`.
- **Google / jj**: the only thing found is that Jujutsu's maintainer is
  "looking into CDC" for large binaries on the roadmap
  ([jj roadmap](https://docs.jj-vcs.dev/latest/roadmap/)); no Git-side design
  doc surfaced within the search budget.
- **Go/Rust libraries**: `restic/chunker` ~450-556 MB/s; `jotfs/fastcdc-go`
  ~1.3 GB/s (2.9x restic), chunk-size SD 345 KB vs 964 KB with NC-2
  ([fastcdc-go](https://github.com/jotfs/fastcdc-go)); Plakar's
  `go-cdc-chunkers` FastCDC 9.1 GB/s, UltraCDC 13.5 GB/s, JC 21.6 GB/s, and a
  *keyed* FastCDC at no cost ([plakar](https://plakar.io/posts/2025-07-11/introducing-go-cdc-chunkers-chunk-and-deduplicate-everything/));
  Rust `fastcdc` crate has v2016/v2020/ronomon variants and stream APIs.
  Keyed chunking is relevant because CCS'25 showed key-recovery attacks on the
  ad-hoc keyed chunkers of Borg, Bupstash, Duplicacy, Restic and Tarsnap
  ([Breaking and Fixing CDC](https://dl.acm.org/doi/10.1145/3719027.3744870)) —
  not a concern for a public release binary, but a reason not to invent one.
- **Merkle-tree indexes**: bup's fanout (extra checksum bits split the chunk
  list into a tree so an edit changes O(log n) tree objects), Perkeep's
  `bytes` schema with `blobRef`/`bytesRef` parts, Xet's chunk→xorb→file
  keyed-BLAKE3 tree. For a single 100 MB file with ~1,600 chunks a flat index
  (64 KB at casync's 40 B/entry) is simpler and the tree buys nothing.

---

## 3. Per-chunk compression penalty

Compressing each chunk independently discards cross-chunk context. Evidence:

- zstd manual: "a dictionary can only increase the compression of a 64KB file
  by 10 percent, compared with a 500 percent improvement for a file of less
  than 1KB" — i.e. the loss from small independent units is severe below a
  few KB and modest at 64 KiB
  ([zstd.1](https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md)).
- Measured here (8.4), missing chunks of a Go binary, zstd -19:
  - 16 KiB chunks, rodata-shift case: individually 4.11 MB vs concatenated
    3.34 MB (**+23%**); 64 KiB: 4.09 vs 3.59 MB (**+13.7%**).
  - 16 KiB, code-change case: **+9.5%**; 64 KiB: **+5.6%**.
  - On the 8 MB stdlib binary: +10% (16 KiB), +4.8% (64 KiB); a 110 KB
    dictionary trained on the *old* binary's chunks cut that to +4.5% / +2.3%.
- Mitigations in the wild:
  - **Shared dictionary** (zchunk): dictionary stored once in the archive,
    must be frozen across releases; zstd `--train` on the previous release is
    the obvious go-binsync analogue but it ties every chunk to that dictionary.
  - **Compress the concatenation** (git packs with per-object zlib but
    delta-chained; restic packs; "chunk packs"): loses random access per chunk
    unless the pack is framed (zstd frames / seekable format).
  - **Per-chunk with fast codec** (Xet: LZ4 per chunk inside xorbs; casync:
    zstd per `.cacnk`): accepts the penalty for O(1) random access.
  - **Rsyncable compression** (`zstd --rsyncable`, `gzip --rsyncable`) keeps a
    compressed stream chunk-stable at "negligible" (zstd) / ~1% (gzip) ratio
    cost, relevant only if the artifact must itself be a compressed stream.
- Framing for go-binsync: the missing-chunk *runs* are few (9-15), so compressing
  each run as one zstd frame rather than each chunk captures most of the
  concatenation benefit while still allowing range fetch per run. Better: the
  delta path in section 7 makes all of this moot for the common case.

---

## 4. Latency and request count

- **S3 numbers (AWS docs):** "consistent small object latencies (and first-byte-
  out latencies for larger objects) of roughly 100-200 milliseconds"; at least
  3,500 PUT / 5,500 GET per second per prefix; recommended parallel range
  fetches of 8-16 MB; single range per request
  ([AWS design patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html),
  [AWS guidelines](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html)).
  A community EC2 benchmark states "S3's 90th percentile time to first byte is
  typically around 20 ms regardless of the object size" in-region
  ([dvassallo/s3-benchmark](https://github.com/dvassallo/s3-benchmark)).
  Pricing: $0.0004 per 1,000 GET, $0.005 per 1,000 PUT, $0.09/GB egress
  ([S3 pricing](https://aws.amazon.com/s3/pricing/)) — request cost is noise.
- **Multi-range:** S3 and GCS support one byte range per request; nginx/Apache
  accept `multipart/byteranges` up to ~8 KB of header, Caddy ~1 MB
  ([bdon.org](https://bdon.org/2025/01/13/multiple-http-ranges)). So "one
  request for all missing ranges" only works on a real HTTP server or CDN.
- **HTTP/2:** GCS documents HTTP/1.1, HTTP/2 and HTTP/3 on its endpoints
  ([GCS request endpoints](https://docs.cloud.google.com/storage/docs/request-endpoints));
  S3's direct REST endpoint has no documented HTTP/2 support (community
  threads say to front it with CloudFront) — treat as unverified. Multiplexing
  helps head-of-line latency but not per-request server time.
- **Arithmetic for a 100 MB binary at 64 KiB chunks (~1,600 chunks):**
  - Index: 64 KB (casync) — one GET.
  - Per-chunk objects, 60% missing ≈ 960 GETs. At 10-way concurrency
    (desync default) and 100 ms each ≈ **10 s** of latency-bound time from a
    laptop; ~2 s in-region at 20 ms. 4 KiB chunks make it 15x worse.
  - Coalesced ranges into one pack: ~10 runs ≈ 10 GETs ≈ **0.2-1 s**, and the
    bytes are the same.
  - Delta path: 1 GET for a manifest, 1 GET for a ~70-250 KB patch.
- Designs that already reflect this: Xet xorbs ("reduce CAS entries by a
  factor of 1,000"), bita's coalescing, zchunk/zsync ranges, git/restic packs,
  OSTree static deltas (motivated by "one HTTP request per changed file"),
  casync's `--chunk-size` guidance and its CDN-friendliness goal. desync's
  README notes OCI registries need "multiple round-trips per chunk", hence its
  concurrency default.

---

## 5. Integrity and verification

| System | Per-chunk | Whole output | Notes |
|---|---|---|---|
| casync/desync | SHA-512/256 of uncompressed chunk, verified on read | implicit: index lists every chunk id; no separate whole-file digest in `.caibx` | `desync verify-index`, `skip-verify` only for proxies |
| bita | blake2 (64 B) per chunk before write, from seed or archive | `source_checksum` in dictionary; `--verify-header` | archive header itself hashed |
| zchunk | per-chunk checksum of compressed (and optionally uncompressed) bytes | header checksum + whole-data checksum + optional signatures | SHA-512/128 truncation option to shrink index |
| zsync/rsync | weak rolling + truncated strong sum per block | whole-file SHA-1 (zsync) / MD4 (rsync) | truncated sums are probabilistic; final whole-file check is the guarantee |
| Xet | chunk hash (keyed BLAKE3) | file hash = Merkle root over chunks; term "verification hashes" | proves possession without revealing hashes |
| BLAKE3 + Bao | 1 KiB leaves, Merkle tree | root hash | verified streaming/slicing; encoded 1 MB → ~1.06 MB, outboard ~62 KB ([bao](https://github.com/oconnor663/bao)) |
| bup / git | SHA-1 per object | tree/commit hash | Merkle by construction |

For go-binsync: per-chunk hashes are needed only to *select* and *verify fetched*
data; the correctness guarantee should be a whole-file hash of the assembled
binary (BLAKE3 at multiple GB/s or SHA-256; 100 MB is well under 100 ms), signed
in the manifest. A Merkle root (BLAKE3/Bao) additionally allows verifying
partial fetches against the same root — useful if the client streams ranges
straight into the target and wants to fail fast — but a flat list of chunk
hashes plus one whole-file hash is equivalent for a single 100 MB file and
simpler. Do the swap atomically (write to temp, fsync, rename) as desync's
local store does for chunks.

---

## 6. Dedup + delta of similar chunks

The backup literature already concluded that chunk dedup leaves ~2x on the
table and recovers it with delta compression of *similar* (non-identical)
chunks:

- **DARE** (Xia et al. DCC'14): "duplicate-adjacency" resemblance detection
  (if neighbours are duplicates, the chunk between them is a delta candidate)
  plus improved super-features; "additional data reduction by a factor of more
  than 2 on top of deduplication" ([DARE](https://ieeexplore.ieee.org/document/6824428/)).
- **Ddelta** (Perf. Eval. 2014): Gear-based CDC inside delta encoding, Spooky
  hash, greedy byte-wise extension; 2.5-8x encoding and 2-20x decoding speedup
  over Xdelta/Zdelta at "comparable" ratio; Gear chunking 2.1x faster than
  Rabin; datasets GCC (43 versions, 14.1 GB), Linux (258 versions, 104 GB),
  Bench (1.54 TB) — sources, not binaries
  ([Ddelta](https://ranger.uta.edu/~jiang/publication/Journals/2014/2014-Perf%20Eval%20-Ddelta-%20A%20Deduplication-Inspired%20Fast%20Delta%20Compression%20Approach.pdf)).
- **Edelta** (HotStorage'15): word-enlarging; 3-10x faster than Ddelta/Xdelta/
  Zdelta; ratio on Linux 99.81% (Xdelta) vs 97.4-98.7% (Gear-based variants)
  ([Edelta](https://ranger.uta.edu/~jiang/publication/Conferences/2015/2015-hotstorage15-Edelta-%20A%20Word-Enlarging%20Based%20Fast%20Delta%20Compression%20Approach.pdf)).
- **Gdelta** (DCC'20 / TOS'24): 3.5-25x faster than Xdelta/Zdelta with 10-240%
  better ratio (authors' claim) ([Gdelta](https://dl.acm.org/doi/10.1145/3664817)).
- **Finesse** (FAST'19): sub-chunk features grouped into super-features;
  3.2-3.5x faster resemblance detection than N-transform SF, +41-85% system
  throughput, comparable DCR; states post-dedup delta gives "about 2x
  additional compression ratio" on backups
  ([Finesse](https://ranger.uta.edu/~jiang/publication/Conferences/2019/2019-FAST-Finesse.pdf)).
- **CARD** (2021): chunk-context-aware detection, up to 75% more redundancy,
  5.6-17.8x faster than N-transform/Finesse ([arXiv 2106.01273](https://arxiv.org/abs/2106.01273)).
- **MeGA** and follow-ups (Locality-Seeker, "Applying Delta Compression to
  Packed Datasets", "Once Rolling Hashing is Enough"): delta-friendly layouts
  and restore performance for post-dedup delta
  ([TOS 2023](https://dl.acm.org/doi/10.1145/3584663),
  [IEEE TC 2024](https://ranger.uta.edu/~jiang/publication/Journals/2024/IEEE-TC(Delta%20Compression%20Yucheng%20Zhang).pdf),
  [EuroSys'26](https://dl.acm.org/doi/10.1145/3767295.3803596)).
- Practical delta engines: bsdiff (suffix sort + bzip2; 20 min / GBs RAM for
  Chrome-size inputs per Zucchini notes), xdelta3, `zstd --patch-from`
  (release notes claim >200x faster than bsdiff at level 1 with patches
  <0.5% of file size on their corpus
  ([zstd v1.4.5](https://github.com/facebook/zstd/releases/tag/v1.4.5))),
  Zucchini (disassembly-aware). OSTree combines both worlds: CAS objects plus
  bsdiff static deltas between name/size-similar objects.

For a two-version binary update the "resemblance detection" problem is
trivial: the base for missing chunk *i* is the old chunk at the same position
(or the whole old file). Section 8.2 shows what that buys: 28-66 KB vs
10-13 MB raw missing chunks.

---

## 7. Implications for go-binsync

**Where CDC does badly, stated plainly.** For a Go server binary, any change
that alters the size of `.text` or `.rodata` — which is every change except a
same-length constant edit — invalidates 40-70% of CDC chunks at 4-64 KiB and
80-93% of aligned 4 KiB pages. This is not a chunker-tuning problem (FastCDC
nc=0 vs nc=2, 4 KiB vs 256 KiB all land in the same band) and it is not fixed
by rsync-style rolling matching either (26-42% unmatched at 512 B). The bytes
change because addresses change. Every source that has measured executables
(bsdiff 2003, Courgette 2009, debdelta, REAPI PR #282's 75%, Rugix) agrees.
Post-2016 CDC research is about *speed* on backup corpora; none of it moves
this number.

**Where CDC/CAS is still the right substrate.**

1. *Storage layout:* content-addressed chunks or packs let the object store
   keep N releases at ~(1 + small) x size, and let any client with *any* older
   version find a base, without the O(N^2) delta explosion casync warns about.
2. *Fallback and cold start:* a host with no prior version, or one whose local
   binary is corrupt or from an unknown build, downloads by manifest and gets
   resumability and per-chunk verification for free.
3. *Same-length edits and non-code sections:* `.noptrdata`, `.data`,
   `.typelink` and most of `.text` on a rodata-only change survive; a
   same-length string edit is 2 chunks. Cheap to exploit, rarely the case.

**Recommended shape (hybrid), in priority order.**

- Publish per release: the full binary (for cold start), a small manifest
  (whole-file BLAKE3/SHA-256, size, section table, chunk list with offsets and
  hashes, list of available delta bases), and *deltas from the last K
  releases* generated at publish time with `zstd --patch-from` (fast, streaming
  decode, ~70-250 KB here) or bsdiff (smallest, 28-66 KB, slower to generate).
  The client picks the delta whose base hash matches its local file; one GET
  for the manifest, one for the patch.
- If no matching base: fall back to chunk sync against the **pack** form of
  the full binary using coalesced range requests (bita model), never per-chunk
  objects on S3/GCS. Runs of missing chunks number ~10-15 even when hundreds
  of chunks are missing, so request count stays small; use 16-64 KiB chunks
  (FastCDC NC-2, min = avg/4, max = avg*4) and compress per *run* or use a
  seekable zstd frame per chunk if random access is needed.
- Build the binary to be delta-friendly: `-trimpath -ldflags="-s -w
  -buildid="`. Unstripped Go binaries carry zlib-compressed DWARF whose every
  byte changes on any edit (bsdiff 4.9 MB vs 66 KB stripped for the *same*
  one-line change). Reproducible builds also mean "no change" really is 0 B.
- Verify the assembled output with the whole-file hash from the signed
  manifest before the atomic rename; per-chunk hashes are for selection and
  early failure only.
- Section-aware tricks (delta `.text`/`.rodata`/`.gopclntab` separately,
  pointer-relativisation à la Courgette/Zucchini) would cut the delta further
  but are second-order once patch sizes are already <1% of the binary.

**Caveats on the experiment.** One project (VictoriaMetrics), one toolchain
(Go 1.26.4, linux/amd64), three synthetic one-line edits, FastCDC with a
fixed random Gear table, no PGO or CGO. Real releases bundle many edits and
dependency bumps, which push CDC reuse *down* further, and the delta sizes
up. Rugix's numbers (delta becomes worse than a full compressed image on
major upgrades) argue for always comparing patch size against the compressed
full binary at publish time and shipping whichever is smaller.

---

## 8. Experiment: CDC, rsync-style, and delta on one-line Go changes

Reproducible from the notes below; all runs on this workstation, 2026-08-26.

### 8.1 Setup

- Subject A: 8.1 MB Go binary importing ~20 stdlib packages (`net/http`,
  `crypto/tls`, `go/types`, ...), `-trimpath`, default (unstripped) link.
- Subject B: VictoriaMetrics `app/victoria-metrics` at commit `cbb34395`,
  `-trimpath -mod=vendor`, 25.6 MB unstripped; 18.6 MB with
  `-ldflags="-s -w -buildid="`.
- Variants: **A** same-length edit of a string constant; **B** string constant
  lengthened by 13-15 bytes (rodata shift); **C** one extra `if` statement (text
  shift of 128-200 bytes).
- Chunker: FastCDC-2016 (Gear, cut-point skipping, NC level 0 or 2), min =
  avg/4, max = avg*4; chunk identity = SHA-256; "missing" = chunk of new file
  whose hash is absent from the old file. "runs" = maximal contiguous runs of
  missing chunks. rsync-style = old file hashed in aligned fixed blocks,
  new file scanned at every byte offset, greedy match.
- Delta encoders: `bsdiff` 4.3, `xdelta3 -9`, `zstd -19 --patch-from`.

### 8.2 Chunk reuse (stripped VictoriaMetrics, 18,638,944 B for all variants)

| Scheme | B: +15 B string (missing / runs) | C: +1 statement (missing / runs) |
|---|---|---|
| positional bytes differing | 31.9% (85.6% of 4 KiB pages) | 40.1% (93.0% of pages) |
| FastCDC 4 KiB NC-2 | 48.8% / 131 | 60.2% / 79 |
| FastCDC 16 KiB NC-2 | 54.0% / 15 | 64.0% / 15 |
| FastCDC 64 KiB NC-2 | 57.3% / 9 | 67.9% / 9 |
| FastCDC 256 KiB NC-2 | 69.8% / 5 | 79.0% / 5 |
| rsync-style 512 B blocks | 26.2% unmatched | 41.7% unmatched |
| rsync-style 4 KiB blocks | 46.1% unmatched | 58.6% unmatched |
| bsdiff patch | **28,479 B** | **66,265 B** |
| zstd -19 --patch-from | 66,833 B | 222,810 B |
| full binary, zstd -19 | 5,985,069 B | 5,985,245 B |

Unstripped (25.6 MB) VictoriaMetrics, same edits: CDC 64 KiB NC-2 43.9% /
13 runs (B) and 71.2% / 12 runs (C); FastCDC 4 KiB 36.2% (B) / 63.8% (C);
fixed 4 KiB aligned 75.8% / 88.8%; bsdiff 51,768 B (B) but **4,878,625 B**
(C) — the DWARF effect; xdelta3 138 KB / 5.17 MB; zstd patch 93 KB / 4.94 MB;
full zstd -19 11.4 MB.

Same-length edit (variant A, both subjects): 98-101 bytes differ positionally
(~80 of them the Go build id), 2 CDC chunks missing at every size (0.1-2.6%),
rsync 512 B leaves 1.6 KB unmatched, bsdiff 310-329 B, zstd patch 1.2-3.2 KB.

8 MB stdlib subject, unstripped: B → CDC 64 KiB 61-62% missing / 7-9 runs,
rsync 512 B 22%, bsdiff 528 KB, zstd patch 535 KB, full zstd 4.1 MB; C → CDC
64 KiB 78-88%, rsync 512 B 38.5%, bsdiff 1.47 MB, zstd patch 1.46 MB.

### 8.3 Where the churn is (per ELF section, positional diff, VictoriaMetrics)

| Section | size | B: bytes differing | C: bytes differing |
|---|---|---|---|
| `.text` | 8,577,801 | 18,027 (0.2%) | 1,167,210 (13.6%) |
| `.rodata` | 3,522,392 | 1,342,685 (38.1%) | 1,658,348 (47.1%) |
| `.gopclntab` | 5,867,114 | 6,428 (0.1%) | 3,450,042 (58.8%) |
| `.itablink` | 5,528 | 778 (14.1%) | 778 (14.1%) |
| `.noptrdata` | 482,881 | 0 | 44 |
| `.data` | 99,474 | 692 (0.7%) | 1,869 (1.9%) |
| `.debug_info` (unstripped) | 1,950,026 | 18,330 (0.9%) | 1,940,853 (99.5%) |
| `.debug_line` (unstripped) | 1,226,060 | 0 | 1,220,697 (99.6%) |

Note the rodata-only edit (B) leaves `.text` and `.gopclntab` almost intact but
still rewrites 38% of `.rodata` (absolute pointers in type/func descriptors),
which is why CDC still loses half the file.

### 8.4 Per-chunk compression penalty (zstd -19, missing chunks only)

| Case | chunk avg | missing chunks | raw | each chunk separately | concatenated | penalty |
|---|---|---|---|---|---|---|
| VM unstripped B | 16 KiB | 463 | 10.42 MB | 4.106 MB | 3.338 MB | +23.0% |
| VM unstripped B | 64 KiB | 124 | 11.26 MB | 4.086 MB | 3.592 MB | +13.7% |
| VM unstripped C | 16 KiB | 791 | 17.24 MB | 9.670 MB | 8.831 MB | +9.5% |
| VM unstripped C | 64 KiB | 210 | 18.27 MB | 9.737 MB | 9.217 MB | +5.6% |
| 8 MB stdlib B | 64 KiB | 52 | 4.99 MB | 2.333 MB | 2.227 MB (+dict: 2.277) | +4.8% (+2.3%) |
| 8 MB stdlib C | 16 KiB | 232 | 5.48 MB | 3.061 MB | 2.886 MB (+dict: 2.965) | +6.1% (+2.8%) |

Read against the delta column in 8.2: even the concatenated, compressed missing
chunks (3.3-9.2 MB) are 50-140x larger than a bsdiff/zstd patch (28-223 KB)
and only 1.3-2x smaller than the full compressed binary (6.0 MB stripped).

---

## References

- Xia et al., "FastCDC: a Fast and Efficient Content-Defined Chunking Approach for Data Deduplication", USENIX ATC 2016. https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf
- Xia et al., "The Design of Fast Content-Defined Chunking for Data Deduplication Based Storage Systems", IEEE TPDS 31(9), 2020. https://csyhua.github.io/csyhua/hua-tpds2020-dedup.pdf
- "A Thorough Investigation of Content-Defined Chunking Algorithms for Data Deduplication", arXiv:2409.06066 (2024). https://arxiv.org/abs/2409.06066
- Rick Winfrey, "A Deep Dive into FastCDC". https://rickwinfrey.com/writings/content-defined-chunking-part-2
- "The Chonkers Algorithm", arXiv:2509.11121. https://arxiv.org/abs/2509.11121
- NetApp SS-CDC. https://github.com/NetApp/SS-CDC
- Truong et al., "Breaking and Fixing Content-Defined Chunking", CCS 2025. https://dl.acm.org/doi/10.1145/3719027.3744870 ; "Chunking Attacks on File Backup Services using CDC", arXiv:2504.02095. https://arxiv.org/abs/2504.02095
- Percival, "Naive Differences of Executable Code" (bsdiff), 2003. https://www.daemonology.net/papers/bsdiff.pdf ; https://www.daemonology.net/bsdiff/
- Chromium, "Software Updates: Courgette". https://www.chromium.org/developers/design-documents/software-updates-courgette/ ; Zucchini. https://chromium.googlesource.com/chromium/src/+/HEAD/components/zucchini/
- debdelta. https://debdelta.debian.net/
- Hydraulic, "Deltas diffed". https://hydraulic.dev/blog/20-deltas-diffed.html
- Rugix, "Efficient Delta Updates". https://rugix.org/blog/efficient-delta-updates/
- Zhao et al., "Large-Scale Analysis of the Docker Hub Dataset", CLUSTER 2019. https://par.nsf.gov/biblio/10167826-large-scale-analysis-docker-hub-dataset ; DupHunter, ATC 2020. https://www.usenix.org/conference/atc20/presentation/zhao
- bazelbuild/remote-apis PR #282 "Add blob split and splice API". https://github.com/bazelbuild/remote-apis/pull/282 ; issue #326. https://github.com/bazelbuild/remote-apis/issues/326 ; buildbarn/go-cdc. https://github.com/buildbarn/go-cdc
- BuildBuddy, "Remote Cache CDC: Reusing Bytes". https://www.buildbuddy.io/blog/content-defined-chunking/
- casync. https://github.com/systemd/casync ; Poettering, "casync — A tool for distributing file system images". https://0pointer.net/blog/casync-a-tool-for-distributing-file-system-images.html ; src/cachunker.h, src/cachunker.c, src/caformat.h, doc/casync.rst (raw.githubusercontent.com/systemd/casync/main/...)
- desync. https://github.com/folbricht/desync ; local.go. https://raw.githubusercontent.com/folbricht/desync/master/local.go
- bita. https://github.com/oll3/bita ; bitar docs. https://docs.rs/bitar/latest/bitar/ ; src/cli.rs ; bitar/proto/chunk_dictionary.proto
- zchunk. https://github.com/zchunk/zchunk ; zchunk_format.txt ; Dieter, "What is zchunk?". https://www.jdieter.net/posts/2018/05/31/what-is-zchunk/ ; createrepo_c PR #92. https://github.com/rpm-software-management/createrepo_c/pull/92
- zsync paper. http://zsync.moria.org.uk/paper/
- rsync technical report. https://rsync.samba.org/tech_report/ ; librsync rdiff. https://librsync.github.io/page_rdiff.html
- OSTree formats (static deltas). https://ostreedev.github.io/ostree/formats/
- restic chunker. https://pkg.go.dev/github.com/restic/chunker ; restic CDC blog. https://restic.net/blog/2015-09-12/restic-foundation1-cdc/ ; restic references. https://restic.readthedocs.io/en/stable/100_references.html
- borg data structures. https://borgbackup.readthedocs.io/en/stable/internals/data-structures.html
- kopia splitter. https://pkg.go.dev/github.com/kopia/kopia/repo/splitter
- bup DESIGN. https://github.com/bup/bup/blob/main/DESIGN.md ; Perkeep bytes schema. https://perkeep.org/doc/schema/bytes
- fastcdc (Rust). https://docs.rs/fastcdc/latest/fastcdc/ ; jotfs/fastcdc-go. https://github.com/jotfs/fastcdc-go ; Plakar go-cdc-chunkers. https://plakar.io/posts/2025-07-11/introducing-go-cdc-chunkers-chunk-and-deduplicate-everything/
- Xet protocol draft. https://jedisct1.github.io/draft-denis-xet/draft-denis-xet.html ; HF Xet dedup spec. https://huggingface.co/docs/xet/en/deduplication ; upload protocol. https://huggingface.co/docs/xet/upload-protocol ; "From Files to Chunks". https://huggingface.co/blog/from-files-to-chunks ; "From Chunks to Blocks". https://huggingface.co/blog/from-chunks-to-blocks
- Dropbox, "Streaming File Synchronization". https://dropbox.tech/infrastructure/streaming-file-synchronization
- [MS-RDC] overview. https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdc/023c996d-4fb1-4107-8914-f142f0e3ba9a ; H3 hash. https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdc/baa2754e-50c0-49ec-9b82-9478201445b0 ; Teodosiu et al., "Optimizing File Replication over Limited-Bandwidth Networks using RDC". https://www.microsoft.com/en-us/research/publication/optimizing-file-replication-over-limited-bandwidth-networks-using-remote-differential-compression/
- Jujutsu roadmap. https://docs.jj-vcs.dev/latest/roadmap/
- git pack format. https://git-scm.com/docs/pack-format
- Xia et al., DARE, DCC 2014. https://ieeexplore.ieee.org/document/6824428/
- Xia et al., Ddelta, Performance Evaluation 79, 2014. https://ranger.uta.edu/~jiang/publication/Journals/2014/2014-Perf%20Eval%20-Ddelta-%20A%20Deduplication-Inspired%20Fast%20Delta%20Compression%20Approach.pdf
- Xia et al., Edelta, HotStorage 2015. https://ranger.uta.edu/~jiang/publication/Conferences/2015/2015-hotstorage15-Edelta-%20A%20Word-Enlarging%20Based%20Fast%20Delta%20Compression%20Approach.pdf
- Tan et al., Gdelta / "The Design of Fast Delta Encoding", ACM TOS 2024. https://dl.acm.org/doi/10.1145/3664817
- Zhang et al., Finesse, FAST 2019. https://ranger.uta.edu/~jiang/publication/Conferences/2019/2019-FAST-Finesse.pdf ; TOS 2023 extension. https://dl.acm.org/doi/10.1145/3584663
- CARD, arXiv:2106.01273. https://arxiv.org/abs/2106.01273
- "Applying Delta Compression to Packed Datasets", IEEE TC 2024. https://ranger.uta.edu/~jiang/publication/Journals/2024/IEEE-TC(Delta%20Compression%20Yucheng%20Zhang).pdf ; "Once Rolling Hashing is Enough", EuroSys 2026. https://dl.acm.org/doi/10.1145/3767295.3803596
- zstd manual (dictionary, --rsyncable, --patch-from). https://github.com/facebook/zstd/blob/dev/programs/zstd.1.md ; zstd v1.4.5 release notes. https://github.com/facebook/zstd/releases/tag/v1.4.5
- AWS S3 performance. https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html ; guidelines. https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html ; pricing. https://aws.amazon.com/s3/pricing/ ; dvassallo/s3-benchmark. https://github.com/dvassallo/s3-benchmark
- bdon, "How many ranges can you fit in one request". https://bdon.org/2025/01/13/multiple-http-ranges
- GCS request endpoints (HTTP/2, HTTP/3). https://docs.cloud.google.com/storage/docs/request-endpoints
- BLAKE3 / Bao. https://github.com/oconnor663/bao

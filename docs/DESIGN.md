# binsync — design

Architecture and reasoning behind the behaviour specified in `README.md`.
Measurements are in `docs/research/` (index: `docs/research/README.md`).
This is the second-round design: the first round (`docs/archive/DESIGN-round1.md`)
was over-scoped, and this document records what was cut and why (§10) as
well as what stays.

Status: design phase, nothing implemented. Numbers quoted below were measured
on 2026-08-26 (linux/amd64; Go 1.26.4 for the first two research rounds,
Go 1.27.0 for the codec prototype's final pass, §3.2) unless marked *estimate*.

---

## 1. Workload and assumptions

| Assumption | Value used for design | Source |
|---|---|---|
| Binary | Go, linux/amd64, stripped (`-s -w`), non-PIE; 30 MB typical, 100–250 MB common, up to 1 GB | user; corpus in `benchmark-scale.md` |
| Change per release | *not* one line: several lines across several packages (a normal patch release); sometimes a minor release with dependency bumps | user |
| Release cadence | many per day; targets are usually one release behind, occasionally several | user |
| WAN | 5–100 Mbit/s, 100–300 ms RTT, 0–2 % packet loss | user; netem profiles A–D in `benchmark-scale.md` |
| Fleet | many targets pulling from one store; no coordination between targets | user |
| Trust | the store endpoint is authenticated by its transport (TLS, SigV4, SSH, local fs); no separate signing key | user (§8) |

What the measurements say about that workload:

- **How much of the file changes.** A multi-package edit that grows `.text`
  by 2.3 KB (`v1→v5`, 29.6 MB) changes **70 %** of the bytes in ~1.95 M
  runs (median run 3 B); a one-line edit changes 13 %. Both are the same
  mechanism: every function after the first grown one moves by a multiple of
  32 B, and every PC-relative reference crossing a moved boundary is
  rewritten; `.gopclntab` (35 % of the file) is offset-based and is rewritten
  almost entirely. Chunk-based transfer (CDC/CAS) therefore re-sends ~90 % of
  a full download (`cdc-cas.md`, `benchmark-local.md`) and is not used.
- **Generic delta encoders.** bsdiff-class approximate matching gets a real
  patch release of an 88 MB binary (kube-apiserver 1.36.3→1.36.4) down to
  **2.06 MB** against a 19.1 MB full download (10.8 %); terraform 5.4 MB /
  25.5 MB; a minor release (prometheus 3.13→3.14) 8.6 MB / 24.3 MB; the
  multi-package synthetic `v1→v5` 470 KB / 8.4 MB. But bsdiff needs ~12× the
  input in RAM and 50–190 s for ~90 MB, and took 267 s and 3.4 GB for a
  393 MB stripped binary; hdiffz manages 46 s at 1.7 GB RSS for a 537 MB
  file (`benchmark-scale.md`).
- **The Go-aware transform (§3).** Aligning old and new by *function name*
  (from `.gopclntab`) and re-deriving the layout-induced churn removes most of
  those bytes: on Go 1.27, one-line 150,475 → 2,207 B (68×; the new sorted
  `.go.type` section makes bsdiff 2.5× worse than on 1.26 while the Go-aware
  patch is unchanged), multi-package `v1→v4` 145,205 → 2,733 B (53×), and
  prometheus 3.13.1→3.13.2 built with Go 1.27 2,691,644 → 111,552 B (24×),
  encoded, decoded and byte-verified end to end, in 2.1 s. The remaining
  bytes are mostly genuinely changed code, plus new type descriptors and the
  pc tables of changed functions (`go-aware-transform.md` §11).
- **The link.** At 20 Mbit/s / 200 ms / 1 % loss one TCP connection carries
  ~1.2 Mbit/s: an 8.4 MB full download takes 57 s single-stream and 7.4 s
  8-way; a 60 KB patch takes 1.0 s; a 1.76 MB patch 8 s. At 5 Mbit/s / 300 ms
  / 2 % loss: full 132 s single (27 s 8-way), 60 KB 1.5 s, 1.76 MB 22 s. A
  conditional GET that returns 304 costs one RTT (0.2–0.6 s)
  (`benchmark-scale.md`, netem section).

Design consequences: patch bytes dominate end-to-end latency on a bad link,
so the codec is where the effort goes; anything larger than a few hundred KB
must be fetched with parallel ranges and be resumable; the poll must be one
conditional GET; and the encoder must scale to 1 GB inputs in seconds, not
minutes, without a whole-file suffix array.

## 2. System overview

Three roles, one store:

```
publisher (CI or workstation)            store (S3 / HTTPS / file / SSH dir)          targets (fleet)
  binsync publish bin s3://…   ──put──▶   blobs/<hash>.blob       (immutable)  ◀─get──  poll latest.json (conditional GET)
                                          patches/<from>-<to>.bsz (immutable)          fetch chain or blob, apply, verify
                                          latest.json             (CAS-replaced)       install, restart, check, or revert
```

Two target shapes share the same `agent` package:

- **Embedded** (`selfupdate`): the service links the library; the old process
  polls, installs and execs the new binary with its listening sockets
  inherited. Zero downtime, no external process, one writable directory.
- **External** (`binsync agent`): a sidecar polls and installs, then runs a
  user command (`--restart`) and optionally a health check (`--healthy`). For
  services that cannot link the library.

Everything below the store is pull-based; there is no push channel, no
per-target registry and no coordinator. Fleet-wide state is "whatever each
target's file hashes to".

## 3. Codec: `bsz` (Go-aware predict-then-correct)

### 3.1 Principle

A patch has two parts:

1. **Prediction inputs** — a compact description of the new binary's
   *layout* (function order and sizes; where data blocks moved), from which
   the decoder rebuilds, deterministically, a *predicted* new binary out of
   the old one: every function relocated to its new address with every
   PC-relative operand re-targeted, every offset-based table regenerated.
2. **Correction** — the difference between the prediction and the real new
   binary, encoded positionally and compressed.

The prediction only has to be deterministic, not correct: an imperfect
prediction costs bytes in the correction, never correctness, because the
output is verified against the release hash before use. This is the
Courgette/Zucchini idea, but keyed on Go's own metadata rather than on
disassembly heuristics: `.gopclntab` survives `-s -w` and names every
function with its entry offset and size, which gives an exact
function-by-function correspondence between releases and the exact new
layout. Nothing in the pipeline needs symbols, DWARF or relocations.

### 3.2 Pipeline

Encoder and decoder run the same prediction; the encoder additionally has the
real new file and emits the correction.

```
parse(old)            ELF sections; pclntab (funcs: name, entry, size, pc tables); moduledata
                      ─ unsupported (non-Go, non-amd64, PIE, not the supported Go release — D14): plain codec §3.8
match(old, new)       new function j ↔ old function i by name (exact; then normalised for
                      closure/deferwrap/generic-instantiation numbering; then by content hash)
layout table          for each new function in order: {same-as-next-old | old index | new name}, Δsize
data maps             per data section (.rodata .noptrdata .data): piecewise-constant shift of old
                      16-byte blocks to their new offsets (run-length: (block, Δ) pairs);
                      blocks containing pointers may only shift by multiples of 8
shift tables          .bss/.noptrbss: (old offset, Δ) runs derived from symbol-order deltas
predict .text         for each new function: copy the matched old body to its new entry;
                      decode x86 (x/arch/x86asm); rewrite every rip-relative / rel32 operand
                      through {function map, data maps, shift tables}; unmatched → zero-filled slot
predict .gopclntab    regenerate functab (entryOff), findfunctab, _func.entryOff, and re-base
                      nameOff/pcdata/funcdata offsets after applying stage-1 deltas to the
                      variable-length blobs (funcnametab, cutab, filetab, pctab, gofunc)
predict data          re-lay blocks through the data maps; rewrite absolute pointers into
                      .text/.rodata/.data/.bss through the same maps
predict type descs    walk the descriptors and itabs from moduledata (Go 1.27: `.go.type`,
                      `typedesclen`/`itaboffset`); rewrite nameOff/typeOff/textOff/ptrToThis and
                      method tables through the same maps — nothing extra is transmitted
correction            new ⊖ predicted, positionally; inside each differing region an exact
                      local match over the prediction's own bytes there; brotli/zstd (§3.5);
                      body split into 8 MiB frames
```

Every Go-aware row was encoded to a patch file, decoded from the old binary
and byte-verified; layout tables, data maps and stage-1 blobs are transmitted
and counted (nothing is an oracle input). The prototype column is
`bench/gotransform/` (`go-aware-transform.md` §10–11); **v1** is this
repository's `delta` package, including its patch header and frame table:

| Pair (toolchain) | bsdiff | hdiffz -p-8 | prototype | **v1** | vs bsdiff |
|---|---:|---:|---:|---:|---:|
| one-line (`v1→v2c`, 29.6 MB, Go 1.27) | 150,475 | 176,929 | 2,207 | **2,262** | 67× |
| +3-byte string (`v1→v2l`, 1.27) | 24,874 | 33,713 | 566 | **438** | 57× |
| multi-package (`v1→v4`, 1.27) | 145,205 | 171,760 | 2,733 | **2,745** | 53× |
| `v3→v4` (1.27) | 30,196 | 40,523 | 578 | **440** | 69× |
| prometheus 3.13.1→3.13.2, built with Go 1.27 (94 MB) | 2,691,644 | 2,719,152 | 111,552 | **95,366** | **28×** |

The real pair is 14 % below the prototype and within 1 % of the 94,470 B that
an hdiffz stage 2 reached (the range §3.4 was aiming for), from an encoder
with no suffix array and a decoder that applies in place. The synthetic pairs
move both ways: the two large ones are tens of bytes above the prototype,
paying for the container (a 120-byte header where the prototype wrote none)
and for the second stage-1 blob's floor, and the two small ones are ~23 %
below it, where a patch that is mostly floor gains most from the brotli tier
(§3.5) and from predicting the unallocated bytes (§7).

The official Go 1.26 builds (prometheus 3.13.1→3.13.2: 291,214 B, 9.3×;
kube-apiserver 1.36.3→1.36.4: 292,972 B, 7.0×) were measured with the
previous codec revision — before pc-table regeneration and pointer consensus —
and were not re-run; the codec targets Go 1.27 only (D14).

Earlier oracle-map pass (§7.5; maps not transmitted, no decoder, now
superseded): terraform 1.15.8→1.15.9 (Go 1.25) 5,427,575 → 1,990,549 (2.7×);
cockroach 26.2.4→26.2.5 (Go 1.25, cgo) 8,588,309 → 4,190,756 (2.0×);
prometheus 3.13.2→3.14.0 minor release (hdiffz) 8,599,007 → 5,416,050 (1.6×
— 6,326 new functions; content-dominated, as it should be).

Go 1.27 moved type descriptors and itabs out of `.rodata` into a sorted
`.go.type` section full of `textOff` fields, which makes generic deltas 2.5×
*worse* on 1.27 for the same change (bsdiff 60,478 → 150,475 B for the
one-liner) and leaves the Go-aware patch unchanged. Three regenerations took
the real pair from ~7× to 24×: the type-descriptor offsets
(nameOff/typeOff/textOff, walking the descriptors from `moduledata`; without
it prometheus/1.27 is 960,168 B), the pc tables (`pctab`, `cutab`,
`go:func.*` are emulated from the new function layout instead of being sent
as a generic delta — they were 100,949 B, 42 % of the patch), and pointer
shifts chosen by majority vote among all pointers into the same symbol
(which fixed a 5-byte-off shift into the short, repetitive `runtime.gcbits`
data that had held `v3→v4` at 4.7×).

What is left, on v1's prometheus/1.27 patch. By stream (raw → compressed on
its own; the patch ships them in one frame, which is why the parts come to
95,232 B and the file to 95,366 B):

| stream | raw | compressed |
|---|---:|---:|
| header | – | 120 |
| layout | 239,371 | 10,379 |
| stage 1a (`funcnametab`+`filetab`) | 2,205 | 1,360 |
| stage 1b (`cutab`+`pctab`+`go:func.*`) | 38,216 | 17,441 |
| stage 2 (whole file) | 119,493 | 66,052 |

And by where the prediction was wrong — 120,852 bytes of a 93.8 MB file,
0.13 %:

| section | mispredicted |
|---|---:|
| `.text` | 63,179 |
| `.go.type` | 45,170 |
| `.gopclntab` | 4,549 |
| `.rodata` | 3,782 |
| `.go.func` | 2,568 |
| `.data`, `.noptrdata`, `.go.buildinfo`, `.go.module`, `.go.fipsinfo` | 1,495 |
| ELF program and section headers | 109 |

`.text` and `.go.type` are 90 % of it and mostly real change: the 52 new
functions and the descriptors that came with them. The remainder is the
open question in §11.1.

Encoder and decoder cost (`/usr/bin/time`, one run each; prometheus/1.27,
94 MB): v1 encode 2.4 s / 902 MB (x86 decoding runs on all cores; the profile
is 38 % `x86asm`, 30 % content-index build); bsdiff 39–43 s / 805 MB;
hdiffz -p-8 7.4 s / 388 MB (8 threads); full `zstd -19 -T0` 9.4 s / 364 MB.
v1 decode 0.9 s / 921 MB RSS, 714 MB of it live heap — against bspatch 0.47 s
/ 192 MB and hpatchz 0.08 s / 25 MB. The correction applies in place (§3.4),
so the decoder holds old + prediction rather than old + prediction + new, but
7.6× is still far from the ≈ 2× §11.3 wants; the rest is the pclntab work,
not the buffers. Wall-clock is no longer the encoder's problem; memory is.

Two lessons the prototype carries into the design:

- Per-Go-release layout handling is real work, not a table of constants:
  1.25 → 1.26 changed `entryOff` handling; 1.26 → 1.27 changed `moduledata`,
  removed `.typelink`/`.itablink`, added `.go.type`/`.go.func`, grew
  `MapType` by 24 B and made `go:func.*` alignment data-dependent. Hence D14:
  one supported Go release, gated by a byte-exact self-prediction check
  (old → old through the whole pipeline), plain codec for everything else.
- Prediction is a compression context, not a correctness mechanism: the
  decoder hashes the predicted base before applying the correction, so
  encoder/decoder divergence is detected, and a wrong prediction costs
  bytes, never a wrong output.

### 3.3 Why no suffix array

Once the layout table is applied the prediction has **exactly** the new
file's length and structure (every function is at its final entry, unmatched
functions as zero-filled slots of the right size, tables regenerated at the
right sizes). The layout table carries the size of every variable-length
pclntab blob and of every function, so the prediction is length-exact even
where its *content* is wrong — which is what makes the correction positional:
walk predicted and new in lockstep, emit a region where they differ. Where a
region falls inside a function whose body genuinely changed, the shifted tail
is recovered by matching against the prediction's own bytes there (§3.4),
which is the same "copy/insert/copy" bsdiff would find, at O(region) memory
and without needing the old body as a second buffer. Unmatched (new)
functions are sent literally; the compressor handles the rest.

Cost model for a 1 GB binary (600 MB `.text`, ~400 K functions): x86 decode
ran at 17–29 MB/s per core in the prototype (6 s for vault's 159 MB `.text`)
and is embarrassingly parallel per function (relocation with 24 goroutines:
1.6 s on vault); the `.rodata` content-map build is O(n) hashing (13 s for
95 MB single-threaded, parallelisable). Measured on the 94 MB pair, the
encoder is 2.4 s and 9.6× the input in RSS and the decoder 0.9 s and 7.6× in
live heap (§3.2) — against bsdiff's 8.6–12× and a suffix array's 5–8×, but
well above the 2–3× this section first estimated, because the working set is
dominated by the per-function pclntab structures rather than by the file
buffers (§11.3). Linear extrapolation puts a 1 GB pair at ~25 s of encode,
which a CI box can afford, and at a decoder footprint no target should be
asked for; that is the constraint §11.3 has to remove.

### 3.4 Correction format (stage 2)

The prototype's stage 2 was purely positional -- runs of `(gap, len, bytes)`
where the prediction differs from the new file. That is optimal where the
prediction is right and wasteful inside a function whose code genuinely
changed: an insertion of five bytes shifts the rest of the function, and a
positional encoder re-sends all of it. Measured on prometheus/1.27, positional
runs cost 111,552 B against the 94,470 B an hdiffz stage 2 reaches (§3.2), and
the gap is entirely inside changed `.text`.

v1 therefore encodes the correction as **positional regions, each written
whichever of two ways is smaller**:

```
correction := uvarint newLen, uvarint nRegions, uvarint ctrlLen, uvarint litLen,
              ctrl[ctrlLen], lit[litLen]                 ← two streams, compressed together
ctrl       := per region, in output order:
                uvarint gap        bytes identical to the prediction since the previous region
                uvarint span<<1|m  length of the region; m = 1 for a match stream, 0 for literals
                if m: triples until span bytes have been emitted:
                  uvarint lit      literal bytes taken from the lit stream
                  uvarint copy     bytes copied from the source cursor
                  varint  seek     signed move of the source cursor before the copy
```

Regions are maximal runs of differing bytes, merged when closer than 6 B. A
region's source window is the *prediction's own bytes* from the region's
start to `min(256, fileLen - regionEnd)` past its end, so a shifted function
tail is one `copy`. The window is not transmitted: it follows from the
region's end and the file's length, which both sides know. Most regions are
two to four bytes -- a relocation the mapper placed wrongly -- and for those
the match stream costs more than the bytes themselves, so the mode bit picks
literals and the encoding degenerates to exactly the prototype's positional
one. Control varints and literals are separate streams because their
statistics differ by an order of magnitude.

Two properties matter operationally. The source window starts at the region
and runs *forward*, into bytes no earlier region has touched, so the decoder
snapshots it, writes the region in place, and **applies the correction over
the prediction buffer** rather than holding prediction and output at once
(§11, memory). And the prediction is length-exact by construction, so `span`
is the same on both sides and every read and write is bounds-checked against
one buffer.

Measured on prometheus/1.27: 119,493 B of correction stream for 120,852
differing bytes, 66,052 B compressed -- against 79,696 B for the prototype's
purely positional stage 2.

**Stages 1a and 1b use the plain codec (§3.8), not this one.** Both are
length-changing: 1a is `funcnametab`+`filetab` old → new, and 1b's predicted
`cutab`/`pctab`/`go:func.*` are padded to the new tables' lengths but never
truncated, so a release that shrank a table leaves the prediction longer than
the truth (the decoder is told the target length). More importantly 1b's
residual is *shifted*, not positional: one pc-value table that changed length
moves every table after it, which a bounded local window cannot follow. On
prometheus, encoding stage 1b positionally costs 66,372 B compressed against
17,441 B for the plain codec (D17).

### 3.5 Compression, and the patch container (`.bsz`)

Payload streams are compressed with the smallest of the candidates the
encoder is willing to spend time on, and the choice is recorded per frame.
Measured on the prometheus/1.27 patch's own streams (pure-Go encoders,
against the `zstd -19` CLI the prototype shelled out to):

| stream | raw | `zstd -19` (CLI) | klauspost zstd best | brotli-11 (`LGWin` 24) |
|---|---:|---:|---:|---:|
| layout | 239,379 | 11,271 | 12,885 (+14 %) | **10,388 (−8 %)** |
| correction | 162,758 | 79,696 | 85,569 (+7 %) | **74,631 (−6 %)** |

Pure-Go zstd is 6–14 % *worse* than the CLI the research numbers were taken
with; pure-Go brotli at quality 11 is 6–8 % *better*. Since patch bytes are
the product, streams take the smaller of zstd and brotli, and the brotli
quality is chosen by size: 11 up to 4 MiB, 10 above.

The quality-10 tier is what blobs get, and it is the reason they are no
longer zstd. Measured on prometheus 3.13.2 (93.8 MB) in the 8 MiB frames a
blob is cut into, encoding eight-way parallel and decoding sequentially:

| frame codec | blob | encode | decode |
|---|---:|---:|---:|
| klauspost zstd, best | 23,838,795 | 2.5 s | 78 ms |
| brotli-5 | 22,963,451 | 0.4 s | 312 ms |
| brotli-9 | 22,335,195 | 1.9 s | 343 ms |
| **brotli-10** | **20,336,968** | **13 s** | **269 ms** |
| `zstd -19 -T0` (CLI, one stream, for reference) | 20,579,646 | 9.4 s | – |

Brotli-10 in independent 8 MiB frames is 15 % smaller than the zstd blob and
*smaller than a single `zstd -19` stream of the whole file*, which the frame
split was expected to cost against. The publisher pays 13 s once per release
and the target pays 269 ms; both are noise beside the 3.5 MB the target does
not download. The first draft's "blobs are always zstd, they must stream"
was an assumption about brotli's cost curve at quality 11, and quality 10 is
a different curve (D22).

Each blob frame therefore carries its own codec tag in the pointer, exactly
as patch frames do, and a blob is not a file any single decompressor reads —
hence the object key is `blobs/<hash>.blob`, not `.zst`.

```
magic "BSZ1"  u8 transform (0 = plain, 1 = go-amd64-v1)  u8 flags
header: uvarint header_len, then
        b3 from[32], b3 to[32], uvarint old_size, uvarint new_size,
        uvarint nframes, per frame: uvarint off, uvarint len, uvarint zlen,
                                    u8 codec (0 raw, 1 zstd, 2 brotli), b3 hash[32]
frames: independently decodable, each ≤ 8 MiB decompressed, concatenating in
        order to the patch body, which for transform 1 is
          layout (section table, moduledata values, function layout, pclntab
                  table offsets, data maps, shift tables, pointer overrides),
          stage-1a delta, stage-1b delta, b3 of the prediction[32],
          stage-2 correction
```

Frames are cut at 8 MiB, not at stream boundaries: a frame costs a 32-byte
hash and a table entry, and compressing the streams separately came to within
0.1 % of compressing them together on both the 94 MB pair and the one-line
one. The prediction's own hash rides in the body so that an encoder and a
decoder that disagree say so (§3.7) instead of producing a wrong binary and
relying on the release hash to catch it.

The header is a bounds-checked varint record rather than the CBOR the
first draft named: it is the only structure in the system that is not
already JSON, and 60 lines of varint reader are less surface than a codec
dependency. Each frame's BLAKE3 is in the header and the whole patch's hash
is in the pointer, so a partially fetched patch is verified frame by frame
and resumed at a frame boundary with a `Range` request. All offsets read
from the patch are bounds-checked against `old_size`/`new_size`; a malformed
patch fails verification, it cannot crash the decoder or write outside the
output.

### 3.6 Transform versioning and the decoder that is already deployed

The decoder that applies a patch is **the old binary's** embedded library (or
the deployed agent). The publisher therefore chooses the transform per patch:

- Embedded: the publisher reads the old binary's `debug/buildinfo` for the
  `binsync` module version, and uses the newest transform that version
  supports. (The old binary is in the publisher's cache; it produced the
  chain's `from` hash.)
- External: the agent version is not visible to the publisher; the current
  transform is used.
- A decoder that meets a transform it does not support **falls back to the
  blob** — a full download, never a failure. Same for a Go version whose
  pclntab format the decoder does not know.

Format stability: the codec depends on `.gopclntab` (`runtime/symtab.go`,
magic `0xfffffff1` since Go 1.20), `moduledata` field order, and the 32-byte
function alignment. The magic is stable across 1.20–1.27 but the surrounding
layout is not: the prototype needed separate code paths for 1.25
(`entryOff`), 1.26 and 1.27 (`.go.type`, `moduledata`, `go:func.*`
alignment).
The codec therefore keys its
pclntab/moduledata/type-descriptor handling on the Go version from
`debug/buildinfo`, and **supports exactly one Go release at a time: the
current stable one (1.27 today; D14)**. A binary built by any other
toolchain gets the plain codec (§3.8). Each new Go release is enabled only
after a byte-exact self-prediction check (old → old through the full
pipeline) on a small corpus built with that release; the previous release is
kept only for as long as it costs nothing. This trades a wider compatibility
matrix — one that the prototype showed is real work per Go minor — for a
codec that is small enough to be reviewed and tested exhaustively; a fleet
that pins an older Go still updates correctly, just with larger patches.

### 3.7 Determinism and safety

- The prediction uses only the old file plus bytes from the patch; no
  environment, no clock, no randomness. Encoder and decoder share the code —
  literally: `encodeGoAMD64` builds its prediction by decoding the layout it
  just encoded and running the decoder's own functions on it, so the bytes
  the correction is measured against are the bytes the decoder will produce.
- Anything that could vary with the machine is a compile-time constant, not a
  core count: the worker counts in the parallel map build and the parallel
  relocation are fixed at 24, because a prediction that depends on `GOMAXPROCS`
  is a prediction two hosts can disagree about.
- The encoder puts the prediction's BLAKE3 in the patch and the decoder checks
  it before applying the correction. A mismatch is a clean, named failure —
  "encoder and decoder disagree, fetch the blob" — rather than a wrong file
  caught only by the release hash.
- Enabling a Go release is gated on a byte-exact self-prediction: every corpus
  binary is run old → old through the whole pipeline and must come out
  identical, with a correction of at most 64 bytes (`TestSelfPrediction`).
  That gate is what caught `pcHeader.textStart`, which Go 1.27 leaves zero
  and the codec was filling in — 8 bytes per patch that no round-trip test
  would ever have shown, because the correction dutifully fixed them.
- Any decode failure of an instruction (1,616 undecodable bytes in
  prometheus's 89 MB of `.text`) leaves those bytes unrelocated; the
  correction fixes them.
- The result is written to a temp file and BLAKE3-hashed before it is
  renamed into place; the hash is compared with `to` from the pointer.

### 3.8 Plain codec (transform 0)

For non-Go, non-amd64, PIE, or otherwise unparseable inputs: a bsdiff-class
encoder over the whole file — an approximate match extended over the bytes
around it, so a shifted region costs a control triple and a difference
stream that is zero wherever the two agree — in the same container and
frames. Two substitutions against bsdiff proper:

- **No suffix array.** The anchor search uses the same position-ordered
  content index as the rest of the codec (`delta/internal/lz`). The suffix
  array is what makes bsdiff need 9–12× the input in RAM and minutes above
  100 MB; on executables the anchors this index finds are long enough that
  bsdiff's extension heuristic does the same work from them.
- **A sparse difference stream.** bsdiff's difference stream is one byte per
  matched byte and almost all zero — 383 K non-zero bytes in 30 MB on the
  reference pair — which bzip2's block sort handles beautifully and every LZ
  compressor handles badly. Carrying the zeros as run headers instead of
  bytes takes that pair from 315 KB to 170 KB, and drops the decoder's
  working set for the stream from the size of the file to nothing.

Measured on the one-line pair (29.6 MB): 169,929 B in 2.2 s and 278 MB RSS,
against bsdiff's 150,475 B in 39–43 s and 805 MB. 13 % more bytes for 18×
the speed and a third of the memory is the right trade for a fallback; the
Go-aware codec is what the bytes are supposed to come from.

It is capped at 256 MB old-file size; above that `publish` publishes the blob
only and says so. The PIE build of the same pair, which the Go-aware codec
declines, takes this path and lands at 72,681 B.

Not doing: zstd `--patch-from` as a codec (3.5× worse than bsdiff on
one-line and on kube-apiserver), HDiffPatch (C, cgo), xdelta3 (worst sizes,
8.9× RAM).

## 4. Releases, pointer and store

### 4.1 Identity

A release is its BLAKE3-256 hash (`b3:<64 hex>`), nothing else. The version
string shown in logs comes from the binary's `debug/buildinfo` (`main` module
version or `vcs.revision`), with the hash as the identity. Reproducible builds
(`-trimpath`, `-buildid=`, `CGO_ENABLED=0`) make the hash derivable from
source, which is what makes "is the fleet on commit X" answerable.

### 4.2 Pointer: `latest.json`

The only mutable object. Small enough to fetch on every poll (≈ 1 KB + 190 B
per frame; a 1 GB blob has 128 frames):

```json
{
  "format": 1,
  "seq": 1724700000123,
  "head": { "hash": "b3:…", "version": "v1.42.0-3-gabc123", "size": 88011776,
            "blob": { "key": "blobs/….blob", "size": 19072059,
                      "frames": [ { "off": 0, "len": 8388608, "zoff": 0, "zlen": 1802331,
                                    "codec": 2, "b3": "b3:…" }, … ] } },
  "chain": [
    { "from": "b3:…prev",  "to": "b3:…head", "key": "patches/…prev-…head.bsz", "size": 358607, "b3": "…" },
    { "from": "b3:…prev2", "to": "b3:…prev", "key": "patches/…prev2-…prev.bsz", "size": 402113, "b3": "…" }
  ]
}
```

- `seq` is the publisher's wall clock in ms; a target ignores a pointer whose
  `seq` is ≤ the last one it accepted (replay protection against a stale
  cache or a rolled-back bucket; it does not need to be exactly monotonic
  across publishers, only larger).
- `chain` lists the last `max_chain` (8) edges, newest first. It is rebuilt
  from the previous pointer on every publish; older patches remain in the
  bucket but are unreachable (a lifecycle rule can delete them after 30 d).
- A target on hash `h` finds the suffix of `chain` that ends at `h`. If it
  exists and its total `size` is below the blob's, it fetches those patches
  (oldest first, applying each with verification); otherwise the blob.
- The pointer is written with `Cache-Control: no-store` and replaced with a
  compare-and-swap (`If-Match` on S3/GCS, `rename(2)` on file/ssh); a lost
  race is retried after re-reading the pointer, so two publishers cannot fork
  the chain.
- Store `format` bumps are forward-only: a target that sees an unknown
  `format` logs and keeps its current binary.

### 4.3 Objects

```
<store>/latest.json
<store>/patches/<from8>-<to8>.bsz      first 8 hex of each hash; immutable; Cache-Control: immutable
<store>/blobs/<hash>.blob              immutable; independent frames of 8 MiB input each, per-frame codec
```

Blobs are frame-split so that a target can fetch them with N parallel `Range`
requests (S3/GCS accept exactly one range per request) and resume at a frame
boundary; each frame is hashed in the pointer. Patches use the same frame
scheme inside the container (§3.4).

### 4.4 Publisher flow

```
binsync publish <bin> <store>:
  hash bin; warn (or refuse without --force) on DWARF/symtab/PIE/modified VCS tree
  read store pointer (or none) → prev = head
  if prev == hash: exit 0 ("already published")
  old = cache[prev] (fetched from the store's blob if the cache lacks it; skip patch if absent)
  patch = Encode(old, bin) unless len(patch) ≥ len(blob)   ← never publish a patch larger than the blob
  put patch, put pointer (CAS; retry on conflict), then put blob frames (parallel)
  cache[hash] = bin
```

Cache: `$XDG_CACHE_HOME/binsync/<hash>` with an LRU cap of 10 releases; a
cold cache means the first publish from a new machine fetches the previous
blob (or publishes blob-only).

Upload order is **patch → pointer → blob** (D13). Every target that is on the
chain — which is every target in the steady state — needs only the patch, and
the pointer becomes visible as soon as the patch (hundreds of KB) is up,
rather than after the blob (tens of MB). On a fast CI → bucket link the
difference is a second or two; from a workstation over the same medium link
the targets are on, it is the difference between ~5 s and ~2 min (a 19 MB
blob over one SSH stream at 1 % loss is ≈ 130 s; the SSH store cannot
parallelise it). The cost is a weaker invariant: a pointer always names an
existing *patch*, but may name a blob that is still uploading. A target that
needs the blob (drifted, or more than `max_chain` behind) and gets a 404
retries with backoff (1 s doubling to 1 min, for up to 30 min) and, if the
publisher died before the blob landed, is healed by the next publish, whose
blob it fetches instead. Patch-only publishes are never left permanently
blob-less: `publish` treats a missing blob for the current head as work to
finish first on its next run.

## 5. Transport

- **Poll**: `GET latest.json` with `If-None-Match`; 304 costs one RTT and no
  bytes. Default interval 5 s remote, 1 s `file://`; the interval doubles up to
  5 min on consecutive errors and resets on success. On S3 the poll is a
  `GetObject` with `IfNoneMatch`; on `file://` it is `stat` + read.
- **Patch fetch**: one `GET` per patch, streamed, frame-verified; on a
  transport error the fetch resumes at the last verified frame with a `Range`
  request (up to 5 retries with backoff).
- **Blob fetch**: 8 parallel `Range` requests over the frame table; each frame
  verified on arrival, written at its offset into the temp file; failed
  frames retried individually. A 404 on a blob the pointer names means the
  publisher has not finished uploading it (§4.4): retry with backoff rather
  than fail. On profile C (20 Mbit/s, 1 % loss) this is
  57 s → 7.4 s for an 8 MB blob and 180 s → 26 s for 25 MB.
- **Stores**: `s3://` (AWS SDK v2, SigV4; also GCS/R2/MinIO via endpoint
  config in the usual env vars), `https://` (read-only, plain `GET`/`HEAD`,
  meant for a CDN or static server in front of a bucket), `file://`,
  `ssh://` (publish-only: `sftp` puts + `rename` for the CAS; the remote
  target polls the same directory as `file://`).

## 6. Target lifecycle

### 6.1 The rule

One update runs at a time; it either ends with the new release running and
`Ready()`/`--healthy` observed, or with the previous file back in place and
the previous process (or a restart of it) serving. There is no intermediate
state a user has to reason about beyond "which binary is at `<path>` and
does `<path>.old` exist".

### 6.2 Install (both shapes)

```
write <path>.tmp.<rand> (same directory) ← decoded bytes; fsync; verify BLAKE3 == head
link(<path>, <path>.old)             (replace any stale .old first)
create <path>.pending                 (contains head hash)
rename(<path>.tmp.<rand>, <path>)     (atomic; running process keeps its old inode)
fsync(dir)
```

Revert is `rename(<path>.old, <path>); unlink(<path>.pending); fsync(dir)`.
Every step is idempotent so a crash at any point leaves either the old or the
new file at `<path>`, and the next start can finish or undo the job from the
marker (§6.5). Requirements: the directory is writable and `<path>` is not a
symlink; no other file is touched.

### 6.3 Embedded (`selfupdate`)

```
old process (agent loop)                          new process
  install (§6.2)
  cmd = exec.Command(<path>, os.Args[1:]...)      ← by path, never /proc/self/exe
  cmd.ExtraFiles = listeners; env BINSYNC_FDS=…,
      BINSYNC_READY=<pipe fd>
  start; wait for: ready-pipe byte | exit | 60 s   selfupdate.Start(): sees BINSYNC_FDS → Listen()
                                                    returns inherited fds; serves; user calls Ready()
  ready   → stop accepting (close listener fds       Ready(): unlink <path>.pending; write byte to pipe
            in this process), run OnShutdown
            callbacks (drain, ≤ 30 s), exit 0
  exit / timeout → SIGTERM, 5 s, SIGKILL;
            revert (§6.2); record head in
            <path>.binsync/failed; keep serving
```

- `Listen(network, addr)` returns an inherited listener when one with the
  same `(network, addr)` was passed in `BINSYNC_FDS`, otherwise a fresh one.
  Both processes accept from the same socket between `start` and `ready`,
  so nothing queued in the accept backlog is lost.
- `Ready()` is the health decision; the library does not probe anything. Do
  your checks (DB connectivity, warm caches) *before* calling it.
- `Done()` closes when this process has been superseded, after `OnShutdown`
  callbacks return; the usual `main` ends with `<-up.Done()`.
- Signals: the old process forwards nothing. If the supervisor (systemd)
  sends SIGTERM to the old process mid-upgrade, the child is killed and the
  file reverted first, so the supervisor restarts the old release.
- `failed` (a file with the head hash): the loop skips that head until the
  pointer changes, so a broken release cannot crash-loop the fleet; the
  publisher's next release clears it.

### 6.4 External (`binsync agent`)

```
install (§6.2) → run --restart CMD (sh -c; must exit 0 within 60 s)
   → if --healthy: poll URL (2xx) or run CMD every 1 s until 60 s
   → ok: unlink <path>.pending
   → not ok / restart failed: revert (§6.2); run --restart again; record failed
```

The agent never signals or supervises the service; the user's command does
whatever "restart" means for them (`systemctl restart`, `kill -HUP`, …).
Without `--healthy` the update is considered successful once `--restart`
exits 0 — that is the deliberate minimal contract.

### 6.5 Crash after the old process is gone

If the new binary crashes after `Ready()` was never called but the old
process already exited (possible in external mode, or if the embedded old
process was killed by the supervisor after `ready`), the supervisor restarts
`<path>`, which is still the new file. `selfupdate.Start()` (and
`binsync agent` on start-up) checks: `<path>.pending` exists **and**
`BINSYNC_FDS` is unset (so this is not an upgrade launch) **and**
`<path>.old` exists → revert, record `failed`, and `exec` `<path>` (now the
old release). That is the whole recovery protocol; no state machine, no
probation window, no health history. A service without a supervisor is not
protected against this case (documented).

### 6.6 What the user sees

Structured log lines (`slog`) per cycle: `poll` (304/changed), `plan` (chain
vs blob, bytes), `fetch`, `apply`, `install`, `restart`, `ready`/`healthy`,
`reverted`, `failed`. Exit codes for `agent --once`: 0 ok/at head, 3
verification failed, 4 no path to head, 5 rolled back.

## 7. Library layout

```
binsync/delta          Encode(old, new []byte, o Options) ([]byte, error); Apply(old, patch []byte, w io.Writer) error
                       transform selection, .bsz container and frames (§3.5), plain codec (§3.8)
binsync/delta/gobin    ELF + pclntab + moduledata parsing for the supported Go release; functab
                       and findfunctab regeneration; type-descriptor walking (§3.2)
binsync/delta/x86      operand relocation and content hashing over x/arch/x86asm
binsync/delta/internal/lz    the one exact-match engine: a content index ordered by position,
                             plus a (lit, copy, seek) op stream; drives stage 1a, stage 1b,
                             the stage-2 regions (§3.4) and the plain codec (§3.8)
binsync/internal/cz    frame compression: raw / zstd / brotli, smallest-wins (§3.5)
binsync/release        Pointer, Edge, Frame, Blob types; MakePlan(pointer, current)
                       (chain | blob | none); Installer.Install/Revert and the pending and
                       failed markers (§6.2, §6.5); hash cache keyed by (dev, inode, size, mtime)
binsync/store          Store interface { Get(key, opts{range, ifNoneMatch}) ; Put(key, r, opts{ifMatch}) }
                       file, https, ssh in-package; s3 registered from binsync/store/s3;
                       StoreSuite is the conformance suite every backend runs
binsync/agent          Loop(ctx, Config, Hooks): poll → plan → fetch → apply → install → hooks.Restart → hooks.Check
binsync/selfupdate     Start/Listen/OnShutdown/Ready/Done built on agent with the exec handoff as Restart
cmd/binsync            publish, agent, diff, patch
```

The s3 backend lives in its own package and registers itself in `store`'s
opener table from its `init`, so a `file://`-only program never links the AWS
SDK. This replaces the build tag the first draft named: a build tag that
silently removes a URL scheme the CLI documents is a worse failure mode than
an import.

Dependencies: `golang.org/x/arch` (x86 decoder), `github.com/klauspost/compress/zstd`,
`github.com/andybalholm/brotli`, `github.com/zeebo/blake3`, AWS SDK v2
(`binsync/store/s3` only), `golang.org/x/crypto/ssh`.

## 8. Security model

- **Authenticity comes from the endpoint.** A target trusts whatever
  `latest.json` the configured store returns; the store URL is the security
  configuration. `https://` requires a valid certificate chain (no
  `InsecureSkipVerify` knob); `s3://` uses SigV4 over TLS with the ambient
  credentials; `ssh://` uses the host key; `file://` trusts the filesystem.
  This is the same trust model as `go install`, `apt` over TLS mirrors, and
  most container registries; it means a compromise of the bucket or of the
  publisher's credentials is a compromise of the fleet. Signed manifests
  would narrow that to "compromise of the signing key" and can be added
  later as an extra field in the pointer without changing the layout.
- **Integrity comes from hashes.** Every byte the target uses is checked
  against a hash that is reachable from the pointer: frame hashes, patch
  hash, and the final file hash. A CDN or proxy that corrupts or substitutes
  content causes a verification failure, not a bad install.
- **Replay/rollback.** `seq` must increase; an attacker who can serve an old
  pointer can hold a target back, not move it to arbitrary content.
- **Decoder hardening.** The patch is untrusted input to the decoder: all
  offsets and lengths are bounds-checked; allocation is bounded by
  `new_size` from the pointer (which is itself bounded by a configured
  `max_size`, default 2 GB).
- **Local.** `<path>` and its directory must be writable only by the service
  user; the agent refuses a `<path>` that is a symlink or world-writable.

## 9. Testing strategy

Three tiers, split by what they need and how long they take. 116 test
functions; `go test ./...` is 1.7 s in total, and no package is over 0.7 s.

- **Unit**, everything in `go test ./...`: codec round-trips over generated
  byte streams and the corruption of every field of every header; pointer
  planning; install/revert driven to a stop after each syscall step, with the
  §6.5 recovery asserted at every stop; the store conformance suite
  (`store.StoreSuite`) run against every backend; the exec handoff's fd
  inheritance.
- **Corpus** (`BINSYNC_CORPUS=<dir> go test ./delta`): the two gates that
  need real binaries. `TestSelfPrediction` runs every corpus binary old → old
  through the whole pipeline and requires a byte-identical result and a
  correction under 64 B — this is what enables a Go release (§3.7).
  `TestCorpusRoundTrip` encodes and applies every *ordered pair* in the
  corpus, in both directions and across build flavours, and requires the
  result to be byte-identical to the target; 32 pairs, 170 s.
- **Benchmark** (manual, `bench/`): patch sizes and encoder time on the
  release corpus; must not regress against §3.2.

## 10. Decisions

| # | Decision | Reason (short) |
|---|---|---|
| D1 | Delta patches, not CDC/CAS | 100× smaller for shifted executables (`cdc-cas.md`) |
| D2 | Go-aware predict-then-correct as the primary codec; plain bsdiff-class fallback | 5.7–38× over bsdiff on the corpus; scales to 1 GB without a suffix array (§3) |
| D3 | Correction is positional after a layout-exact prediction | removes the encoder's memory/time cliff; the layout table makes prediction length-exact |
| D4 | One mutable pointer, immutable content-addressed objects, chain of prev→head patches | one conditional GET to poll; CAS prevents forks; skipped releases follow the chain or take the blob |
| D5 | Blob and patches split into 8 MiB frames with per-frame hashes | parallel ranged fetch (5–8× under loss) and resume; S3/GCS have no multi-range |
| D6 | No signing key; endpoint trust + hashes | setup friction; same model as `go install`; can be layered later (§8) |
| D7 | Two hooks (`--restart`, `--healthy`) / two calls (`Ready`, `OnShutdown`) | covers the initial use-cases; everything else is the user's code |
| D8 | Three outcomes per update (ready, reverted, failed-and-skipped); `.pending` marker for post-exit crashes | simplest thing a user can reason about; no probation state machine |
| D9 | Same-socket fd inheritance for handoff; `SO_REUSEPORT` refused | only loss-free mechanism on all kernels (`zero-downtime-upgrade.md`) |
| D10 | Hardlink + rename install; exec by path | atomic, revertible, and never runs a deleted inode via `/proc/self/exe` |
| D11 | `-s -w` required (warning, `--force` to override) | unstripped DWARF makes every patch ≈ full size |
| D12 | Poll only (no push, no poke) | 304 poll is one RTT; workstation case is served by `file://` at 1 s |
| D13 | Publish order patch → pointer → blob; blob 404s are retried | the pointer goes live after hundreds of KB, not tens of MB — from a workstation over a lossy link that is ~5 s vs ~2 min (§4.4); steady-state targets never touch the blob |
| D14 | Go-aware codec supports one Go release at a time (the current stable, 1.27); everything else takes the plain codec | pclntab/type layouts change per minor; one version + a self-prediction gate keeps the codec small and testable (§3.6) |
| D15 | Correction = positional regions, each written as literals or as a bounded local match, whichever is smaller | recovers the bsdiff-quality bytes inside changed functions that purely positional runs re-send, at O(region) memory, and lets the decoder apply in place (§3.4) |
| D16 | Every stream takes the smaller of zstd and brotli; brotli quality 11 up to 4 MiB, 10 above | pure-Go zstd is 6–14 % worse than the `zstd -19` the research numbers used, pure-Go brotli is better at both qualities; patch bytes are the product (§3.5) |
| D17 | Stages 1a and 1b use the plain codec, not the positional correction | their residual is *shifted*, not positional — one pc table that changed length moves every table after it. Positionally, stage 1b costs 66,372 B on prometheus against 17,441 B (§3.4) |
| D18 | The source window of a correction region is derived (`min(256, fileLen − end)` past its end), not transmitted | both sides know the region's end and the file's length; a transmitted window is 2–4 varints per region and there are tens of thousands of them (§3.4) |
| D19 | The patch body is one frame per 8 MiB, not one frame per stream | a frame costs a 32-byte hash and a table entry; the streams compressed separately came within 0.1 % of compressing them together (§3.5) |
| D20 | The encoder transmits the BLAKE3 of its prediction, and every prediction worker count is a compile-time constant | encoder/decoder divergence becomes a named failure and a blob fallback instead of a wrong file caught by the release hash; a prediction that varies with `GOMAXPROCS` is one two hosts can disagree about (§3.7) |
| D21 | The prediction fills the bytes no allocated section covers: gaps cleared, `.shstrtab` copied from the old file's tail | the base is a copy of the old file, so every section that moved left stale bytes behind in its gap — 3,780 mispredicted bytes on prometheus, 109 after (§3.2) |
| D22 | Blob frames are brotli-10, not zstd, and carry a per-frame codec tag | 15 % smaller than the zstd blob and smaller than a single `zstd -19` stream, for 13 s of publisher CPU and 269 ms of target CPU on a 94 MB binary (§3.5). Supersedes the "blobs are always zstd" half of D16 |

Cut from the first-round design (and why): private signing keys and key
rotation (D6); the `poke` push endpoint and control socket (D12); inline
patches in the pointer (saves one RTT only for tiny patches; complicates the
pointer); direct non-chain edges and a K-matrix of patches (chain + blob
covers skipped releases; publisher cost stays O(1)); multiple channels per
store (use prefixes); a probation state machine with health windows and
canary hooks (D8); systemd-notify integration; zstd `--patch-from` as a
secondary codec (§3.8); most CLI flags (`README.md` §5 lists all that
remain).

## 11. Open questions

The last question §3.3 asked — whether the local match inside changed regions
lands nearer the 94,470 B of a hdiffz stage 2 than the 111,552 B of purely
positional runs — is answered: 95,366 B (§3.2). What remains:

1. **`.go.type` residual.** 45,170 B of the prometheus/1.27 patch is type
   descriptors: ~31.6 KB are genuinely new descriptors (inherent), ~7.9 KB
   are wrong rewrites and ~4.5 KB names. Only the consensus pass has been
   applied; an anchor pass (descriptor start → symbol) may recover the wrong
   rewrites. The changed `.text` (63,179 B, 52 % of the mispredicted bytes)
   is the other large item and is real change.
2. **arm64** in the Go-aware codec: fixed-width instructions make operand
   relocation *simpler* (ADRP/ADD page+offset pairs, B/BL imm26); needs a Go
   arm64 corpus to validate. Until then arm64 targets get the plain codec.
3. **Memory — the one open question that is a constraint, not a saving.**
   Applying the correction in place (§3.4) removed one whole copy of the
   file, and the decoder still peaks at 714 MB of live heap for a 94 MB
   binary, 7.6×. The buffers are only 2× of that; the rest is the
   prediction's own working set, and three items account for most of it:
   the 110,147 `_func` records rebuilt as individual slices in
   `predictPcln` (arena-allocate them into one backing array), the
   position-ordered index over `pctab` and `go:func.*` at 8 bytes per
   position, and the encoder's habit of holding `old` and `pred` while it
   also holds `new`. A 1 GB binary at this ratio asks a target for 7.6 GB,
   which is not a thing a fleet can be asked for; §3.3's ≈ 2× is the target
   and none of the three fixes changes the format.
4. **Encoder details.** `.text` is x86-decoded twice (relocation and
   shift-table derivation; caching saves ~0.3 of 2.4 s). The pointer-override
   table costs 5.5 KB of layout for a 26.5 KB net gain and its second
   consensus round gained nothing on prometheus. The remaining 109
   mispredicted bytes of ELF program and section headers could be predicted
   exactly from the layout, which already carries every section's address,
   offset and size; it is ~30 lines for ~0.1 % and has not been written.

# go-binsync — system design

Status: authoritative design, v1 (2026-08-26). `README.md` specifies *behaviour*;
this document records *why* and *how*: requirements, evidence, decisions,
wire formats, algorithms, and the performance model. Research and benchmarks
that the decisions rest on are in `docs/research/` (index in
`docs/research/README.md`); numbers quoted here cite those documents.

Contents

1. Requirements and latency budget
2. Evidence summary
3. Architecture
4. Decisions (D1–D18)
5. Formats
6. Algorithms
7. Upgrade lifecycle details
8. Security model
9. Performance model and targets
10. Testing and benchmarking strategy
11. Roadmap and open questions

---

## 1. Requirements and latency budget

**Workload.** One statically linked Go web-server binary, 30–100 MB, rebuilt many
times a day. Consecutive releases usually differ by one line of code or one text
constant. Targets are a fleet of Linux hosts reached over a high-latency link:
either an object store (S3/GCS/R2, ~30 ms in-region first-byte, 100–200 ms
envelope, one byte-range per request) or SSH from a workstation (100+ ms RTT).

**Objective.** Minimise end-to-end latency from "release binary exists on the
build box" to "every target is executing it, healthy, with no dropped
connections", subject to: byte-exact result, authenticated release, automatic
rollback, and support for targets that skipped releases or drifted.

**Latency budget (50 MB binary, 100 ms RTT, 100 Mbit/s).** The components, with
the design target for each:

| Stage | Naive (scp + restart) | go-binsync target | Note |
|---|---|---|---|
| Hash new release | — | 5 ms | BLAKE3 in Go: 9.6 GB/s single-thread on this box (`benchmark-local.md`) |
| Encode delta | — | ≤ 1 s warm, ≤ 5 s cold | warm = base index precomputed (D3) |
| Upload | 50 MB ≈ 4 s + RTTs | 1 PUT ≈ 2 RTT (0.2 s) | patch ≤ 512 KiB rides in the pointer |
| Notify | — | 1 RTT (poke) or ≤ poll interval | D10 |
| Detect + fetch | 6–8 RTT SSH + 4 s | 1 conditional GET (1 RTT warm) | patch inline; second GET only if large |
| Apply + verify | — | ≤ 0.3 s | linear apply, ~150–700 MB/s |
| Install | rename | 3 syscalls + 2 fsync (≤ 50 ms) | D13 |
| Exec + ready + probe | restart gap (connections dropped) | app start time; zero-loss overlap | D12 |
| **Total (go-binsync-controlled)** | **≈ 8–10 s + dropped connections** | **≈ 1.5–2.5 s** | dominated by encode and 2–3 RTTs |

Bytes barely matter at this bandwidth once the patch is < 1 MB (10 ms per
100 KB); round trips and encode time dominate. The design therefore spends its
complexity on (a) a shift-tolerant encoder that is fast when warm and (b) a
one-request hot path.

## 2. Evidence summary

Measured on this workstation (Go 1.26.4, linux/amd64) unless noted. Full
tables: `docs/research/benchmark-local.md`, `cdc-cas.md` §8,
`go-binary-layout.md` §0–2.

| Finding | Number | Source |
|---|---|---|
| A code edit that grows a function past its 32-byte padding shifts every later function and rewrites rel32/RIP-relative operands in *unmoved* functions | +47 B edit → 7,281 functions shifted, 114,519 one-byte operand edits in 9,410 unmoved functions | go-binary-layout §2.2 |
| A longer string literal shifts `.rodata` (symbols sorted by size, then index) and rewrites references binary-wide | +15 B string → 1,421 rodata symbols moved; 38 % of `.rodata` bytes differ | go-binary-layout §2.3, cdc-cas §8.3 |
| Same-length constant edit is nearly free | ~100 bytes differ; bsdiff 310 B | cdc-cas §8.2 |
| Compressed DWARF avalanches | unstripped one-line patch 4.9–5.6 MB vs 66–119 KB stripped | go-binary-layout §1, cdc-cas §8.2 |
| CDC chunk reuse on a shifted Go binary | 57 % (rodata shift) / 68 % (code shift) of 64 KiB chunks missing; 49–60 % at 4 KiB; rsync-style 512 B blocks still 26–42 % unmatched | cdc-cas §8.2 |
| Delta vs chunk transfer | bsdiff 28–66 KB, zstd `--patch-from` 67–223 KB, missing chunks compressed 3.3–9.2 MB, full zstd 6.0 MB (18.6 MB binary) | cdc-cas §8.2 |
| Tool ranking on the 29.6 MB stripped bench binary, one added statement (v1→v2c) | bsdiff 60.5 KB (6.0 s), hdiffz -m zstd 75.3 KB (1.17 s; 0.47 s with 8 threads, 80 MB), zstd -19 `--patch-from` 213 KB (15.7 s; `--long`/`--ultra` no help), xdelta3 lzma 344 KB / djw 464 KB (0.9 s), zstd -3 1.7 MB (0.08 s); apply 0.01–0.10 s for all | benchmark-local §3 |
| Where the bytes change (same case) | 13.4 % of bytes in 965 K runs (median 3 B): `.gopclntab` 3.07 MB = 29 % of the section (858 K runs of shifted 32-bit offsets; pclntab is 35 % of the file), `.rodata` 439 KB, `.text` 460 KB (69–100 % of `.text` runs are 1-byte RIP-relative displacement edits); only 21.6 % of the file survives in unchanged runs ≥ 256 KiB | benchmark-local §2 |
| Real CDC tools on the same pair | desync 16:64:256 KiB: 273 of 386 chunks new = 7.7 MB compressed = 92 % of the full download; 4 KiB chunks no better; casync default 7.6 MB | benchmark-local §4 |
| Chained vs direct patches | v1→v2c→v3→v4 costs 1.72× (bsdiff, zstd) to 1.81× (xdelta3) the direct v1→v4 patch | benchmark-local §3 |
| Mismatched build flags for the base | F1 base → F2 target: bsdiff 161 KB (2.7×), zstd 490 KB (2.3×) — degraded but still 17–52× smaller than full | benchmark-local §3 |
| PIE | +6.3 % size; patches larger (397 vs 332 KB zstd), 66k `R_X86_64_RELATIVE` entries with absolute addresses | go-binary-layout §2.5 |
| Pure-Go feasibility | `index/suffixarray` over 29.5 MB: 2.33 s, 118 MB; klauspost zstd raw-dict `best`: 261 KB in 0.83 s (levels below `best`: 10 MB, unusable); go-bsdiff: 69.6 KB in 9.0 s, 616 MB | bench/out/logs/05-*.log |
| x86 BCJ (E8/E9 → absolute) pre-transform | no gain: bsdiff 60.5 → 61.5 KB | bench/out/logs/05-bcj.log |
| Disassembly-aware patchers on ELF | Zucchini vs mbsdiff: ~10 % on Linux Firefox (33 % on Windows PE); Courgette 30 % from assembly form alone; 15–20 min, 1 GB to generate for Chrome | binary-delta §2.3 |
| Object-store request economics | S3/GCS: no multi-range GET; ~30 ms median TTFB, 100–200 ms envelope; conditional GET 304 = 1 request, no egress; S3 `If-Match` PUT since 2024-11 | update-systems §3 |
| Socket handoff | same-socket inheritance is loss-free on every kernel; `SO_REUSEPORT` drops queued connections unless kernel ≥ 5.14 and `tcp_migrate_req=1` | zero-downtime §2 |
| Replacing a running binary | `open(O_WRONLY)` → ETXTBSY; `rename` works; `/proc/self/exe` then executes the *old* inode; `os.Executable()` returns the path (= new file) | go-binary-layout §5, zero-downtime §5 |

## 3. Architecture

```
 build box / workstation                 store (S3/GCS/R2/file/https)            target host
 ┌───────────────────────┐   PUT/CAS     ┌──────────────────────────────┐  GET   ┌─────────────────────────────┐
 │ go-binsync publish        │ ────────────►│ channels/<ch>/latest (BSYP)  │◄──────│ go-binsync agent  (or agent pkg │
 │  hash · encode · sign  │              │ releases/<hash>.json          │       │  embedded in the service)    │
 │  cache: bins + indexes │              │ patches/<from>-<to>.bsz       │       │  poll · verify · plan ·      │
 └──────────┬────────────┘              │ blobs/<hash>.zst (seekable)   │       │  fetch · apply · install     │
            │ POST /poke (optional)      └──────────────────────────────┘       └──────────┬──────────────────┘
            └───────────────────────────────────────────────────────────────────────────────┤ control socket
                                                                                             ▼
 workstation ── ssh host go-binsync recv ── stdio framed protocol ──────────────────► ┌─────────────────────────────┐
 (go-binsync push: same code path, no store)                                          │ service (go-binsync/upgrade)    │
                                                                                   │ old proc: exec new, pass fds,│
                                                                                   │ wait ready, probe, commit /  │
                                                                                   │ abort; child: Ready(), serve │
                                                                                   └─────────────────────────────┘
```

Packages and their dependencies (arrows = imports):

```
cmd/go-binsync ─► agent ─► release, store, delta, install, hooks
              upgrade (no dependency on the network packages; importable by any service)
              delta   ─► klauspost/compress/zstd, index/suffixarray, lukechampine.com/blake3
              release ─► crypto/ed25519, blake3, canonical JSON
              store   ─► net/http (+ SigV4 signer), GCS JSON API, os
              install ─► os, syscall (link/rename/fsync), blake3
```

Data flow of a publish: hash → look up base(s) in local cache → load or build
base index → encode hot patch (parallel partitions) → sign manifest → upload
patch (inline or object) → CAS pointer → upload blob + release manifest →
poke → background: direct patches from older bases → re-CAS pointer.

Data flow of a sync: conditional GET pointer → verify signature/seq/expiry →
hash base (cached) → plan path in graph → fetch edges (parallel) → apply in
sequence, hashing each intermediate → `pre-swap` hook → install → lifecycle.

## 4. Decisions

Each decision states the choice, the evidence, and what was rejected.

**D1 — Delta encoding against the deployed release, not chunk sync.**
CDC/CAS re-sends 40–70 % of a Go binary for any size-changing edit because
addresses change, not just move (cdc-cas §8; REAPI's 75 % reuse on executables).
Delta encoders send 30–120 KB. CDC's remaining merits (any-base recovery,
storage dedup) are not worth its request count on S3/GCS (one range per
request; hundreds of missing chunks in ~10 runs). Rejected: casync/desync
model, zsync/rsync block matching (26–42 % unmatched at 512 B blocks).

**D2 — Primary codec `bsz`: bsdiff-class approximate matching with
zstd-compressed streams, pure Go.**
bsdiff's "extend matches while ≥ 50 % of bytes agree, emit bytewise
differences" turns the +32 operand rewrites into a near-zero diff stream that
compresses ~3–4× better than exact-match LZ (zstd `--patch-from`) and ~8×
better than xdelta3 on our workload (60 / 213 / 464 KB). HDiffPatch is
comparable (75 KB) but C-only. Classic bsdiff's container (bzip2, three streams,
no integrity) is replaced: varint control triples, zstd streams, per-partition
output hashes, bounds checks (CVE-2014-9862 class bugs in bspatch). Rejected:
cgo HDiffPatch (cross-compilation, static agents), xdelta3 (size), shelling out
to external tools.

**D3 — Precomputed base index on the publisher.**
Encode cost is on the critical path (a 1-line change ships in seconds;
`index/suffixarray` alone is 2.3 s for 30 MB, ~4–8 s at 50–100 MB). After each
publish the publisher builds and caches the suffix array of the *just
published* release (off the critical path); the next publish reuses it, so the
warm encode is only the parallel match scan (target < 1 s). Cache key: release
hash; storage 4 bytes per input byte. Rejected: hash-based matcher only
(bidiff-style, ~2.3× larger patches); always cold SA.

**D4 — Partitioned patch frames aligned to ELF sections.**
The new file is split into partitions (≈ 4–8 MiB, boundaries snapped to section
starts) each encoded and decodable independently against the whole old file.
Gives parallel encode/apply, streaming apply with bounded memory, early failure
via per-partition hashes, and prevents a change in `.rodata` from perturbing
the encoder's state in `.gopclntab`. Measured per-section sums equal whole-file
patches within ±1 % (go-binary-layout §7.3), so partitioning costs nothing in
size. Section-aware framing is also the hook for future Go-aware transforms
(D6).

**D5 — Secondary codec `zstd` (raw-dictionary frames, `zstd --patch-from`
compatible).**
klauspost/compress at `SpeedBestCompression` with the old file as raw
dictionary produces 261 KB in 0.83 s for the case where bsz gives 60 KB; lower
levels do not find long matches (10 MB) and are excluded. It is the fast path
when the base index is cold and encode latency matters more than 200 KB, and it
is interoperable with the `zstd` CLI for debugging. Chosen per publish
(`--codec`); the publisher may also pick it automatically when the SA is cold
and the file is large (policy, §6.1).

**D6 — Executable-aware transforms are deferred (measured negative for the
cheap one).**
The x86 BCJ E8/E9 transform gave no gain (60.5 → 61.5 KB): converting to
absolute addresses helps references *from* shifted code *to* unshifted targets
and hurts the reverse, and both populations exist — and on our binary the
dominant `.text` churn is RIP-relative `lea`/`mov` displacements into a
shifted `.rodata`, which BCJ does not touch at all. A real Courgette/Zucchini
label scheme buys ~10–30 % on ELF (Firefox Linux data) at high complexity and
fragility. The largest structured residual is `.gopclntab` (35 % of the file;
29–61 % of it rewritten as constant-shifted 32-bit offsets on a code change),
which bsdiff already reduces to a few tens of KB; a pclntab-aware transform
could take that to a few KB.
At 100 Mbit/s a 60 KB patch costs 5 ms; this is not where latency goes. The
container reserves a `transform` id so a future codec can add it without a
format break. Rejected for v1: Zucchini port, pclntab-aware encoding, DWARF
decompress/recompress.

**D7 — BLAKE3-256 content ids everywhere.**
9.6 GB/s single-thread in Go here (SHA-256 with SHA-NI: 2.6 GB/s); hashing a
100 MB binary is ~10 ms, so the agent can afford to hash on every check when the
inode cache misses. Ids are `b3:<64 hex>`. SHA-256 of the head is also recorded
in the manifest for interop with other tooling but is not used for decisions.

**D8 — Patch graph with chain edges plus bounded direct edges; full blob
fallback.**
Every publish creates the edge prev→new (needed anyway for the hot path). Chain
edges are immutable, so a target K releases behind already has a path (K tiny
patches, fetched in parallel, applied sequentially with a hash check between
steps; apply is ~0.1–0.2 s each). A three-hop chain measured 1.7–1.8× the bytes
of the direct patch — still ~100 KB. After the hot path, the publisher adds direct
edges from the last N (default 3) releases so common stragglers need one patch.
The agent plans the fewest-bytes path with a chain cap (default 16), else full.
Rejected: Firefox-style K-wide matrix on the critical path (K encodes before
anyone can update); Windows-UUP reverse+forward hub (two applies for everyone,
periodic rebasing, and retained reverse deltas on targets — solves a problem
chain edges already solve for a fleet whose old versions are known);
on-demand server-side generation (requires a live server, not an object store).

**D9 — One signed mutable pointer object per channel; everything else
content-addressed and immutable; hot patch inline.**
Targets poll `channels/<ch>/latest` with `If-None-Match`; a 304 is one request
and no egress. When a release lands, the pointer body carries the manifest, the
recent graph (so path planning needs no further GET) and the hot patch if
≤ 512 KiB (adds ~4 ms of transfer instead of a +130–230 ms second request).
The pointer is replaced with `If-Match` CAS so concurrent publishers cannot
lose a sequence number. Objects are cacheable forever (CDN-friendly); the
pointer is `no-store`. Rejected: TUF's 4-GET flow (borrow its rollback/freeze
checks instead); HEAD-then-GET; per-chunk objects; a live update server.

**D10 — Poll plus optional poke.**
No object store has a watch primitive and event notifications take seconds to a
minute (update-systems §4). The agent polls (default 5 s; 1 RTT, one request)
and exposes `POST /poke` so `publish --poke` or CI can trigger an immediate
check; the poke carries no data, so it needs no trust. Rejected: long-poll
server (extra infrastructure), object-store events.

**D11 — SSH push path shares the codec and lifecycle, not the store.**
For workstation→server the store is overhead: `push` opens SSH (cheap with
ControlMaster), asks the remote for its base hash, encodes from the local cache,
streams manifest + patch, and receives lifecycle events. Same `release`,
`delta`, `install`, `upgrade` code; only transport differs. Signed by default,
unsigned allowed only if the remote opted in.

**D12 — Zero-downtime handoff by same-socket fd inheritance, run by the old
process (tableflip model), with health probation before commit.**
Only sharing one accept queue is loss-free on every kernel. The old process
holds sockets until COMMITTED, so abort is free. Improvements over tableflip:
readiness *and* probe-based probation before the old process drains; explicit
abort reasons; a startup self-check to cover crashes after the old process has
exited; `O_NONBLOCK` re-armed on inherited fds in the child (Go's `exec.Cmd`
clears it). systemd fd-store mode is offered for services that cannot link the
library (accept pause, not loss). `SO_REUSEPORT` handoff refused (D-evidence:
`tcp_migrate_req` only ≥ 5.14, default off).

**D13 — Install = temp + fsync + `link(path, path.old)` + `rename` + fsync(dir);
`upgrade.pending` marker; one retained generation.**
`rename` is atomic and unaffected by ETXTBSY; hardlink-then-rename keeps a valid
executable at the path at every instant (go-update's rename-rename has a gap and
never fsyncs). The marker lets the *next* process start detect "new build
crashed after old exited" and revert before the supervisor's `StartLimitBurst`.
Re-exec is always by path, never `/proc/self/exe` (old inode).

**D14 — Ed25519-signed manifests with monotonic `seq` and `expires`;
signature verified before anything else is parsed.**
Sign the manifest (which names the patch and blob hashes), not the blobs: one
signature covers the result. `seq` defeats rollback replays, `expires` freeze
attacks (TUF's threat model, update-systems §6). Keys in minisign format so
operators can reuse existing key hygiene. Multiple signatures allowed for
rotation. Rejected: sigstore/Rekor on the hot path; unsigned patches with only a
result hash (go-update model).

**D15 — Pure Go, no cgo.**
Agents must cross-compile and run in `FROM scratch` containers; the encoder
runs on a build box where a few seconds are acceptable and can be hidden
(D3). Dependencies: `klauspost/compress`, `lukechampine.com/blake3`, an S3
SigV4 signer, `x/sys/unix`.

**D16 — Publisher enforces delta-friendly builds.**
Refuse (default) binaries with `.debug_*` or `.symtab` because they inflate a
one-line patch 20–40× (5 MB vs 0.12 MB) and no encoder fixes compressed DWARF.
Warn on PIE, `vcs.modified`, and missing `-trimpath`. Reproducible builds make
"expected hash" derivable from the artifact and let any build box regenerate an
old base.

**D17 — Full blob as zstd seekable frames; ranged parallel fetch.**
Independent 8 MiB frames plus a seek table (zstd's seekable format) let a
straggler or drifted target fetch 4 ranges in parallel (S3 ~85–95 MB/s per
connection) and decode concurrently, and let a resumed download restart at a
frame. Also used by `push` when the base is unknown.

**D18 — Probation defaults favour speed but never skip readiness.**
`ready-timeout` 60 s, `min-healthy` 5 s, `healthy-deadline` 60 s,
`drain-timeout` 30 s. Probation runs while the old process still serves, so it
is not downtime; operators optimising pure build→reload latency can set
`--min-healthy 0` and rely on readiness plus the startup self-check.

## 5. Formats

All integers little-endian. Hashes are 32-byte BLAKE3-256 digests unless in
JSON, where they are `"b3:" + hex`. Canonical JSON = RFC 8785 (JCS): sorted
keys, no insignificant whitespace, UTF-8.

### 5.1 Manifest container (`BSYP`)

Used for the channel pointer, release manifests, and the `push` protocol.

```
off  size  field
0    4     magic "BSYP"
4    1     container version = 1
5    3     reserved (0)
8    4     manifest_len (u32)
12   n     manifest: canonical JSON (UTF-8), exactly manifest_len bytes
     2     sig_count (u16), ≥ 1
     ×     sig_count × { key_id: 8 bytes, signature: 64 bytes }
     8     inline_len (u64), 0 if no inline patch
     m     inline patch: a complete patch container (§5.3), exactly inline_len bytes
```

- `key_id` = first 8 bytes of BLAKE3(public key bytes).
- `signature` = Ed25519 over `"github.com/wjordan/go-binsync/manifest/v1\n" || manifest` (domain
  separated; no prehash — Ed25519 handles long messages).
- Verifiers parse nothing in `manifest` until at least one signature verifies
  against a pinned key. `inline_len` and the inline bytes are covered by the
  manifest's `inline.size` and `inline.hash`.

### 5.2 Manifest JSON

```json
{
  "v": 1,
  "kind": "pointer",                       // "pointer" | "release" | "push"
  "channel": "prod",
  "seq": 1234,                             // strictly increasing per channel
  "created": "2026-08-26T10:00:00Z",
  "expires": "2026-09-25T10:00:00Z",       // default created + 30 d
  "head": {
    "hash": "b3:…", "sha256": "…", "size": 29561097,
    "version": "v1.2.3-4-g1234abc",
    "blob": { "key": "blobs/b3:….zst", "size": 9497944, "hash": "b3:…" },
    "buildinfo": { "go": "go1.26.4", "path": "example.com/srv", "vcs.revision": "…", "vcs.modified": false },
    "elf": { "pie": false, "dwarf": false, "symtab": false, "sections": [["text", 4096, 14781841], …] }
  },
  "graph": {
    "nodes": [ { "hash": "b3:…", "seq": 1233, "version": "…", "size": … }, … ],   // last 16 releases incl. head
    "edges": [ { "from": "b3:…", "to": "b3:…", "codec": "bsz", "size": 61234, "hash": "b3:…", "key": "patches/b3:…-b3:….bsz" }, … ]
  },
  "inline": { "from": "b3:…", "to": "b3:…", "codec": "bsz", "size": 61234, "hash": "b3:…" },   // or null
  "publisher": { "key_id": "0123456789abcdef", "tool": "github.com/wjordan/go-binsync/0.1.0", "host": "ci-42" }
}
```

Validation order on the target: signature → `v`/`kind` → `channel` equals the
configured one → `expires` > now → `seq` > last accepted (persisted) → `head`
fields well-formed → graph edges reference known nodes and the inline entry (if
any) matches `inline_len`/hash. `kind: "release"` objects omit `graph` and
`inline`; `kind: "push"` omits `channel`/`seq` (the push transport provides
freshness) unless the sender sets them.

### 5.3 Patch container

```
off  size  field
0    4     magic "BSZ1"
4    1     container version = 1
5    1     codec: 1 = bsz, 2 = zstd-prefix
6    1     transform: 0 = none (reserved for D6)
7    1     flags (bit0: partitions carry old-range hints)
8    32    from_hash
40   32    to_hash
72   8     old_size
80   8     new_size
88   4     partition_count N (≥ 1)
92   4     header_crc32c over bytes 0..91
96   N×64  partition table entries:
             new_off u64, new_len u64, old_hint_off u64, old_hint_len u64,
             frame_len u64, out_hash 32 bytes (BLAKE3 of new[new_off, new_off+new_len))
     ...   frames, in partition order, each exactly frame_len bytes
```

Partitions tile `[0, new_size)` exactly, in increasing order. The trailing
32 bytes after the last frame are a BLAKE3 of everything before them (whole-file
patch integrity for offline use; the manifest's edge hash is the authoritative
one). Apply refuses any partition whose decoded output length ≠ `new_len` or
whose hash ≠ `out_hash`, and any control instruction that would read outside
`[0, old_size)` or write outside the partition.

**bsz frame:**

```
ctrl_len u32, diff_len u32, extra_len u32, then ctrl, diff, extra — each an
independent zstd frame (klauspost, level "best" for diff/ctrl, "better" for extra).
ctrl (decompressed) = sequence of { diff_n uvarint, extra_n uvarint, seek svarint }
```

Semantics per triple, with `oldpos` starting at 0 for each partition:
copy `diff_n` bytes as `old[oldpos+i] + diff[i]` (mod 256), then `extra_n`
literal bytes from `extra`, then `oldpos += diff_n + seek`. Sum of `diff_n +
extra_n` over the partition = `new_len`. Empty diff and extra streams are
encoded as zero-length zstd frames.

**zstd-prefix frame:** one partition; the frame is a standard zstd frame (or
frame sequence) encoded with `old` as the raw content dictionary and window log
= ⌈log₂(old_size + new_size)⌉ (≤ 31). `zstd -d --patch-from=old --long=<wlog>
--memory=<size>` decodes the frame bytes after the header.

### 5.4 Full blob

`blobs/<hash>.zst` is a zstd *seekable* stream: independent frames of 8 MiB
uncompressed, followed by a skippable frame (magic `0x184D2A5E`) holding the
seek table `[compressed_size u32, decompressed_size u32]×n`, `n` (u32),
descriptor, and the seekable magic `0x8F92EAB1`. Targets fetch the last 4 KiB
first (one ranged GET), then frames in parallel ranges.

### 5.5 Object keys and headers

| Key | Mutability | Headers |
|---|---|---|
| `channels/<ch>/latest` | mutable, CAS (`If-Match` ETag; `If-None-Match: *` on first create) | `Cache-Control: no-store`, `Content-Type: application/vnd.go-binsync.pointer` |
| `releases/<hash>.json` (BSYP) | immutable | `public, max-age=31536000, immutable` |
| `patches/<from>-<to>.<codec>` | immutable | same |
| `blobs/<hash>.zst` | immutable | same |

Hashes in keys use the `b3:` prefix form. The publisher writes all immutable
objects with `If-None-Match: *` and treats "already exists" as success.

### 5.6 Target state directory

```
<state>/state.json        { "channel", "last_seq", "last_head", "base_cache": { "path","dev","ino","size","mtime_ns","ctime_ns","hash" }, "last_result": {…} }
<state>/upgrade.pending   { "old_hash","new_hash","started","attempt","crashes" }  — exists from INSTALLED until COMMITTED/ABORTED
<state>/lock              flock(2) held by the running agent/sync
<path>.old                previous binary (hard link), one generation
<path>.go-binsync-tmp.<rand> staging file (same directory; removed on failure)
```

### 5.7 Control socket protocol (agent ↔ service)

Unix stream socket, newline-delimited JSON, one request per connection.

```
→ {"op":"status"}
← {"state":"idle","pid":123,"hash":"b3:…","version":"…","started":"…"}

→ {"op":"upgrade","path":"/srv/app/server","expect":"b3:…","ready_timeout":"60s",
   "probe":{"url":"http://127.0.0.1:8080/healthz","min_healthy":"5s","deadline":"60s"},
   "drain_timeout":"30s"}
← {"event":"starting","child_pid":456}
← {"event":"ready"}                      // child wrote the ready byte
← {"event":"healthy"}                    // probation passed (or omitted when no probe)
← {"event":"committed"}                  // old process now draining; connection closes when it exits
   -- or --
← {"event":"aborted","reason":"ready-timeout","exit_status":null}

→ {"op":"abort"}                         // operator/agent-initiated during STARTING/PROBING
← {"event":"aborted","reason":"operator"}
```

The service refuses `upgrade` while one is in flight (`{"error":"upgrade in
progress"}`) and refuses a `path` whose hash ≠ `expect` (it hashes the file
before exec — cheap, and prevents racing an unrelated write).

### 5.8 Child environment and fds (embedded mode)

```
fd 3           ready pipe (write end); child writes one byte 0x2A after Ready()
fd 4           parent-alive pipe (read end); EOF ⇒ parent exited
fd 5..         inherited listeners, in the order given by BINSYNC_FDS
env BINSYNC_UPGRADE_CHILD=1
env BINSYNC_FDS=[{"name":"http","network":"tcp","addr":"[::]:8080","fd":5}, {"name":"control","network":"unix","addr":"/run/app/go-binsync.sock","fd":6}]
```

The child sets `O_NONBLOCK` on every inherited fd before `os.NewFile`. Unix
socket paths are unlinked only by the process that finally stops with no
successor. `os.Args[0]` for the child is the installed path; `argv` otherwise
copied from the parent.

### 5.9 `push`/`recv` protocol (stdio)

Frames: `len u32 | json header | payload(len - header_len)`.

```
recv → {"hello":1,"base":"b3:…","size":…,"version":"…","mode":"embedded","allow_unsigned":false}
push → {"manifest":true}  payload = BSYP container (kind "push", inline patch or none)
push → {"blob":true, "frames":n}  payload = seekable zstd blob     (only when no patch)
recv → {"event":"…"} …                                             (lifecycle events, as §5.7)
recv → {"result":"ok"|"failed","code":0..75,"hash":"b3:…"}
```

## 6. Algorithms

### 6.1 Encoder (`bsz`)

1. **Base index.** `sa = suffixarray.New(old)` (SA-IS, 32-bit ints below 2 GiB).
   Serialized to `<cache>/index/<hash>.sa` after publish; loaded with `mmap` on
   the next publish. Cold build: ~80 ms/MB on this box.
2. **Partitioning.** Read the new file's ELF section headers (`debug/elf`);
   candidate cut points are section starts. Choose partitions of ~6 MiB (tunable)
   whose starts are the nearest section start ≤ the ideal cut; partitions never
   span a section start unless the section is larger than the partition size
   (then cut inside it). Non-ELF inputs use fixed cuts.
3. **Match scan** (per partition, one goroutine each, bounded by `GOMAXPROCS`):
   bsdiff's scan — at each `scan` position search the SA for the longest match
   (`binary search + LCP`), keep it if it explains ≥ 8 bytes beyond the current
   approximate extension, extend forward/backward while ≥ 50 % of bytes match,
   emit `(diff_n, extra_n, seek)`. The old-range hint recorded in the partition
   table is the min/max `oldpos` used (informational; lets a future apply
   prefetch).
4. **Streams.** Encode ctrl varints; diff bytes; extra bytes. Compress each with
   klauspost zstd (`SpeedBestCompression`, window 4 MiB for diff/ctrl — the
   diff stream is near-zero and compresses to a few KB regardless).
5. **Selection.** If the resulting patch ≥ 90 % of the blob size, the publisher
   omits the edge (targets will use the blob). If the SA is cold and
   `old_size > 64 MiB` and `--codec auto`, use `zstd-prefix` for the hot patch
   and let the background job replace it with a `bsz` edge.

Complexity: scan is O(m log n) string comparisons; memory = old + new + SA
(≈ 6× old). Target: warm encode of a 50 MB binary in < 1 s on 8 cores.

### 6.2 Apply

Sequential over partitions (or parallel with `--parallel`, each into its own
output range via `WriteAt`); per partition, stream-decode the three zstd frames,
execute triples with strict bounds checks, write into the staging file, feed a
per-partition BLAKE3 and the whole-file BLAKE3. Old file accessed via `mmap`
(read-only) or `ReadAt`. Peak memory ≈ zstd windows + I/O buffers (tens of MB),
never old + new.

### 6.3 Path planning

Graph from the pointer (nodes ≤ 16, edges ≤ ~50). Dijkstra by edge `size` from
`base` to `head`, with hop count ≤ `--max-chain`; if no path, use the blob.
Ties prefer fewer hops. All edges of the chosen path are fetched concurrently
(inline edge from memory), applied in order, verifying each intermediate hash
before it becomes the base for the next step. The intermediate outputs live in
the staging directory and are removed after the final verify.

### 6.4 Base hash cache

`state.json.base_cache` is valid iff `(dev, ino, size, mtime_ns, ctime_ns)` of
`<path>` are unchanged; otherwise rehash (≈ 10 ms/100 MB). The hash is also
recomputed after every install.

## 7. Upgrade lifecycle details

The state machine and hook contract are in `README.md` §7. Additional
mechanics:

- **Who runs what.** `go-binsync/upgrade` (in the service) owns processes: exec,
  fd passing, readiness, probation, drain, abort/kill. `go-binsync/install` owns
  files: stage, link, rename, marker, revert. `go-binsync/agent` sequences them and
  runs hooks. In self-updating services all three run in-process.
- **Exec.** `syscall.ForkExec` via `os.StartProcess` with `Files = [stdin,
  stdout, stderr, readyW, aliveR, listeners…]`, `Env` extended as §5.8, `Dir`
  inherited. On `ETXTBSY` (Go fork/exec race, golang/go#22315) retry with
  backoff up to 1 s.
- **Readiness.** Parent selects on the ready pipe and the child's exit; whichever
  first. `Ready()` in the child also updates a PID file if configured
  (`sd_notify MAINPID=` when under systemd `Type=notify`).
- **Probation.** Parent probes `probe.url` every 500 ms (or runs `probe.cmd`);
  requires continuous success for `min_healthy` before `deadline`; the probe is
  expected to target the new process (e.g. a port only the child announces, or
  a `/healthz?pid=` echo) — in same-socket mode both processes accept, so the
  probe should carry a `X-Binsync-Expect: <new_hash>` header that the service
  echoes with its own hash; mismatch ⇒ retry, not failure.
- **Commit.** Parent closes its copies of the listeners *after* the child is
  healthy, calls registered shutdown callbacks, `http.Server.Shutdown(drain)`,
  then `Close()`, then exits 0. The child sees EOF on the alive pipe and
  finalises (removes the marker via the agent, or itself when self-updating).
- **Abort.** `SIGTERM` child, 5 s, `SIGKILL`; the parent keeps its listeners
  throughout so nothing queued is lost except connections the child already
  accepted (it is asked to drain those during the 5 s).
- **Startup self-check** ordering: first statement in `main` (`upgrade.Init()`),
  before any listener is created, so a broken build fails before it can take
  traffic.
- **systemd unit sketch** (embedded mode): `Type=notify`, `NotifyAccess=all`,
  `KillMode=mixed`, `Restart=on-failure`, `StartLimitBurst=5`, and the agent as
  a separate unit with `After=`.

## 8. Security model

Threats and mitigations (TUF-derived):

| Threat | Mitigation |
|---|---|
| Store compromise / MITM serves a malicious binary | Ed25519 signature over the manifest; hashes of patch and blob in the manifest; result hash verified before install |
| Rollback to an older signed release | `seq` strictly increasing per channel, persisted on the target |
| Freeze (serve a stale but valid pointer forever) | `expires`; the agent logs and refuses expired pointers; a "stale head" metric |
| Mix-and-match (valid patch, wrong base) | edges carry from/to hashes; base hash checked before apply; output hash after |
| Malicious patch exploiting the decoder | strict bounds checks; decoder fuzzed; the patch bytes are hashed against the signed manifest before decoding begins |
| Key compromise | multiple pinned keys, rotation by publishing with old+new signatures for a grace period; revocation by removing the key from targets |
| Local attacker with write access to `<path>` | out of scope (equivalent to root on the target); state dir and binary directory should be root-owned |
| Poke endpoint abuse | pokes carry no data; rate-limited; optional bearer token |

## 9. Performance model and targets

Model (100 Mbit/s, 100 ms RTT, S3-like 30 ms TTFB, 4-way parallel):

| Scenario | Requests | Bytes | Network time | Encode | Apply | Total |
|---|---|---|---|---|---|---|
| Hot path, inline patch (60 KB) | 1 | 60 KB | 0.13 s | 0.8 s warm | 0.2 s | **≈ 1.2 s** + exec/ready |
| Hot path, patch object (300 KB) | 2 | 300 KB | 0.28 s | 0.8 s | 0.2 s | ≈ 1.3 s |
| 5 releases behind (chain, 5 × 80 KB) | 1 + 5 ∥ | 400 KB | 0.3 s | — | 1.0 s | ≈ 1.4 s |
| Drifted / cold (full blob 15 MB, 4 ranges) | 1 + 5 | 15 MB | 1.5 s | — | 0.1 s | ≈ 1.7 s |
| CDC/CAS (rejected, for reference) | 1 + ~10 coalesced ranges | 3–9 MB | 0.6–1.2 s | — | 0.2 s | ≈ 1–1.5 s but 50–100× the bytes |
| Full copy over cold SSH + restart | 6–8 RTT + 50 MB | 50 MB | 4.8 s | — | — | ≈ 5 s + dropped connections |

At 20 Mbit/s / 200 ms RTT the bytes start to matter: hot path ≈ 1.3 s, chain
≈ 1.6 s, full blob ≈ 8 s — which is why a small patch and single request are
both required.

Targets for the implementation (to be enforced by `bench/`):

- Encode: warm < 1 s, cold < 6 s for a 50 MB binary on 8 cores; patch size
  within 1.3× of C bsdiff on the bench corpus.
- Apply: > 200 MB/s of output, < 64 MB RSS.
- Agent: hot path ≤ 2 requests; idle poll = 1 request; CPU idle otherwise.
- Handoff: zero failed connections in a `wrk`/`hey` soak across 100 upgrades
  (integration test with `tc netem` for latency).

## 10. Testing and benchmarking strategy

- **Codec**: property tests (random edits, shifts, insertions; apply(encode) is
  identity), fuzzing of the decoder against malformed containers (must never
  read/write out of bounds; must fail closed), cross-check `zstd-prefix` frames
  with the `zstd` CLI, golden patches for the bench corpus, size/time
  regressions gated in CI (`bench/run.sh --check`).
- **Release/manifest**: signature and canonicalisation vectors; seq/expiry
  rejection cases; CAS conflict simulation.
- **Store**: contract tests against a local S3 emulator (MinIO) and `file://`;
  conditional GET/PUT semantics; range requests.
- **Install**: crash injection between each syscall (path always executable,
  `.old` always valid); ETXTBSY behaviour with a running child.
- **Upgrade**: end-to-end tests with a real HTTP server under load, asserting
  zero connection errors across upgrades, abort on non-ready child, abort on
  failed probe, self-revert after crash loop; run in a network namespace with
  `tc qdisc netem delay 100ms` to check the request budget.
- **Bench corpus**: `bench/testsrv` variants (v1…v4, F1–F3, PIE) kept
  reproducible; report regenerated by `bench/run.sh`.
- All unit tests < 100 ms each; integration tests tagged and < 1 s except the
  soak test.

## 11. Roadmap and open questions

Phases: (1) `delta` codecs + CLI `diff/patch` + bench gate; (2) `release`,
`store`, `install`, `sync/publish` against `file://` and S3; (3) `upgrade` +
`agent` embedded mode with the soak test; (4) `push/recv`, systemd mode, hooks,
`gc`; (5) hardening: fuzzing, key rotation, metrics.

Open questions to settle during implementation (each with the intended
default):

1. Warm encode speed of the pure-Go match scan on 50–100 MB inputs — the
   suffix-array search in Go may need an LCP array or a sampled index to hit
   < 1 s; fallback is `zstd-prefix` for the hot patch (D5).
2. Whether the per-partition zstd window should be shared across partitions
   (single frame, larger window) for the `extra` stream; measure on the corpus.
3. Probe targeting in same-socket mode (§7): the `X-Binsync-Expect` echo
   requires a one-line handler in the service; alternative is a child-only
   ephemeral port.
4. Whether to ship a CDC "heal" for drifted targets later (v1: full blob).
5. Go 1.27's closure-naming change and PGO as sources of layout drift — track
   patch sizes across toolchain upgrades in the bench.

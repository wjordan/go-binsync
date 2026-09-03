# go-binsync

> **Archived.** This repo is a two-day spike from August 2026, kept as-is for
> the record. It tested two ideas: that reading the function table the Go
> linker leaves in a stripped binary makes a patch orders of magnitude smaller
> than a byte-level differ, and that a running service can replace its own
> executable and hand over its sockets without dropping a connection. Both were
> carried into separate projects and neither is developed here —
> [presage](https://github.com/wjordan/presage) generalises the codec past Go
> into a structure-aware patcher for ELF, x86-64 and DWARF, and
> [succession](https://github.com/wjordan/succession) is the install-and-handoff
> library. Everything below was measured on this code and is not being updated.

Small, fast, verified, zero-downtime updates of a deployed Go binary.

go-binsync keeps the binary on a remote host identical to an authoritative
release. It sends a patch against the version already on disk, verifies the
result by hash, installs it atomically, and re-executes the running service with
its listening sockets handed over — reverting to the previous binary if the new
one does not come up. It ships as a Go library and a CLI.

## The headline number

Change one string literal, in one handler, in a 30 MB Go service. Nothing else
in the source moves, but the linker moves 13 % of the bytes in the file: every
function after the edit shifts, and every reference that crosses the shift is
rewritten. That is what a byte-level differ has to encode, and what a Go-aware
one can predict.

```
one-line change to a 30 MB Go binary — patch bytes on the wire, linear scale

  hdiffz -p-8  ████████████████████████████████████████████████  176,929
  bsdiff       ████████████████████████████████████████▊         150,475
  go-binsync   ▌                                                   2,262   ← 67× smaller than bsdiff
```

bsdiff and hdiffz are the strongest general-purpose binary differs available.
They compare the two files as strings of bytes, so they have to encode every
function that moved and every reference that was rewritten. go-binsync reads the
function table the Go linker leaves in the binary, matches functions between the
two builds by name, and works out where each one landed, so most of those bytes
never go on the wire.

## Why patch size matters

Say you run a large fleet of servers that all run the same binary — a web
application, a telemetry pipeline, it doesn't matter; call it one big monolithic
Go binary. You want to iterate on it quickly, rolling new releases out across
the whole fleet as fast and as reliably as you can. The binary is large:
hundreds of MB is ordinary, and it grows with every release.

Sending a full copy to every host is slowest exactly where you can least afford
it. A link with congestion or packet loss delivers a fraction of its nominal
rate, so a download that takes seconds inside a datacentre trickles for minutes
in a remote region. A patch of a few tens of KB fits in a couple of round trips
and is done; §1 puts numbers on the difference.

The usual ways to send less are a compressed archive of each release, a
content-defined chunk store, or a general-purpose binary diff — §1.1 measures
all three. go-binsync goes a step further, borrowing the idea behind
[Courgette](https://www.chromium.org/developers/design-documents/software-updates-courgette/),
the technique Google uses to ship Chrome updates: read the structure the
compiler left behind, predict where everything moved, and send only the
correction. Courgette gets that structure by disassembling; go-binsync gets it
from Go's own function table, which a stripped binary still carries.

## Status

The library, the CLI and the public demo are implemented, and every number in
this README is measured on them. The demo is live at
<https://go-binsync-demo.fly.dev> — pick a region, watch a real 94 MB release
arrive as a 95 KB patch and verify (`bench/demo` builds and serves it). The one
piece that is not done is the decoder's memory footprint (`docs/DESIGN.md`
§11.3).

This README is the behavioural specification; `docs/DESIGN.md` records the
architecture and the reasoning; `docs/research/` holds the measurements the
design rests on; `docs/DEMO.md` specifies the public demo;
`docs/design-brief.html` condenses all four onto one page.

---

## 1. What it costs

The one-line change above isolates the layout effect. A real incremental
release carries genuinely new code as well, so it is the more honest case to
quote. Three things decide how fast one lands: how long the publisher takes to
encode it, how many bytes cross the link, and how long those bytes take on a
realistic link. Measured on prometheus 3.13.1 → 3.13.2, a 94 MB stripped binary
built with Go 1.27, fetched over a medium-quality link: 20 Mbit/s, 200 ms RTT,
1 % packet loss.

| | Full download (`zstd -19`) | Generic delta (hdiffz) | go-binsync (Go-aware delta) |
|---|---:|---:|---:|
| Bytes sent | 20.6 MB | 2.7 MB | **0.095 MB** |
| Encode time · peak memory | 9 s · 0.36 GB | 7 s · 0.39 GB | **2.4 s** · 0.90 GB |
| Apply on the target | 0.1 s · 13 MB | 0.1 s · 25 MB | 0.9 s · 0.92 GB |
| Transfer on that link | ≈ 2.4 min (≈ 20 s with 8 parallel ranges) | ≈ 15 s | **≈ 1.0 s** |

Bytes dominate as the link degrades: with 1 % loss a single TCP stream carries
about 1.2 Mbit/s whatever the link rate, so the full download takes minutes and
the generic patch a quarter of a minute, while go-binsync's patch fits in a
couple of round trips — 213× less than the 20.3 MB blob a cold target would
take, and 28× less than the best generic delta. (go-binsync fetches that blob
with parallel ranged requests, which recovers most of the loss penalty; a small
patch never pays it in the first place.) The encoder is faster than the generic
tools because it never builds a suffix array over the file. Memory is the weak
number: the decoder peaks at 7.6× the size of the binary, most of it the
prediction's working set rather than the file buffers, and getting that to ≈ 2×
is the open item (`docs/DESIGN.md` §11.3).

Every pair the codec is measured on, in bytes. Each row is encode → patch →
decode → byte-exact compare, with every table counted inside the patch:

| Pair (all built with Go 1.27) | bsdiff | hdiffz | go-binsync | vs bsdiff |
|---|---:|---:|---:|---:|
| one-line change, 30 MB | 150,475 | 176,929 | **2,262** | **67×** |
| +3-byte string literal, 30 MB | 24,874 | 33,713 | **438** | 57× |
| multi-package edit (+2.3 KB code), 30 MB | 145,205 | 171,760 | **2,745** | 53× |
| second multi-package step (v3 → v4), 30 MB | 30,196 | 40,523 | **440** | 69× |
| prometheus 3.13.1 → 3.13.2, 94 MB | 2,691,644 | 2,719,152 | **95,366** | 28× |

On a minor release with thousands of new functions the patch is dominated by
the new content and the gain drops to about 1.6×. That is the expected result:
prediction removes the cost of the layout shift, and genuinely new code still
has to be sent.

### 1.1 Why a Go-aware delta

When a Go function grows by a few bytes, every function after it moves, every
reference that crosses the move is rewritten, and the third of the file that
is offset tables (`.gopclntab`) changes with it. A one-line edit rewrites 13 %
of the bytes; a change touching several packages rewrites 70 %; a real release
79–87 %, in runs a few bytes long. Any technique that looks only at bytes pays
for that. Each rung below understands more of the binary than the one above it:

| | one-line change, 30 MB | prometheus 3.13.1 → 3.13.2, 94 MB |
|---|---:|---:|
| whole file, `zstd -19` | 8.66 MB | 20.6 MB |
| chunk store (casync/desync CDC) | 8.02 MB | 25.7 MB |
| whole file, `xz -9` | 7.92 MB | 18.4 MB |
| `xdelta3 -9` | 1.93 MB | 15.2 MB |
| `zstd --patch-from` | 538 KB | 8.5 MB |
| bsdiff / hdiffz | 150 / 177 KB | 2.69 / 2.72 MB |
| **go-binsync** | **2,262 B** | **95,366 B** |

A chunk store re-sends 93 % of a fresh archive on the one-liner and *more*
than one on the real release, because almost no chunk survives the shift and
the ones that do compress alone. Exact-match delta coders (`zstd
--patch-from`, xdelta3) find the moved code but pay for every shifted operand.
Approximate matchers (bsdiff, hdiffz) absorb the shifted operands as cheap
byte differences and do several times better, at the price of a suffix array
over the whole file. go-binsync predicts the shift rather than encoding it.

A stripped Go binary still carries the function table, with names and sizes.
go-binsync uses it to align the old and new releases *by function name*, predict
where everything landed in the new binary, and send only the correction. The
prediction is deterministic and the output is hash-verified, so a bad prediction
costs patch bytes and nothing else. Because the predicted layout is exact, the
encoder never builds a whole-file suffix array — the step that makes bsdiff need
9–12× the input in RAM and minutes of CPU above 100 MB.

The prediction covers code, data, the pclntab with its pc tables, and the type
descriptors. On the prometheus release it is wrong in 120,852 bytes — 0.13 % of
the file — and what the patch carries is mostly real change: 63,179 B of changed
code, 45,170 B of type descriptors (two-thirds of them genuinely new), 17,441 B
of pc tables for the functions that changed, and 10,379 B of layout tables.
Design and measurements: `docs/DESIGN.md` §3; the research behind it:
`docs/research/go-aware-transform.md`.

## 2. Concepts

| Term | Meaning |
|---|---|
| **Release** | One exact binary, identified by its BLAKE3-256 hash (`b3:<hex>`). Carries a version string taken from the binary's build info. |
| **Store** | A URL where releases are published and polled: `s3://bucket/prefix`, `https://host/prefix` (read-only), `file:///dir`, or `ssh://host/dir` (publish-only; the remote side polls the same directory as `file://`). One store URL = one release stream; use different prefixes for different services or environments. |
| **Pointer** | `<store>/latest.json`: the one mutable object; names the head release and the recent patch chain. |
| **Patch** | Immutable object turning release A's bytes into release B's. Publishing creates the patch *previous head → new head*, so patches form a chain. |
| **Blob** | The full compressed binary of a release, for targets that cannot follow the chain. |
| **Target** | A host holding the binary at a fixed path; its current release is whatever hashes to a known release. |

## 3. Quick start

```sh
go install github.com/wjordan/go-binsync/cmd/go-binsync@latest
```

Build delta-friendly (`-s -w` is required; see §8), publish, run:

```sh
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o out/server ./cmd/server
go-binsync publish out/server s3://my-bucket/releases/server
```

**Embedded (recommended):** the service updates itself — no agent process.

```go
func main() {
    up := selfupdate.Start(selfupdate.Config{Store: "s3://my-bucket/releases/server"})
    ln, _ := up.Listen("tcp", ":8080")   // inherited across upgrades
    srv := &http.Server{Handler: handler}
    up.OnShutdown(func() { srv.Shutdown(context.Background()) })
    go srv.Serve(ln)
    up.Ready()                            // "serving and healthy": lets the previous process exit
    <-up.Done()                           // this process has been superseded (or ctx cancelled)
}
```

**External:** for a service that cannot link the library.

```sh
go-binsync agent s3://my-bucket/releases/server /srv/app/server \
    --restart 'systemctl restart app' --healthy http://127.0.0.1:8080/healthz
```

**Workstation → server over SSH**, no object store:

```sh
go-binsync publish out/server ssh://host/var/lib/app/releases      # remote service polls file:///var/lib/app/releases
```

## 4. Guarantees

1. **Identity.** After a successful update the file at the target path is
   byte-for-byte the head release, verified by BLAKE3 before it becomes visible.
2. **Authenticity by endpoint, integrity by hash.** The pointer is trusted
   because of how it was fetched (TLS with certificate validation, SigV4 over
   TLS, SSH, or the local filesystem). Everything else is verified against
   hashes carried in the pointer: patch frames, blob frames, and the final file.
   A pointer whose `seq` is not greater than the last accepted one is ignored.
   There are no signing keys to generate or distribute.
3. **Minimal transfer.** If the target's current release is on the published
   chain, only the chain patches are fetched (up to 8 releases behind);
   otherwise the blob. The publisher never publishes a patch larger than the
   blob, and a target that cannot read a patch takes the blob rather than
   failing. A pointer always names an existing patch; the blob may still be
   uploading for a short while after the pointer changes (publishing uploads
   the small patch first so targets on the chain see the release sooner), and
   a target that needs it retries until it appears.
4. **Fail fast on drift.** The current file is hashed (cached by inode) *before*
   any patch is applied; an unknown hash goes straight to the blob.
5. **Atomic install.** New bytes are written to a temp file in the same
   directory, fsynced, the current binary is hard-linked to `<path>.old`, the
   temp file is renamed over `<path>`, and the directory is fsynced. `<path>` is
   a valid executable at every instant; the running process keeps its old inode.
6. **Zero-downtime upgrade (embedded).** The old process starts the new binary
   with its listening sockets inherited; both accept from the same socket until
   the new process calls `Ready()`; only then does the old process drain and
   exit. Nothing queued on the socket is lost.
7. **Rollback.** If the new process exits or fails to call `Ready()` within
   60 s, it is killed, `<path>.old` is renamed back, and the old process
   continues. If the new process crashes *after* the old one has exited, the
   next start finds the `.pending` marker and reverts to `.old` before
   starting. External mode: if `--healthy` fails after `--restart`, the file is
   reverted and `--restart` runs again. A release that was reverted is recorded
   in `<path>.go-binsync/failed` and skipped until the pointer changes, so a broken
   release cannot crash-loop a target.
8. **One update at a time.** Updates are serialised per target; a newer pointer
   observed mid-update is picked up on the next cycle.

## 5. CLI

Defaults are chosen so that the commands below need no flags; the few flags
that exist are listed.

### `go-binsync publish <binary> <store>`
Hashes the binary, encodes the patch from the current head (kept in the local
release cache, `$XDG_CACHE_HOME/go-binsync`), uploads the patch, replaces the
pointer with a compare-and-swap, then uploads the blob in parallel frames, so
targets on the chain see the release as soon as the patch is up. Exits 0
without changes if the head already has this hash; finishes a blob upload a
previous run left incomplete.
Warns if the binary contains DWARF or a symbol table, is PIE, or was built from
a modified VCS tree; `--force` publishes anyway.
Flags: `--force`, `--cache DIR`.

### `go-binsync agent <store> <path>`
Polls the pointer (conditional GET, every 5 s for remote stores, 1 s for
`file://`), and when the head changes: fetches, applies, verifies, installs,
then runs `--restart CMD`, then `--healthy URL|CMD` (if given; up to 60 s); on
failure reverts and restarts again. Errors are logged and retried with backoff.
Flags: `--restart CMD` (required), `--healthy URL|CMD`, `--once` (one cycle,
exit 0 if at head), `--poll DURATION`. State (hash cache, the `failed` marker)
lives in `<path>.go-binsync/` next to the binary.

### `go-binsync diff <old> <new> -o <patch>` / `go-binsync patch <old> <patch> -o <new>`
Offline codec access for development and benchmarking; `patch` verifies the
result hash.
Flags: `diff -v` (report where the patch bytes went), `diff --plain` (skip the
Go-aware codec).

Exit codes: 0 ok · 1 error · 2 usage · 3 verification failed · 4 no path to
head · 5 rolled back.

## 6. Library

Module path `github.com/wjordan/go-binsync`. Pure Go, no cgo.

| Package | Role |
|---|---|
| `delta` | The codec: `Encode(old, new) → patch`, `Apply(old, patch) → new`, bounds-checked, hash-verified per frame. Go-aware transform for stripped linux/amd64 Go binaries; generic delta for anything else (see §9). |
| `release` | Pointer/manifest types, chain planning, hash cache, atomic install and revert. |
| `store` | `Get` (with range and conditional headers), `Put` (with CAS) for `s3://`, `https://`, `file://`, `ssh://`. |
| `selfupdate` | Embedded lifecycle: `Start`, `Listen`, `OnShutdown`, `Ready`, `Done`; the `.pending` self-check runs inside `Start`. |
| `agent` | The poll → apply → install → restart → check loop used by the CLI and by `selfupdate`. |

## 7. Upgrade lifecycle (embedded)

Two calls carry the whole protocol: `Ready()` in the new process, `OnShutdown`
in the old one. Everything else is a file rename.

```
old process:  new head → fetch → apply → verify → install (link .old, rename, fsync) → write <path>.pending
              exec <path> (by path, never /proc/self/exe) with listeners as inherited fds + a ready pipe
              wait: child Ready()            → stop accepting, run OnShutdown callbacks, exit 0
                    child exits / 60 s pass  → SIGTERM, 5 s, SIGKILL; rename .old back; record failed; keep serving

new process:  selfupdate.Start: if <path>.pending exists, this process was not launched by a parent,
              and <path>.old exists → the previous new build crashed: rename .old back and exec it
              else: inherit the listeners, serve, call Ready() → remove .pending, tell the parent
```

Draining: `OnShutdown` callbacks run when the old process has been superseded;
the usual body is `http.Server.Shutdown` (which sends `Connection: close` /
HTTP/2 GOAWAY and waits for in-flight requests). `Shutdown` does not track
hijacked connections (WebSockets, SSE, gRPC streams); close those in the same
callback. The old process exits when the callbacks return or after 30 s.

`SO_REUSEPORT`-style handoff is not offered: it drops queued connections on
kernels < 5.14 or with `net.ipv4.tcp_migrate_req=0`.

## 8. Building delta-friendly binaries

```sh
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o server ./cmd/server
```

| Knob | Effect |
|---|---|
| `-ldflags=-s -w` (strip DWARF + symtab) | **Required** (`publish` warns). Unstripped, every patch is ≥ 60 % of a full download because DWARF rewrites wholesale; stripping cuts a prometheus patch from 27.9 MB to 2.7 MB and the binary by ~30 %. |
| `-ldflags=-buildid=` | removes the ~80 always-changing bytes; identical sources give identical binaries on any build box |
| `-trimpath`, `CGO_ENABLED=0` | reproducible builds — the head hash is derivable from source |
| `-buildmode=exe` (Linux default) | keep; PIE is 6 % larger with larger patches |
| PGO | freeze the profile across releases you intend to delta |

## 9. Supported inputs and fallbacks

- The Go-aware codec supports stripped, non-PIE linux/amd64 binaries built by
  the current stable Go release (1.27); each Go release is validated against a
  self-prediction check before it is enabled, and the publisher picks the
  transform the *deployed* decoder can read.
- Anything else — other toolchains, arm64, PIE, non-Go binaries, or an unknown
  layout — uses the generic delta (≤ 256 MB) and, beyond that, the blob.
- A target that meets a patch it cannot read fetches the blob. Every path
  ends in the same hash-verified file.

## 10. Store layout

```
<store>/latest.json                 pointer: head release, blob location, recent patch chain   (no-store)
<store>/patches/<from>-<to>.bsz     immutable patch                                            (immutable)
<store>/blobs/<hash>.blob           immutable full binary, in independently fetchable frames    (immutable)
```

Formats are specified in `docs/DESIGN.md`.

## 11. Not in v1

- Signed manifests (authenticity comes from the endpoint; a signing layer can be
  added without changing the store layout).
- Multiple channels per store, direct (non-chain) patches, inline patches in the
  pointer, push notifications, hook scripts beyond `--restart`/`--healthy`,
  health probation in embedded mode (do your own checks before `Ready()`).
- Multi-file bundles, Windows, non-Linux targets, P2P fan-out, CDC-based repair
  of drifted targets (they download the blob).

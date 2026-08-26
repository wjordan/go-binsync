# binsync

Fast, verified, zero-downtime updates of a single deployed binary.

`binsync` takes an authoritative release binary on a build box or workstation and
makes a remote copy identical to it, transferring only a small delta against the
version already deployed, verifying the result against a signed manifest, and then
re-executing the running service with its listening sockets handed over — with
automatic rollback if the new build does not come up healthy.

It is a Go library (`binsync/...`) and a CLI (`binsync`). This README is the
behavioural specification; `docs/DESIGN.md` records the architecture and the
reasoning behind each decision, and `docs/research/` holds the research and
benchmarks the design is based on.

**Status: design phase.** No implementation exists yet; everything below is a
specification of intended behaviour.

---

## 1. The problem it solves

A large compiled service binary (the reference workload is a 30–100 MB Go web
server) is rebuilt many times a day with tiny source changes, and each build has
to reach a fleet of remote hosts and be running there as fast as possible. The
naive path — copy the whole binary, restart the service — costs seconds of
transfer per host and drops connections on restart.

Two facts drive the design (numbers from `docs/research/`):

- A one-line change to a Go program changes *everything after it*. Functions
  after the edit shift by 32 bytes, and every call or data reference that
  crosses the shift point is rewritten, so 40–70 % of the file changes at
  chunk granularity. Content-defined chunking (casync/desync-style) therefore
  re-sends roughly half the binary; a shift-tolerant delta encoder (bsdiff
  class) sends 30–120 KB. Unstripped Go binaries embed *compressed* DWARF, which
  changes wholesale on any edit and inflates the same delta to ~5 MB.
- Over a high-latency link the round trips cost more than the bytes. A 100 KB
  patch takes ~10 ms at 100 Mbit/s; a single extra HTTP request costs 100–200 ms
  against object storage. The transfer path is designed around *one* conditional
  GET on the hot path.

## 2. Concepts

| Term | Meaning |
|---|---|
| **Release** | One exact binary, identified by its content hash (`b3:<hex>`, BLAKE3-256) plus an optional human version string and a per-channel monotonically increasing sequence number. |
| **Channel** | A named stream of releases (`prod`, `staging`, …). Each channel has exactly one *head* (latest) release at any time. |
| **Store** | Where releases are published: an S3/GCS/R2-compatible bucket, a plain HTTP(S) static directory (read-only for targets), or a local directory. |
| **Patch** | An immutable object that transforms one release's bytes into another's (`from → to`). Patches form a directed **patch graph** whose nodes are release hashes. |
| **Pointer** | The single small mutable object per channel that names the head release, carries the signed manifest and the recent patch graph, and may embed the hot-path patch. |
| **Target** | A host running the service, holding the binary at a fixed path. |
| **Agent** | The `binsync agent` process (or the embedded `binsync/agent` package) that keeps a target in sync with a channel and drives the upgrade lifecycle. |
| **Base** | The release a target currently has on disk, identified by hashing the file. |
| **Hook** | User-provided executable invoked at defined lifecycle events with context in environment variables. |

## 3. Quick start

```sh
# once: create a signing key; distribute key.pub to targets
binsync keygen -o ./key

# build delta-friendly (see §9), then publish to a channel
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o ./out/server ./cmd/server
binsync publish --store s3://my-bucket/binsync --channel prod --key ./key ./out/server

# on each target: keep /srv/app/server in sync, upgrade in place, probe health
binsync agent --store s3://my-bucket/binsync --channel prod --pubkey /etc/binsync/key.pub \
  --path /srv/app/server --probe http://127.0.0.1:8080/healthz --hooks /etc/binsync/hooks.d
```

Workstation → server without any object store (SSH transport):

```sh
binsync push --path /srv/app/server --probe http://127.0.0.1:8080/healthz ./out/server host1 host2
```

In the service, hand sockets to the next version (embedded mode):

```go
up, err := upgrade.New(upgrade.Config{Control: "/run/app/binsync.sock"})
ln, err := up.Listen("tcp", ":8080")     // inherited across upgrades
go http.Serve(ln, handler)
up.Ready()                               // "I am serving"; unblocks the parent's handoff
<-up.Exit()                              // parent drained; shut down
```

## 4. Behaviour guarantees

1. **Identity.** After a successful `sync`, the file at `--path` is byte-for-byte
   the published release, verified by BLAKE3 hash before it is made visible.
   The agent never trusts a patch result it has not hashed.
2. **Authenticity.** Every manifest is Ed25519-signed by a publisher key; targets
   verify the signature against pinned public keys before reading anything else
   from it. Unsigned, mis-signed, expired, or non-monotonic (sequence ≤ last
   accepted) manifests are rejected and logged; the target keeps running what
   it has.
3. **Minimal transfer.** If the target's base is a node in the published patch
   graph, only patches are downloaded; the agent picks the path with the fewest
   bytes. Otherwise the full compressed binary is downloaded. Patch size is never
   worse than the full download: the publisher ships whichever is smaller.
4. **Fail fast on drift.** The base hash is checked *before* any patch is applied
   (cached by inode/size/mtime/ctime, recomputed otherwise). A modified or
   unknown base goes straight to the full download.
5. **Atomic install.** The new binary is written to a temporary file in the same
   directory, fsynced, hard-linked as `<path>.old` (previous), renamed over
   `<path>`, and the directory is fsynced. There is no instant at which `<path>`
   is missing or partially written. Running processes keep executing the old
   inode.
6. **Zero-downtime upgrade** (embedded mode). The old process starts the new
   binary with the listening sockets inherited; both accept from the same socket
   until the new process reports ready *and* passes health probes; only then does
   the old process drain and exit. No connection in the accept queue is lost.
7. **Rollback.** If the new process fails to start, crashes, times out before
   ready, or fails probation, it is killed, `<path>.old` is renamed back, and the
   old process continues serving. If the new process crashes *after* the old
   one has exited, the next start detects the pending upgrade marker and reverts
   to `<path>.old` before the supervisor's restart limit is hit.
8. **One upgrade in flight.** A second sync while an upgrade is in progress is
   queued behind it, never interleaved.
9. **Hooks fail closed before the point of no return.** A non-zero `pre-*` hook
   aborts the upgrade; later hooks are informational unless configured
   `fail_on_error`.

## 5. CLI

All commands accept `--log-level`, `--json` (machine-readable output), and read
defaults from `BINSYNC_*` environment variables (`--store` ↔ `BINSYNC_STORE`, etc.).

### `binsync keygen -o <prefix>`
Writes `<prefix>` (Ed25519 private key, mode 0600) and `<prefix>.pub`. Keys use the
minisign key file format so existing minisign keys work unchanged.

### `binsync publish [flags] <binary>`
Publishes `<binary>` as the new head of a channel.

| Flag | Default | Meaning |
|---|---|---|
| `--store URL` | required | `s3://bucket/prefix`, `gs://…`, `file:///dir`, `https://…` (S3-compatible endpoints via `--endpoint`) |
| `--channel NAME` | `default` | channel to advance |
| `--key PATH` | required | signing key |
| `--version STR` | from `debug/buildinfo` (VCS revision + dirty flag) or short hash | human version label |
| `--from PATH…` | last releases in the local cache | additional base binaries to generate direct patches from |
| `--direct-from-last N` | 3 | after the hot patch, also generate direct patches from the previous N releases (in the background, pointer is re-published when done) |
| `--codec NAME` | `bsz` | delta codec: `bsz` (bsdiff-class, smallest), `zstd` (`zstd --patch-from`-compatible, fastest) |
| `--inline-max SIZE` | `512KiB` | embed the hot patch in the pointer if not larger than this |
| `--poke URL…` | none | after publishing, notify these agents (HTTP `POST /poke`) so they check immediately |
| `--require-stripped` | true | refuse binaries with DWARF (`.debug_*`) or a symbol table; `--no-require-stripped` to downgrade to a warning |
| `--cache DIR` | `$XDG_CACHE_HOME/binsync` | local cache of previously published binaries, used as patch bases |

Behaviour:
1. Hashes the binary; if the channel head already has this hash, exits 0 with no changes.
2. Reads the current pointer (for the sequence number and recent graph).
3. Encodes the *hot patch* (previous head → new) and uploads it (inline in the pointer if small enough, else as `patches/<from>-<to>.<codec>`), uploads the full blob `blobs/<hash>.zst`, writes the immutable release manifest `releases/<hash>.json`, then atomically replaces the pointer with an `If-Match` compare-and-swap on its ETag. On CAS failure it re-reads and retries (another publisher advanced the channel; the sequence is bumped past theirs).
4. Optionally pokes agents, then generates direct patches from the previous N releases and republishes the pointer with the extra edges.
5. Stores the binary in the local cache for future `--from` use.

The pointer is written with `Cache-Control: no-store`; all other objects with
`Cache-Control: public, max-age=31536000, immutable`.

### `binsync agent [flags]`
Long-running: keeps a target synced with a channel and upgrades the service.

| Flag | Default | Meaning |
|---|---|---|
| `--store`, `--channel` | required | as above |
| `--pubkey PATH…` | required | one or more trusted publisher public keys |
| `--path PATH` | required | the deployed binary |
| `--poll DURATION` | `5s` | pointer poll interval (conditional GET; a 304 costs one request and no egress) |
| `--listen ADDR` | none | HTTP endpoint for `POST /poke` (immediate check) and `GET /status` |
| `--mode MODE` | `auto` | `embedded` (control socket to a process using `binsync/upgrade`), `systemd` (fd store + `systemctl restart`), `restart` (run `--restart-cmd`), `none` (sync file only) ; `auto` picks `embedded` if `--control` connects, else errors |
| `--control PATH` | `/run/binsync/<basename>.sock` | control socket of the running service |
| `--probe URL` / `--probe-cmd CMD` | none | health probe run against the new process; required for `on-healthy` to fire, otherwise readiness alone commits |
| `--ready-timeout` | `60s` | new process must call `Ready()` within this |
| `--min-healthy` | `5s` | probes must pass continuously for this long |
| `--healthy-deadline` | `60s` | probation must complete within this |
| `--drain-timeout` | `30s` | old process graceful shutdown budget before forced close |
| `--hooks DIR` | none | hook executables (see §7.3) |
| `--hook-timeout` | `30s` | per-hook |
| `--state DIR` | `/var/lib/binsync/<basename>` | last accepted sequence, base-hash cache, upgrade marker |
| `--max-chain N` | `16` | longest patch path before falling back to the full blob |
| `--parallel N` | `4` | concurrent connections for multi-patch and ranged full downloads |

Loop: poll → verify manifest → plan path from base → fetch → apply → verify →
`pre-swap` hook → install → upgrade lifecycle (§7) → record sequence. Every
failure is logged with a stable reason code and retried on the next poll with
exponential backoff for transient store errors.

### `binsync sync [agent flags]`
One-shot version of the agent loop: exit 0 if the target is at head afterwards
(including "already at head"), non-zero otherwise. Suitable for cron or CI.

### `binsync push [flags] <binary> <host>…`
Direct workstation→server deployment over SSH; no store needed.

For each host (in parallel, `--parallel` default 8) it runs
`ssh <host> binsync recv --path PATH [lifecycle flags]` and speaks a
length-framed protocol over stdio: the remote reports its base hash; the local
side looks the base up in its cache (`--cache`), encodes (or reuses a cached)
patch, and streams a signed manifest plus patch; the remote applies, verifies,
installs, runs the lifecycle, and streams events back. If the base is unknown
locally the full binary is streamed zstd-compressed. Exit status is non-zero if
any host failed; `--json` prints one result line per host. `--key` is optional:
without it the manifest is unsigned and `recv` must have been started with
`--allow-unsigned` (intended for development).

### `binsync recv` — internal, the remote half of `push`.

### `binsync diff <old> <new> -o <patch>` / `binsync patch <old> <patch> -o <new>`
Offline codec access (`--codec`). `patch` verifies the embedded target hash.

### `binsync status [--store --channel] [--path]`
Prints channel head, local base, whether a patch path exists, and the last
upgrade result. `binsync status --local` reads only the state directory.

### `binsync rollback [--path]`
Renames `<path>.old` back over `<path>` (if present) and triggers the lifecycle
so the previous version is running again. Refuses if no `.old` exists.

### `binsync gc --store --keep-releases N [--keep-days D] [--dry-run]`
Deletes blobs and patches not reachable from the retained releases of any channel.

### `binsync verify --pubkey … (<pointer-file> | --store --channel)`
Verifies a manifest signature and reports its contents.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success / nothing to do |
| 1 | unexpected error |
| 2 | usage error |
| 3 | verification failure (signature, hash, sequence, expiry) |
| 4 | no usable path to head and full blob unavailable |
| 5 | upgrade aborted and rolled back (service still running previous version) |
| 6 | hook vetoed (`pre-*` hook non-zero) |
| 75 | startup self-check reverted to the previous binary (`EX_TEMPFAIL`; used by `binsync/upgrade` in the service itself) |

## 6. Library

Module path `binsync` (placeholder until a home is chosen). Pure Go, no cgo, so
agents cross-compile and run in static containers.

| Package | Purpose |
|---|---|
| `binsync/delta` | Codec interface and implementations: `bsz` (bsdiff-class approximate matching, zstd-compressed streams, partitioned for parallel encode/decode) and `zstd` (frames compatible with `zstd --patch-from`). `Encode(ctx, old io.ReaderAt, oldSize, new io.ReaderAt, newSize, w io.Writer, Options)`; `Apply(ctx, old io.ReaderAt, patch io.Reader, w io.Writer) (Result, error)`. Apply is streaming, bounds-checked, and computes the output hash. |
| `binsync/release` | Manifest and pointer types, canonical encoding, Ed25519 sign/verify, sequence and expiry rules, patch graph and `Plan(graph, base, head) ([]Edge, error)` (fewest bytes, ≤ max chain). |
| `binsync/store` | `Store` interface: `Get` (with `Range` and `If-None-Match`), `Put` (with `If-Match`/`If-None-Match: *`, `Cache-Control`), `Delete`, `List`. Implementations: S3-compatible (SigV4), GCS, local filesystem, read-only HTTP(S). |
| `binsync/install` | Hash cache, atomic install (`link`+`rename`+fsync), `.old` management, upgrade marker. |
| `binsync/upgrade` | In-process lifecycle for the service: inherited listeners, `Ready()`, `Exit()`, control socket, startup self-check, drain helpers (`Shutdown` integration, `RegisterOnShutdown` for hijacked connections). |
| `binsync/agent` | The poll/plan/fetch/apply/upgrade loop as a library, so a service can update itself without a separate agent process (`agent.Run(ctx, agent.Config)`). |
| `binsync/hooks` | Hook runner (env contract, timeouts, at-least-once). |

The CLI is a thin wrapper over these packages.

## 7. Upgrade lifecycle

### 7.1 Modes

- **embedded** — the service links `binsync/upgrade`. The agent stages and
  installs the file, then asks the running process (over the control socket) to
  upgrade. The *old process* runs the handoff state machine: it execs `<path>`
  (never `/proc/self/exe`, which is the old inode) with listening sockets passed
  as inherited file descriptors plus a readiness pipe, waits for `Ready()`,
  runs probation, and either commits (drains, exits) or aborts (kills the child).
  The agent observes events on the control socket and performs file rollback.
- **systemd** — the service uses socket activation or the fd store
  (`FileDescriptorStoreMax=`); the agent installs the file and runs
  `systemctl restart <unit>`. Connections queue in the kernel backlog during the
  restart (a pause, not a loss). Rollback = restore file + restart again.
- **restart** — arbitrary `--restart-cmd`; no zero-downtime guarantee.
- **none** — file sync only; something else restarts the service.

`SO_REUSEPORT`-style handoff (separate sockets) is deliberately not offered: it
resets queued connections on kernels < 5.14 or with `net.ipv4.tcp_migrate_req=0`.

### 7.2 State machine (embedded mode)

```
IDLE ──sync──► STAGED      temp file written+fsynced, hash+signature verified; hook pre-swap
                 │  link(path, path.old); rename(tmp, path); fsync(dir); write upgrade.pending
                 ▼
              INSTALLED    hook pre-exec; agent → control socket: upgrade
                 │  old process execs <path> with inherited fds + ready pipe
                 ▼
              STARTING     child exit / ready-timeout ─────────────────────► ABORTING
                 │  child writes ready byte; hook on-ready
                 ▼
              PROBING      probe fails / healthy-deadline ─────────────────► ABORTING
                 │  probes pass for min-healthy; hook on-healthy
                 ▼
              COMMITTED    old: stop accepting, Shutdown(drain-timeout) then Close, exit
                 │  remove upgrade.pending; record sequence; hook post-upgrade (old exited)
                 ▼
               IDLE

ABORTING: SIGTERM child, grace 5s, SIGKILL; rename(path.old, path); fsync(dir);
          remove upgrade.pending; hook on-abort then on-rollback; old process unchanged → IDLE
```

Invariants: exactly one upgrade in flight; the old process holds its listening
sockets until COMMITTED; no old code runs after COMMITTED + drain; `<path>.old`
is only replaced by the *next* upgrade's STAGED step; `<path>` is a valid
executable at every instant.

**Startup self-check** (in `binsync/upgrade`, runs before anything else in the
service): if `upgrade.pending` exists and this process is not the child of an
in-flight upgrade (no `BINSYNC_UPGRADE_CHILD` env), increment the marker's crash
counter; above the threshold (default 3) rename `<path>.old` back and exit 75 so
the supervisor restarts the previous build. This covers "new build crashes after
the old process is gone".

### 7.3 Hooks

Executables in `--hooks DIR`, run in lexical order for each event, with a
per-hook timeout, at-least-once semantics (must be idempotent). Environment:

```
BINSYNC_EVENT        pre-fetch | pre-swap | pre-exec | on-ready | on-healthy | on-abort | on-rollback | post-upgrade
BINSYNC_PATH         deployed binary path
BINSYNC_STAGED       staged (verified) file path, from pre-swap on
BINSYNC_OLD_HASH / BINSYNC_NEW_HASH
BINSYNC_OLD_VERSION / BINSYNC_NEW_VERSION
BINSYNC_OLD_PID / BINSYNC_NEW_PID
BINSYNC_REASON       for on-abort / on-rollback: exec-failed | crashed | ready-timeout | probe-failed | hook-veto | operator
BINSYNC_EXIT_STATUS  child's exit status when known
BINSYNC_ATTEMPT      1-based attempt counter for this release
```

`pre-*` non-zero → abort (exit 6). Others are logged unless `--hooks-fail-on-error`.

### 7.4 Draining

On COMMITTED the old process stops accepting, `http.Server.Shutdown` (sends
`Connection: close` / HTTP/2 `GOAWAY`, waits for in-flight requests) is given
`--drain-timeout`, then `Close`. Hijacked connections (WebSocket, SSE, gRPC
streams) are *not* tracked by `Shutdown`; services must register them via
`upgrade.OnShutdown` or they are cut at the deadline. gRPC servers should call
`GracefulStop` from an `OnShutdown` callback.

## 8. Store layout and formats

```
<prefix>/channels/<channel>/latest            mutable pointer (no-store, CAS-updated)
<prefix>/releases/<to-hash>.json              immutable signed release manifest
<prefix>/blobs/<hash>.zst                     full binary, zstd, seekable frames
<prefix>/patches/<from-hash>-<to-hash>.<codec> immutable patch objects
```

**Pointer** = framed container: magic `BSYP`, length-prefixed canonical-JSON
manifest, Ed25519 signature over the manifest bytes, optional inline patch whose
hash and size are named inside the manifest. The manifest contains: `channel`,
`seq`, `created`, `expires` (default 30 days), `head {hash, version, size, blob,
buildinfo}`, `graph` (the last M=16 releases and every patch edge among them with
codec and size), `inline_patch {from, to, codec, size, hash}`, and the publisher's
key id. Targets refuse a pointer whose `seq` is not greater than the last one
they accepted for that channel, or whose `expires` has passed.

**Patch container (`bsz`)**: header (magic `BSZ1`, from-hash, to-hash, old and
new sizes, codec parameters, partition table); then one frame per partition of
the *new* file, each independently decodable (own zstd-compressed control, diff
and extra streams, own BLAKE3 of its output range). Partitions align to ELF
section boundaries where possible so a change in one section never desynchronises
the encoder in the next, and so encode and apply parallelise. All lengths and
offsets are bounds-checked against the header on apply.

**`zstd` codec**: a standard zstd frame produced with the old binary as raw
dictionary (`--patch-from` semantics) wrapped in the same header. Interoperable
with `zstd -d --patch-from=<old>`.

**Blob**: zstd with independent 8 MiB frames and a trailing frame index, so a
target can fetch it as parallel range requests and decode them concurrently.

Exact byte layouts are specified in `docs/DESIGN.md`.

## 9. Building delta-friendly binaries

`binsync publish` inspects the ELF and, by default, refuses binaries that would
produce needlessly large patches. Recommended build:

```sh
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o server ./cmd/server
```

| Knob | Effect on a one-line change (25 MB Go binary, measured) |
|---|---|
| `-ldflags=-s -w` (strip DWARF + symtab) | patch 5.2 MB → 0.12 MB; binary −29 %. **Required.** |
| `-ldflags=-buildid=` | removes ~80 bytes of always-changing build id; makes identical sources produce identical binaries regardless of build directory |
| `-trimpath`, `CGO_ENABLED=0` | reproducible builds — two build boxes produce the same hash |
| `-buildmode=exe` (Linux default) | keep; PIE adds 6 % size and larger patches (relocation table carries absolute addresses) |
| PGO profile | freeze the profile across releases you intend to delta; a changing profile re-shapes many functions |
| `-X` with timestamps | avoid; a changing constant of different length shifts `.rodata` |

`publish` warns when the binary is PIE, contains DWARF, has a symbol table, or
has a `vcs.modified=true` buildinfo (uncommitted changes).

## 10. Limits and non-goals (v1)

- Linux targets only (ELF, `rename(2)` semantics, `SCM_RIGHTS`/fd inheritance).
  The delta codec itself is portable.
- One binary per channel. Multi-file bundles (assets, config) are out of scope;
  ship them separately or embed them (`//go:embed`).
- Delta codecs are generic (work on any file); Go-specific section awareness is
  used only for partitioning. Executable-aware pointer relativisation
  (Courgette/Zucchini-style) is a future codec, not v1 — on ELF it buys ~10–30 %
  over bsdiff for a large complexity cost, and a 100 KB patch already costs
  less than one round trip.
- No content-defined-chunk "heal" for drifted targets: a drifted base downloads
  the full blob (ranged, parallel). Measured chunk reuse on a shifted Go binary is
  only 30–50 %, not worth the request count.
- No peer-to-peer fan-out; put a CDN in front of the store for very large fleets.
- No Windows.

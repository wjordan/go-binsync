# Public demo: the patch explorer (phase 1)

A deployed, browser-usable proof of concept for the one idea binsync exists
for: **an incremental release of a Go binary is a tiny patch that really does
reconstruct the new binary, and it crosses the world in about a second**.
Scope is the binary update only — no service lifecycle, no agent. One page,
one interaction, real downloads over real distance.

Requirements it satisfies: usable immediately in a browser or via plain HTTP
(no install, no build); one feature; sample data built in (nothing to upload).

## 1. The interaction

The viewer picks a **release pair** and a **region**, presses **Update**, and
watches four steps happen for real, timed by the browser:

```
1. fetch pointer    latest.json                                        1 round trip     0.28 s  (jnb)
2. fetch patch      patches/<from>-<to>.bsz     112 KB   ── measured ──▶                 0.9 s
3. apply            old + patch → new           on the Machine (1a) / in the browser (1b) 1.0 s
4. verify           BLAKE3(new) == pointer.head.hash                   ✔ b3:41ac80f4…
```

Below the timeline, two comparison buttons fetch the *same release* the naive
ways from the same region, so the viewer measures the difference rather than
reading it: **generic delta** (`hdiffz`, 2.7 MB for the prometheus pair) and
**full download** (`zstd -19`, 20.6 MB). The static ladder from the design
brief (whole file / chunk store / xdelta3 / `--patch-from` / bsdiff /
binsync) sits under the buttons for the numbers a viewer does not want to
wait for, and the netem table from `benchmark-scale.md` §6 is linked for the
lossy-link case the real path cannot show (§3).

Nothing is cached: every press re-fetches the patch (`Cache-Control:
no-store` plus a nonce in the query string), so every run is a real transfer.
Each step shows bytes, milliseconds, and which Machine served it (the server
sets `X-Served-By: <FLY_MACHINE_ID> <FLY_REGION>` from its runtime
environment). That is the whole page.

**Phase 1a** applies on the Machine: the browser POSTs "apply pair X", the
Machine runs the decoder against its local copy of the old binary and returns
apply time, hash and verification result. The bytes the viewer watched cross
the world are exactly the bytes a real target would fetch. **Phase 1b** moves
the apply into the browser (WASM) as a trust upgrade — "the 112 KB you just
watched download, plus the old binary, is the new binary" — once the pure-Go
codec exists (§4).

## 2. Sample data

Three pairs, all built with Go 1.27 (`-trimpath -ldflags="-s -w -buildid="`),
precomputed by `bench/demo/build-assets.sh` and baked into the image:

| Pair | Old | Patch (binsync) | hdiffz | Full (`zstd -19`) |
|---|---:|---:|---:|---:|
| one-line change, `testsrv` | 30.0 MB | 2.2 KB | 177 KB | 8.7 MB |
| multi-package edit, `testsrv` | 30.0 MB | 2.7 KB | 172 KB | 8.7 MB |
| prometheus 3.13.1 → 3.13.2 | 93.7 MB | 112 KB | 2.7 MB | 20.6 MB |

Per pair the image holds `old.bin`, `latest.json` (the real pointer format),
`patches/<from>-<to>.bsz`, `compare/patch.hdiff`, `compare/new.zst`, and
`meta.json` (sizes, hashes, encode timings from the build box). Total ≈ 200 MB
of assets. Default on page load: the prometheus pair from the region farthest
from the viewer (the page picks it from the first pointer fetch's timings).

## 3. Deployment: one Fly app, one Machine per region

```
fly.toml       app = binsync-demo; [http_service] internal_port 8080, force_https,
               auto_stop_machines = "suspend", min_machines_running = 0
regions        jnb, nrt, gru, syd  (+ ord as the nearby control)
machine        shared-cpu-2x, 2 GB (the prototype decoder peaks at ≈ 650 MB on the 94 MB pair)
image          debian-slim + demo server (Go) + assets + zstd + hdiffz/hpatchz + bench/gotransform
```

**Routing.** Fly routes every request to the nearest Machine, the opposite
of what the demo wants. With one Machine per region the page simply sets
`fly-force-region: <region>` on every fetch (Fly's documented steering
header; no fallback, so an unreachable region shows as an error rather than
a silently nearer one). The region list is hard-coded in the page; the
`ord` Machine is the control.

**What the real path measures, honestly.** The viewer's browser reaches the
nearest Fly edge, which carries the request over Fly's backbone to the chosen
region. That is a real, long, uncontrolled path — a viewer in Europe fetching
from `jnb` or `nrt` sees ≈ 200–300 ms RTT and a full 20 MB download that
takes many seconds — but it is a *good* path: the backbone has near-zero
loss, so the demo shows the cost of distance (round trips, slow start on the
big objects), not the collapse of a single TCP stream under 1 % loss that
the netem measurements show. The page says so next to the timings and links
the netem table for that case. Nothing is simulated in phase 1: the numbers
are whatever the Internet did.

**Optional lossy Machine (phase 1.5, only if the real path proves too smooth
to make the point).** `CONFIG_NET_SCH_NETEM=y` in the Fly Machine kernel
(verified: `zcat /proc/config.gz | grep NETEM`) and the Machine is root in
its own VM, so a fifth Machine can shape its own `eth0` from a `LINK` env in
its entrypoint — `ethtool -K eth0 tso off gso off gro off` then `tc qdisc add
dev eth0 root netem rate 20mbit delay 100ms loss 1% limit 2000` — and be
selected with `fly-force-instance-id`. Caveat if added: netem shapes the
Machine's egress only (data direction), so it is slightly kinder than the
symmetric netem of the benchmark. No application-layer throttling under any
circumstances; a token bucket with random drops does not reproduce TCP under
loss.

**Cost and abuse.** Five shared-cpu Machines suspended when idle cost a few
dollars a month. Egress is the real cost — a full-download comparison is
20 MB and jnb/nrt egress is Fly's most expensive tier — so the full-download
and hdiffz buttons are rate-limited per client IP (token bucket, ~10 per
hour) and the page says so; server-side apply is one at a time per Machine.
No uploads, no user-supplied binaries, no parameters beyond pair and region;
the worst a visitor can do is press buttons.

**Server.** One Go program: static page, `GET /api/pairs`, the asset routes
above with `no-store`, and `POST /api/apply?pair=…` which runs `gotransform
decode old patch out` (phase 1a; `delta.Apply` in-process later), hashes the
output, deletes it, and returns `{apply_ms, hash, verified}`. The same routes
are the demo's API.

## 4. Phases and what each needs from the product

| | Needs | Status |
|---|---|---|
| **1a** server-side apply | nothing beyond the prototype: `bench/gotransform` encodes the assets at build time and decodes on the Machine; `zstd`/`hdiffz` installed in the image | can ship now |
| **1b** in-browser apply | `binsync/delta.Apply` in pure Go for `GOOS=js GOARCH=wasm` (no `os/exec`, no cgo; zstd via `klauspost/compress`; stage-1 blob delta in Go), BLAKE3 in WASM, and section-by-section apply so the 94 MB pair fits a tab (≈ 2× the file instead of ≈ 7×) | after the `delta` package |
| **2** release board | a real **Publish** button cycling through a ring of releases, real targets (behind netem links, now that the kernel is known to support it) running the agent, the lifecycle path | separate spec, after the agent exists |

The prototype is a research tool, not the product codec: its container
format will change when `delta` is written. Phase 1a therefore pins the
image to one prototype build, and the `.bsz` files it serves are labelled as
prototype patches in `meta.json`.

## 5. Not in phase 1

Publishing from the page, live targets, the agent/lifecycle path, uploads of
arbitrary pairs, persistence of any kind, simulated links, and any region the
viewer can define beyond the fixed list (so results are comparable between
viewers).

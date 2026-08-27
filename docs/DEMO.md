# Public demo: the patch explorer (phase 1)

Live at <https://binsync-demo.fly.dev> (five Fly regions: ord, jnb, nrt, gru,
syd; Machines suspend when idle).

A deployed, browser-usable proof of concept for the one idea binsync exists
for: **an incremental release of a Go binary is a tiny patch that really does
reconstruct the new binary, and it crosses the world in about a second**.
Scope is the binary update only — no service lifecycle, no agent. One page,
one interaction, real downloads over real distance.

Requirements it satisfies: usable immediately in a browser or via plain HTTP
(no install, no build); one feature; sample data built in (nothing to upload).

## 1. The interaction

The viewer picks a **release pair** and a **region**, presses **Update**, and
watches four steps happen for real, timed by the browser. On load the page
measures one round trip to every region (twice, keeping the second: a
suspended Machine's first request is paying for the resume, not for the
distance), puts the number on each region button, and selects the farthest —
the demo is about distance, so the default should not be next door.

```
1. fetch pointer    latest.json                                        1 round trip     0.28 s  (jnb)
2. fetch patch      patches/<from8>-<to8>.bsz    95 KB   ── measured ──▶                 0.9 s
3. apply            old + patch → new           on the Machine (1a) / in the browser (1b) 0.9 s
4. verify           BLAKE3(new) == pointer.head.hash                   ✔ b3:9e207efb…
```

Below the timeline, two comparison buttons fetch the *same release* the naive
ways from the same region, so the viewer measures the difference rather than
reading it: **generic delta** (`hdiffz -m-6 -p-8 -c-zstd-21-24`, 2.7 MB for
the prometheus pair) and **full download** (the store's own blob, 20.3 MB —
what a drifted target really fetches). The static ladder from the design
brief (whole file / chunk store / xdelta3 / `--patch-from` / bsdiff /
binsync) sits under the buttons for the numbers a viewer does not want to
wait for, and the netem numbers from `benchmark-scale.md` §6 stand in for the
lossy-link case the real path cannot show (§3). Rows the page does not
actually serve are starred, because a table that mixes "we just sent you
this" with "we measured this once" and marks neither is not a comparison, it
is an advertisement.

The transfer bars under the patch and the two comparisons share one scale —
the slowest transfer of the visit — so pressing all three renders the
difference at the size it actually is.

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
watched download, plus the old binary, is the new binary" — once the decoder
fits a tab (§4).

The apply is serialised, one at a time per Machine, and the pages go back to
the kernel as soon as it is done: the decoder's working set is 7.6× the
binary (`docs/DESIGN.md` §11.3) and the Machine has 2 GB.

## 2. Sample data

Three pairs, all built with Go 1.27 (`-trimpath -ldflags="-s -w -buildid="`),
precomputed by `bench/demo/build-assets.sh` and baked into the image. Sizes
are the bytes actually served, measured:

| Pair | Old | Patch (binsync) | hdiffz | Full download (blob) |
|---|---:|---:|---:|---:|
| one-line change, `testsrv` | 29,995,235 | **2,262** | 176,199 | 8,555,312 |
| multi-package edit, `testsrv` | 29,995,235 | **2,745** | 172,002 | 8,556,056 |
| prometheus 3.13.1 → 3.13.2 | 93,741,283 | **95,366** | 2,719,152 | 20,336,968 |

Each pair is a **real binsync store**, built by publishing the old release
into an empty `file://` store and then publishing the new one: the page fetches
the same `latest.json`, `patches/<from8>-<to8>.bsz` and `blobs/<hash>.blob` a
real fleet would, and the four steps it shows are the four steps the agent
takes. Next to the store the image holds `old.bin` (what the Machine applies
against), `compare/patch.hdiff` and `meta.json`. Total ≈ 222 MB of assets.
Default on page load: the prometheus pair from the region farthest from the
viewer (see §1).

## 3. Deployment: one Fly app, one Machine per region

```
fly.toml       app = binsync-demo; [http_service] internal_port 8080, force_https,
               auto_stop_machines = "suspend", min_machines_running = 0
regions        jnb, nrt, gru, syd  (+ ord as the nearby control)
machine        shared-cpu-2x, 2 GB (the decoder peaks at ≈ 0.92 GB on the 94 MB pair,
               so applies are serialised one at a time per Machine)
image          scratch + the demo server (one static Go binary) + the assets
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

**Server.** One Go program (`bench/demo`), no dependencies beyond the binsync
module: the page, `GET /api/pairs`, the store objects under
`/s/<pair>/<key>` with `Cache-Control: no-store`, `/s/<pair>/compare/hdiff`,
and `POST /api/apply?pair=…` which calls `delta.Apply` in process against its
local `old.bin`, hashes the result with BLAKE3, drops it, and returns
`{apply_ms, hash, verified, size}`. Nothing is written to disk and nothing
is kept between requests. The same routes are the demo's API.

## 4. Phases and what each needs from the product

| | Needs | Status |
|---|---|---|
| **1a** server-side apply | the shipped `binsync` CLI builds the asset stores; `binsync/delta.Apply` runs in the server | ready |
| **1b** in-browser apply | `binsync/delta` already builds for `GOOS=js GOARCH=wasm` (pure Go, no `os/exec`, no cgo), so what is left is the decoder's footprint: 7.6× the binary is 0.7 GB for the prometheus pair, which no tab will give you. Blocked on `docs/DESIGN.md` §11.3 | after the decoder's memory |
| **2** release board | a real **Publish** button cycling through a ring of releases, real targets (behind netem links, now that the kernel is known to support it) running the agent, the lifecycle path | separate spec |

Phase 1a serves the real patch format: the assets are produced by `binsync
publish` and the page applies them with the same `delta.Apply` a target
runs, so what the demo proves is what the product does.

## 5. Not in phase 1

Publishing from the page, live targets, the agent/lifecycle path, uploads of
arbitrary pairs, persistence of any kind, simulated links, and any region the
viewer can define beyond the fixed list (so results are comparable between
viewers).

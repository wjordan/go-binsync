# The patch explorer

The public demo specified in `docs/DEMO.md`: one page that updates a deployed
Go binary in front of you, from the region you pick, and verifies the result.

Everything it serves is real. The assets are go-binsync stores built by
`go-binsync publish`; the page fetches the pointer, plans the update from the
chain exactly as `release.MakePlan` does, fetches the patch, and the server
applies it with `delta.Apply` against its own copy of the old release and
hashes what comes out. If the page says the release verifies, a target would
have installed it.

## Build and run locally

```sh
bench/demo/build-assets.sh            # ~10 min, writes bench/out/demo-assets (222 MB)
go run ./bench/demo -addr :8080 -assets bench/out/demo-assets
```

`build-assets.sh` needs the corpus (`bench/build.sh` for the synthetic pairs,
`bench/scale/fetch_corpus.sh` for prometheus and `hdiffz`).

## Container

```sh
docker build -f bench/demo/Dockerfile -t go-binsync-demo .   # from the repository root
docker run --rm -p 8080:8080 go-binsync-demo
```

The image is `scratch` plus one static binary and the assets: 240 MB, almost
all of it the three old binaries and the blobs.

## Deploy

```sh
bench/demo/deploy.sh          # APP=go-binsync-demo REGIONS="ord jnb nrt gru syd"
```

One Machine per region, suspended when idle. The page sets `fly-force-region`
on every request, so the region buttons steer to a specific Machine rather
than to whichever one is nearest; a region with no Machine shows as
unreachable rather than being silently served from next door.

## Routes

| | |
|---|---|
| `GET /` | the page (one file, no external requests) |
| `GET /api/pairs` | the pairs, their sizes and hashes, and which Machine answered |
| `GET /s/{pair}/latest.json` | the pointer |
| `GET /s/{pair}/patches/{from8}-{to8}.bsz` | the patch |
| `GET /s/{pair}/blobs/{hash}.blob` | the full release, rate-limited |
| `GET /s/{pair}/compare/patch.hdiff` | the generic delta, rate-limited |
| `POST /api/apply?pair=…` | apply on the Machine; `{apply_ms, hash, want, verified, size}` |

Everything is served `no-store`: a run that came out of a cache measures
nothing. The two multi-megabyte objects are capped at ten per client — they
are the demo's entire egress bill.

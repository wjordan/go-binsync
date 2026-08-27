#!/usr/bin/env bash
# Build the patch explorer's assets (docs/DEMO.md 2).
#
# Each pair becomes a real go-binsync store -- the same latest.json, patches/ and
# blobs/ a real fleet would poll -- built by publishing the old release and
# then the new one. Next to it go the two comparisons the page offers: the
# generic delta (hdiffz) and the full download (the store's own blob, which is
# already zstd of the whole binary).
#
# Usage: bench/demo/build-assets.sh [OUTDIR]
# Inputs: bench/out/bin127 and bench/out/corpus127 (bench/build.sh,
#         bench/scale/fetch_corpus.sh).
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OUT=${1:-$ROOT/bench/out/demo-assets}
BIN=$ROOT/bench/out/bin127
COR=$ROOT/bench/out/corpus127

command -v hdiffz >/dev/null || { echo "hdiffz not on PATH (bench/scale/fetch_corpus.sh builds it)" >&2; exit 1; }

BS=$(mktemp -d)/go-binsync
trap 'rm -rf "$(dirname "$BS")"' EXIT
(cd "$ROOT" && go build -o "$BS" ./cmd/go-binsync)

# pair <id> <title> <blurb> <old file> <new file>
pair() {
  local id=$1 title=$2 blurb=$3 old=$4 new=$5
  local d=$OUT/$id
  echo "== $id"
  rm -rf "$d"
  mkdir -p "$d/store" "$d/compare"
  local cache="$d/.cache"
  mkdir -p "$cache"

  "$BS" publish --cache "$cache" "$old" "file://$d/store"
  "$BS" publish --cache "$cache" "$new" "file://$d/store"
  rm -rf "$cache"

  cp "$old" "$d/old.bin"
  hdiffz -m-6 -SD -d -f -p-8 -c-zstd-21-24 "$old" "$new" "$d/compare/patch.hdiff" >/dev/null

  local patch
  patch=$(ls "$d"/store/patches/*.bsz)
  python3 - "$d" "$id" "$title" "$blurb" "$old" "$new" "$patch" <<'PY'
import json, os, sys
d, pid, title, blurb, old, new, patch = sys.argv[1:8]
ptr = json.load(open(os.path.join(d, "store", "latest.json")))
meta = {
    "id": pid, "title": title, "blurb": blurb,
    "old_name": os.path.basename(old), "new_name": os.path.basename(new),
    "old_size": os.path.getsize(old), "new_size": os.path.getsize(new),
    "patch_key": os.path.relpath(patch, os.path.join(d, "store")),
    "patch_size": os.path.getsize(patch),
    "hdiff_size": os.path.getsize(os.path.join(d, "compare", "patch.hdiff")),
    "blob_key": ptr["head"]["blob"]["key"],
    "blob_size": ptr["head"]["blob"]["size"],
    "from_hash": ptr["chain"][0]["from"], "to_hash": ptr["head"]["hash"],
}
json.dump(meta, open(os.path.join(d, "meta.json"), "w"), indent=2)
print("   patch %d  hdiff %d  blob %d" % (meta["patch_size"], meta["hdiff_size"], meta["blob_size"]))
PY
}

mkdir -p "$OUT"
pair one-line "One-line change" \
  "A single string literal in one handler of a 30 MB Go service." \
  "$BIN/v1-F3" "$BIN/v2c-F3"
pair multi-package "Multi-package edit" \
  "Edits across four packages of the same service, including a new exported function." \
  "$BIN/v1-F3" "$BIN/v4-F3"
pair prometheus "prometheus 3.13.1 to 3.13.2" \
  "A real upstream patch release of a 94 MB binary, rebuilt with Go 1.27." \
  "$COR/prometheus/3.13.1/prometheus" "$COR/prometheus/3.13.2/prometheus"

du -sh "$OUT"

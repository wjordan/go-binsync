#!/usr/bin/env bash
# Build every variant of bench/testsrv in each flag config into bench/out/bin/.
# Usage: build.sh            (all)      build.sh v1 v2c   (subset of variants)
set -euo pipefail
BENCH="$(cd "$(dirname "$0")" && pwd)"
OUT="$BENCH/out/bin"; mkdir -p "$OUT"
export PATH=/usr/local/go/bin:$PATH
export GOFLAGS=-mod=mod
VARIANTS=("$@"); [ ${#VARIANTS[@]} -eq 0 ] && VARIANTS=(v1 v2s v2l v2c v2p v3 v4)
cd "$BENCH/testsrv"
build() { # build <variant> <cfg> <out> <flags...>
  local v=$1 cfg=$2 out=$3; shift 3
  local t0=$(date +%s.%N)
  go build "$@" -o "$out" .
  local t1=$(date +%s.%N)
  printf '%-4s %-5s %10d bytes  %5.1fs  go build %s -o %s .\n' "$v" "$cfg" "$(stat -c %s "$out")" "$(echo "$t1-$t0"|bc)" "$*" "$(basename "$out")"
}
for v in "${VARIANTS[@]}"; do
  python3 "$BENCH/gen.py" "$v" >/dev/null
  build "$v" F1 "$OUT/$v-F1"
  build "$v" F2 "$OUT/$v-F2" -trimpath -ldflags="-s -w"
  build "$v" F3 "$OUT/$v-F3" -trimpath -ldflags="-s -w -buildid=" -buildvcs=false
  if [ "$v" = v1 ] || [ "$v" = v2c ]; then
    build "$v" F2pie "$OUT/$v-F2pie" -buildmode=pie -trimpath -ldflags="-s -w"
  fi
  if [ "$v" = v1 ]; then
    # reproducibility: rebuild into a different path
    build "$v" F1b "$OUT/$v-F1-rebuild"
    build "$v" F2b "$OUT/$v-F2-rebuild" -trimpath -ldflags="-s -w"
    for c in F1 F2; do
      if cmp -s "$OUT/v1-$c" "$OUT/v1-$c-rebuild"; then echo "REPRO $c: identical"; else echo "REPRO $c: DIFFER ($(cmp -l "$OUT/v1-$c" "$OUT/v1-$c-rebuild" | wc -l) bytes)"; fi
    done
  fi
done
python3 "$BENCH/gen.py" v1 >/dev/null   # leave tree at baseline
echo "readelf: F1=$(readelf -h "$OUT/v1-F1" | awk '/Type:/{print $2}') F2=$(readelf -h "$OUT/v1-F2" | awk '/Type:/{print $2}') F2pie=$(readelf -h "$OUT/v1-F2pie" | awk '/Type:/{print $2}')"

#!/usr/bin/env bash
# Build v5 (F2 only) and re-check that gen.py's main.go round-trip leaves v4-F2 bit-identical.
set -euo pipefail
BENCH="$(cd "$(dirname "$0")/.." && pwd)"; OUT="$BENCH/out/bin"
export PATH=/usr/local/go/bin:$PATH GOFLAGS=-mod=mod
cd "$BENCH/testsrv"
for v in v4 v5; do
  python3 "$BENCH/gen.py" $v >/dev/null
  t0=$(date +%s.%N)
  go build -trimpath -ldflags="-s -w" -buildvcs=false -o "$OUT/$v-F2.tmp" .   # -buildvcs=false: the v1..v4 binaries were built before bench/ was under git; without it Go now stamps vcs.* into .go.buildinfo (+128 B .rodata) and nothing matches
  printf '%-3s F2 %10d bytes %5.1fs\n' $v "$(stat -c %s "$OUT/$v-F2.tmp")" "$(echo "$(date +%s.%N)-$t0" | bc)"
done
if cmp -s "$OUT/v4-F2" "$OUT/v4-F2.tmp"; then echo "REPRO v4-F2 after gen.py round-trip: identical"; else echo "REPRO v4-F2: DIFFER"; fi
rm -f "$OUT/v4-F2.tmp"; mv "$OUT/v5-F2.tmp" "$OUT/v5-F2"
python3 "$BENCH/gen.py" v1 >/dev/null
readelf -h "$OUT/v5-F2" | awk '/Type:/{print "v5-F2 ELF " $2}'

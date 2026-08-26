#!/usr/bin/env bash
# Build and run the pure-Go feasibility probes (Step 5). Logs to out/logs/05-*.log.
set -u
BENCH="$(cd "$(dirname "$0")" && pwd)"
export PATH=/usr/local/go/bin:$PATH
OUT=$BENCH/out; B=$OUT/bin; L=$OUT/logs; G=$OUT/goprobe; HD=$OUT/tools/HDiffPatch
mkdir -p "$G" "$L" "$OUT/bcj"
(cd "$BENCH/goprobe" && go build -o "$G/" ./...) || exit 1
T="/usr/bin/time -f wall_%e_s_maxrss_%M_KB"
(timeout 300 $T "$G/sa" "$B/v1-F2" > "$L/05-sa-F2.log" 2>&1; timeout 300 $T "$G/sa" "$B/v1-F1" > "$L/05-sa-F1.log" 2>&1) &
(timeout 120 "$G/hash" "$B/v1-F2" > "$L/05-hash.log" 2>&1) &
(timeout 900 $T "$G/kzstd" "$B/v1-F2" "$B/v2c-F2" > "$L/05-kzstd.log" 2>&1; echo "exit=$?" >> "$L/05-kzstd.log") &
(timeout 200 $T "$G/gobsdiff" "$B/v1-F2" "$B/v2c-F2" > "$L/05-gobsdiff.log" 2>&1; echo "exit=$?" >> "$L/05-gobsdiff.log") &
(
  cd "$OUT" || exit 1
  for v in v1 v2c v2l; do "$G/bcj" enc "bin/$v-F2" "bcj/$v-F2.bcj"; done
  "$G/bcj" dec bcj/v1-F2.bcj bcj/v1-F2.roundtrip && cmp bcj/v1-F2.roundtrip bin/v1-F2 && echo "bcj round-trip OK (dec(enc(v1)) == v1)"
  echo "bytes changed by bcj on v1-F2: $(cmp -l bin/v1-F2 bcj/v1-F2.bcj | wc -l)"
  for n in v2c v2l; do for kind in plain bcj; do
    if [ $kind = plain ]; then O=bin/v1-F2; N=bin/$n-F2; else O=bcj/v1-F2.bcj; N=bcj/$n-F2.bcj; fi
    zstd -q -f -19 --patch-from=$O $N -o bcj/$n.$kind.zst
    zstd -q -f -19 --long=27 --patch-from=$O $N -o bcj/$n.$kind.l27.zst
    bsdiff $O $N bcj/$n.$kind.bsdiff
    xdelta3 -f -e -9 -S djw -B 268435456 -W 16777216 -s $O $N bcj/$n.$kind.xd3
    "$HD/hdiffz" -m-6 -SD -d -f -p-1 -c-zstd-21-24 $O $N bcj/$n.$kind.hdiff >/dev/null
    printf 'v1->%s %-5s zstd19=%d zstd19-long27=%d bsdiff=%d xdelta3=%d hdiffz-m6=%d\n' $n $kind \
      $(stat -c %s bcj/$n.$kind.zst) $(stat -c %s bcj/$n.$kind.l27.zst) $(stat -c %s bcj/$n.$kind.bsdiff) $(stat -c %s bcj/$n.$kind.xd3) $(stat -c %s bcj/$n.$kind.hdiff)
  done; done
  echo "differing bytes v1->v2c: plain=$(cmp -l bin/v1-F2 bin/v2c-F2 | wc -l) bcj=$(cmp -l bcj/v1-F2.bcj bcj/v2c-F2.bcj | wc -l)"
  echo "differing bytes v1->v2l: plain=$(cmp -l bin/v1-F2 bin/v2l-F2 | wc -l) bcj=$(cmp -l bcj/v1-F2.bcj bcj/v2l-F2.bcj | wc -l)"
) > "$L/05-bcj.log" 2>&1 &
wait
cat "$L"/05-sa-F2.log "$L"/05-sa-F1.log "$L"/05-hash.log "$L"/05-bcj.log "$L"/05-gobsdiff.log "$L"/05-kzstd.log

#!/usr/bin/env bash
# Reproduce the whole local benchmark. Usage: run.sh [step...]  (steps: tools build diff delta cdc probes model; default all)
# Outputs: bench/out/bin (binaries), bench/out/patches, bench/out/results/*.json, bench/out/logs/*.log
set -uo pipefail
BENCH="$(cd "$(dirname "$0")" && pwd)"; cd "$BENCH"
OUT=$BENCH/out; L=$OUT/logs; mkdir -p "$L" "$OUT/tools"
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
STEPS=("$@"); [ ${#STEPS[@]} -eq 0 ] && STEPS=(tools build diff delta cdc probes model)
has() { for s in "${STEPS[@]}"; do [ "$s" = "$1" ] && return 0; done; return 1; }

if has tools; then
  sudo -n apt-get install -y xdelta3 bsdiff libbz2-dev 2>&1 | tail -n 3 | tee "$L/00-apt.log"
  if [ ! -x "$OUT/tools/HDiffPatch/hdiffz" ]; then (
    cd "$OUT/tools" || exit 1
    for r in HDiffPatch lzma zstd zlib; do [ -d $r ] || timeout 300 git clone --depth 1 https://github.com/sisong/$r; done
    cd HDiffPatch && timeout 600 make -j"$(nproc)" LDEF=0 MD5=0 XXH=0 BZIP2=0 VCD=0 BSD=0 2>&1 | tail -n 3
    ./hdiffz -v; ./hpatchz -v
  ) 2>&1 | tee "$L/00-hdiffpatch-build.log"; fi
  { go version; zstd --version; xdelta3 -V 2>&1 | head -n 1; "$OUT/tools/HDiffPatch/hdiffz" -v; casync --version | head -n 1; xz --version | head -n 1;
    dpkg -s bsdiff | grep Version; dpkg -s xdelta3 | grep Version; desync --version 2>&1 | head -n 1; python3 -c 'import numpy;print("numpy",numpy.__version__)'; } > "$L/00-versions.log" 2>&1
fi
if has build; then timeout 900 ./build.sh 2>&1 | tee "$L/01-build.log"; fi
if has diff; then
  cd "$OUT/bin" || exit 1
  for p in "F2 v1 v2s" "F2 v1 v2l" "F2 v1 v2c" "F2 v1 v2p" "F2 v1 v3" "F2 v1 v4" "F2 v3 v4" "F1 v1 v2c" "F2pie v1 v2c" "F3 v1 v2c"; do
    set -- $p; timeout 300 python3 "$BENCH/analyze_diff.py" "$2-$1" "$3-$1" > "$L/02-diff-$1-$2-$3.log" 2>&1 &
  done; wait; cd "$BENCH"
fi
if has delta; then timeout 3000 python3 delta_bench.py --workers 8 --reps 3 2>&1 | tee "$L/03-delta-bench.log"; fi
if has cdc; then
  timeout 1500 python3 cdc_desync.py > "$L/04-cdc-desync.log" 2>&1 &
  timeout 2400 python3 cdc_sim.py > "$L/04-cdc-sim.log" 2>&1 &
  wait
fi
if has probes; then timeout 960 ./goprobe.sh 2>&1 | tee "$L/05-goprobe.log"; fi
if has model; then python3 render_delta.py > "$OUT/results/delta_tables.md"; python3 render_cdc.py > "$OUT/results/cdc_tables.md"; python3 model.py 2>&1 | tee "$L/06-model.log"; fi

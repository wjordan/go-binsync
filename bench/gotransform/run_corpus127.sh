#!/bin/bash
# Go 1.27 pass: baselines (bsdiff, hdiffz -m-6 -p-8) and the Go-aware
# encode/decode (positional and hdiffz stage 2) under /usr/bin/time -v.
# Usage: run_corpus127.sh NAME OLD NEW [OLD NEW ...]   (logs to ../out/logs/$LOGPREFIX-NAME.log,
# LOGPREFIX defaults to 08-gt127-run; GT selects the binary)
cd "$(dirname "$0")"
NAME=$1; shift
L=../out/logs/${LOGPREFIX:-08-gt127-run}-$NAME.log
SP=${GOTRANSFORM_TMP:-/tmp/claude-1000/-home-will-Code-binsync/49262ac6-3517-4d5a-b8ec-a8634d869b18/scratchpad}/run-$NAME
mkdir -p $SP; export GOTRANSFORM_TMP=$SP
HD=../out/tools/HDiffPatch
T() { timeout 600 /usr/bin/time -f 'TIME_WALL=%e TIME_USER=%U TIME_SYS=%S MAXRSS_KB=%M' "$@"; }
GT=${GT:-./gotransform-127}
while [ $# -ge 2 ]; do
  OLD=$1; NEW=$2; shift 2
  echo "=== PAIR $OLD -> $NEW  ($(date))" | tee -a $L
  { echo "--- baseline bsdiff"; T bsdiff $OLD $NEW $SP/base.bsdiff; echo "SIZE bsdiff=$(stat -c %s $SP/base.bsdiff)";
    echo "--- baseline bspatch"; T bspatch $OLD $SP/base.out $SP/base.bsdiff; cmp $OLD $SP/base.out >/dev/null 2>&1; cmp $NEW $SP/base.out && echo "bspatch OK";
    echo "--- baseline hdiffz -m-6 -p-8"; T $HD/hdiffz -m-6 -SD -d -f -p-8 -c-zstd-21-24 $OLD $NEW $SP/base.hdiff; echo "SIZE hdiffz=$(stat -c %s $SP/base.hdiff)";
    echo "--- baseline hpatchz"; T $HD/hpatchz -f $OLD $SP/base.hdiff $SP/base.hout; cmp $NEW $SP/base.hout && echo "hpatchz OK";
    echo "--- goaware encode -s1 h -s2 p"; T $GT encode -s1 h -s2 p $OLD $NEW $SP/p.gtp;
    echo "--- goaware decode (positional)"; T $GT decode $OLD $SP/p.gtp $SP/p.out; cmp $NEW $SP/p.out && echo "decode positional OK";
    echo "--- goaware encode -s1 h -s2 h"; T $GT encode -s1 h -s2 h -stats=false $OLD $NEW $SP/h.gtp;
    echo "--- goaware decode (hdiffz)"; T $GT decode $OLD $SP/h.gtp $SP/h.out; cmp $NEW $SP/h.out && echo "decode hdiffz OK";
    echo "--- goaware encode -s1 b -s2 b"; T $GT encode -s1 b -s2 b -stats=false $OLD $NEW $SP/b.gtp;
    echo "--- goaware decode (bsdiff)"; T $GT decode $OLD $SP/b.gtp $SP/b.out; cmp $NEW $SP/b.out && echo "decode bsdiff OK";
  } 2>&1 | tee -a $L
  rm -f $SP/*.out $SP/*.hout $SP/*.bsdiff $SP/*.hdiff
done
echo "=== ALL DONE $(date)" | tee -a $L

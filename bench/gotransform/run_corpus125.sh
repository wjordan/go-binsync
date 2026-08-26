#!/bin/bash
# Go 1.25 corpus pairs (moduledata in .noptrdata; go:func.*/findfunctab in .rodata).
cd "$(dirname "$0")"
C=../out/corpus
L=../out/logs
GT=./gotransform-corpus125
cp gotransform $GT
run() { # name old new enc
  local name=$1 old=$2 new=$3 enc=$4
  echo "### $name ($old -> $new) $(date +%T)"
  $GT inventory "$old" "$new" > $L/06-gt-corpus-$name-inventory.log 2>&1
  $GT sectdiff "$old" "$new" > $L/06-gt-corpus-$name-sectdiff.log 2>&1
  $GT predict -enc $enc -dump 10 "$old" "$new" > $L/06-gt-corpus-$name-predict.log 2>&1
  $GT pclnpredict -enc $enc "$old" "$new" > $L/06-gt-corpus-$name-pclnpredict.log 2>&1
  $GT datapredict -enc $enc "$old" "$new" > $L/06-gt-corpus-$name-datapredict.log 2>&1
  echo "### done $name $(date +%T)"
}
run terraform-1.15.8-1.15.9 $C/terraform/1.15.8/terraform $C/terraform/1.15.9/terraform bh
run cockroach-26.2.4-26.2.5 $C/cockroach/26.2.4/cockroach.stripped $C/cockroach/26.2.5/cockroach.stripped bh

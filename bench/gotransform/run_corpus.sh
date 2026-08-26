#!/bin/bash
# Runs the gotransform tools on adjacent-release real-world pairs (Go 1.26 only).
cd "$(dirname "$0")"
C=../out/corpus
L=../out/logs
GT=./gotransform-corpus
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
run kube-apiserver-1.36.3-1.36.4 $C/kube-apiserver/1.36.3/kube-apiserver $C/kube-apiserver/1.36.4/kube-apiserver bh
run prometheus-3.13.1-3.13.2 $C/prometheus/3.13.1/prometheus.stripped $C/prometheus/3.13.2/prometheus.stripped bh
run prometheus-3.13.2-3.14.0 $C/prometheus/3.13.2/prometheus.stripped $C/prometheus/3.14.0/prometheus.stripped bh
run vault-2.0.3-2.0.4 $C/vault/2.0.3/vault.stripped $C/vault/2.0.4/vault.stripped h

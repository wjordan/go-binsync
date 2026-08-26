#!/bin/bash
# Pass 2 with the final tool (bss shift tables, Go 1.25 table bounds, typelink/itablink
# prediction). Pass-1 logs are kept; these go to 06-gt-corpus2-*.log.
cd "$(dirname "$0")"
C=../out/corpus; L=../out/logs; GT=./gotransform-c2; T="timeout -k 30 900"
run() { # name old new enc tools...
  local name=$1 old=$2 new=$3 enc=$4; shift 4
  for tool in "$@"; do
    echo "### $name $tool $(date +%T)"
    $T $GT $tool -enc $enc "$old" "$new" > $L/06-gt-corpus2-$name-$tool.log 2>&1 || echo "### FAILED rc=$? $name $tool"
  done
  echo "### done $name $(date +%T)"
}
laneA() {
  run prometheus-3.13.1-3.13.2 $C/prometheus/3.13.1/prometheus.stripped $C/prometheus/3.13.2/prometheus.stripped bh predict whole
  run kube-apiserver-1.36.3-1.36.4 $C/kube-apiserver/1.36.3/kube-apiserver $C/kube-apiserver/1.36.4/kube-apiserver bh predict whole
  run terraform-1.15.8-1.15.9 $C/terraform/1.15.8/terraform $C/terraform/1.15.9/terraform bh predict pclnpredict datapredict whole
  run prometheus-3.13.2-3.14.0 $C/prometheus/3.13.2/prometheus.stripped $C/prometheus/3.14.0/prometheus.stripped bh predict whole
}
laneB() {
  run cockroach-26.2.4-26.2.5 $C/cockroach/26.2.4/cockroach.stripped $C/cockroach/26.2.5/cockroach.stripped bh predict pclnpredict datapredict whole
  run vault-2.0.3-2.0.4 $C/vault/2.0.3/vault.stripped $C/vault/2.0.4/vault.stripped h predict whole
}
laneA & laneB & wait
echo "### all done $(date +%T)"

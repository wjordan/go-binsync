#!/bin/bash
# Re-run after the entryOff base fix (runtime.text, not .text start): cockroach pclnpredict now,
# cockroach whole after pass 2 finishes (its pass-2 whole may have used the old binary).
cd "$(dirname "$0")"
C=../out/corpus; L=../out/logs; GT=./gotransform-c2; T="timeout -k 30 900"
old=$C/cockroach/26.2.4/cockroach.stripped; new=$C/cockroach/26.2.5/cockroach.stripped
echo "### cockroach pclnpredict (fixed) $(date +%T)"
$T $GT pclnpredict -enc bh $old $new > $L/06-gt-corpus2-cockroach-26.2.4-26.2.5-pclnpredict.log 2>&1 || echo "### FAILED rc=$?"
until grep -q 'all done' $L/06-gt-corpus2-run.log; do sleep 20; done
echo "### cockroach whole (fixed) $(date +%T)"
$T $GT whole -enc bh $old $new > $L/06-gt-corpus2-cockroach-26.2.4-26.2.5-whole.log 2>&1 || echo "### FAILED rc=$?"
echo "### all done3 $(date +%T)"

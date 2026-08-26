#!/bin/bash
cd "$(dirname "$0")"
C=../out/corpus; L=../out/logs; GT=./gotransform-whole2
$GT whole -enc bh $C/kube-apiserver/1.36.3/kube-apiserver $C/kube-apiserver/1.36.4/kube-apiserver > $L/06-gt-corpus-kube-apiserver-1.36.3-1.36.4-whole.log 2>&1
# wait for the other corpus jobs to finish before the big ones
until grep -q 'done vault' $L/06-gt-corpus-run.log && grep -q 'done cockroach' $L/06-gt-corpus125-run.log; do sleep 30; done
$GT whole -enc bh $C/prometheus/3.13.1/prometheus.stripped $C/prometheus/3.13.2/prometheus.stripped > $L/06-gt-corpus-prometheus-3.13.1-3.13.2-whole.log 2>&1
$GT whole -enc bh $C/terraform/1.15.8/terraform $C/terraform/1.15.9/terraform > $L/06-gt-corpus-terraform-1.15.8-1.15.9-whole.log 2>&1
$GT whole -enc bh $C/cockroach/26.2.4/cockroach.stripped $C/cockroach/26.2.5/cockroach.stripped > $L/06-gt-corpus-cockroach-26.2.4-26.2.5-whole.log 2>&1
$GT whole -enc h $C/vault/2.0.3/vault.stripped $C/vault/2.0.4/vault.stripped > $L/06-gt-corpus-vault-2.0.3-2.0.4-whole.log 2>&1
$GT whole -enc bh $C/prometheus/3.13.2/prometheus.stripped $C/prometheus/3.14.0/prometheus.stripped > $L/06-gt-corpus-prometheus-3.13.2-3.14.0-whole.log 2>&1
echo all done

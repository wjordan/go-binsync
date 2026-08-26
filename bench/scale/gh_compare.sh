#!/usr/bin/env bash
# Source-level size of each release delta via the GitHub compare API.
# The compare endpoint caps .files at 300 (and commits at 250); when the cap is hit we
# fall back to the unified diff (Accept: application/vnd.github.diff) and count there.
set -uo pipefail
SPEC="prometheus/prometheus:v3.13.1:v3.13.2 prometheus/prometheus:v3.13.2:v3.14.0 kubernetes/kubernetes:v1.36.3:v1.36.4 hashicorp/terraform:v1.15.8:v1.15.9 hashicorp/vault:v2.0.3:v2.0.4 cockroachdb/cockroach:v26.2.4:v26.2.5"
S="${TMPDIR:-/tmp}"
echo "| repo | base..head | commits | files (API) | +lines | -lines | files (diff) | +/- (diff) | .go files (diff) | capped? |"
echo "|---|---|---:|---:|---:|---:|---:|---:|---:|---|"
for s in $SPEC; do IFS=: read -r repo a b <<< "$s"
  j=$(gh api "repos/$repo/compare/$a...$b" 2>/dev/null)
  commits=$(echo "$j" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("total_commits","?"))')
  read -r nf add del <<< "$(echo "$j" | python3 -c 'import json,sys;d=json.load(sys.stdin);f=d.get("files",[]);print(len(f),sum(x["additions"] for x in f),sum(x["deletions"] for x in f))')"
  capped=no; [ "$nf" -ge 300 ] && capped="yes(300 cap)"
  df="$S/cmp-$(echo $repo | tr / _)-$a-$b.diff"
  gh api -H 'Accept: application/vnd.github.diff' "repos/$repo/compare/$a...$b" > "$df" 2>"$df.err" || true
  if [ -s "$df" ] && grep -q '^diff --git' "$df"; then
    dfiles=$(grep -c '^diff --git' "$df"); dgo=$(grep -c '^diff --git a/.*\.go ' "$df")
    dadd=$(grep -c '^+[^+]' "$df"); ddel=$(grep -c '^-[^-]' "$df")
    dpm="+$dadd/-$ddel"
  else dfiles="n/a ($(head -c 80 "$df" "$df.err" 2>/dev/null | tr '\n' ' ' | cut -c1-60))"; dgo="-"; dpm="-"; fi
  echo "| $repo | $a..$b | $commits | $nf | $add | $del | $dfiles | $dpm | $dgo | $capped |"
done

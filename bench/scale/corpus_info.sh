#!/usr/bin/env bash
# Write bench/out/corpus/README.md: per binary size, Go version/build settings
# (go version -m), ELF type, presence of .symtab/.debug_*, cgo; also produce
# strip --strip-all copies of any unstripped binary (<bin>.stripped).
set -euo pipefail
BENCH="$(cd "$(dirname "$0")/.." && pwd)"; C="$BENCH/out/corpus"
export PATH=/usr/local/go/bin:$PATH
PAIRS="prometheus:3.13.1:3.13.2 prometheus:3.13.2:3.14.0 kube-apiserver:1.36.3:1.36.4 terraform:1.15.8:1.15.9 vault:2.0.3:2.0.4 cockroach:26.2.4:26.2.5"
{
echo "# Corpus: official linux/amd64 release binaries (fetched $(date -u +%F) by bench/scale/fetch_corpus.sh)"
echo
echo "| project | version | binary | size B | Go | ELF | trimpath | CGO | buildmode | ldflags | .symtab | .debug_* | stripped copy |"
echo "|---|---|---|---:|---|---|---|---|---|---|---|---|---|"
for proj in prometheus kube-apiserver terraform vault cockroach; do
  for d in $(ls -d "$C/$proj"/*/ | sort -V); do
    ver=$(basename "$d"); bin=$(find "$d" -maxdepth 1 -type f ! -name '*.stripped' ! -name '*.txt' | head -1)
    vm=$(go version -m "$bin" 2>/dev/null || true)
    echo "$vm" > "$d/goversion.txt"
    gov=$(echo "$vm" | head -1 | awk '{print $2}')
    tp=$(echo "$vm" | awk '$1=="build" && $2 ~ /^-trimpath=/{sub("-trimpath=","",$2);print $2}'); tp=${tp:-no}
    cgo=$(echo "$vm" | awk '$1=="build" && $2 ~ /^CGO_ENABLED=/{sub("CGO_ENABLED=","",$2);print $2}'); [ -z "$cgo" ] && { readelf -d "$bin" 2>/dev/null | grep -q NEEDED && cgo="1 (no buildinfo; dynamic libc)" || cgo="- (no buildinfo)"; }
    bm=$(echo "$vm" | awk '$1=="build" && $2 ~ /^-buildmode=/{sub("-buildmode=","",$2);print $2}'); bm=${bm:--}
    ld=$(echo "$vm" | awk '$1=="build" && $2 ~ /^-ldflags=/{$1="";sub("^ -ldflags=","");print}' | tr -d '"' | cut -c1-70); ld=${ld:--}
    et=$(readelf -h "$bin" | awk '/Type:/{print $2}')
    st=$(readelf -S -W "$bin" | grep -c ' \.symtab ' || true); dbg=$(readelf -S -W "$bin" | grep -c ' \.debug_' || true)
    strippedcol="-"
    if [ "$st" -gt 0 ] || [ "$dbg" -gt 0 ]; then
      [ -s "$bin.stripped" ] || strip --strip-all -o "$bin.stripped" "$bin"
      strippedcol="$(stat -c %s "$bin.stripped") B"
    fi
    echo "| $proj | $ver | $(basename "$bin") | $(stat -c %s "$bin") | $gov | $et | $tp | $cgo | $bm | \`$ld\` | $([ "$st" -gt 0 ] && echo yes || echo no) | $dbg sections | $strippedcol |"
  done
done
echo
echo "## Pairs"
echo
echo "| pair | old | new | same Go toolchain | notes |"
echo "|---|---|---|---|---|"
for p in $PAIRS; do IFS=: read -r proj a b <<< "$p"
  ga=$(head -1 "$C/$proj/$a/goversion.txt" | awk '{print $2}'); gb=$(head -1 "$C/$proj/$b/goversion.txt" | awk '{print $2}')
  echo "| $proj $a -> $b | $ga | $gb | $([ "$ga" = "$gb" ] && echo yes || echo NO) | $([ "$proj" = prometheus ] && [ "$b" = 3.14.0 ] && echo 'MINOR release ("big change" case)' || echo 'patch release') |"
done
echo
echo "## Sections (readelf -S -W) of each OLD binary: see \`<version>/sections.txt\`; full \`go version -m\` in \`<version>/goversion.txt\`"
for proj in prometheus kube-apiserver terraform vault cockroach; do for d in "$C/$proj"/*/; do
  bin=$(find "$d" -maxdepth 1 -type f ! -name '*.stripped' ! -name '*.txt' | head -1); readelf -S -W "$bin" > "$d/sections.txt"; done; done
} > "$C/README.md"
cat "$C/README.md"

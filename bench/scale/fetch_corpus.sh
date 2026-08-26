#!/usr/bin/env bash
# Download official linux/amd64 release binaries for adjacent versions of real Go
# projects into bench/out/corpus/<project>/<version>/<binary>.
# Usage: fetch_corpus.sh [project...]   (default: all)
set -euo pipefail
BENCH="$(cd "$(dirname "$0")/.." && pwd)"
C="$BENCH/out/corpus"; mkdir -p "$C"
CURL=(curl -sSL --retry 3 --max-time 840)

# project version url kind(tar.gz|zip|raw|tgz) member
SPEC=$(cat <<'S'
prometheus 3.13.1 https://github.com/prometheus/prometheus/releases/download/v3.13.1/prometheus-3.13.1.linux-amd64.tar.gz tar prometheus-3.13.1.linux-amd64/prometheus
prometheus 3.13.2 https://github.com/prometheus/prometheus/releases/download/v3.13.2/prometheus-3.13.2.linux-amd64.tar.gz tar prometheus-3.13.2.linux-amd64/prometheus
prometheus 3.14.0 https://github.com/prometheus/prometheus/releases/download/v3.14.0/prometheus-3.14.0.linux-amd64.tar.gz tar prometheus-3.14.0.linux-amd64/prometheus
kube-apiserver 1.36.3 https://dl.k8s.io/v1.36.3/bin/linux/amd64/kube-apiserver raw kube-apiserver
kube-apiserver 1.36.4 https://dl.k8s.io/v1.36.4/bin/linux/amd64/kube-apiserver raw kube-apiserver
terraform 1.15.8 https://releases.hashicorp.com/terraform/1.15.8/terraform_1.15.8_linux_amd64.zip zip terraform
terraform 1.15.9 https://releases.hashicorp.com/terraform/1.15.9/terraform_1.15.9_linux_amd64.zip zip terraform
vault 2.0.3 https://releases.hashicorp.com/vault/2.0.3/vault_2.0.3_linux_amd64.zip zip vault
vault 2.0.4 https://releases.hashicorp.com/vault/2.0.4/vault_2.0.4_linux_amd64.zip zip vault
cockroach 26.2.4 https://binaries.cockroachdb.com/cockroach-v26.2.4.linux-amd64.tgz tar cockroach-v26.2.4.linux-amd64/cockroach
cockroach 26.2.5 https://binaries.cockroachdb.com/cockroach-v26.2.5.linux-amd64.tgz tar cockroach-v26.2.5.linux-amd64/cockroach
S
)
want=("$@")
fetch() {
  local proj=$1 ver=$2 url=$3 kind=$4 member=$5
  local dir="$C/$proj/$ver"
  local bin="$dir/$(basename "$member")"
  mkdir -p "$dir"
  if [ -s "$bin" ]; then echo "have $bin ($(stat -c %s "$bin") B)"; return; fi
  local t0=$(date +%s)
  case $kind in
    raw) "${CURL[@]}" -o "$bin.part" "$url"; mv "$bin.part" "$bin" ;;
    tar) "${CURL[@]}" -o "$dir/archive.tgz" "$url"; tar -xzf "$dir/archive.tgz" -C "$dir" --strip-components=1 "$member"; rm -f "$dir/archive.tgz" ;;
    zip) "${CURL[@]}" -o "$dir/archive.zip" "$url"; (cd "$dir" && unzip -oq archive.zip "$member"); rm -f "$dir/archive.zip" ;;
  esac
  chmod +x "$bin"
  echo "fetched $proj $ver -> $bin ($(stat -c %s "$bin") B) in $(( $(date +%s) - t0 )) s"
}
while read -r proj ver url kind member; do
  [ -z "$proj" ] && continue
  if [ ${#want[@]} -gt 0 ] && ! printf '%s\n' "${want[@]}" | grep -qx "$proj"; then continue; fi
  fetch "$proj" "$ver" "$url" "$kind" "$member" &
done <<< "$SPEC"
wait
echo "corpus:"; find "$C" -type f -perm -u+x | xargs -r ls -l | awk '{print $5, $9}'

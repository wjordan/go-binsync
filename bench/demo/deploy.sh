#!/usr/bin/env bash
# Deploy the patch explorer to Fly (docs/DEMO.md 3). Run from the repository
# root, after bench/demo/build-assets.sh -- the assets are baked into the
# image, so a deploy without them is a 404 machine.
#
#   bench/demo/build-assets.sh
#   bench/demo/deploy.sh
set -euo pipefail

APP=${APP:-binsync-demo}
REGIONS=${REGIONS:-"ord jnb nrt gru syd"}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && cd .. && pwd)
cd "$ROOT"

[ -d bench/out/demo-assets/prometheus ] || { echo "run bench/demo/build-assets.sh first" >&2; exit 1; }
command -v flyctl >/dev/null || { echo "flyctl not on PATH" >&2; exit 1; }

flyctl apps list | grep -q "^$APP " || flyctl apps create "$APP"
flyctl deploy --config bench/demo/fly.toml --app "$APP" --ha=false --region "${REGIONS%% *}"

# One Machine per region. The page's region buttons are this list.
for r in $REGIONS; do
  flyctl scale count 1 --config bench/demo/fly.toml --app "$APP" --region "$r" --yes
done

flyctl status --app "$APP"
echo
echo "https://$APP.fly.dev"

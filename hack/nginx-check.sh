#!/usr/bin/env bash
# Runs `nginx -t` on the nginx configs the chart renders for the registry proxies.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=nginxinc/nginx-unprivileged:1.27.5-alpine
REG=last-registry
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

helm template last charts/last-operator -n last-system >"$tmp/render.yaml"

# extract <ConfigMap name> <out file>: the default.conf of that ConfigMap, de-indented.
extract() {
  awk -v name="$1" '
    function emit() {
      if (doc ~ ("\nkind: ConfigMap\n") && doc ~ ("\nmetadata:\n  name: " name "\n")) printf "%s", doc
      doc = ""
    }
    /^---/ { emit(); next }
    { doc = doc $0 "\n" }
    END { emit() }' "$tmp/render.yaml" |
    awk '/^  default.conf: \|/ { f = 1; next } f && /^    / { sub(/^    /, ""); print; next } f { exit }' >"$2"
  [ -s "$2" ] || { echo "nginx-check: no default.conf found in ConfigMap $1" >&2; exit 1; }
}

extract last-registry-front "$tmp/front.conf"
extract last-registry-pull-proxy "$tmp/pull.conf"
: >"$tmp/stub"

hosts=(--add-host "last-registry.$REG.svc:127.0.0.1" --add-host "last-registry-front.$REG.svc:127.0.0.1")

echo "== last-registry-front"
docker run --rm "${hosts[@]}" \
  -v "$tmp/front.conf:/etc/nginx/conf.d/default.conf:ro" \
  -v "$tmp/stub:/etc/nginx/auth/pusher:ro" -v "$tmp/stub:/etc/nginx/auth/admin:ro" \
  "$IMAGE" nginx -t
echo "== last-registry-pull-proxy"
docker run --rm "${hosts[@]}" \
  -v "$tmp/pull.conf:/etc/nginx/conf.d/default.conf:ro" \
  "$IMAGE" nginx -t

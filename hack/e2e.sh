#!/usr/bin/env bash
# Build images, install the chart on kind, and run the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${KIND_CLUSTER:-last-e2e}"
TAG="${IMAGE_TAG:-e2e}"
REGISTRY="${IMAGE_REGISTRY:-ghcr.io/moogie-labs}"
# Must match nginxImage in test/e2e/operator_e2e_test.go. Preloaded so the
# cluster never pulls from a public registry during the test.
NGINX_IMAGE="nginx:1.27-alpine"

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER"

make images IMAGE_TAG="$TAG" IMAGE_REGISTRY="$REGISTRY" COMPONENTS="operator gateway audit-init"
for c in operator gateway audit-init sandbox-base; do
  kind load docker-image --name "$CLUSTER" "$REGISTRY/last-$c:$TAG"
done
docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1 || docker pull "$NGINX_IMAGE"
# `kind load docker-image` fails for a pulled multi-platform image on Docker's
# containerd image store: the saved index names platforms that were never
# pulled. Load an archive of the node's platform only. Docker before 28 has no
# `save --platform` (and no such store by default), so fall back there.
NGINX_TAR="$(mktemp)"
trap 'rm -f "$NGINX_TAR"' EXIT
if docker save --platform "$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" -o "$NGINX_TAR" "$NGINX_IMAGE"; then
  kind load image-archive --name "$CLUSTER" "$NGINX_TAR"
else
  echo "docker save --platform failed; falling back to kind load docker-image" >&2
  kind load docker-image --name "$CLUSTER" "$NGINX_IMAGE"
fi

make helm-crds
# Helm installs crds/ only on first install and never upgrades them; apply them
# so a reused cluster runs the current schemas.
kubectl apply --server-side --force-conflicts -f charts/last-operator/crds/
helm upgrade --install last charts/last-operator -n last-system --create-namespace \
  --set image.tag="$TAG" --set image.registry="$REGISTRY" --set image.pullPolicy=Never \
  --set audit.bundled.size=1Gi \
  --set sandbox.image="$REGISTRY/last-sandbox-base:$TAG" \
  --wait --wait-for-jobs --timeout 10m
# A rebuilt image under the same tag is only picked up by new pods.
kubectl rollout restart -n last-gateway deploy/last-gateway
kubectl rollout restart -n last-system deploy/last-operator
kubectl rollout status -n last-gateway deploy/last-gateway --timeout 5m
kubectl rollout status -n last-system deploy/last-operator --timeout 5m

go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m

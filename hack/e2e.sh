#!/usr/bin/env bash
# Build images, install the chart on kind, and run the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${KIND_CLUSTER:-last-e2e}"
TAG="${IMAGE_TAG:-e2e}"
REGISTRY="${IMAGE_REGISTRY:-ghcr.io/moogie-labs}"

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER"

make images IMAGE_TAG="$TAG" IMAGE_REGISTRY="$REGISTRY" COMPONENTS="gateway audit-init"
for c in gateway audit-init sandbox-base; do
  kind load docker-image --name "$CLUSTER" "$REGISTRY/last-$c:$TAG"
done

make helm-crds
# Helm installs crds/ only on first install and never upgrades them; apply them
# so a reused cluster runs the current schemas.
kubectl apply --server-side --force-conflicts -f charts/last-operator/crds/
helm upgrade --install last charts/last-operator -n last-system --create-namespace \
  --set image.tag="$TAG" --set image.registry="$REGISTRY" --set image.pullPolicy=Never \
  --set audit.bundled.size=1Gi \
  --set sandbox.image="$REGISTRY/last-sandbox-base:$TAG" \
  --wait --wait-for-jobs --timeout 10m

go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m

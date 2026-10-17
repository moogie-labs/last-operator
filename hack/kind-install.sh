#!/usr/bin/env bash
# Build the images and install the chart on a kind cluster, creating the
# cluster if needed. Used by hack/e2e.sh and hack/demo.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${KIND_CLUSTER:-last-e2e}"
TAG="${IMAGE_TAG:-e2e}"
REGISTRY="${IMAGE_REGISTRY:-ghcr.io/moogie-labs}"
# Must match nginxImage in test/e2e/operator_e2e_test.go. Preloaded so the
# cluster never pulls from a public registry during the test.
NGINX_IMAGE="nginx:1.27-alpine"
# Must match registry.bundled.image and registry.bundled.proxyImage in the
# chart values; preloaded for the same reason.
REGISTRY_IMAGE="registry:3.0.0"
REGISTRY_PROXY_IMAGE="nginxinc/nginx-unprivileged:1.27.5-alpine"

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --config hack/kind-config.yaml --wait 120s
kubectl config use-context "kind-$CLUSTER"

# One EXIT trap cleans every temp file and the port-forward (a second trap
# would replace this one). wait reaps the port-forward so bash prints nothing.
TMP_FILES=()
PF=""
cleanup() {
  { [ -z "$PF" ] || { kill "$PF" && wait "$PF"; }; } 2>/dev/null || true
  [ "${#TMP_FILES[@]}" -eq 0 ] || rm -f "${TMP_FILES[@]}"
}
trap cleanup EXIT

# `kind load docker-image` fails for a pulled multi-platform image on Docker's
# containerd image store: the saved index names platforms that were never
# pulled. Load an archive of the node's platform only. Docker before 28 has no
# `save --platform` (and no such store by default), so fall back there.
load_image() {
  local image="$1" tar
  docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
  tar="$(mktemp)"
  TMP_FILES+=("$tar")
  if docker save --platform "$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" -o "$tar" "$image"; then
    kind load image-archive --name "$CLUSTER" "$tar"
  else
    echo "docker save --platform failed; falling back to kind load docker-image" >&2
    kind load docker-image --name "$CLUSTER" "$image"
  fi
}

make images IMAGE_TAG="$TAG" IMAGE_REGISTRY="$REGISTRY" COMPONENTS="operator gateway audit-init"
for c in operator gateway audit-init sandbox-base; do
  kind load docker-image --name "$CLUSTER" "$REGISTRY/last-$c:$TAG"
done
load_image "$NGINX_IMAGE"
load_image "$REGISTRY_IMAGE"
load_image "$REGISTRY_PROXY_IMAGE"

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

# Serve the sandbox base from the bundled registry so sandbox pods pull it by
# digest through the node-local pull proxy, as they would in a real install.
BASE_TAR="$(mktemp)"
TMP_FILES+=("$BASE_TAR")
docker save -o "$BASE_TAR" "$REGISTRY/last-sandbox-base:$TAG"
kubectl port-forward -n last-registry svc/last-registry-front 15000:5000 >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 50); do
  # Any HTTP response (even 401) means the forward accepts connections.
  if curl -s -o /dev/null http://127.0.0.1:15000/v2/; then break; fi
  sleep 0.2
done
PUSHER_PASSWORD="$(kubectl get secret -n last-gateway last-registry-pusher -o jsonpath='{.data.password}' | base64 --decode)"
BASE_DIGEST="$(LAST_REGISTRY_USERNAME=pusher LAST_REGISTRY_PASSWORD="$PUSHER_PASSWORD" \
  go run ./hack/registrypush --to "localhost:15000/last/base:$TAG" --tar "$BASE_TAR")"
unset PUSHER_PASSWORD
echo "Pushed sandbox base image: $BASE_DIGEST"
kubectl apply --server-side --field-manager kind-install -f - <<EOF
apiVersion: last.moogie-labs.com/v1alpha1
kind: OperatorConfig
metadata:
  name: default
spec:
  sandbox:
    baseImage: localhost:5000/last/base@$BASE_DIGEST
EOF

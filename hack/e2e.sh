#!/usr/bin/env bash
# Install on kind (hack/kind-install.sh), check the bundled registry, and run
# the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

make nginx-check
hack/kind-install.sh

# The check pushes the sandbox base as a fresh image and pulls it on every node.
BASE_TAR="$(mktemp)"
trap 'rm -f "$BASE_TAR"' EXIT
docker save -o "$BASE_TAR" "${IMAGE_REGISTRY:-ghcr.io/moogie-labs}/last-sandbox-base:${IMAGE_TAG:-e2e}"
hack/registry-check.sh --context "kind-${KIND_CLUSTER:-last-e2e}" --image-tar "$BASE_TAR"

go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m

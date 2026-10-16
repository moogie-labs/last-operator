#!/usr/bin/env bash
# Install on kind (hack/kind-install.sh) and run the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

hack/kind-install.sh
go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m

#!/usr/bin/env bash
# Check the bundled registry on any cluster: a fresh image pushed through the
# front proxy pulls on every node through the node-local pull proxy, the pull
# proxy refuses everything off its allowlist, and the front proxy enforces its
# method and credential matrix. Prints one PASS or FAIL line per check and
# exits 0 only if all pass.
#
# Usage: hack/registry-check.sh [--context CTX] [--image-tar FILE]
set -euo pipefail
cd "$(dirname "$0")/.."

CTX=""
IMAGE_TAR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --context) CTX="${2:?--context needs a value}"; shift 2 ;;
    --image-tar) IMAGE_TAR="${2:?--image-tar needs a value}"; shift 2 ;;
    *) echo "usage: $0 [--context CTX] [--image-tar FILE]" >&2; exit 2 ;;
  esac
done

kc() {
  if [ -n "$CTX" ]; then kubectl --context "$CTX" "$@"; else kubectl "$@"; fi
}

REG_NS="last-registry"
GW_NS="last-gateway"
CHECK_NS="last-registry-check"
FRONT_PORT=15000
PROXY_PORT=15001
ACCEPT="application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

TMP_DIR="$(mktemp -d)"
PIDS=()
cleanup() {
  local p
  for p in "${PIDS[@]:-}"; do
    [ -z "$p" ] || { kill "$p" && wait "$p"; } 2>/dev/null || true
  done
  rm -rf "$TMP_DIR"
  kc delete namespace "$CHECK_NS" --wait=false --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

FAILED=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1${2:+: $2}"; FAILED=1; }
die() { echo "FAIL $1" >&2; exit 1; }

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
for port in "$FRONT_PORT" "$PROXY_PORT"; do
  ! port_in_use "$port" || die "port $port is already in use"
done

# port_forward NAMESPACE TARGET LOCAL_PORT:REMOTE_PORT: waits until the forward
# accepts connections and fails loudly if it never does.
port_forward() {
  local ns="$1" target="$2" ports="$3" pf ok=""
  # Run kubectl itself in the background (not the kc function, whose subshell
  # would be killed without its kubectl child).
  kubectl ${CTX:+--context "$CTX"} port-forward -n "$ns" "$target" "$ports" >/dev/null 2>&1 &
  pf=$!
  PIDS+=("$pf")
  for _ in $(seq 1 100); do
    kill -0 "$pf" 2>/dev/null || die "port-forward to $target exited early"
    if curl -s -o /dev/null "http://127.0.0.1:${ports%%:*}/"; then ok=1; break; fi
    sleep 0.2
  done
  [ -n "$ok" ] || die "port-forward to $target did not become ready"
}

secret_password() {
  kc get secret -n "$GW_NS" "$1" -o jsonpath='{.data.password}' | base64 --decode
}

# curl config files keep credentials off the command line.
write_curl_config() {
  local file="$1" user="$2" password="$3"
  (umask 077; printf 'user = "%s:%s"\n' "$user" "$password" >"$file")
}

# --- 1. Push a fresh image -------------------------------------------------
NODES=()
while IFS= read -r n; do NODES+=("$n"); done < <(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
[ "${#NODES[@]}" -gt 0 ] || die "no nodes found"
ARCH="$(kc get node "${NODES[0]}" -o jsonpath='{.status.nodeInfo.architecture}')"

port_forward "$REG_NS" svc/last-registry-front "$FRONT_PORT:5000"
PUSHER_PASSWORD="$(secret_password last-registry-pusher)"
ADMIN_PASSWORD="$(secret_password last-registry-admin)"
write_curl_config "$TMP_DIR/pusher.curl" pusher "$PUSHER_PASSWORD"
write_curl_config "$TMP_DIR/admin.curl" admin "$ADMIN_PASSWORD"

LABEL="last.check=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
if [ -n "$IMAGE_TAR" ]; then
  SRC=(--tar "$IMAGE_TAR")
else
  SRC=(--from docker.io/library/busybox:1.37.0 --platform "linux/$ARCH")
fi
DIGEST="$(LAST_REGISTRY_USERNAME=pusher LAST_REGISTRY_PASSWORD="$PUSHER_PASSWORD" \
  go run ./hack/registrypush --to "localhost:$FRONT_PORT/last/images:registry-check" "${SRC[@]}" --label "$LABEL")" \
  || die "push of the check image failed"
unset PUSHER_PASSWORD ADMIN_PASSWORD
[[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || die "push returned an invalid digest"
pass "push $DIGEST"

# --- 2. Pull on every node -------------------------------------------------
# A namespace left by a killed run could hold stale pods: remove it and wait until it is gone.
if kc get namespace "$CHECK_NS" >/dev/null 2>&1; then
  kc delete namespace "$CHECK_NS" --wait=false --ignore-not-found >/dev/null || die "could not delete leftover namespace $CHECK_NS"
  for _ in $(seq 1 60); do
    kc get namespace "$CHECK_NS" >/dev/null 2>&1 || break
    sleep 2
  done
  if kc get namespace "$CHECK_NS" >/dev/null 2>&1; then die "leftover namespace $CHECK_NS still exists after 120s"; fi
fi
kc create namespace "$CHECK_NS" >/dev/null || die "could not create namespace $CHECK_NS"
POD_NAMES=()
for i in "${!NODES[@]}"; do
  node="${NODES[$i]}"
  POD_NAMES+=("registry-check-$i")
  kc apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: registry-check-$i
  namespace: $CHECK_NS
spec:
  nodeName: $node
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: check
      image: localhost:5000/last/images@$DIGEST
      command: ["true"]
YAML
done
for i in "${!NODES[@]}"; do
  node="${NODES[$i]}"
  pod="${POD_NAMES[$i]}"
  phase=""
  for _ in $(seq 1 120); do
    phase="$(kc get pod -n "$CHECK_NS" "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  if [ "$phase" = "Succeeded" ]; then
    pass "pull on $node"
  else
    reason="$(kc get pod -n "$CHECK_NS" "$pod" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}{" "}{.status.containerStatuses[0].state.waiting.message}' 2>/dev/null || true)"
    fail "pull on $node" "phase ${phase:-unknown}; ${reason:-no waiting reason}"
  fi
done

# --- 3. The pull went through the node-local proxy -------------------------
for node in "${NODES[@]}"; do
  proxy_pod="$(kc get pod -n "$REG_NS" -l app.kubernetes.io/name=last-registry-pull-proxy \
    --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -z "$proxy_pod" ]; then
    fail "proxy on $node" "no pull-proxy pod on the node"
    continue
  fi
  found=""
  for _ in $(seq 1 10); do
    if kc logs -n "$REG_NS" "$proxy_pod" 2>/dev/null | grep -qF "GET /v2/last/images/manifests/$DIGEST"; then found=1; break; fi
    sleep 1
  done
  if [ -n "$found" ]; then pass "proxy on $node"; else fail "proxy on $node" "no manifest GET in $proxy_pod logs"; fi
done

# --- 4. The pull proxy's refusals ------------------------------------------
PROXY_POD="$(kc get pod -n "$REG_NS" -l app.kubernetes.io/name=last-registry-pull-proxy \
  -o jsonpath='{.items[0].metadata.name}')"
[ -n "$PROXY_POD" ] || die "no pull-proxy pod found"
port_forward "$REG_NS" "pod/$PROXY_POD" "$PROXY_PORT:5000"

# expect_status LABEL EXPECTED URL_BASE [curl args...]
expect_status() {
  local label="$1" want="$2" url="$3" got
  shift 3
  got="$(curl -s -o /dev/null -w '%{http_code}' "$@" "$url" || true)"
  if [ "$got" = "$want" ]; then pass "$label"; else fail "$label" "want $want, got ${got:-none}"; fi
}

P="http://127.0.0.1:$PROXY_PORT"
M="/v2/last/images/manifests/$DIGEST"
for method in PUT POST PATCH DELETE; do
  expect_status "pull proxy refuses $method manifest" 403 "$P$M" -X "$method"
done
expect_status "pull proxy refuses _catalog" 403 "$P/v2/_catalog"
expect_status "pull proxy refuses tags/list" 403 "$P/v2/last/images/tags/list"
expect_status "pull proxy refuses another repository" 403 "$P/v2/other/manifests/$DIGEST"
expect_status "pull proxy refuses a tag" 403 "$P/v2/last/images/manifests/registry-check" -H "Accept: $ACCEPT"
expect_status "pull proxy refuses a trailing newline" 403 "$P$M%0a" -H "Accept: $ACCEPT"
expect_status "pull proxy serves the manifest by digest" 200 "$P$M" -H "Accept: $ACCEPT"

# --- 5. The front proxy's method matrix ------------------------------------
F="http://127.0.0.1:$FRONT_PORT"
ZERO="sha256:$(printf '0%.0s' $(seq 1 64))"
expect_status "front anonymous GET" 200 "$F$M" -H "Accept: $ACCEPT"
expect_status "front anonymous HEAD" 200 "$F$M" -I -H "Accept: $ACCEPT"
expect_status "front anonymous PUT" 401 "$F$M" -X PUT
expect_status "front anonymous DELETE" 401 "$F$M" -X DELETE
expect_status "front pusher DELETE" 401 "$F$M" -X DELETE --config "$TMP_DIR/pusher.curl"
expect_status "front admin PUT" 401 "$F$M" -X PUT --config "$TMP_DIR/admin.curl"
expect_status "front admin DELETE of a missing digest" 404 "$F/v2/last/images/manifests/$ZERO" -X DELETE --config "$TMP_DIR/admin.curl"

# --- 6. Summary ------------------------------------------------------------
if [ "$FAILED" -ne 0 ]; then
  echo "registry check FAILED"
  exit 1
fi
echo "registry check passed"

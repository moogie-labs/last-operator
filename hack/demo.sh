#!/usr/bin/env bash
# M3's done-criterion against a real model (M3 spec §1, §8): install on kind with
# an anthropic LLMProvider, then create an Intent with kubectl last, which asks
# you its questions and for each approval. Opt-in and never run in CI: it spends
# API credit. The key comes from ANTHROPIC_API_KEY and goes only into a Secret
# in last-gateway, through stdin, never on a command line.
#
# Record a transcript with: hack/demo.sh 2>&1 | tee demo.log
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "Set ANTHROPIC_API_KEY to run the demo." >&2
  exit 1
fi
export KIND_CLUSTER="${KIND_CLUSTER:-last-demo}"
MODEL="${DEMO_MODEL:-claude-opus-5-5}"
NS="${DEMO_NAMESPACE:-demo}"
REQUEST="${DEMO_REQUEST:-run an nginx web server serving a hello page}"
PROVIDER=demo-kubernetes
LLM=demo-anthropic

hack/kind-install.sh

# The model's API key, under the key name the gateway reads. Server-side apply
# keeps the key out of the last-applied-configuration annotation, and nothing
# after this needs it.
printf '%s' "$ANTHROPIC_API_KEY" | kubectl create secret generic "$LLM" -n last-gateway \
  --from-file=apiKey=/dev/stdin --dry-run=client -o yaml | kubectl apply --server-side -f - >/dev/null
unset ANTHROPIC_API_KEY

# A namespace for the demo, a Provider that can edit only it, and the model.
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: $PROVIDER, namespace: last-gateway}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: $PROVIDER-edit, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit}
subjects: [{kind: ServiceAccount, name: $PROVIDER, namespace: last-gateway}]
---
apiVersion: last.moogie-labs.com/v1alpha1
kind: Provider
metadata: {name: $PROVIDER}
spec:
  type: kubernetes
  description: "Edit access to namespace $NS on this cluster"
  kubernetes: {serviceAccountRef: {name: $PROVIDER}}
---
apiVersion: last.moogie-labs.com/v1alpha1
kind: LLMProvider
metadata: {name: $LLM}
spec:
  type: anthropic
  model: $MODEL
  secretRef: {name: $LLM}
EOF

# Point the operator at the model, and let Intents use the Provider.
kubectl apply --server-side --force-conflicts --field-manager=last-demo -f - <<EOF
apiVersion: last.moogie-labs.com/v1alpha1
kind: OperatorConfig
metadata: {name: default}
spec:
  llmProviderRef: {name: $LLM}
  defaults:
    providers: [$PROVIDER]
EOF

go build -o bin/kubectl-last ./cmd/kubectl-last
echo
echo "Creating an Intent in namespace $NS from: $REQUEST"
bin/kubectl-last "$REQUEST" -n "$NS"

# Verify the WORM audit chain the run wrote.
echo
echo "Verifying the audit chain"
PUB="$(mktemp)"
kubectl port-forward -n last-audit svc/last-rustfs 19000:9000 >/dev/null 2>&1 &
PF=$!
trap 'kill "$PF" 2>/dev/null || true; rm -f "$PUB"' EXIT
kubectl get cm -n last-gateway last-audit-pubkey -o jsonpath='{.data.ed25519\.pub}' > "$PUB"
echo >> "$PUB"
for _ in $(seq 30); do
  if curl -s -o /dev/null http://127.0.0.1:19000; then break; fi
  sleep 1
done
LAST_AUDIT_ENDPOINT=http://127.0.0.1:19000 \
LAST_AUDIT_CHAIN_ID="$(kubectl get cm -n last-gateway last-audit-config -o jsonpath='{.data.chainId}')" \
AWS_ACCESS_KEY_ID="$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.accessKey}' | base64 --decode)" \
AWS_SECRET_ACCESS_KEY="$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.secretKey}' | base64 --decode)" \
  bin/kubectl-last audit verify --pubkey-file "$PUB"

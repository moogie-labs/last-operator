# last-operator

An AI-driven Kubernetes operator that turns natural-language requests into
reviewed, audited, continuously reconciled infrastructure.
Design: [`docs/superpowers/specs/2026-09-26-last-operator-design.md`](docs/superpowers/specs/2026-09-26-last-operator-design.md).

**Status:** foundation (M0–M1). The CRDs and the write-once audit log are in
place; the agent, sandboxes, and approvals arrive in later milestones.

## What works today

- All `last.moogie-labs.com/v1alpha1` CRDs, with validation.
- The **gateway**, the only component that will ever act, writes a hash-chained,
  Ed25519-signed audit log to S3 Object Lock storage in compliance mode, and
  refuses to start if the store is unreachable or not locked correctly.
- A bundled single-node **RustFS** audit store, provisioned with least-privilege users.
- `kubectl last audit verify | log | conformance`.

## Quick start (kind)

```bash
make test-e2e            # builds images, installs the chart on kind, runs the suite
```

Verify the audit chain yourself:

```bash
kubectl port-forward -n last-audit svc/last-rustfs 9000:9000 &
export LAST_AUDIT_ENDPOINT=http://127.0.0.1:9000
export LAST_AUDIT_CHAIN_ID=$(kubectl get cm -n last-gateway last-audit-config -o jsonpath='{.data.chainId}')
export AWS_ACCESS_KEY_ID=$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.accessKey}' | base64 -d)
export AWS_SECRET_ACCESS_KEY=$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.secretKey}' | base64 -d)
kubectl get cm -n last-gateway last-audit-pubkey -o jsonpath='{.data.ed25519\.pub}' > pub.pem
go run ./cmd/kubectl-last audit verify --pubkey-file pub.pem
```

## Production audit storage

The bundled RustFS store is the easy default. For production compliance use,
point the chart at an external S3 bucket (or Ceph RGW) that has Object Lock in
compliance mode, and run `kubectl last audit conformance` against it. The
writer credentials must be in a Secret in the `last-gateway` namespace before
install, so pre-create that namespace yourself and turn off `namespaces.create`
(the bundled `last-audit` namespace isn't used in external mode, so it doesn't
need to be pre-created):

```bash
kubectl create namespace last-gateway

kubectl create secret generic last-audit-writer -n last-gateway \
  --from-literal=accessKey=<your-access-key> \
  --from-literal=secretKey=<your-secret-key>

helm install last charts/last-operator -n last-system --create-namespace \
  --set audit.bundled.enabled=false \
  --set audit.external.endpoint=https://s3.us-east-1.amazonaws.com \
  --set audit.external.existingSecret=last-audit-writer \
  --set namespaces.create=false \
  --set audit.bucket=<your-object-lock-bucket>
```

## Development

| Command | What it runs |
|---|---|
| `make test` | unit + envtest |
| `make test-integration` | integration tests against RustFS (Docker) |
| `make helm-lint` | chart lint and render |
| `make test-e2e` | kind end-to-end suite |

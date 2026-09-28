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

## Building the CLI

```bash
go build -o bin/kubectl-last ./cmd/kubectl-last
```

Put `bin/kubectl-last` on your `PATH` (keeping the name `kubectl-last`) to use it
as `kubectl last`.

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
kubectl last audit verify --pubkey-file pub.pem
```

`verify` prints the fingerprint of the key it verified against
(`key fingerprint: sha256:…`). Record that fingerprint out of band, and check
it on later runs: the published key lives in a mutable ConfigMap. It cannot
detect a truncated tail by itself, so also record the head seq it reports and
pass it back with `--expect-head N`, which fails if the chain now ends before
seq N. An empty chain fails (usually a wrong `--chain` or `--bucket`) unless you
pass `--allow-empty`.

Install and upgrade the chart only with `helm install` / `helm upgrade`. The
chart looks up the existing chain ID and generated secrets in the cluster;
`helm template` or GitOps rendering cannot, so each render would generate a new
chain ID and new credentials.

If the gateway refuses to start because chain verification failed, do not
delete anything. Run `kubectl last audit verify` to find the first bad seq, and
treat it as a security incident.

## Production audit storage

The bundled RustFS store is the easy default. For production compliance use,
point the chart at an external S3 bucket (or Ceph RGW) that has Object Lock in
compliance mode, and run `kubectl last audit conformance` against it.

The bucket needs Object Lock with a COMPLIANCE-mode default retention of at
least `audit.retentionDays`; otherwise the gateway refuses to start. Each entry is
locked for the longer of `audit.retentionDays` and the bucket's default
retention.

`conformance` reports results per credential set. A set that is refused all
writes passes most checks without proving that the store enforces the lock, so
also run it with admin credentials (`LAST_ADMIN_ACCESS_KEY` /
`LAST_ADMIN_SECRET_KEY`). Each run leaves a locked probe object under
`conformance/` for `--retention` (default 24h).

The writer credentials must be in a Secret in the `last-gateway` namespace
before install, so pre-create that namespace yourself and turn off
`namespaces.create` (the bundled `last-audit` namespace isn't used in external
mode, so it doesn't need to be pre-created):

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

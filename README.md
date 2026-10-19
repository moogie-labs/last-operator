# last-operator

An AI-driven Kubernetes operator that turns natural-language requests into
reviewed, audited, continuously reconciled infrastructure.
Design: [`docs/superpowers/specs/2026-09-26-last-operator-design.md`](docs/superpowers/specs/2026-09-26-last-operator-design.md).

**Status:** M3 (real LLMs). The operator plans with Anthropic Claude or an
OpenAI-compatible model, asks clarifying questions, runs approved discovery
commands, and executes approved plans in sandbox pods. Every action goes
through the gateway, which records it in a write-once audit log. Image builds,
day-2 changes, allow rules and rule suggestions arrive in M4–M7.

## What works today

- All `last.moogie-labs.com/v1alpha1` CRDs, with validation.
- The **gateway**, the only component that will ever act, writes a hash-chained,
  Ed25519-signed audit log to S3 Object Lock storage in compliance mode, and
  refuses to start if the store is unreachable or not locked correctly.
- A bundled single-node **RustFS** audit store, provisioned with least-privilege users.
- `kubectl last "<request>"`, which creates an Intent and follows it: it asks you
  the model's questions, shows each discovery command and the plan, and asks for
  your approval. Also `kubectl last answer | plan | approve | deny NAME`.
- **Intent identity, approvals and Delete.** Approvals are bound to the Intent's
  UID, and `kubectl delete intent` tears down what the Intent created, behind a
  finalizer and your approval.
- `kubectl last audit verify | log | conformance`.

## Building the CLI

```bash
go build -o bin/kubectl-last ./cmd/kubectl-last
```

Put `bin/kubectl-last` on your `PATH` (keeping the name `kubectl-last`) to use it
as `kubectl last`.

## Using `kubectl last`

```bash
kubectl last "run an nginx web server serving a hello page" -n team-x
```

The plugin creates an Intent and follows it, every two seconds:

- **Questions.** Each clarifying question is shown as a numbered menu or a
  free-text prompt.
- **Discovery commands and the plan.** The plugin first recomputes each one's
  hash from the content it is about to show. One that does not hash to the hash
  the operator states is never shown or decided: the plugin warns once and keeps
  following. Each verified one is shown with its commands, files (by path and
  size), Providers and egress, then the plugin asks `Approve? [y/N/d]`, or
  `Approve? [y/N/d/f]` when it carries files:
  - `y` approves;
  - `d` denies, after asking for a reason, which the model reads. Denying a plan
    fails the Intent; denying a discovery command lets the model plan on;
  - `f` shows it again with the files' contents, then asks again;
  - `N` or Enter leaves it pending, and prints the `approve` and `deny` commands
    bound to its hash.
- **Ready.** At Ready it lists the ManagedResources and output Secrets. Following
  also ends at Drifted, after the status line.

Ctrl-C stops following; the Intent carries on. Pick it up again with:

```bash
kubectl last plan NAME [--files]        # what the Intent is waiting for (read-only)
kubectl last answer NAME                # answer its open questions
kubectl last approve NAME --hash H      # approve exactly what plan showed
kubectl last deny NAME --hash H --reason R
```

`plan` prints the open questions, or the pending discovery command or plan with
the `approve` and `deny` commands bound to its hash; `--files` adds the files'
contents. Without `--hash`, `approve` and `deny` decide on whatever the Intent
asks about when they run, and show it first. Every decision is an
`IntentApproval` that you create under your own identity, for the hash you were
shown, and the plugin never shows or decides content that does not hash to its
stated hash.

## Deleting an Intent

```bash
kubectl delete intent web -n team-x
```

A finalizer, `last.moogie-labs.com/teardown`, holds the Intent until its
teardown completes:

- **Plan.** The operator reads the plan the Intent executed from the audit log
  and asks the model to order its teardown commands. The model can reorder them,
  not change, add or drop any. The Intent shows `AwaitingApproval` for stage
  Delete.
- **Approval.** A Delete always needs your approval; no allow rule or Auto mode
  covers it. Approve it as usual, with `kubectl last plan web -n team-x` and
  `kubectl last approve web -n team-x --hash H`, or `y` at the prompt of a
  following `kubectl last`.
- **Teardown.** The phase is `TearingDown` while the commands run. On success the
  finalizer is released, and the ManagedResources and output Secrets go with the
  Intent. Following a deletion ends with `Intent team-x/web torn down.`
- **Failure.** The Intent is `Failed` with reason `TeardownFailed` and keeps its
  finalizer. Change the retry annotation to plan the teardown again:
  `kubectl annotate intent web -n team-x last.moogie-labs.com/retry=2 --overwrite`.
- **Nothing to tear down.** An Intent that never executed, or whose plan has no
  teardown commands, is released at once. The latter raises a Warning
  `NoTeardown` event.

Removing the finalizer by hand lets the Intent go without a teardown. What it
created stays in the world.

## Demo against Claude (opt-in)

```bash
export ANTHROPIC_API_KEY=...            # spends API credit
make demo                               # kind + chart + an anthropic LLMProvider, then kubectl last
```

The key goes only into a Secret in `last-gateway`. The demo never runs in CI.

The M4 real-model check bakes a tool into an image:

```bash
DEMO_REQUEST="install the AWS CLI in an image and print its version" make demo
```

The build reaches the Debian mirrors (the demo opens ports 80 and 443 for builds, and the
sandbox commands of its Intents gain the same egress, not only the builds)
and spends API credit.

On a cloud cluster, `0.0.0.0/0:80` build and sandbox egress also reaches the instance
metadata endpoint (169.254.169.254). Don't run the demo's egress settings on EKS or GKE
nodes unless that endpoint is blocked.

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

## Image retention and garbage collection

The gateway keeps each built image's last-used time. Once a day
(`gateway.imageGCInterval`) it deletes images unused for longer than
`OperatorConfig.spec.images.unusedRetention` (default 30 days) that no Intent
plan or ManagedResource still references, auditing every deletion. With the
bundled registry, it pauses builds while it runs, and the `registry-gc` sidecar
then reclaims the blobs' disk space. External registries: GC deletes the
manifests without pausing builds, and blob storage relies on the registry's own
lifecycle policy.

Every chart upgrade recreates the bundled registry pod, because the GC
sidecar's image follows the chart's tag. Expect a short registry outage, or up
to 25 minutes if a garbage collection is running when the pod stops.

## External registry

With `registry.mode: external`, the push and pull Secrets
(`kubernetes.io/dockerconfigjson`, in `last-gateway`) are yours to keep fresh.
ECR tokens expire every 12 hours, so run something that rewrites both Secrets
before then (a CronJob, External Secrets). The gateway reads the push Secret on
every image resolve and re-copies the pull Secret before every run when it
changed, so no restart is needed.

For Docker Hub, name the repository `index.docker.io/<namespace>/<name>`. The
`auths` key may be `index.docker.io` or the standard
`https://index.docker.io/v1/`; a `docker.io/...` repository matches neither.

Providers can't mount the push or pull Secret while `registry.external` names
it. Once you remove `registry.external` or point it at other Secrets, the old
ones are ordinary Secrets again; delete them once nothing uses them.

## Checking the bundled registry

The bundled registry relies on the node-local pull proxy using a `hostPort`, which
not every platform allows. On a new platform, run `hack/registry-check.sh
[--context CTX]` after installing the chart. It pushes a fresh image, pulls it on
every node through `localhost:5000`, and checks that the pull proxy and the front
proxy refuse everything they should. It prints one `PASS` or `FAIL` line per
check and exits non-zero if any fail. By default it pushes busybox from Docker
Hub on the machine running the script (not in the cluster); pass `--image-tar FILE`
(a `docker save` archive) if that machine cannot reach it. The script needs a
repo checkout, Go, kubectl and curl, plus Docker only if you `docker save` the
archive yourself.

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
`namespaces.create`. With it off, the chart doesn't create any namespace, so
also pre-create `last-sandbox`, `last-build` and `last-registry`. The registry
namespace (hostPort DaemonSet) and the build namespace must carry the
`pod-security.kubernetes.io/enforce: privileged` label, or the pull proxy is
rejected and `helm --wait` hangs. `--set registry.bundled.enabled=false` skips
`last-registry` entirely. The bundled `last-audit` namespace isn't used in
external mode, so it doesn't need to be pre-created:

```bash
kubectl create namespace last-gateway
kubectl create namespace last-sandbox
kubectl create namespace last-registry
kubectl create namespace last-build
kubectl label namespace last-registry last-build pod-security.kubernetes.io/enforce=privileged

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

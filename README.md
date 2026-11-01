# last-operator

An AI-driven Kubernetes operator that turns natural-language requests into
reviewed, audited, continuously reconciled infrastructure.
Design: [`docs/superpowers/specs/2026-09-26-last-operator-design.md`](docs/superpowers/specs/2026-09-26-last-operator-design.md).

**Status:** M5b (day two: delete, edit and drift). The operator plans with
Anthropic Claude or an OpenAI-compatible model, asks clarifying questions, runs
approved discovery commands, and executes approved plans in sandbox pods,
building the images their commands need. It tears an Intent down when you
delete it, plans an Update when you edit it, and plans a Remediate when its
probes find drift. Every action goes through the gateway, which records it in a
write-once audit log. Auto modes, retries and adaptive probing are in M5c; allow
rules arrive in M6 and rule suggestions in M7.

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
- **Day 2.** Editing an executed Intent plans an Update, and drift plans a
  Remediate, each behind your approval.
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
  - `d` denies, after asking for a reason. Denying a discovery command lets the
    model plan on, and the model reads why. Denying a plan, of any stage, fails
    the Intent with `ApprovalDenied` and records the reason on the Intent;
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
- **Approval.** A Delete needs your approval, unless the `delete` stage is Auto and the
  plan only orders the teardown commands the Intent executed (see Auto modes
  below), or allow rules cover every command (see Allow rules below). Approve it as usual, with `kubectl last plan web -n team-x` and
  `kubectl last approve web -n team-x --hash H`, or `y` at the prompt of a
  following `kubectl last`.
- **Teardown.** The phase is `TearingDown` while the commands run. On success the
  finalizer is released, and the ManagedResources and output Secrets go with the
  Intent. Following a deletion ends with `Intent team-x/web torn down.`
- **Failure.** The Intent is `Failed` with reason `TeardownFailed` and keeps its
  finalizer. Change the retry annotation to plan the teardown again:
  `kubectl annotate intent web -n team-x last.moogie-labs.com/retry=2 --overwrite`.
- **Partial plans.** A Create or Update that failed or was interrupted after
  starting at least one step may have made things the executed plan does not
  tear down. The Delete's teardown is the executed plan's teardown, then the
  teardown of every such partial plan from oldest to newest, with duplicates
  dropped. Because a plan can stop part way, ask the model for teardown commands
  that succeed when the object is gone or was never made, such as
  `kubectl delete … --ignore-not-found`; the system prompt does. An Intent whose
  history holds a partial plan needs approval or rules to be deleted, even in
  Auto.
- **Nothing to tear down.** An Intent that never executed and never started a
  step is released at once. If the union above has no teardown commands, the
  Intent is released too, with a Warning `NoTeardown` event.
- **Waiting.** While the audit log or the API server cannot say what the Intent
  executed, the deletion waits and retries, with a Warning `TeardownBlocked`
  event saying why.

Removing the finalizer by hand lets the Intent go without a teardown. What it
created stays in the world.

## Allow rules

Allow rules let commands run without approval. A `CommandRule` (namespaced) or
`ClusterCommandRule` (cluster-wide) is written by a person; the operator and the
gateway can read rules but not write them (a ValidatingAdmissionPolicy,
`last-rules`, refuses it). A rule never widens policy: `CheckCommand` still
applies to a covered command.

A command is covered at a stage when some rule lists that stage (Create and
Update are one class), lists all of the command's providers, covers its egress,
matches its argv, and matches every `image.install` command. A script is never
covered, and neither is an argv with a `${steps.` reference. Patterns are literals,
globs within one argument (`*`, `?`, `[…]`), `re:<RE2>` (anchored), or `...` for
the remaining arguments (last position only).

**A rule must cover the whole plan.** For a Create or Update, that is every step,
every probe (at the plan's stage) and every teardown command (at stage
`Delete`). `status.plan.uncoveredSteps` lists what is not covered, as `<stepID>`,
`probe:<probeID>` and `teardown:<resource>.<i>`. When nothing is uncovered the
Intent skips `AwaitingApproval` ("Running plan H without approval (allow
rules)"); the audit shows `authorizedBy {kind: rule}`, one `rule.observed` entry
per rule used, and a `rules` map in `phase.begin`. A Deny still wins, and each
step re-checks the current rules.

Approve and make rules in one step:

```bash
kubectl last approve web -n team-x --always-allow            # prompt: Exact, Generalized or Just this once
kubectl last approve web -n team-x --always-allow=generalized --hash H
kubectl last approve web -n team-x --always-allow=exact --stages Create,Delete --cluster
kubectl last approve web -n team-x --always-allow=generalized --providers prod
```

- **Exact** matches this argv only. **Generalized** keeps `argv[0]` and the first
  two non-flag arguments literal, keeps flag names, turns a flag's separate value
  and later positionals into `[!-]*` (so they cannot swallow a flag) and the value
  of `--flag=value` into `--flag=*`. Code never varies: the value of a code flag
  (`-c`, `-e`, `-E`, `--eval`, `-Command`, `-r`, …) stays literal, and from an
  interpreter (`sh`, `bash`, `python3`, `node`, `perl`, `pwsh`, … at any position)
  or a `--` argument on, every argument stays literal, so `kubectl exec web -- sh -c
  date` generalizes to itself. You can edit the pattern at the prompt.
- Rules are created as you (so your RBAC applies), then the approval. They are
  named `<intent>-<entry>-<hash8>` and labelled
  `last.moogie-labs.com/intent: <intent>`.
- Stages default to the plan's stage for steps and probes and `Delete` for
  teardown. A command with no providers gets the union of the plan's providers;
  if the plan names none, pass `--providers`.
- A script or a `${steps.` reference cannot be covered; it is approved just this
  once, and the CLI says so.

`kubectl last rules lint rules.yaml` (or `-` for stdin) warns about rules that
are too broad: `...` right after the binary, a glob or regex after a destructive
verb (`delete`, `rm`, `destroy`, `terminate-*`, `drop`, `purge`), and a file flag
(`-f`, `--filename`, `--from-file`) with `...` or a glob. It always reminds you
that rules match argv only, not file contents or stdin, so a rule for
`kubectl apply -f *` allows any manifest those credentials permit. It exits 1 only
for a pattern that matches nothing. `kubectl last plan` shows which rule covers
each entry.

Related limits: at most 6 remediations in 24 hours (`status.remediationTimes`)
before a drifting Intent fails `RemediationFailed`; a spec edit clears the count.

## Editing an Intent and drift

An Intent that has executed is not frozen.

- **Edit it.** Change `spec.request` or `spec.answers` of a Ready, Drifted or
  Failed Intent (`kubectl edit intent web -n team-x`) and the operator plans an
  **Update**. The model sees the plan the Intent executed, read from the audit
  log, and the new request. `kubectl last plan web -n team-x` shows it as
  `(stage Update)`; approve it as usual. When it succeeds, the gateway deletes
  the ManagedResources the Update dropped, each audited as
  `managedresource.deleted`, and the Intent runs its probes and returns to Ready.
  The Update's own steps must remove the real objects of a dropped resource. A
  kept resource keeps its output Secret, which the gateway moves to the
  resource's new ManagedResource when an earlier one is dropped, audited as
  `output.moved`.
- **Drift.** A scheduled probe that fails moves the Intent to `Drifted`. The
  operator runs the probes again to confirm. If they all pass, the Intent goes
  back to Ready with a Warning `DriftCleared` event and nothing to approve.
  Otherwise the model plans a **Remediate** that repairs what the executed plan
  manages, without changing it, and the Intent waits in `AwaitingApproval` for
  stage Remediate. A Remediate plan carries a nonce, so a repeat of the same
  repair after the next drift needs a new approval. Once approved it runs as
  `Remediating`, then `Verifying`, then Ready.
- **Limits.** `status.remediations` counts consecutive remediations. At 3, the
  next drift fails the Intent with `RemediationFailed`; a scheduled probe run
  in which every probe passes resets the count. Editing the spec while a
  remediation is in flight abandons it and plans an Update.
- **Denying a Remediate.** Like any denied plan, a denied Remediate fails the
  Intent with `ApprovalDenied`. A Failed Intent runs no probes, so drift is no
  longer watched. Edit the spec to plan an Update, which you approve as usual,
  or change the retry annotation to plan a new Remediate. A denied plan is never
  retried automatically, even in Auto.
- **Approving.** `approve` and `deny` act only while the Intent is
  `AwaitingApproval`: during planning, `status.plan` is still the previous plan
  and is never approvable. A plan that carries a nonce is shown as
  `(stage Remediate)`. After a `DriftCleared` return the plan shown is the last
  one recorded: the executed plan, labelled `(executed plan)`, or an earlier
  remediation's plan, labelled `(stage Remediate)`.

## Auto modes, retries and probes

- **Modes.** Each stage key runs in Manual or Auto: `discovery`, `create`
  (Create and Update), `reconcile` (Remediate) and `delete`. The default is
  Manual. Auto needs `allowAutoModes.<key>: true` in `OperatorConfig.spec.defaults`
  and in every IntentPolicy of the namespace, and then `modes.<key>: Auto` in an
  IntentPolicy, the defaults or `spec.modes` of the Intent. A `Manual` in an
  IntentPolicy is final. `kubectl last plan web -n team-x` shows what resolved,
  as `Modes: discovery=Manual create=Auto reconcile=Auto delete=Manual`.
- **What Auto skips.** An Auto plan runs without an IntentApproval ("Running plan
  H without approval (Auto)"), and the audit records `authorizedBy` `auto` with
  the stage key and no `approval.observed`. An Auto discovery command runs
  without a prompt. Auto stays inside limits: a Remediate may only re-run steps
  the executed plan ran, and a Delete may only reorder the executed teardown
  commands. A plan outside them waits for your approval as in Manual. A Deny
  still stops an Auto phase.
- **Retries.** A Failed Intent in an Auto stage retries by itself after 1m, 2m,
  4m, and so on up to 1h (`Retries: n` in `kubectl last plan`), except after
  `ApprovalDenied`. In any mode, changing the retry annotation retries a Failed
  Intent of any stage at once:
  `kubectl annotate intent web -n team-x last.moogie-labs.com/retry=3 --overwrite`.
- **Replans.** A failed step makes the model plan again, at most 3 times
  (`Replans: n`); then the Intent fails with `PlanningFailed`.
- **Probes.** Probes run every minute after a change or drift, double after each
  run in which every probe passes up to 5m, and go back to 1m on a failure. Each
  interval is jittered by up to 10%. `probes.fastInterval`, `probes.interval` and
  the floor `probes.minInterval` (1m) set them.

## Upgrading to M5a

- **Approvals name the Intent's UID now.** An IntentApproval made before M5a has
  no `intentUID`, so it authorizes nothing new. An Intent still waiting for
  approval needs approving again with this `kubectl last`. Probes of an Intent
  that already executed keep running: the gateway's own record of the executed
  plan authorizes them, with no standing approval.
- **Approvals expire** after `approvals.maxAge` in OperatorConfig (default 24h).
- **Older Intents have no finalizer.** An Intent created before M5a is not
  torn down when you delete it; what it created stays. To have one torn down,
  recreate it under M5a.

## Upgrading to M5b

- **Edits start an Update.** An Intent edited while Ready under an older
  operator, whose edit that operator ignored, starts an Update on the first
  reconcile after the upgrade. Review and approve it, or set `spec.request` back.
- **Use a `kubectl last` as new as the operator.** Older plugins refuse a
  Remediate plan, which they cannot approve.
- **Drifted Intents start a remediation.** An Intent already `Drifted` when you
  upgrade starts a Remediate on the first reconcile: the operator confirms the
  drift, then asks for your approval.
- **The first sweep deletes extra ManagedResources.** At lead, the gateway's
  record sweep deletes the ManagedResources each Intent controls beyond its
  executed plan's resources, with their output Secrets, each audited as
  `managedresource.deleted`.
- **Some older plans cannot be remediated.** An executed plan from before M5a
  whose teardown commands use `${steps…}` references fails the gateway's checks
  for a Remediate, so its drift cannot be repaired. Edit the Intent to plan an
  Update instead.

## Upgrading to M5c

- **Staged Auto settings take effect at once.** Before M5c, `spec.modes` on an
  Intent, and `modes` and `allowAutoModes` in IntentPolicies and
  `OperatorConfig.spec.defaults`, were stored but did nothing. From the first
  reconcile after the upgrade they decide, so a cluster that staged them runs
  those stages in Auto. Check them before you upgrade:
  `kubectl get intentpolicies -A -o yaml`, `kubectl get operatorconfig -o yaml`
  and `kubectl get intents -A -o yaml`.
- **Failed Intents in an Auto stage retry at once.** The backoff counts from
  when the Intent failed, which is long past for an Intent that failed before
  the upgrade, so its first retry is due immediately. `ApprovalDenied` is never
  retried by itself.
- **Drift is remediated unattended where `reconcile` is Auto.** A Ready or
  `Drifted` Intent whose `reconcile` stage resolves to Auto remediates its next
  drift without asking, within Auto's limits. A plan already waiting in
  `AwaitingApproval` keeps waiting for your approval.
- **Probes restart at the fast interval.** Every operator restart, the upgrade
  included, probes each Intent at `probes.fastInterval` again (1m by default),
  backing off from there. The first runs after a restart are spread over the
  first tenth of that interval, so they do not all start at once.

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

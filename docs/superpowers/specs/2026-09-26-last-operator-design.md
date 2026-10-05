# last-operator — Design

- **Status:** Draft for review
- **Date:** 2026-09-26
- **API group:** `last.moogie-labs.com/v1alpha1`
- **License intent:** Open source

## 1. Overview

`last-operator` is a Kubernetes operator that accepts natural-language
administrative requests ("run a web server serving X", "spin up a
Postgres for team-x", "create an EC2 instance") and provisions,
tracks, reconciles, and tears down whatever those requests describe.

An LLM agent turns each request into a concrete, reviewable **plan**
made of **shell commands** that run in short-lived sandbox pods. Shell
is the only way the system acts on the world:

- Kubernetes is reached with `kubectl`.
- HTTP APIs are reached with `curl`.
- Clouds and SaaS are reached with their CLIs.

Any tools a command needs beyond the base image are **baked into an
image** that the gateway builds automatically. The image is pinned by
digest and reused for every later run of that command, including drift
probes.

The resources a plan creates are then continuously reconciled against
the approved state.

Three properties are central to the design:

1. **The agent can think, but it cannot act on its own.** Every side
   effect goes through a separate, trusted **gateway**. The gateway
   holds all credentials, launches the sandboxes, enforces policy and
   the approved plan, and refuses anything else.
2. **Every action is recorded, write-once.** The gateway writes a
   hash-chained, signed audit entry to WORM storage *before* each
   action and refuses the action if the write fails. A rogue operator
   cannot act without leaving a record, and cannot alter or remove
   the record afterwards.
3. **Manual mode stays usable.** Admins build up **allow rules**
   (wildcard patterns over commands) so that commands they have already
   judged safe don't need approving again. The operator suggests rules
   and never re-suggests ones that were denied.

### 1.1 Goals

- Provision and track "anything" reachable from a CLI. No platform is
  special-cased.
- Serve both self-service developers and sysadmins.
- Configurable **Auto / Manual** modes for each lifecycle stage
  (discovery, create/update, reconcile, delete). Modes are layered
  cluster → namespace → Intent.
- Full reconciliation with drift detection. The LLM is **not** called
  on every reconcile.
- Pluggable LLM providers from day one.
- A tamper-proof, complete audit trail of operator actions.

### 1.2 Non-goals (v1)

See §21.

## 2. Users and interfaces

**Users:** developers (self-service, usually Manual mode) and
sysadmins (approvers, policy and rule authors).

**Interfaces (v1):**

1. **`Intent` CR via `kubectl apply`**, which works well with GitOps:

   ```yaml
   apiVersion: last.moogie-labs.com/v1alpha1
   kind: Intent
   metadata:
     name: team-x-web
     namespace: team-x
   spec:
     request: "Run an nginx web server serving the static site in configmap team-x-site, 2 replicas, internal only"
   ```

   ```
   $ kubectl get intents -n team-x
   NAME         PHASE              STAGE    AGE
   team-x-web   AwaitingApproval   Create   14s
   ```

2. **`kubectl last` plugin.** It creates the same CRs, and is interactive
   where that helps:

   ```
   kubectl last "run an nginx web server serving team-x-site"   # create + follow
   kubectl last answer team-x-web                              # answer clarifying questions
   kubectl last plan team-x-web                                # show plan / pending command
   kubectl last approve team-x-web [--always-allow]            # approve; optionally create allow rules
   kubectl last deny team-x-web
   kubectl last rules suggestions                              # review operator-suggested rules
   kubectl last audit log --intent team-x/team-x-web           # audit timeline
   kubectl last audit verify                                   # verify the WORM chain
   ```

## 3. Architecture and trust boundary

```
 kubectl / kubectl-last ──► Intent / IntentApproval / CommandRule CRs (API server)
                                  │ watch
                    ┌─────────────▼─────────────┐
                    │  Operator (last-system)   │  UNTRUSTED: runs the agent loop
                    │  RBAC: own CRs only       │  no credentials, no Secrets
                    └─────────────┬─────────────┘
                                  │ gRPC (the ONLY way to act)
                    ┌─────────────▼─────────────┐
                    │ Gateway (last-gateway)    │  TRUSTED: holds all credentials,
                    │ policy, rule & plan       │  LLM keys, audit signing key
                    │ enforcement; audit writer │
                    └──┬───────────┬────────────┘
                       │           │
                     LLM APIs   Sandbox pods (last-sandbox) ──► kubectl / curl / any CLI
                       │           per-execution NetworkPolicy + injected credentials
                       │           image@digest ◄── Registry (last-registry, bundled; or external)
                       │                               ▲ push (build pods, no provider creds)
                    ┌──▼────────────────────────┐
                    │ Audit sink (WORM)         │  bundled RustFS (last-audit ns)
                    │ S3 Object Lock, compliance│  or external S3-compatible
                    └───────────────────────────┘
```

**Threat model.** The operator process is untrusted. It runs an LLM
over user-supplied text and command output, so it can be manipulated
through prompt injection, buggy, or driven by malicious model output.
Cluster admins are trusted (see §17.2 for what that implies).

**Namespaces:**

| Namespace | Contents | Operator RBAC | Gateway RBAC |
|---|---|---|---|
| `last-system` | Operator; cluster-scoped rule suggestions | none beyond its CRs | none |
| `last-gateway` | Gateway, credential Secrets, signing key, Kubernetes provider ServiceAccounts | **none** | full within ns (incl. `serviceaccounts/token`) |
| `last-sandbox` | Sandbox and build pods, per-execution NetworkPolicies and credential Secrets | **none** | create/delete Pods, NetworkPolicies, Secrets |
| `last-audit` | Bundled RustFS + root credentials | **none** | **none** (S3 API only) |
| `last-registry` | Bundled Distribution registry + GC sidecar, nginx front proxy, node-local pull proxy DaemonSet, registry credentials | **none** | **none** (registry API only) |

## 4. Components

### 4.1 Operator (`cmd/operator`)

A kubebuilder controller manager. It:

- Reconciles `Intent` and `ManagedResource`.
- Runs the agent loop (prompt construction, the planning conversation,
  and parsing the plan) by calling the gateway's `CallLLM`.
- Drives discovery commands, plan execution, and probes through the
  gateway.
- Creates `CommandRuleSuggestion`s (§9.5).
- Writes status on its CRs.

**RBAC** (one ClusterRole, `charts/last-operator/files/operator-clusterrole.yaml`,
checked by `test/admission`; plus Leases and Events in its own namespace for
leader election):

- get/list/watch on `Intent`; update/patch on `intents/status`; update on
  `intents/finalizers`.
- get/list/watch/create/update/patch on `ManagedResource` and its status.
  **No delete**: deleting a ManagedResource garbage-collects its output
  Secret with no audit entry.
- get/list/watch on `IntentApproval`, `IntentPolicy`, and `OperatorConfig`.
- create/patch on `events.k8s.io` Events.
- Later milestones add:
  - create on `CommandRuleSuggestion` (M7). The `spec.decision` field is
    excluded by admission (§9.5).
  - get/list/watch on `CommandRule` and `ClusterCommandRule` (M6).
  - update/patch on `Intent`, for finalizers (M5).
- **No** create on `IntentApproval`, `CommandRule`, or
  `ClusterCommandRule`. **No** writes to `IntentPolicy`, `OperatorConfig`,
  `Provider`, `LLMProvider`, RBAC, or admission policies.
- **No** access to Secrets, Pods, ConfigMaps, NetworkPolicies,
  ServiceAccount tokens, impersonation, or anything else.

### 4.2 Gateway (`cmd/gateway`)

A gRPC server and the only component able to cause side effects.

**Transport and caller authentication.** The operator calls the gateway
over gRPC on the cluster network (Service `last-gateway`). Every call
carries the operator's projected ServiceAccount token (audience
`last-gateway`, short-lived, mounted by the kubelet). The gateway validates
it with the TokenReview API and accepts only the operator's ServiceAccount
(`system:serviceaccount:last-system:last-operator`). A NetworkPolicy limits
ingress to the gRPC port to the operator's pods in `last-system`. There is no mTLS in v1.
The token check is the authentication boundary, and the audit entry records
the authenticated caller.

**API (called by the operator):**

| RPC | Purpose |
|---|---|
| `CallLLM` | Send a conversation to the resolved `LLMProvider` |
| `RunCommand` | Run one command (or script) in a sandbox pod within an open phase |
| `WriteOutputSecret` | Write provisioned outputs to a Secret in the Intent's namespace |
| `BeginPhase` / `EndPhase` | Open/close a phase context (intent, stage, subject hash) that later calls are checked against |
| `QueryAudit` | Read-only, filtered queries over the audit log (used for rule suggestions) |
| `RunProbes` | Run the probes of the Intent's last executed plan (§10), from the gateway's own record |

**Responsibilities:**

- **Credential custody.** Loads Provider and LLMProvider credentials
  from `last-gateway`. For Kubernetes Providers, it mints short-lived
  ServiceAccount tokens. Credentials are injected into sandbox pods
  and outgoing LLM calls and never returned to the operator.
- **Independent policy resolution.** Reads `OperatorConfig`,
  `IntentPolicy`, `Intent`, `IntentApproval`, and the rule CRs directly
  from the API server. It never trusts the operator's statements
  about modes, approvals, or rules.
- **Phase, approval, rule, and plan enforcement** (§8, §9).
- **Sandbox launcher** (§11).
- **Image builder.** Resolves image specs to digests, building and
  pushing images when needed (§11.2).
- **Probe scheduler.** Enforces the concurrency cap and queue (§10.1).
- **Audit writing**, fail-closed (§14).
- **LLM provider adapters** (§12).

A single active replica, chosen by leader election, is the only audit
writer.

### 4.3 Sandbox pods

Short-lived pods in `last-sandbox`, launched only by the gateway. Every
command the system runs executes in one. See §11.

### 4.4 Audit sink

WORM object storage via the S3 Object Lock API. The Helm chart bundles
RustFS; external S3-compatible storage is also supported. See §14.

### 4.5 Image registry

An OCI registry that holds images built by the gateway. By default the
Helm chart bundles Distribution v3 in `last-registry`, behind an nginx
front proxy, with a node-local pull proxy. An external registry (ECR, GHCR, Harbor, …) can be used
instead. See §11.3.

### 4.6 `kubectl-last` (`cmd/kubectl-last`)

A CLI plugin. It creates and edits Intents, handles clarifying
questions, shows plans and pending commands, approves and denies
(optionally creating allow rules under the user's own identity),
reviews rule suggestions, and reads and verifies the audit log
(using the shared `internal/audit` package with read-only S3
credentials).

## 5. API (CRDs)

All CRDs are in `last.moogie-labs.com/v1alpha1`.

| Kind | Scope | Written by |
|---|---|---|
| `Intent` | Namespaced | Users (spec), operator (status) |
| `IntentApproval` | Namespaced | Approvers only (never the operator) |
| `ManagedResource` | Namespaced, owned by an Intent | Operator |
| `CommandRule` | Namespaced | Namespace rule authors (never the operator) |
| `ClusterCommandRule` | Cluster | Cluster admins (never the operator) |
| `CommandRuleSuggestion` | Namespaced | Operator creates; humans decide |
| `IntentPolicy` | Namespaced | Admins |
| `OperatorConfig` | Cluster, singleton named `default` | Admins |
| `Provider` | Cluster | Admins |
| `LLMProvider` | Cluster | Admins |

### 5.1 Intent

```yaml
kind: Intent
spec:
  request: "…natural language…"
  answers:                       # answers to status.questions
    - id: q1
      value: "internal only"
  modes:                         # optional; may only be as strict or stricter than policy
    discovery: Manual            # Auto | Manual — commands run while planning
    create: Manual               # covers the Create and Update stages
    reconcile: Auto              # Remediate stage
    delete: Manual
status:
  phase: AwaitingApproval
  stage: Create                  # Discovery | Create | Update | Remediate | Delete
  observedGeneration: 3
  questions:
    - id: q1
      text: "Should the server be reachable from outside the cluster?"
      options: ["internal only", "public via Ingress"]
  pendingCommand:                # set while a discovery command awaits approval
    hash: "sha256:…"
    command: { … §8.1 command … }
    reason: "Find the cluster's default IngressClass"
  plan:
    hash: "sha256:…"
    summary: "Deployment (2 replicas nginx), Service (ClusterIP)"
    steps: [ … §8.1 … ]
    resources: [ … ManagedResource specs incl. probes … ]
    uncoveredSteps: [s2]         # steps not matched by any allow rule
    generalizations:             # LLM-proposed wildcard pattern per uncovered command (§9.4)
      s2: { argv: ["kubectl", "apply", "-n", "team-x", "-f", "..."], lint: ["file argument"] }
  suggestedRules: [ … names of CommandRuleSuggestions created for this plan … ]
  conditions: [ … ]
```

Changing `spec.request` or `spec.answers` increments `generation` and
starts an **Update** planning cycle.

### 5.2 IntentApproval

```yaml
kind: IntentApproval
spec:
  intentRef: { name: team-x-web }
  stage: Create                  # Discovery | Create | Update | Remediate | Delete
  subjectHash: "sha256:…"        # plan hash, or pendingCommand hash for Discovery
  decision: Approve              # Approve | Deny
  reason: "…"                    # optional; shown to the agent on Deny
  approvedBy: "alice@example.com"
```

- A **ValidatingAdmissionPolicy** requires `spec.approvedBy ==
  request.userInfo.username`, makes the object immutable, and denies
  the operator's ServiceAccount and every ServiceAccount in the
  gateway namespace (Kubernetes Provider identities must never be able
  to approve). The gateway re-checks both when it uses an approval.
- Who may approve is decided by RBAC: `create` on `intentapprovals` in
  that namespace.
- An approval is valid only for the matching `(intent, stage,
  subjectHash)`.
- A `Deny` on a discovery command is returned to the agent. The agent
  may propose a different command, ask a clarifying question, or give
  up with a reason.

### 5.3 ManagedResource

A child of an Intent, one per provisioned thing:

```yaml
kind: ManagedResource
spec:
  intentRef: { name: team-x-web }
  description: "nginx Deployment team-x/team-x-web"
  identity: { kind: Deployment, namespace: team-x, name: team-x-web }   # free-form, for humans + agent
  desired: { … snapshot of the approved desired state … }
  probes: [ … §10 … ]
  outputsSecretRef: { name: team-x-web-outputs }                         # optional
  outputs: { password: "${steps.s3.json.password}" }                     # Secret key → step reference
  teardown: [ … commands that delete this resource … ]
status:
  observed: { … last probe results … }
  lastProbeTime: …
  conditions:
    - type: InSync      # True / False (drift)
```

### 5.4 Allow rules

```yaml
kind: CommandRule               # or ClusterCommandRule (same spec, cluster-scoped)
metadata: { name: aws-ec2-describe, namespace: team-x }
spec:
  description: "Read-only EC2 inspection"
  stages: [Discovery, Remediate] # which stages this rule may skip approval for
  providers: [aws-prod]          # the command's providers must be a subset of this list
  match:
    argv: ["aws", "ec2", "describe-*", "..."]
  install:                       # image.install commands this rule also covers (same pattern syntax)
    - argv: ["pip", "install", "awscli==1.*"]
  egress: ["*.amazonaws.com:443"] # the command's declared egress must be a subset of this list
  source:                        # provenance, set by the CLI
    createdFrom: approval | suggestion | manual
    suggestionRef: { name: … }
```

The pattern syntax is in §9.2.

### 5.5 CommandRuleSuggestion

```yaml
kind: CommandRuleSuggestion
metadata: { namespace: team-x }  # cluster-targeted suggestions live in last-system
spec:
  targetScope: Namespace         # Namespace → CommandRule; Cluster → ClusterCommandRule
  trigger: PlanTime | RepeatedApproval | PeriodicReview
  rationale: "Lets drift remediation for this Deployment run without approval"
  proposed: { …CommandRule spec… }
  examples: [ …concrete commands this would have matched, from the audit log… ]
  breadthWarnings: [ …lint findings, §9.2… ]
  decision: Pending              # Pending | Accepted | Denied (humans only)
  decidedBy: ""
  decisionReason: ""
```

### 5.6 Configuration CRs

```yaml
kind: OperatorConfig            # cluster defaults; same fields as IntentPolicy, plus:
spec:
  llmProviderRef: { name: anthropic-default }
  sandbox:
    baseImage: ghcr.io/moogie-labs/last-sandbox-base@sha256:…   # pinned by digest
    # When `sandbox.baseImage` is unset, the gateway falls back to the Helm value `sandbox.image` (development and e2e installs only; production pins the image by digest here).
    maxConcurrent: 20           # cluster-wide cap on running sandbox + build pods
  registry:
    mode: bundled               # bundled | external
    external:                   # when mode: external
      repository: 123456789.dkr.ecr.us-east-1.amazonaws.com/last-images
      pushSecretRef: { name: registry-push }     # in last-gateway
      pullSecretRef: { name: registry-pull }     # copied into last-sandbox for pods
  images:
    buildEgress: ["pypi.org:443", "files.pythonhosted.org:443", "registry.npmjs.org:443", "github.com:443", "objects.githubusercontent.com:443"]
    unusedRetention: 30d        # delete images not referenced by any live plan for this long
  probes:
    minInterval: 1m             # floor; no layer may go below it
  suggestions:
    repeatThreshold: 3          # manual approvals of the same pattern before suggesting
    reviewInterval: 24h         # periodic review cadence
  defaults: { …IntentPolicy spec… }
---
kind: IntentPolicy              # namespaced; can only tighten OperatorConfig.defaults
spec:
  modes:   { discovery: Manual, create: Manual, reconcile: Manual, delete: Manual }
  allowAutoModes: { discovery: false, create: false, reconcile: false, delete: false }
  allowScripts: true            # free-form scripts (always need approval in Manual mode)
  providers: [cluster-team-x]   # which Providers Intents here may use
  egress:
    allowed: []                 # CIDR:port or host:port (FQDN needs Cilium) patterns
  probes:
    interval: 5m                # steady-state interval (the adaptive ceiling)
    fastInterval: 1m            # used right after a change or drift (§10.1)
---
kind: Provider
spec:
  type: kubernetes              # kubernetes | env-secret
  description: "Workloads in namespace team-x"
  kubernetes:
    serviceAccountRef: { name: team-x-deployer }   # in last-gateway; admins bind its RBAC
    tokenTTL: 10m
---
kind: Provider
spec:
  type: env-secret
  description: "AWS prod account 1234"
  secretRef: { name: aws-prod } # Secret in last-gateway
  env: { AWS_ACCESS_KEY_ID: access_key_id, AWS_SECRET_ACCESS_KEY: secret_access_key }
  files: {}                     # optional: secret key → file path in the sandbox
---
kind: LLMProvider
spec:
  type: anthropic | openai | openai-compatible | scripted
  model: "…"
  endpoint: "…"                 # for openai-compatible
  secretRef: { name: anthropic-key }
  scripted: { configMapRef: { name: llm-script } }   # type scripted only: fixed responses for tests and demos
```

## 6. Configuration layering and defaults

Settings resolve as **`OperatorConfig.defaults` → `IntentPolicy` →
`Intent.spec.modes`**. Each layer can only tighten the one above it:

- A mode can move from Auto to Manual, never from Manual to Auto,
  unless the policy layer sets `allowAutoModes.<stage>: true`.
- Lists (providers, egress) resolve to the **intersection** of the
  layers.
- Allow rules are additive: the cluster's `ClusterCommandRule`s plus the
  namespace's `CommandRule`s.

The gateway resolves the effective configuration itself for every
phase. The operator resolves it too, but only for display in status.

**Shipped defaults:**

| Setting | Default |
|---|---|
| All modes | `Manual` |
| Allow rules | None |
| Providers | None. The Helm chart can optionally create one `kubernetes` Provider scoped by a ServiceAccount the admin binds. |
| Sandbox egress | Deny all except the Kubernetes API server (only when a `kubernetes` Provider is attached) |
| Audit retention | 365 days, compliance mode |
| Probe interval | 1m after a change or drift, backing off to 5m; floor 1m |
| Sandbox concurrency | 20 pods cluster-wide |
| Registry | Bundled (Distribution v3 + nginx front proxy + node-local pull proxy) |
| Unused image retention | 30 days |

## 7. Intent lifecycle

```
Pending → Clarifying ⇄ (user answers)
        → Planning ⇄ Discovery commands (each: rule match / approval* / run)
        → AwaitingApproval* → Executing → Verifying → Ready
                                                        │
  spec edited  ──► Planning (stage=Update)              ▼
  probe fails  ──► Drifted → Remediating (stage=Remediate; approval* if reconcile=Manual) → Ready
  deleted      ──► finalizer → Planning (stage=Delete) → AwaitingApproval* → TearingDown → removed
  any failure  ──► Failed (reason in conditions; retried with backoff or on spec edit)

  * skipped when the effective mode for that stage is Auto,
    or when every command involved matches an allow rule for that stage (§9.3)
```

- **Clarifying.** The agent may ask questions instead of acting. They
  appear in `status.questions`; answers go in `spec.answers`.
- **Planning.** An agentic conversation. The LLM may run **discovery
  commands** to inspect the world. Each one is treated as a
  single-command approval subject (stage `Discovery`). When it needs
  approval, the Intent shows `phase: AwaitingDiscoveryApproval` with
  `status.pendingCommand`. The planning conversation is kept in memory
  and the audit log, so it resumes after approval.
- **AwaitingApproval.** Waits for a matching `IntentApproval`.
- **Executing.** Runs the plan's steps deterministically (§8.3).
- **Verifying.** Runs every probe once; all must pass. A failure here
  marks the Intent `Failed` with the probe details, so bad probes
  surface at creation time instead of as false drift later.
- **Delete.** A finalizer holds the Intent until teardown completes.
  The teardown plan is built from each ManagedResource's `teardown`
  commands and handed to the agent to review for ordering.
- **Failures (M2).** A step that exits non-zero sends the Intent back to
  Planning with the failure as context. Any other failure moves it to
  `Failed`, with the reason in its `Ready` condition:
  - `PlanningFailed`
  - `ApprovalDenied`
  - `ExecutionFailed`
  - `ExecutionInterrupted`
  - `VerificationFailed`

  A spec edit retries a failed Intent. Automatic retry with backoff
  arrives with Auto modes (M5). Until then every replan needs a fresh
  approval.
- **Operator restarts during Executing.** Step results live only in the
  operator's memory and the gateway's record, so the operator never
  resumes a phase. An `Executed` condition checkpoints the phase:
  - **Phase open:** the operator ends it as failed and marks the Intent
    `ExecutionInterrupted`.
  - **Steps succeeded but the end is unrecorded:** the operator retries
    `EndPhase`. If the gateway has already closed the phase, the operator
    accepts the execution only when `RunProbes` reports the plan's hash as
    the last executed plan.

**Operator restarts during Planning:** the operator reloads the
conversation from `QueryAudit` (`llm.*` and `command.*` entries for the
current phase) and continues. Until `QueryAudit` arrives (M3), a restart during
Planning starts the conversation over.

## 8. Commands, plans, and enforcement

### 8.1 Command format

Every action is a **command**:

```yaml
id: s1
reason: "Create the Deployment"          # shown to approvers
run:
  argv: ["kubectl", "apply", "-f", "/work/deploy.yaml"]   # exec'd directly, no shell
  # — or —
  script: "kubectl get deploy -A -o json | jq '.items | length'"   # run with /bin/sh -c
image:                                    # optional; omitted = base image as-is
  install:                                # baked into an image at build time (§11.2)
    - argv: ["pip", "install", "awscli==1.33.0"]
files:                                    # written to /work before running
  /work/deploy.yaml: "…manifest…"
providers: [cluster-team-x]               # credentials to inject
egress: ["10.0.0.1:443"]                  # beyond what providers imply
timeout: 5m
```

- `argv` elements may reference earlier step results with
  `${steps.<id>.stdout}` or `${steps.<id>.json.<path>}`.
- **`argv` commands can match allow rules. `script` commands never
  match.** A script needs approval whenever its stage is Manual, and
  `allowScripts: false` forbids scripts entirely.
- **Image spec.** The image spec is always based on
  `OperatorConfig.sandbox.baseImage` (pinned by digest). Its
  `specHash` is the SHA-256 of the canonical JSON of `{baseDigest,
  install}`. Commands with the same `specHash` share one image. The
  resolved digest is recorded in the audit entry for every command.
  Install commands are `argv` only; there are no install scripts.

### 8.2 Plan structure

```yaml
plan:
  hash: "sha256:…"   # SHA-256 of the RFC 8785 canonical JSON of steps + resources
  steps:     [ …commands… ]
  resources: [ …ManagedResource specs with probes and teardown commands… ]
```

### 8.3 Enforcement

The operator walks the plan, calling `RunCommand` for each step. For
each call, the gateway:

1. Confirms that a phase is open for `(intent, stage, subjectHash)`.
2. **Confirms authorization.** Any one of these is enough:
   - the effective mode for the stage is Auto;
   - a valid `IntentApproval` exists for this `subjectHash`;
   - every command in the subject (the discovery command, or every
     plan step, including its `image.install` commands) matches an
     allow rule for this stage (§9.3).

   The gateway reads the plan from `Intent.status` and checks its hash
   against `subjectHash`.
3. Confirms the call matches the **next** unexecuted step. The gateway
   resolves `${steps…}` references itself, from its own record of
   earlier results, and requires the resolved arguments, files,
   providers, and egress to match exactly.
4. Confirms providers and egress are allowed by policy.
5. Resolves the image spec to a digest, building the image first if
   needed (§11.2). A build is covered by the same authorization as the
   command that needs it.
6. Writes the `command.start` audit entry, runs the sandbox (§11), and
   writes `command.result`.

Any mismatch produces a `policy.denied` audit entry and an error to the
operator. If a step fails, the Intent returns to **Planning** with the
failure as context. The new plan has a new hash and must be authorized
again.

**The gateway takes the snapshot from its own records.** For
Remediate, Delete, and probes, the gateway uses the **last successfully
executed plan** for the Intent from the audit log: `phase.begin`
records the full plan and `phase.end` records the outcome. It never
trusts `ManagedResource.spec` or `Intent.status` for these, because
the operator can write both. In v1 the gateway keeps that record (the
open phase and the last executed plan per Intent) in a gateway-owned
ConfigMap in `last-gateway`, written only after the matching
`phase.begin`/`phase.end`. The operator has no access to it, and a
gateway restart closes an open phase as `interrupted` rather than
resuming it. The record is bounded (about 900 KiB), so the
gateway refuses at `BeginPhase` a plan too large to record once executed.

- **Auto-mode remediation** is limited to re-running commands from that
  plan. If the remediation plan contains any other command, it needs
  approval (or matching rules) regardless of mode.
- **Teardown commands** must be those recorded in that plan. Changed
  teardown commands need approval or matching rules.

## 9. Allow rules and suggestions

### 9.1 Purpose

Allow rules let Manual mode scale. Once a human decides a pattern of
command is acceptable for given stages and providers, matching commands
no longer need approval. Only humans create rules. The operator only
suggests them.

### 9.2 Pattern syntax

`match.argv` is a list of per-position patterns, matched against the
command's fully resolved argv:

| Pattern | Matches |
|---|---|
| `kubectl` | Exactly that argument |
| `describe-*`, `v1.?` | Glob within one argument (`*`, `?`, `[…]`); never spans arguments |
| `re:^[a-z0-9-]{1,63}$` | Anchored RE2 regex against one argument |
| `...` | Zero or more remaining arguments (last position only) |

**Constraints, enforced by a ValidatingAdmissionPolicy on the rule
CRDs:**

- `argv[0]` must be a literal.
- `...` is only allowed in the last position.
- `stages` and `providers` must be non-empty.
- The operator's ServiceAccount may not create or update rules.

**Breadth lint.** The gateway flags breadth in rule suggestions, and
the CLI flags it when creating rules. Examples:

- `...` directly after the binary name;
- globs on subcommand positions of known destructive verbs (`delete`,
  `rm`, `terminate-*`);
- file arguments (`-f`, `--from-file`).

**Rules match argv only, not the contents of `files` or anything read
from stdin.** A rule for `kubectl apply -f ...` allows applying *any*
manifest those credentials permit. The lint calls this out.

**Image install commands:** a command matches a rule only if each of its
`image.install` commands also matches an entry in the rule's `install`
list. Otherwise a rule for `aws ...` would allow running whatever binary
an unapproved install step had baked into the image at `aws`.

### 9.3 Matching

A command is **covered** at stage S if some rule (namespace
`CommandRule` or `ClusterCommandRule`) meets all of these:

- `S ∈ rule.stages`;
- the command's `providers ⊆ rule.providers`;
- the command's declared egress ⊆ `rule.egress`;
- `run.argv` matches `rule.match.argv`;
- every `image.install` command matches an entry in `rule.install`;
- the command is `argv`, not `script`.

A plan is covered when every step is covered. `status.plan.uncoveredSteps`
lists the ones that aren't, so approvers can see what needs attention.
The gateway does the matching itself. The operator's copy is for
display only.

### 9.4 Approving with "always allow"

`kubectl last approve <intent> --always-allow` (and the interactive
prompt) walks the uncovered commands. For each command, the approver
picks one of:

- **Exact:** a rule matching this command's argv literally.
- **Generalized:** the LLM's suggested wildcard pattern (§9.5), shown
  with the lint results and example commands it would match. The
  approver can edit it before accepting.
- **Just this once:** no rule.

The CLI creates the chosen `CommandRule` or `ClusterCommandRule`
objects **as the approver**, so RBAC on rule creation applies naturally.
Then it creates the `IntentApproval`. Each rule records
`source.createdFrom: approval`.

### 9.5 Operator suggestions

The operator creates `CommandRuleSuggestion`s with an LLM-proposed
generalized pattern, a rationale, and real example commands. They are
created at three points:

| Trigger | When | Typical content |
|---|---|---|
| `PlanTime` | With every new plan | Rules that would let this Intent's **remediation** and **teardown** run unattended, and rules generalizing the plan's own commands |
| `RepeatedApproval` | A command group has been approved manually `repeatThreshold` times (counted via `QueryAudit`). Commands are grouped by binary, leading subcommands (the first non-flag arguments, up to 3), provider set, and stage. | A rule covering those approvals |
| `PeriodicReview` | Every `reviewInterval` | Consolidated rules from recent audit history (e.g. merging several narrow rules) |

The operator also writes a generalized pattern for each uncovered
command to `status.plan.generalizations` (and to
`status.pendingCommand` for discovery), so the "Generalized" option in
§9.4 is available during approval. Generalizations are not part of
`planHash`: they are advice to humans and never authorize anything by
themselves.

**Deciding.** `kubectl last rules suggestions` lists pending suggestions.
Accepting creates the rule as the human and sets `decision: Accepted`.
Denying sets `decision: Denied` and an optional reason. A
ValidatingAdmissionPolicy lets only non-operator users set
`decision`, `decidedBy`, and `decisionReason`, and stamps `decidedBy`
from `request.userInfo`.

**No re-suggesting denied patterns.** Before creating a suggestion,
the operator compares it with every `Denied` suggestion in the same
scope. It drops the new suggestion if the denied pattern is **equal to
or narrower than** the new one, meaning the new pattern would match
everything the denied one did. Narrower suggestions are still allowed:
a denied `aws ec2 ...` does not block a later `aws ec2 describe-* ...`.
Denied suggestions are kept indefinitely to preserve this memory, and
the operator never deletes them (it has no delete permission).

Matching is compared per position (literal ⊂ glob ⊂ `...`). When the
operator can't decide whether one pattern contains another (e.g.
between two regexes), it treats the new pattern as broader and does
not suggest it.

## 10. Probes and reconciliation

A probe is a command plus a CEL assertion over its result:

```yaml
probes:
  - id: replicas
    command:
      run: { argv: ["kubectl", "get", "deploy", "team-x-web", "-n", "team-x", "-o", "json"] }
      providers: [cluster-team-x]
    assert: "result.exitCode == 0 && result.json.spec.replicas == 2 && result.json.status.readyReplicas == 2"
```

- `result` has `exitCode`, `stdout`, `stderr`, and `json` (stdout parsed
  as JSON when possible).
- Probes are part of the approved plan, so running them later needs no
  new approval. The gateway runs only probes that appear in the
  Intent's last executed plan (§8.3). Editing probes on a
  ManagedResource cannot change what runs.
- The `ManagedResource` controller runs probes on the schedule in §10.1
  and does not call the LLM. All probes of one Intent that share the
  same provider set, egress set, and image run in a single sandbox pod:
  one **probe run**. (Grouping by egress as well keeps a probe from
  receiving network access granted only to its neighbours.)
- If an assertion fails, `InSync=False`, the Intent moves to `Drifted`,
  and the LLM builds a remediation plan.

### 10.1 Scheduling

- **Adaptive interval.** After an execution, a remediation, or a
  detected drift, an Intent is probed at `probes.fastInterval`. Each
  probe run in which every probe passes doubles the interval, up to
  `probes.interval`. Any failure resets it to `fastInterval`.
- **Floor.** No layer can set an interval below
  `OperatorConfig.probes.minInterval`.
- **Fixed interval (M2).** Until adaptive intervals arrive (M5), each
  Intent is probed at a fixed interval: the shortest `probes.interval` set
  by `OperatorConfig.spec.defaults` or any `IntentPolicy` in its namespace
  (default 5m), never below `probes.minInterval`. Runs are scheduled on a
  grid from the previous run's start; grid points that pass while a run is
  in flight are skipped and counted.
- **Jitter.** Each scheduled run is offset by a random ±10% of its
  interval, so Intents created together don't probe in lockstep.
- **No overlap.** An Intent never has more than one probe run in flight
  or queued. If the previous run hasn't finished when the next one is
  due, the new run is skipped and the skip is counted in metrics.
- **Concurrency cap and queue.** The gateway runs at most
  `sandbox.maxConcurrent` sandbox and build pods cluster-wide. Beyond
  that, requests queue with priority: plan execution and teardown
  first, then discovery, then image builds for probes, then probes.
  Queue depth and wait time are exported as metrics.

### 10.2 Compact audit entries for probe runs

A probe run writes **two** audit entries instead of one set per command:

- `probe.start`, written **before** anything runs. It lists every
  probe command, the image digest, the credentials to be issued, and
  the egress to be granted.
- `probe.result`, written afterwards. It records each probe's exit
  code, output (truncated as in §11.1), assertion outcome, and the
  egress revocation.

This keeps the write-before-action guarantee (§14.1) while cutting a
probe run from about 7–8 entries to 2.

## 11. Sandbox execution

### 11.1 Sandbox pods

Every command runs in a sandbox pod:

- **Image.** Either the base image
  (`OperatorConfig.sandbox.baseImage`, which contains common basics:
  `sh`, `curl`, `kubectl`, `jq`, `git`, `python3`) or a built image for
  the command's image spec (§11.2). Images are always referenced by
  digest. Nothing is installed at runtime.
- **Pod.** Created by the gateway in `last-sandbox`, with no
  ServiceAccount token mounted, non-root, a read-only root filesystem
  except for a writable `emptyDir` at `/work`, CPU and memory limits,
  and `activeDeadlineSeconds` set from `timeout`.
- **Execution.** The `files` are written, then `run` executes. `argv` is
  exec'd directly. `script` runs with `/bin/sh -c`.
- **Credentials.** For each provider in `providers`, the gateway creates
  a short-lived Secret, mounts it, and deletes it when the pod ends:
  - `env-secret`: the configured env vars and files;
  - `kubernetes`: a token minted with the TokenRequest API for the
    Provider's ServiceAccount (lifetime `tokenTTL`) and a kubeconfig
    pointing at the in-cluster API server.

  A `credential.issued` audit entry records the Provider and scope,
  not the values.
- **Egress.** `last-sandbox` has a default-deny NetworkPolicy. For each
  execution the gateway creates a NetworkPolicy selecting only that pod
  (by a unique label). It allows the command's declared egress, and the
  API server when a `kubernetes` provider is attached, all intersected
  with policy.
  The policy is removed afterwards, and both actions are audited.
  - **Requirement:** the cluster's CNI must enforce NetworkPolicy.
    Host-name egress rules require Cilium (or equivalent). Without it,
    only CIDR rules are supported.
  - **Egress gate.** CNIs may program a new pod's policy a moment after
    the pod starts (kindnet: about one second), leaving egress open.
    The runner therefore runs nothing until it has seen every
    configured *egress canary* (an `IP:port` that is reachable whenever
    egress is open and that the run's policy denies) time out on
    at least three consecutive rounds spanning at least two seconds; after 30 seconds it
    fails every command closed. The gateway rejects any grant that
    covers a canary. Canaries should include one whose backend is not
    itself selected by a NetworkPolicy, since some CNIs police traffic
    to policy-selected pods on a different path. The CNI must *drop*
    denied packets (not reject them), and the gateway's http port must
    stay reachable from any namespace while it serves as a canary.
- **Output.** The gateway captures stdout, stderr (each truncated at
  1 MiB, with truncation recorded), and the exit code into the audit
  log (`command.result`) and returns them to the operator. Before
  either, it replaces every credential value it issued for that run
  (raw, and base64-encoded) with `[REDACTED]`. This is best-effort: a
  command that re-encodes a value defeats it, so short-lived
  credentials and the audit trail remain the real controls.

### 11.2 Automatic image builds

When a command has `image.install`, the gateway resolves its
`specHash` to a digest:

1. **Lookup.** The gateway keeps a `specHash → digest` index, rebuilt
   at startup from `image.build.result` audit entries. The audit log,
   not registry tags, is authoritative, so a changed tag is never
   trusted. If an entry exists and the digest is still in the
   registry, the gateway uses it.
2. **Build.** Otherwise the gateway generates a Containerfile:
   ```
   FROM <baseImage>@<baseDigest>
   USER root
   RUN ["pip", "install", "awscli==1.33.0"]      # one exec-form RUN per install argv
   USER 65532
   ```
   It then launches a **build pod** in `last-sandbox`, using rootless
   BuildKit. The build pod:
   - gets **no provider credentials**, ever;
   - has egress only to `OperatorConfig.images.buildEgress` (package
     sources such as PyPI, npm, and GitHub releases, intersected with
     policy) and to the registry;
   - receives the registry's push-only credential (bundled: the
     front proxy's *pusher* credential, which cannot delete; external:
     `pushSecretRef`) through a Secret that exists only for the build;
   - has CPU, memory, and time limits.
3. **Record.** The gateway writes `image.build.start` (specHash and
   Containerfile) before the build, and `image.build.result` (digest
   and build log, truncated at 1 MiB) afterwards. The command then
   runs on `image@digest`.
4. **Single flight.** Concurrent requests for the same `specHash` wait
   on one build.
5. **Failure.** A failed build fails the command, and the build log is
   returned to the agent as context.

Once built, an image is reused by every later command with the same
spec, including probes, remediation, and teardown. Unpinned installs
(`pip install awscli`) are therefore frozen at build time, and probes
don't change behavior when upstream packages change. A **new** plan
with the same spec reuses the image. To pick up new package versions,
the plan must pin a different version.

**Garbage collection.** Once a day, the gateway deletes images whose
`specHash` isn't referenced by any existing Intent's last executed
plan and that haven't been used for `images.unusedRetention`. Each
deletion is audited (`image.deleted`). For the bundled registry this
works in two parts (§11.3):

1. The gateway deletes manifests through the registry API.
2. Blob storage is reclaimed in a GC window, which the gateway
   coordinates.

External registries use their own retention and GC after the manifest
is deleted.

### 11.3 Registry

**Bundled (default).** The Helm chart installs the following in
`last-registry`:

```
 kubelet ──► node-local pull proxy (DaemonSet, hostPort 5000; GET/HEAD only)
                 │
 build pods ─────┼──► front proxy (nginx Deployment) ──► Distribution v3 (PVC)
 gateway ────────┘      GET/HEAD: anonymous             storage.delete.enabled: true
                        PUT/POST/PATCH: pusher cred     no auth of its own; ingress
                        DELETE: admin cred (gateway)    only from the front proxy
                                                        + GC sidecar
```

- **Distribution v3** (CNCF, the reference OCI registry) stores images
  on a PVC. It has no authentication of its own. A NetworkPolicy
  allows ingress only from the front proxy.
- **Front proxy.** nginx with per-method basic auth, the approach
  Distribution's docs recommend:
  - `GET`/`HEAD` are anonymous;
  - push methods (`PUT`/`POST`/`PATCH`) require the **pusher**
    credential, which build pods receive;
  - `DELETE` requires the **admin** credential, held only by the
    gateway.

  Both credentials are generated at install time. The operator has no
  access to either.
- **Node-local pull proxy.** A DaemonSet on `hostPort` 5000 that
  forwards only `GET`/`HEAD` to the front proxy. Images are referenced
  as `localhost:5000/last/images@sha256:…`, so kubelet pulls through
  the proxy on its own node. containerd allows plain HTTP to
  `localhost` by default, so this needs no TLS certificates or
  containerd configuration (the classic `kube-registry-proxy`
  pattern). Anonymous pulls are acceptable because images never
  contain credentials: builds never receive any.
- **Tags don't matter for integrity.** Anyone holding the pusher
  credential (i.e. a build pod) could push arbitrary content under
  any tag. That cannot affect what runs, because the gateway only ever
  references digests recorded in the audit log (§11.2). The worst case
  is storage consumption, which GC cleans up.
- **Coordinated GC.** Distribution's `registry garbage-collect` is
  unsafe while uploads are in progress, and every upload comes from a
  build the gateway starts. The gateway therefore runs GC as follows:
  1. Write `image.gc.start`.
  2. Pause new builds (they queue) and wait for running builds to
     finish.
  3. Delete unused manifests through the front proxy with the admin
     credential.
  4. Call the **GC sidecar** in the registry pod. It shares the PVC,
     runs `registry garbage-collect`, and listens on an endpoint that
     requires the admin credential and is reachable only from the
     gateway.
  5. Resume builds and write `image.gc.result` with the bytes freed.

  Pulls continue throughout. A build waiting during GC is delayed, not
  failed.
- **Validation required:** the pull-proxy pattern must be tested early
  (M4) on kind, k3s, EKS, and GKE Standard. Platforms that forbid
  `hostPort` (e.g. GKE Autopilot) must use an external registry.

**External (optional).** `registry.mode: external` points at a
repository in ECR, GHCR, Harbor, or similar. The gateway uses
`pushSecretRef` to push and delete. Sandbox pods use `pullSecretRef` as
an `imagePullSecret`.

## 12. LLM providers

`CallLLM` takes a provider-neutral request (messages, the plan and
command schemas, max tokens). Adapters in the gateway translate it for
each provider: `anthropic`, `openai`, and `openai-compatible` (for
local and self-hosted models). Every request and response is audited
(`llm.request` / `llm.response`), so the audit log also serves as the
complete transcript store.

## 13. Credentials

v1 uses **operator-wide credentials**: Providers are cluster-scoped,
and each namespace's `IntentPolicy.providers` decides which ones its
Intents may use. Credentials are held **only** by the gateway. The
operator never sees them, and sandboxes receive only the Providers
named in the command, for the lifetime of that pod. Scoping
credentials to namespaces or requesters is out of scope for v1 (§21).

Kubernetes Provider tokens are bound to a per-run anchor Secret in the
gateway namespace, so cleanup (or the reaper) revokes them at once. A
Provider may never name the gateway's own ServiceAccount, the audit
signing key or writer credentials, or another run's objects; the
gateway refuses such a Provider, and an admission policy forbids the
gateway from minting a token for its own ServiceAccount.

## 14. Audit log (write-once, read-many)

### 14.1 Guarantees

1. **Complete.** No side effect happens without an audit entry written
   first. The gateway is the only path to side effects, and it is
   fail-closed: if the audit write fails, the action is refused.
2. **Immutable.** Written entries cannot be modified or deleted during
   the retention period. S3 Object Lock in **compliance mode** enforces
   this, including against the bucket owner.
3. **Tamper-evident.** Gaps, reordering, and modification are
   detectable through the hash chain, sequence numbers, and Ed25519
   signatures.

### 14.2 Entry format

One object per entry, at `chains/<chainId>/<seq zero-padded to 20>.json`:

```json
{
  "v": 1,
  "seq": 1042,
  "chainId": "3f1c…",
  "prevHash": "sha256:…",
  "ts": "2026-09-26T18:04:11.123Z",
  "type": "command.start",
  "actor": "operator",
  "intent": "team-x/team-x-web",
  "stage": "Create",
  "subjectHash": "sha256:…",
  "authorizedBy": { "kind": "rule", "name": "team-x/kubectl-get-deploy" },
  "payload": { },
  "hash": "sha256:…",
  "sig": "ed25519:…"
}
```

- `v` is the entry format version (currently `1`); verifiers reject any
  other value. It is covered by `hash`.
- `hash` is the SHA-256 of the RFC 8785 canonical JSON of every field
  except `hash` and `sig`. `sig` is an Ed25519 signature over `hash`.
- The stored object body is the RFC 8785 canonical JSON of the whole
  entry (including `hash` and `sig`), byte for byte; verifiers reject any
  other encoding, unknown fields, and trailing data. (The example above is
  pretty-printed for reading.)
- Payload numbers must be integers with magnitude at most 2^53, which RFC
  8785 serializes unchanged; larger or non-integral numbers must be
  encoded as strings, and the writer rejects them otherwise.
- `authorizedBy` records what allowed the action: an `IntentApproval`,
  a rule, or an Auto mode (with the policy it came from).
- The signing key is a Secret in `last-gateway`, and the public key is
  published in a ConfigMap and the release notes. KMS-backed signing is
  out of scope for v1.
- Writes use `PutObject` with `If-None-Match: *`, and each object gets
  compliance-mode retention.

**Entry types:** `gateway.start`, `checkpoint`, `phase.begin`,
`phase.end`, `llm.request`, `llm.response`, `command.start`,
`command.result`, `credential.issued`, `egress.granted`,
`egress.revoked`, `probe.start`, `probe.result` (§10.2),
`image.build.start`, `image.build.result`, `image.deleted`,
`image.gc.start`, `image.gc.result` (§11.2, §11.3),
`approval.observed`, `rule.observed` (a rule created, changed, or
deleted, seen via watch), `suggestion.decided`, `policy.denied`,
`output.written` (records a WriteOutputSecret with each value as its SHA-256).

`phase.end` records an outcome: `succeeded` (every step ran and exited 0),
`failed` (the operator ended a phase whose step failed or was refused),
`interrupted` (a gateway restart or leadership change closed an open
phase; its plan never reopens), or `aborted` (the gateway could not record
the phase it just began).

**Redaction:** credential values are never logged. Values written to
output Secrets are logged as SHA-256 hashes. LLM payloads are logged in
full; command output is logged in full apart from the issued-credential
redaction in §11.1. Operators should treat the audit bucket as
sensitive (§17.3).

### 14.3 Chain continuity

- There is one chain per installation, written by the single active
  gateway replica (lease-based).
- On startup the gateway lists the chain to find its head, verifies the
  last checkpoint and every entry after it, and then writes
  `gateway.start`, linked to the head. If verification fails, the
  gateway refuses to start.
- A signed `checkpoint` entry (containing the previous entry's seq and
  hash) is written every 1,000 entries or 10 minutes, whichever comes
  first. Each checkpoint also writes an index object
  `checkpoints/<chainId>/<seq>.json` (`{seq, hash}`, compliance-locked),
  so startup and verification can find the latest checkpoint without
  listing the whole chain. The verifier checks each index object
  against the chain.
- **Chain start:** the first entry has seq 0 and `prevHash` set to
  `sha256:` followed by 64 zeros.
- **Unclear write failures.** If a write fails in a way that leaves it
  unclear whether it succeeded, the gateway retries the same bytes. A
  412 (key exists) counts as success only if the stored bytes are
  identical; otherwise the chain has diverged and the gateway stops.
  After repeated failures the writer stops accepting writes until the
  gateway restarts and re-reads the chain, so all actions are refused
  in the meantime (fail-closed).

### 14.4 Storage backends

**Bundled RustFS** (Apache 2.0, S3-compatible, supports Object Lock;
`last-audit` namespace, enabled by default). The chart deploys a
single-node RustFS StatefulSet with its own templates rather than a
subchart, because Helm subcharts can only install into the parent
release's namespace. Users and policies are created through RustFS's
native admin API (`/rustfs/admin/v3/*`, plain JSON, SigV4-signed). MinIO was
considered, but its community edition was archived in 2026 and no
longer ships images.

- An install Job creates the bucket with Object Lock enabled and sets
  default compliance-mode retention.
- The gateway receives a RustFS user whose policy allows only
  `s3:PutObject`, `s3:PutObjectRetention` (needed to set retention on
  each write; compliance mode still refuses any shortening),
  `s3:GetObject`, `s3:GetObjectVersion`, `s3:ListBucket`,
  `s3:ListBucketVersions`, and `s3:GetBucketObjectLockConfiguration`.
- A separate read-only user is created for `kubectl last audit`.
- RustFS root credentials exist only in `last-audit`. Neither the
  operator nor the gateway has RBAC there.
- **The restricted users are defense in depth.** The integrity
  guarantee comes from compliance mode, which stops even the root user
  from deleting objects or shortening retention. The conformance check
  (below) verifies this with root credentials.
- **Production guidance:** the docs recommend external S3 (or Ceph RGW)
  for production compliance use. RustFS is young (v1.x), and the bundled
  store is the easy default.

**External S3-compatible storage:** the admin supplies a bucket that
already has Object Lock in compliance mode, plus credentials limited to
the same permissions. At startup the gateway **refuses to start** if the
lock is missing, not in compliance mode, or has a shorter retention
than configured. This startup check only reads the bucket's lock
configuration.

**WORM conformance check.** `kubectl last audit conformance` (and a CI
job against the bundled store) writes a probe object with short
compliance retention to a `conformance/` prefix. It then asserts
that:

- **These are refused:**
  - deleting the object version;
  - writing the key with `If-None-Match: *`;
  - shortening retention;
  - switching the object to governance mode;
  - deleting the bucket.
- **These leave the original version intact:**
  - writing the key without `If-None-Match`, which S3 semantics allow
    to create a *new* version;
  - a plain `DELETE`, which creates a delete marker.

  The verifier flags both of these separately (§14.5).

It runs every check twice, once with the gateway's credentials and
once with credentials supplied by an admin (e.g. root), and reports
each result. It never runs automatically at gateway startup, so it
doesn't write objects to production buckets unexpectedly. It is
intended for CI, for evaluating a new backend, and for periodic audits.

### 14.5 Verification and reading

- `kubectl last audit verify [--from-checkpoint N]` checks signatures,
  hash links, sequence continuity, that each seq has exactly one
  object version and no delete markers, and checkpoint consistency.
  It reads each entry by version ID. It exits non-zero on any failure
  and names the first bad seq.
- **Truncation detection.** Compliance mode prevents deleting the end of
  the chain, but `verify --expect-head N` also fails if the chain ends
  before seq N. N can come from any external record, for example the
  gateway's `last_audit_chain_seq` metric history.
- `kubectl last audit log --intent ns/name [--follow]` prints a
  readable timeline.
- The `internal/audit` package is shared by the gateway and the CLI.

### 14.6 Independent cross-check (documented)

The docs describe how to configure Kubernetes API server audit
logging for the operator and gateway ServiceAccounts, and for the
Kubernetes Provider ServiceAccounts, and ship it to the same bucket
under `k8s-audit/`. This record is produced by the API server, not the
gateway. It is documented and not enforced, because it is cluster-level
configuration.

## 15. Outputs

- Non-sensitive outputs (endpoints, IDs) go in `ManagedResource.status`.
- Sensitive outputs (passwords, connection strings) are written by the
  gateway (`WriteOutputSecret`) to a Secret in the Intent's namespace.
  The Secret is owned by the ManagedResource and referenced by
  `outputsSecretRef`. The plan declares each value in the resource's
  `outputs` (Secret key → `${steps.<id>.json.<path>}`), so the LLM never
  needs to repeat it, and approvers see what will be written. The gateway
  writes exactly the declared outputs and refuses any other values.
- The operator can create Secrets only through this RPC, never
  directly.

## 16. Observability

Prometheus metrics from both the operator and the gateway, labeled by
namespace and Intent:

- LLM tokens (in/out) and calls, per provider and model
- Commands by stage, outcome, and authorization kind (approval / rule / auto)
- Sandbox pod startup and run durations; sandbox queue depth and wait time
- Image builds (count, duration, failures), image cache hits, registry size
- Probe runs skipped due to overlap (`last_operator_probe_runs_skipped_total`)
- Approvals pending and time-to-approve
- Rule suggestions created, accepted, and denied
- Audit writes, audit write failures, and chain seq
- `policy.denied` counts
- Probe runs, failures, and drift events (`last_operator_probe_runs_total`, by result)

The CRs emit standard Kubernetes Events for phase transitions.

## 17. Security model

### 17.1 Summary of controls

| Risk | Control |
|---|---|
| Prompt-injected or rogue agent acts without permission | Gateway is the only path to side effects; approval / rule / Auto checks; plan conformance |
| Operator forges approvals or rules | `IntentApproval`, `CommandRule`, `ClusterCommandRule`, and suggestion decisions are unwritable by the operator (RBAC + admission) |
| Operator grants itself Auto mode or more providers | Config CRs are read-only to the operator; the gateway resolves policy itself |
| Command chaining defeats allow rules | Rules match structured argv only; scripts never match |
| Unapproved binary masquerades as an allowed one | `image.install` commands must also match the rule; images pinned by digest, with the digest mapping taken from the audit log rather than registry tags |
| Build step leaks or abuses credentials | Build pods never receive provider credentials; egress limited to `buildEgress` + registry |
| Credential leakage via LLM context | Credentials are never returned to the operator; injected only into the sandbox |
| Operator hides its tracks | Fail-closed WORM audit written by the gateway; hash chain + signatures + compliance lock |
| Command escapes its scope | Sandbox with no API token, per-execution egress, short-lived credentials limited to the named Providers |
| Operator reads sandbox state directly | Operator RBAC has no access to pods, `pods/log`, or Secrets in the sandbox namespace (namespaced Roles, not a ClusterRole); results reach it only through the gateway, after redaction and audit |
| Gateway tampers with its own audit trust anchors | A `ValidatingAdmissionPolicy` forbids the gateway ServiceAccount from changing or deleting its signing key, writer credentials (bundled `last-audit-writer` or the configured external Secret), or chain config |

### 17.2 Known limitations

- **Cluster admins are trusted.** A cluster admin can redeploy the
  gateway with modified code or change policy. Entries already written
  stay protected by compliance mode. Protecting future entries against
  a malicious admin would require external anchoring, which is out of
  scope.
- **Operator-wide credentials.** Every namespace allowed to use a
  Provider shares its full permissions.
- **Auto-mode discovery runs before any plan exists.** With
  `discovery: Auto`, the agent can run arbitrary commands with the
  allowed Providers' credentials during planning, and nothing checks
  them against a plan. They are fully audited. Admins who want
  unattended discovery with less risk should prefer allow rules for
  known-safe inspection commands over Auto mode.
- **Rules can be broad.** A rule is exactly as safe as its pattern.
  Rules don't inspect `files` or stdin. The breadth lint warns but does
  not block.
- **No hard caps on agent turns or cost.** Runaway agents are visible
  in metrics but not stopped automatically.
- **Shell commands can do anything their credentials allow.** The audit
  log records the exact command and output, but not the individual API
  calls the CLI makes. Provider-side audit trails (e.g. CloudTrail,
  Kubernetes audit) complement it.
- **Every command and probe starts a pod.** Expect a few seconds of
  latency per command, plus at least two seconds for the egress gate
  (§11.1). The first use of a new image spec also waits for
  a build, from about 30 seconds to a few minutes.
- **Audit volume at scale.** Even with compact probe entries, each probe
  run writes 2 objects under compliance retention, and every write goes
  through one serialized writer. As a rough estimate, 500 Intents
  probed every 5 minutes produce about 100M+ objects per year. Group
  commit (batching entries into segment objects) is the planned fix and
  is out of scope for v1.
- **Build pods are less isolated than sandboxes.** Rootless BuildKit
  needs relaxed seccomp/AppArmor profiles and user namespaces. This is
  mitigated by giving builds no credentials and limited egress.
- **Bundled registry node pulls** rely on `hostPort` and containerd's
  plain-HTTP allowance for `localhost`. Platforms that forbid either
  must use an external registry.
- **Built images can contain vulnerable packages.** Images are frozen at
  build time and not rescanned or rebuilt automatically.
- **A command can falsify its own result.** The runner and the command
  share a UID, so a command (or a child that escapes its process group)
  can write a forged `LAST-RESULT` line to the runner's stdout and
  override its own, or a sibling probe's, exit code and output. The
  script itself is still recorded in `command.start`, and every such
  command was planned and approved.
- **Credential redaction is best-effort.** A command that re-encodes a
  credential value defeats it (§11.1).
- **The egress gate is a point-in-time check.** It proves policy is in
  force when the command starts. A CNI that fails open when its agent
  restarts (kindnet queues policy decisions with bypass) can reopen
  egress mid-run. A CNI that rejects denied packets instead of dropping
  them makes every run fail closed.
- **Probe reliability** depends on the LLM writing good probes. The
  Verifying phase catches probes that fail immediately, but not probes
  that pass trivially.

### 17.3 Audit data sensitivity

The audit log contains full prompts, commands, and outputs, which may
include business data. Read access to the audit bucket should be
restricted to the same level as admin access.

## 18. Testing strategy

- **Unit tests:** plan and command canonicalization and hashing; policy
  layering and intersection; plan conformance, including `${steps…}`
  resolution; argv pattern matching (globs, regexes, `...`, install
  coverage, provider and egress subsets); pattern containment used for
  denied-suggestion suppression; breadth lint; CEL probe evaluation;
  image spec hashing and Containerfile generation; adaptive probe
  interval, jitter, and overlap skipping; sandbox queue priority.
- **Audit tamper tests** (`internal/audit`): deleted entry, reordered
  entries, modified payload, invalid signature, extra object version,
  forged checkpoint, truncated tail. Each must fail `verify` with the
  right seq.
- **envtest:** Intent, ManagedResource, and suggestion flows against a
  real API server, using a **scripted fake LLM** (deterministic
  request → response fixtures) and a fake gateway.
- **Gateway integration tests:** against RustFS via testcontainers,
  covering fail-closed behavior, refusal to start without compliance
  lock, and denial of unauthorized or mismatched commands; against
  Distribution + the nginx front proxy via testcontainers, covering
  build → push → digest recorded, single flight, rebuilding the index
  from the audit log, ignoring a tampered tag, per-method auth (a
  pusher cannot delete; anonymous users cannot push), and coordinated
  GC (builds queue during GC; referenced blobs survive).
- **Admission tests:** the operator ServiceAccount cannot create
  approvals or rules or set suggestion decisions; `approvedBy` /
  `decidedBy` spoofing is rejected; rule constraints are enforced
  (literal `argv[0]`, `...` last only).
- **End-to-end tests on kind** (with kind's default kindnet CNI, which enforces NetworkPolicy): full
  Helm install with bundled RustFS and registry, `kubectl last audit conformance` passing; a command whose image
  requires a build, then runs from the node-local proxy; the web server
  demo driven by the
  scripted LLM through discovery, approval with `--always-allow`,
  execution, probes, drift remediation covered by a rule, and teardown;
  then `audit verify`.
- **Real-LLM runs** are opt-in (not in default CI) and serve as early
  evals of planning and pattern-generalization quality.

## 19. Repository layout

A single Go module:

```
api/v1alpha1/            CRD types
cmd/operator/            controller manager
cmd/gateway/             gateway server
cmd/kubectl-last/        CLI plugin
internal/agent/          agent loop, plan/command schema, prompts, rule generalization
internal/controller/     Intent, ManagedResource, suggestion reconcilers
internal/gateway/        RPC server, phase + authorization enforcement
internal/gateway/llm/    anthropic, openai, openai-compatible adapters
internal/rules/          argv pattern matching, containment, breadth lint (shared)
internal/policy/         layered config resolution (shared by operator + gateway)
internal/audit/          chain, signing, S3 sink, verifier (shared with CLI)
internal/sandbox/        pod, credential, and NetworkPolicy builders; scheduler + queue
internal/images/         image spec hashing, Containerfile generation, builds, registry client, GC
images/sandbox-base/     base sandbox image
charts/last-operator/    Helm chart (+ RustFS subchart; Distribution, front proxy, pull proxy; admission policies)
images/registry-gc/      GC sidecar (wraps `registry garbage-collect`)
docs/
```

## 20. Milestones

| # | Milestone | Scope | Done when |
|---|---|---|---|
| M0 | Scaffolding | kubebuilder project, all CRD types, Helm skeleton, CI (lint, unit, envtest) | `make test` green in CI; CRDs install on kind |
| M1 | Audit foundation | `internal/audit`; gateway skeleton with fail-closed writes, leader election, startup verification; bundled RustFS with compliance lock; `kubectl last audit verify/log/conformance` | Tamper tests pass; conformance passes against bundled RustFS in CI; gateway refuses to act when the store is down |
| M2 | Sandbox + lifecycle (fake LLM) | Sandbox pods on the base image only (commands with `image.install` rejected until M4), `kubernetes` and `env-secret` Providers, per-execution egress; concurrency cap + queue; Intent state machine; plans and hashing; `IntentApproval` + admission; plan enforcement; probes with compact audit, fixed interval, and no overlap; ManagedResource; output Secrets | e2e: scripted-LLM web server Intent goes Pending → Ready via `kubectl` in a sandbox, with a verified audit chain |
| M3 | Real LLMs + web server demo | Anthropic and OpenAI-compatible adapters; Clarifying and Discovery phases; `kubectl last` create/answer/plan/approve/deny | `kubectl last "run an nginx web server serving X"` works end to end against a real model |
| M4 | Image builds + registry | `image.install`; rootless BuildKit build pods; bundled Distribution + nginx front proxy + node-local pull proxy; external registry mode; audit-backed digest index; single flight; coordinated GC with sidecar. **Starts with a spike validating the pull proxy on kind, k3s, EKS, and GKE Standard.** | e2e: a command needing a tool outside the base image (e.g. `awscli`) builds once, and later runs and probes reuse the digest with no rebuild |
| M5 | Day 2 | Update stage; drift → remediation; teardown with finalizers; Auto modes and layering; adaptive probe intervals + jitter + floor | e2e: edit, drift, and delete flows in Manual and Auto modes |
| M6 | Allow rules | `CommandRule` / `ClusterCommandRule` + admission; gateway matching (incl. `image.install`); `uncoveredSteps`; `approve --always-allow` with exact/generalized patterns; breadth lint | e2e: a second similar Intent runs without approval because of rules from the first |
| M7 | Suggestions | `CommandRuleSuggestion` + admission; PlanTime, RepeatedApproval, and PeriodicReview triggers; denied-pattern suppression; `kubectl last rules suggestions` | e2e: a denied suggestion is never re-suggested; a narrower one is |

## 21. Out of scope for v1

- MCP as a tool type (an MCP server may still be run *inside* a shell
  command like any other program)
- Native (non-shell) Kubernetes or HTTP tools
- Warm or pooled sandboxes
- Runtime installs (all installs are baked into built images)
- Vulnerability scanning or automatic rebuilds of built images
- Audit group commit (batching entries into segment objects)
- Multi-cluster / hub-and-spoke
- Hard turn, token, or cost caps
- Namespace-scoped credentials and requester-identity credentials
- GCS / Azure immutable-blob audit sinks
- Gateway high availability beyond a single leader
- External anchoring of the audit chain
- Chat (Slack/Teams) and web UI interfaces

## 22. Decision log

| Decision | Choice |
|---|---|
| Tool surface | Shell only, in sandboxes (Kubernetes via `kubectl`, HTTP via `curl`); MCP dropped |
| Execution model | Agentic planning with discovery commands; deterministic plan execution |
| Reconciliation | Probes (commands + CEL); LLM only on drift; adaptive interval (1m → 5m), jitter, 1m floor, no overlap, cluster-wide sandbox cap with priority queue; 2 audit entries per probe run |
| Approval | Auto/Manual per stage (discovery, create, reconcile, delete), layered cluster → namespace → Intent, default Manual |
| Approvers | RBAC on `IntentApproval` |
| Allow rules | Structured argv wildcard patterns; cluster + namespace scope; can cover all stages; created only by humans |
| Rule suggestions | Operator suggests at plan time, on repeated approvals, and periodically; LLM generalizes patterns; denied patterns (and broader ones) are never re-suggested |
| Ambiguity | Clarifying questions (status/spec, interactive in CLI) |
| Updates | Edit the Intent |
| Credentials | Operator-wide Providers, held only by the gateway; Kubernetes via minted ServiceAccount tokens |
| Sandbox | Short-lived pods on digest-pinned images, per-execution egress |
| Tool installation | Baked into images built automatically by the gateway (rootless BuildKit, no credentials); no runtime installs |
| Registry | Bundled Distribution v3 by default (nginx front proxy with per-method auth, node-local pull proxy, gateway-coordinated GC); external registry optional. Chosen over zot for maturity. |
| Tracking | Child `ManagedResource` CRs |
| Outputs | Secret per resource + status |
| Transcripts | In the audit log |
| Audit | Gateway-written, fail-closed, hash-chained, signed, S3 Object Lock compliance; bundled RustFS or external; WORM conformance check in CI and on demand |
| Threat model | Untrusted operator; trusted cluster admins |
| LLM | Pluggable from day one (Anthropic, OpenAI, OpenAI-compatible) |
| Stack | Go + kubebuilder |
| Topology | Single cluster |
| First demo | In-cluster web server |

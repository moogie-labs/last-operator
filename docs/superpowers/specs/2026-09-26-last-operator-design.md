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

An LLM agent turns each request into a concrete, reviewable **plan**.
The plan runs through tools (Kubernetes, HTTP, shell, and MCP servers).
The resources it creates are then continuously reconciled against the
approved state.

Two properties are central to the design:

1. **The agent can think, but it cannot act on its own.** Every side
   effect goes through a separate, trusted **gateway**. The gateway
   holds all credentials, enforces policy and the approved plan, and
   refuses anything else.
2. **Every action is recorded, write-once.** The gateway writes a
   hash-chained, signed audit entry to WORM storage *before* each
   action and refuses the action if the write fails. A rogue operator
   cannot act without leaving a record, and cannot alter or remove
   the record afterwards.

### 1.1 Goals

- Provision and track "anything" reachable via Kubernetes, HTTP APIs,
  CLIs, or MCP servers. No platform is special-cased.
- Serve both self-service developers and sysadmins.
- Configurable **Auto / Manual** approval modes for each lifecycle stage
  (create/update, reconcile, delete). Modes are layered cluster →
  namespace → Intent.
- Full reconciliation with drift detection. The LLM is **not** called
  on every reconcile.
- Pluggable LLM providers from day one.
- A tamper-proof, complete audit trail of operator actions.

### 1.2 Non-goals (v1)

See §21.

## 2. Users and interfaces

**Users:** developers (self-service, usually Manual approval) and
sysadmins (approvers and policy authors; can grant themselves Auto
mode through policy).

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
   NAME         PHASE              RESOURCES   AGE
   team-x-web   AwaitingApproval   3           14s
   ```

2. **`kubectl last` plugin.** It creates the same CRs, and is interactive
   where that helps:

   ```
   kubectl last "run an nginx web server serving team-x-site"   # create + follow
   kubectl last answer team-x-web                              # answer clarifying questions
   kubectl last plan team-x-web                                # show plan + planHash
   kubectl last approve team-x-web                             # create IntentApproval
   kubectl last audit log --intent team-x/team-x-web           # audit timeline
   kubectl last audit verify                                   # verify the WORM chain
   ```

## 3. Architecture and trust boundary

```
 kubectl / kubectl-last ──► Intent / IntentApproval CRs (API server)
                                  │ watch
                    ┌─────────────▼─────────────┐
                    │  Operator (last-system)   │  UNTRUSTED: runs the agent loop
                    │  RBAC: own CRs only       │  no credentials, no Secrets
                    └─────────────┬─────────────┘
                                  │ gRPC (the ONLY way to act)
                    ┌─────────────▼─────────────┐
                    │ Gateway (last-gateway)    │  TRUSTED: holds all credentials,
                    │ policy + plan enforcement │  LLM keys, audit signing key
                    │ audit writer (fail-closed)│
                    └──┬──────┬──────┬──────┬───┘
                       │      │      │      │
                     LLM    MCP /  K8s   Sandbox pods ──► per-execution NetworkPolicy
                    APIs    HTTP   API   (last-sandbox ns, runtime install)
                                  │
                    ┌─────────────▼─────────────┐
                    │ Audit sink (WORM)         │  bundled MinIO (last-audit ns)
                    │ S3 Object Lock, compliance│  or external S3-compatible
                    └───────────────────────────┘
```

**Threat model.** The operator process is untrusted. It runs an LLM
over user-supplied text and tool output, so it can be manipulated through
prompt injection, buggy, or driven by malicious model output. Cluster
admins are trusted (see §17.2 for what that implies).

**Namespaces:**

| Namespace | Contents | Operator RBAC | Gateway RBAC |
|---|---|---|---|
| `last-system` | Operator | none (runs here) | none |
| `last-gateway` | Gateway, credential Secrets, signing key | **none** | full within ns |
| `last-sandbox` | Sandbox pods, per-execution NetworkPolicies | **none** | create/delete Pods, NetworkPolicies, Secrets |
| `last-audit` | Bundled MinIO + root credentials | **none** | **none** (S3 API only) |

## 4. Components

### 4.1 Operator (`cmd/operator`)

A kubebuilder controller manager. It:

- Reconciles `Intent` and `ManagedResource`.
- Runs the agent loop (prompt construction, the planning conversation,
  and parsing the plan) by calling the gateway's `CallLLM`.
- Drives plan execution step by step through the gateway.
- Writes status on its CRs.

**RBAC:** get/list/watch/update/patch on `Intent`, `ManagedResource`
(and create/delete on `ManagedResource`), their status subresources,
`Events`, and `Leases`. get/list/watch on the configuration CRs
(`OperatorConfig`, `IntentPolicy`, `Provider`, `ToolSource`,
`LLMProvider`) and on `IntentApproval`. **No** create on
`IntentApproval`. **No** access to Secrets, Pods, NetworkPolicies, or
any other resource kind. All Kubernetes changes to workloads go
through the gateway's Kubernetes tool.

### 4.2 Gateway (`cmd/gateway`)

A gRPC server and the only component able to cause side effects.

**API (called by the operator):**

| RPC | Purpose |
|---|---|
| `CallLLM` | Send a conversation plus tool schemas to the resolved `LLMProvider` |
| `CallTool` | Invoke a tool (Kubernetes, HTTP, Shell, MCP) during a phase |
| `RunSandbox` | Run a shell step in a sandbox pod |
| `WriteOutputSecret` | Write provisioned outputs to a Secret in the Intent's namespace |
| `BeginPhase` / `EndPhase` | Open/close a phase context (intent, stage, planHash) that later calls are checked against |

**Responsibilities:**

- **Credential custody.** Loads Provider and LLMProvider secrets from
  `last-gateway`. Credentials are injected into outgoing calls and
  sandbox pods and are never returned to the operator.
- **Independent policy resolution.** Reads `OperatorConfig`,
  `IntentPolicy`, `Intent`, and `IntentApproval` directly from the API
  server. It never trusts the operator's statements about modes or
  approvals.
- **Phase and plan enforcement** (§8.3).
- **Audit writing**, fail-closed (§14).
- **Kubernetes tool.** Its own ServiceAccount carries whatever RBAC
  admins grant for provisioning in-cluster workloads. Admins can scope
  it with a ClusterRole; the Helm chart ships a default that covers
  common workload kinds.
- **Sandbox launcher** (§11).
- **MCP client** for all MCP servers (§10.4).
- **LLM provider adapters** (§12).

A single active replica, chosen by leader election, is the only audit
writer.

### 4.3 Sandbox pods

Short-lived pods in `last-sandbox`, launched only by the gateway, for
shell steps and for launched MCP servers. See §11.

### 4.4 Audit sink

WORM object storage via the S3 Object Lock API. The Helm chart bundles
MinIO; external S3-compatible storage is also supported. See §14.

### 4.5 `kubectl-last` (`cmd/kubectl-last`)

A CLI plugin. It creates and edits Intents, handles clarifying questions
interactively, shows plans, creates `IntentApproval`s, and reads and
verifies the audit log (using the shared `internal/audit` package with
read-only S3 credentials).

## 5. API (CRDs)

All CRDs are in `last.moogie-labs.com/v1alpha1`.

| Kind | Scope | Written by |
|---|---|---|
| `Intent` | Namespaced | Users (spec), operator (status) |
| `IntentApproval` | Namespaced | Approvers only (never the operator) |
| `ManagedResource` | Namespaced, owned by an Intent | Operator |
| `IntentPolicy` | Namespaced | Admins |
| `OperatorConfig` | Cluster, singleton named `default` | Admins |
| `Provider` | Cluster | Admins |
| `ToolSource` | Cluster | Admins |
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
    create: Manual               # Auto | Manual  (covers Create and Update stages)
    reconcile: Auto
    delete: Manual
status:
  phase: AwaitingApproval
  stage: Create                  # Create | Update | Remediate | Delete
  observedGeneration: 3
  questions:
    - id: q1
      text: "Should the server be reachable from outside the cluster?"
      options: ["internal only", "public via Ingress"]
  plan:
    hash: "sha256:…"
    summary: "Deployment (2 replicas nginx), Service (ClusterIP), ConfigMap mount"
    steps: [ … see §8.1 … ]
    resources: [ … ManagedResource specs incl. probes … ]
  conditions: [ … ]
  auditChain: "…chainId…"        # where to find this Intent's audit entries
```

Changing `spec.request` or `spec.answers` increments `generation` and
starts an **Update** planning cycle.

### 5.2 IntentApproval

```yaml
kind: IntentApproval
spec:
  intentRef: { name: team-x-web }
  stage: Create                  # must match the Intent's current stage
  planHash: "sha256:…"           # binds the approval to exactly one plan
  approvedBy: "alice@example.com"
```

- A **ValidatingAdmissionPolicy** requires `spec.approvedBy ==
  request.userInfo.username` and makes the object immutable after
  creation.
- Who may approve is decided by RBAC: `create` on `intentapprovals` in
  that namespace.
- The operator's ServiceAccount is denied create, both by RBAC and by
  an explicit clause in the admission policy.
- An approval is valid only for the matching `(intent, stage,
  planHash)`. If the plan changes, a new approval is required.

### 5.3 ManagedResource

A child of an Intent, one per provisioned thing:

```yaml
kind: ManagedResource
spec:
  intentRef: { name: team-x-web }
  description: "nginx Deployment team-x-web"
  identity:                      # how to find it again
    tool: kubernetes
    ref: { apiVersion: apps/v1, kind: Deployment, namespace: team-x, name: team-x-web }
  desired: { … snapshot of the approved desired state … }
  probes: [ … see §9 … ]
  outputsSecretRef: { name: team-x-web-outputs }   # optional
  teardown: [ … plan steps that delete this resource … ]
status:
  observed: { … last probe results … }
  lastProbeTime: …
  conditions:
    - type: InSync      # True / False (drift)
```

### 5.4 Configuration CRs

```yaml
kind: OperatorConfig            # cluster defaults; same fields as IntentPolicy, plus:
spec:
  llmProviderRef: { name: anthropic-default }
  mcpDiscovery:
    registries: ["https://registry.modelcontextprotocol.io"]
  sandbox:
    baseImage: ghcr.io/moogie-labs/last-sandbox-base:<version>
  defaults: { …IntentPolicy spec… }
---
kind: IntentPolicy              # namespaced; can only tighten OperatorConfig.defaults
spec:
  modes:   { create: Manual, reconcile: Manual, delete: Manual }
  allowAutoModes: { create: false, reconcile: false, delete: false }
  toolSources: [kubernetes]     # which ToolSources Intents here may use
  providers: []                 # which Providers Intents here may use
  egress:
    allowed: []                 # CIDR:port or host:port (FQDN needs Cilium) patterns
  probeInterval: 5m
  agentProposedMCP:
    launch:    { allowedSources: [] }   # e.g. "npm:@modelcontextprotocol/*", "ghcr.io/org/*"
    inCluster: { namespaceSelector: null, serviceSelector: null }   # null = none allowed; {} would match everything
    remote:    { allowedHosts: [] }
    trustReadOnlyHints: true    # see §8.2
---
kind: Provider
spec:
  type: aws                     # free-form label shown to the agent
  description: "AWS prod account 1234"
  secretRef: { name: aws-prod } # Secret in last-gateway
  injectAs:                     # how the gateway presents credentials to tools
    env: { AWS_ACCESS_KEY_ID: access_key_id, AWS_SECRET_ACCESS_KEY: secret_access_key }
---
kind: ToolSource
spec:
  type: Kubernetes | HTTP | Shell
  http: { allowedHosts: [...] }          # for HTTP
---
kind: LLMProvider
spec:
  type: anthropic | openai | openai-compatible
  model: "…"
  endpoint: "…"                          # for openai-compatible
  secretRef: { name: anthropic-key }     # Secret in last-gateway
```

## 6. Configuration layering and defaults

Settings resolve as **`OperatorConfig.defaults` → `IntentPolicy` →
`Intent.spec.modes`**. Each layer can only tighten the one above it:

- A mode can move from Auto to Manual, never from Manual to Auto,
  unless the policy layer sets `allowAutoModes.<stage>: true`.
- Lists (tool sources, providers, egress, MCP allowlists) resolve to the
  **intersection** of the layers.

The gateway resolves the effective configuration itself for every
phase. The operator resolves it too, but only for display in status.

**Shipped defaults:**

| Setting | Default |
|---|---|
| All modes | `Manual` |
| ToolSources installed | `Kubernetes` only (`HTTP`, `Shell` are opt-in) |
| Sandbox egress | Deny all |
| Agent-proposed MCP | All three forms available; all allowlists empty (nothing allowed) |
| Audit retention | 365 days, compliance mode |
| Probe interval | 5m |

## 7. Intent lifecycle

```
Pending → Clarifying ⇄ (user answers) → Planning → AwaitingApproval* → Executing → Verifying → Ready
                                                                                     │
  spec edited  ──► Planning (stage=Update)                                           ▼
  probe fails  ──► Drifted → Remediating (stage=Remediate; approval* if reconcile=Manual) → Ready
  deleted      ──► finalizer → Planning (stage=Delete) → AwaitingApproval* → TearingDown → removed
  any failure  ──► Failed (reason in conditions; retried with backoff or on spec edit)

  * skipped when the effective mode for that stage is Auto
```

- **Clarifying.** The agent may ask questions instead of producing a
  plan. They appear in `status.questions`; answers go in
  `spec.answers`. `kubectl last` presents them interactively.
- **Planning.** An agentic conversation. The LLM may make **read-only**
  tool calls (§8.2) to inspect the world, then emits a structured plan.
- **AwaitingApproval.** Waits for a matching `IntentApproval`.
- **Executing.** Runs the plan's steps deterministically (§8.3).
- **Verifying.** Runs every probe of every ManagedResource; all must
  pass. A failure here marks the Intent `Failed` with the probe details,
  so bad probes surface at creation time instead of as false drift
  later.
- **Delete.** A finalizer holds the Intent until teardown completes.
  The teardown plan is built from each ManagedResource's `teardown`
  steps and handed to the agent to review for ordering. It needs
  approval when `delete: Manual`.

## 8. Plans, execution, and enforcement

### 8.1 Plan structure

```yaml
plan:
  hash: "sha256:…"                   # SHA-256 of the canonical JSON of steps + resources
  steps:
    - id: s1
      tool: kubernetes
      op: apply
      mutating: true
      args: { manifest: { … } }
    - id: s2
      tool: mcp
      server: { form: remote, url: "https://mcp.vendor.com/mcp", providerRef: vendor-prod }
      op: create_zone
      mutating: true
      args: { name: "team-x.example.com", account: "${steps.s1.result.metadata.uid}" }
      egress: []
    - id: s3
      tool: shell
      mutating: true
      install: ["pip install awscli==1.33.0"]
      script: "aws ec2 run-instances …"
      providerRef: aws-prod
      egress: ["*.amazonaws.com:443"]
  resources: [ …ManagedResource specs… ]
```

- `args` may reference earlier step results with
  `${steps.<id>.result.<path>}`.
- Canonicalization follows RFC 8785 (JSON Canonicalization Scheme).
- MCP steps pin the server exactly (§10.4). Launched servers are
  pinned to an exact version.

### 8.2 What is allowed in each phase

| Phase | Allowed calls |
|---|---|
| Planning / Clarifying | `CallLLM`; **read-only** tool calls only |
| Executing / Remediating / TearingDown | Exactly the approved plan's steps, in order |
| Verifying / probing | The probes' read-only calls only |

**What counts as read-only:**

- **Kubernetes:** `get` / `list`.
- **HTTP:** `GET` / `HEAD`.
- **MCP:** `tools/list`, `resources/*`, and tools annotated
  `readOnlyHint: true` when `trustReadOnlyHints` is true.
  `readOnlyHint` is declared by the server itself; trusting it is a
  documented policy choice.
- **Shell:** never read-only. It cannot run in these phases.

### 8.3 Execution and enforcement

The operator walks the plan and calls `CallTool` / `RunSandbox` for
each step. For each call, the gateway:

1. Confirms that a phase is open for `(intent, stage, planHash)`.
2. Confirms authorization: either a valid `IntentApproval` exists, or
   the effective mode for the stage is Auto. The gateway reads the
   plan from the Intent's status and checks its hash against
   `planHash`.
3. Confirms the call matches the **next** unexecuted step. The gateway
   resolves `${steps…}` references itself, from its own record of
   earlier results (kept in memory for the phase and also present in
   the audit log), and requires the resolved arguments to match
   exactly.
4. Writes the `tool.call` audit entry, performs the call, and writes the
   `tool.result` entry.

Any mismatch produces a `policy.denied` audit entry and an error to
the operator. If a step fails, the Intent returns to **Planning** with
the failure as context. The new plan has a new hash, so Manual mode
requires a new approval.

**Auto-mode remediation** is limited to re-applying the approved
`desired` snapshot. If the agent's remediation plan contains steps
outside that snapshot, the Intent moves to `AwaitingApproval`
regardless of mode.

**The gateway takes the snapshot from its own records.** It never
trusts `ManagedResource.spec` or `Intent.status`, because the operator
can write both. The gateway uses the **last successfully executed
plan** for the Intent, taken from the audit log: `phase.begin` records
the full plan body and `phase.end` records the outcome. Probe calls
(§9) and teardown steps are checked against that plan in the same way.

## 9. Probes and reconciliation

A probe is a read-only tool call plus a CEL assertion:

```yaml
probes:
  - id: replicas
    call: { tool: kubernetes, op: get, args: { apiVersion: apps/v1, kind: Deployment, namespace: team-x, name: team-x-web } }
    assert: "result.spec.replicas == 2 && result.status.readyReplicas == 2"
```

- The `ManagedResource` controller runs probes every `probeInterval`
  through the gateway, without calling the LLM.
- If any assertion fails, `InSync=False`, the Intent moves to
  `Drifted`, and the LLM builds a remediation plan (§8.3).
- Probe calls must pass the read-only rules in §8.2, and must match a
  probe in the Intent's last executed plan (§8.3). Editing probes on a
  ManagedResource therefore cannot change what the gateway runs. Probes that use
  launched MCP servers relaunch the pinned server each time. That cost
  is accepted in v1.

## 10. Tools

### 10.1 Kubernetes

Operations are `get`, `list`, `apply` (server-side apply), and `delete`.
They run as the gateway's ServiceAccount, which is limited by the RBAC
admins give it. Every `apply` and `delete` is audited as `k8s.mutation`.

### 10.2 HTTP

Arbitrary HTTP requests to hosts on the ToolSource's `allowedHosts` list,
intersected with policy. Credentials come from a Provider named in the
step. Only `GET` and `HEAD` count as read-only.

### 10.3 Shell

Scripts run in sandbox pods (§11). A shell step is always mutating.
Shell steps are only available when the `Shell` ToolSource is installed
and allowed by policy.

### 10.4 MCP (agent-proposed)

MCP servers are **not** registered by admins. The agent finds them and
proposes them in its plan, and policy decides which ones are allowed.
The gateway is the only MCP client.

**Discovery.** The gateway provides a built-in read-only tool,
`mcp_discover`. It searches:

- the registries listed in `OperatorConfig.mcpDiscovery.registries`;
- in-cluster Services labeled `last.moogie-labs.com/mcp: "true"` that
  match the policy's selectors.

**Forms:**

| Form | Plan pins | Connection | Policy gate |
|---|---|---|---|
| `launch` | Package or image **with exact version**, install command, `providerRef`, egress | Gateway launches a sandbox pod running `last-mcp-bridge` (stdio → streamable HTTP; NetworkPolicy allows ingress from the gateway only) | `launch.allowedSources` |
| `inCluster` | Service namespace/name/port/path | Gateway connects via streamable HTTP | `inCluster.namespaceSelector` + `serviceSelector` |
| `remote` | URL, `providerRef` for auth | Gateway connects via streamable HTTP; auth injected from the Provider | `remote.allowedHosts` |

**Inspection during planning.** The agent may call `tools/list` and
read-only tools (§8.2) on candidate servers:

- `inCluster` and `remote` servers must pass policy before the gateway
  connects.
- `launch` servers are inspected in a sandbox with **no credentials**
  and egress limited to the package source.

**Execution.** Server identity is part of the plan and therefore part
of `planHash`. The gateway connects only to the exact server that was
approved.

## 11. Sandbox execution

- **Image.** Every sandbox uses `OperatorConfig.sandbox.baseImage`.
  Tools are installed at runtime by the step's `install` commands, and
  those commands are part of the approved plan.
- **Pod.** Created by the gateway in `last-sandbox`, with a new
  ServiceAccount and no API token mounted
  (`automountServiceAccountToken: false`), non-root, a read-only root
  filesystem except for a writable `emptyDir` work directory, and CPU
  and memory limits.
- **Credentials.** The gateway creates a short-lived Secret holding
  only the credentials of the step's `providerRef`, mounts it, and
  deletes it when the step ends. A `credential.issued` audit entry
  records the Provider and scope, not the values.
- **Egress.** `last-sandbox` has a default-deny NetworkPolicy. For each
  execution the gateway creates a NetworkPolicy selecting only that pod
  (by a unique label) that allows the step's declared egress
  (intersected with policy) plus the package sources its `install`
  commands need. The policy is removed afterwards. Both actions are
  audited (`egress.granted` / `egress.revoked`).
  - **Requirement:** the cluster's CNI must enforce NetworkPolicy.
    Host-name egress rules require Cilium (or equivalent). Without it,
    only CIDR rules are supported.
- **Output.** The gateway streams stdout, stderr, and the exit code into
  the audit log (`sandbox.output`) and returns them to the operator.

## 12. LLM providers

`CallLLM` takes a provider-neutral request (messages, tool schemas,
max tokens). Adapters in the gateway translate it for each provider:
`anthropic`, `openai`, and `openai-compatible` (for local and
self-hosted models). Every request and response is audited
(`llm.request` / `llm.response`), so the audit log also serves as the
complete transcript store.

## 13. Credentials

v1 uses **operator-wide credentials**: one Provider per platform
account, available to any namespace whose policy lists it. They are
held **only** by the gateway. The operator never sees credential
values, and sandboxes receive only the Provider named in the step, for
the duration of that step. Scoping credentials to namespaces is out of
scope for v1 (§21). The `Provider` CR is designed so it can be added
later without breaking changes.

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
  "seq": 1042,
  "chainId": "3f1c…",
  "prevHash": "sha256:…",
  "ts": "2026-09-26T18:04:11.123Z",
  "type": "tool.call",
  "actor": "operator",
  "intent": "team-x/team-x-web",
  "stage": "Create",
  "planHash": "sha256:…",
  "payload": { },
  "hash": "sha256:…",
  "sig": "ed25519:…"
}
```

- `hash` is the SHA-256 of the canonical JSON of every field except
  `hash` and `sig`. `sig` is an Ed25519 signature over `hash`.
- The signing key is a Secret in `last-gateway`, and the public key is
  published in the Helm release notes and a ConfigMap. KMS-backed
  signing is out of scope for v1.
- Writes use `PutObject` with `If-None-Match: *`, and each object gets
  compliance-mode retention.

**Entry types:** `gateway.start`, `checkpoint`, `phase.begin`,
`phase.end`, `llm.request`, `llm.response`, `tool.call`, `tool.result`,
`k8s.mutation`, `sandbox.start`, `sandbox.output`, `credential.issued`,
`egress.granted`, `egress.revoked`, `approval.observed`,
`policy.denied`.

**Redaction:** credential values are never logged. Values written to
output Secrets are logged as SHA-256 hashes. LLM and tool payloads are
logged in full. Operators should treat the audit bucket as sensitive
(§17.3).

### 14.3 Chain continuity

- There is one chain per installation, written by the single active
  gateway replica (lease-based).
- On startup the gateway lists the chain to find its head, verifies the
  last checkpoint and every entry after it, and then writes
  `gateway.start`, linked to the head. If verification fails, the
  gateway refuses to start.
- A signed `checkpoint` entry (containing the current seq and hash) is
  written every 1,000 entries or 10 minutes, whichever comes first, so
  verification can start from any checkpoint.

### 14.4 Storage backends

**Bundled MinIO** (Helm subchart, `last-audit` namespace, enabled by
default):

- An install Job creates the bucket with Object Lock enabled and sets
  default compliance-mode retention.
- The gateway receives a MinIO user whose policy allows only
  `s3:PutObject`, `s3:GetObject`, `s3:ListBucket`, and
  `s3:GetBucketObjectLockConfiguration`. It cannot delete objects,
  change retention, or change lock configuration.
- A separate read-only user is created for `kubectl last audit`.
- MinIO root credentials exist only in `last-audit`. Neither the
  operator nor the gateway has RBAC there.

**External S3-compatible storage:** the admin supplies a bucket that
already has Object Lock in compliance mode, plus credentials limited to
the same permissions. At startup the gateway calls
`GetObjectLockConfiguration` and **refuses to start** if the lock is
missing, not in compliance mode, or has a shorter retention than
configured.

### 14.5 Verification and reading

- `kubectl last audit verify [--from-checkpoint N]` checks signatures,
  hash links, sequence continuity, that each seq has exactly one
  object version, and checkpoint consistency. It exits non-zero on any
  failure and names the first bad seq.
- `kubectl last audit log --intent ns/name [--follow]` prints a
  readable timeline for one Intent.
- The `internal/audit` package is shared by the gateway and the CLI.

### 14.6 Independent cross-check (documented)

The docs describe how to configure Kubernetes API server audit
logging for the operator and gateway ServiceAccounts and ship it to the
same bucket under `k8s-audit/`. This record is produced by the API
server, not the gateway. It is a documented setup step and is not
enforced, because it is cluster-level configuration.

## 15. Outputs

- Non-sensitive outputs (endpoints, IDs) go in `ManagedResource.status`.
- Sensitive outputs (passwords, connection strings) are written by the
  gateway (`WriteOutputSecret`) to a Secret in the Intent's namespace.
  The Secret is owned by the ManagedResource and referenced by
  `outputsSecretRef`.
- The operator can create Secrets only through this RPC, never
  directly.

## 16. Observability

Prometheus metrics from both the operator and the gateway, labeled by
namespace and Intent:

- LLM tokens (in/out) and calls, per provider and model
- Tool calls by tool, outcome, and phase
- Sandbox runs and durations
- Audit writes, audit write failures, and chain seq
- `policy.denied` counts
- Probe runs, failures, and drift events

The CRs emit standard Kubernetes Events for phase transitions.

## 17. Security model

### 17.1 Summary of controls

| Risk | Control |
|---|---|
| Prompt-injected or rogue agent acts without permission | Gateway is the only path to side effects; plan-conformance checks; phase rules |
| Operator forges approvals | `IntentApproval` cannot be created by the operator (RBAC + admission); approvals bind to `planHash` |
| Operator grants itself Auto mode or more tools | Config CRs are read-only to the operator; the gateway resolves policy itself |
| Credential leakage via LLM context | Credentials are never returned to the operator; injected only at the call or sandbox |
| Operator hides its tracks | Fail-closed WORM audit written by the gateway; hash chain + signatures + compliance lock |
| Shell step escapes its scope | Sandbox with no API token, per-execution egress, short-lived per-Provider credentials |
| Malicious MCP server | Policy allowlists per form; exact version pins; approval binds to server identity; launched servers are sandboxed |

### 17.2 Known limitations

- **Cluster admins are trusted.** A cluster admin can redeploy the
  gateway with modified code or change policy. Entries already written
  stay protected by compliance mode. Protecting future entries against
  a malicious admin would require external anchoring, which is out of
  scope.
- **Operator-wide credentials.** Every namespace allowed to use a
  Provider shares its full permissions. Plan approval and enforcement
  are the main mitigation.
- **No hard caps on agent turns or cost.** Runaway agents are visible
  in metrics but not stopped automatically.
- **`readOnlyHint` is self-declared by MCP servers.** Policy can
  disable trusting it, at the cost of less inspection during planning.
- **Arbitrary shell scripts** can do anything their injected
  credentials allow. The audit log records the exact script and
  output, but not the individual API calls the script makes.
  Provider-side audit trails (e.g. CloudTrail) complement it.
- **Probe reliability** depends on the LLM writing good probes. The
  Verifying phase catches probes that fail immediately, but not probes
  that pass trivially.

### 17.3 Audit data sensitivity

The audit log contains full prompts, tool payloads, and outputs, which
may include business data. Read access to the audit bucket should be
restricted to the same level as admin access.

## 18. Testing strategy

- **Unit tests:** plan canonicalization and hashing; policy layering
  and intersection; plan conformance, including `${steps…}` resolution;
  phase rules; CEL probe evaluation.
- **Audit tamper tests** (`internal/audit`): deleted entry, reordered
  entries, modified payload, invalid signature, extra object version,
  forged checkpoint, truncated tail. Each must fail `verify` with the
  right seq.
- **envtest:** Intent and ManagedResource reconcilers against a real
  API server, using a **scripted fake LLM** (deterministic
  request → response fixtures) and a fake gateway.
- **Gateway integration tests:** against MinIO via testcontainers,
  covering fail-closed behavior when MinIO is unreachable, refusal to
  start without compliance lock, `IntentApproval` validation, and
  plan-mismatch denial.
- **Admission tests:** the operator ServiceAccount cannot create
  `IntentApproval`, and `approvedBy` spoofing is rejected.
- **End-to-end tests on kind** (with Calico for NetworkPolicy): full
  Helm install with bundled MinIO, the web server demo driven by the
  scripted LLM, then `audit verify`.
- **Real-LLM runs** are opt-in (not in default CI) and serve as early
  evals of planning quality.

## 19. Repository layout

A single Go module:

```
api/v1alpha1/            CRD types
cmd/operator/            controller manager
cmd/gateway/             gateway server
cmd/kubectl-last/        CLI plugin
cmd/last-mcp-bridge/     stdio → streamable HTTP bridge for launched MCP servers
internal/agent/          agent loop, plan schema, prompts
internal/controller/     Intent, ManagedResource reconcilers
internal/gateway/        RPC server, policy resolution, plan enforcement
internal/gateway/tools/  kubernetes, http, shell, mcp adapters
internal/gateway/llm/    anthropic, openai, openai-compatible adapters
internal/audit/          chain, signing, S3 sink, verifier (shared with CLI)
internal/sandbox/        pod + NetworkPolicy builders
internal/policy/         layered config resolution (shared by operator + gateway)
charts/last-operator/    Helm chart (+ MinIO subchart)
docs/
```

## 20. Milestones

| # | Milestone | Scope | Done when |
|---|---|---|---|
| M0 | Scaffolding | kubebuilder project, all CRD types, Helm skeleton, CI (lint, unit, envtest) | `make test` green in CI; CRDs install on kind |
| M1 | Audit foundation | `internal/audit`; gateway skeleton with fail-closed writes, leader election, startup verification; bundled MinIO with compliance lock; `kubectl last audit verify/log` | Tamper tests pass; gateway refuses to act when MinIO is down |
| M2 | Lifecycle (fake LLM) | Intent state machine; plans and hashing; `IntentApproval` + admission; Kubernetes tool; plan enforcement; probes; ManagedResource; output Secrets | envtest + e2e: scripted-LLM web server Intent goes Pending → Ready with a verified audit chain |
| M3 | Real LLMs + web server demo | Anthropic and OpenAI-compatible adapters; Clarifying phase; `kubectl last` create/answer/plan/approve | `kubectl last "run an nginx web server serving X"` works end to end against a real model |
| M4 | Day 2 | Update stage; drift → remediation; teardown with finalizers; Auto modes and layering | e2e: edit, drift, and delete flows in Manual and Auto modes |
| M5 | HTTP + MCP (remote, in-cluster) | HTTP ToolSource; gateway MCP client; `mcp_discover`; `remote` and `inCluster` forms with policy gates | e2e with an in-cluster test MCP server |
| M6 | Shell + launched MCP | Sandbox pods, runtime install, per-execution egress, credential injection; `last-mcp-bridge`; `launch` form | e2e: shell step and a launched MCP server provisioning a test resource |

## 21. Out of scope for v1

- Multi-cluster / hub-and-spoke
- Building custom sandbox images
- Hard turn, token, or cost caps
- Namespace-scoped credentials and requester-identity credentials
- GCS / Azure immutable-blob audit sinks
- Gateway high availability beyond a single leader
- External anchoring of the audit chain
- Warm pools or caching for sandboxes and launched MCP servers
- Chat (Slack/Teams) and web UI interfaces
- Admin-registered MCP servers (MCP is agent-proposed only)

## 22. Decision log

| Decision | Choice |
|---|---|
| Execution model | Agentic tool-use during planning; deterministic plan execution |
| Reconciliation | Deterministic probes; LLM only on drift |
| Approval | Auto/Manual for each stage, layered cluster → namespace → Intent, default Manual |
| Approvers | RBAC on `IntentApproval` |
| Ambiguity | Clarifying questions (status/spec, interactive in CLI) |
| Updates | Edit the Intent |
| Credentials | Operator-wide, held only by the gateway |
| Tools | Kubernetes, HTTP, Shell (ToolSources) + agent-proposed MCP |
| Sandbox | Short-lived pods, base image + runtime install, per-execution egress |
| Tracking | Child `ManagedResource` CRs |
| Outputs | Secret per resource + status |
| Transcripts | In the audit log |
| Audit | Gateway-written, fail-closed, hash-chained, signed, S3 Object Lock compliance; bundled MinIO or external |
| Threat model | Untrusted operator; trusted cluster admins |
| LLM | Pluggable from day one (Anthropic, OpenAI, OpenAI-compatible) |
| Stack | Go + kubebuilder |
| Topology | Single cluster |
| First demo | In-cluster web server |

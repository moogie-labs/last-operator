# M2a (gateway RPC + sandbox) follow-ups

Deferred findings from the per-task and final whole-branch reviews of
`docs/superpowers/plans/2026-10-03-m2a-gateway-rpc-sandbox.md`. Items fixed
during M2a are not listed.

## Required before M2b wires an RPC to the executor

- **Bind Kubernetes Provider tokens to a per-run object.** Today a minted
  token is valid for the pod's full possible lifetime (Σ timeouts + 60s +
  2m, up to ~6h) even after cleanup, so a value that slips past redaction
  (re-encoded output) stays usable. `TokenRequest.BoundObjectRef` must name
  an object in the ServiceAccount's own namespace (`last-gateway`), so this
  needs a per-run anchor Secret there: the gateway creates it before minting,
  binds the token to its UID, and cleanup/reaper delete it (both must then
  cover `last-gateway` as well as `last-sandbox`). Requires label-scoped
  create/delete on Secrets in `last-gateway` for the gateway — design that
  RBAC carefully, since the signing-key Secret lives there too.
- **Operator RBAC must not reach the sandbox.** No `pods`, `pods/log`, or
  `secrets` in `last-sandbox` (use namespaced Roles, not the scaffolded
  cluster-wide `config/rbac/role.yaml`). Otherwise the operator can read the
  unredacted envelope from pod logs, or a command's
  `/dev/termination-log` message from pod status, before cleanup (spec
  §17.1). Confirm the termination-log channel in the e2e.
- **Refuse side-effecting RPCs when the audit writer is nil** (non-leader
  replicas serve gRPC today; fine for `Ping`).
- **Withhold command output from the RPC caller when the outcome entry was
  not recorded**, so the operator never knows more than the log.
- **Derive `Priority` from the Intent phase** in the gateway; never trust a
  caller-supplied value.
- **Fill `RunRequest.Actor` / `ProbeRunRequest.Actor` from
  `gateway.CallerFrom(ctx)`.**
- **Restrict gRPC ingress (9090) with a `podSelector`** for the operator
  pod once the operator Deployment's labels exist.

- **Configure egress canaries in the gateway.** `ExecutorConfig.EgressCanaries`
  is empty (no gate) unless set. M2b must set at least the gateway's own
  Service `ClusterIP:8080` plus one canary whose backend is not selected by
  any NetworkPolicy (on kindnet the node InternalIP:10250 works; choose per
  CNI), and fail startup if none is configured.

## Address in M2b

- **Egress canary coverage after DNAT.** The gateway rejects grants that
  cover a canary's `IP:port`, but not grants on the canary Service's
  endpoint IPs or pod CIDR. Such a grant makes the gate fail closed after
  30s with a misleading "not enforced" error; resolve canary endpoints
  (as `APIServerEgress` does) and reject those too.
- **Gate failures need a typed marker** so a probe whose gate failed is not
  read as an assertion failure or drift.
- **`DeadlineOverhead` must cover the gate** (≥ `EgressGateTimeout` plus
  image pull); validate it instead of relying on the 60s default.
- E2E: follow `IMAGE_REGISTRY`/`IMAGE_TAG` instead of a hard-coded image;
  creating the target Pod can fail on an immediate rerun while its
  namespace is still Terminating.

- **Shutdown drain:** cancel in-flight runs and wait for cleanup + outcome
  entries before exit; size `terminationGracePeriodSeconds` accordingly (also
  in the foundation follow-ups).
- **Leader-start pass for unterminated runs:** a crash, panic, or failed
  outcome append leaves `command.start`/`probe.start` (or `egress.granted`)
  without its closing entry. At leader start, scan the chain tail for runs
  without an outcome and append one (also covers the lost `egress.revoked`
  case when the policy delete succeeds but the append fails).
- **Reaped runs without a policy leave no audit trace**; consider one entry
  per reaped run (needs a spec entry type).
- **Queue depth and wait-time metrics** for the scheduler (spec §10.1).
- **Egress "intersected with policy"** (spec §11.1): the bound must compare
  parsed `EgressRule` CIDRs, never input strings — `::ffff:0:0/96`
  normalizes to `0.0.0.0/0`.
- **`Gateway.Run` gRPC wiring** (listener, `OperatorIdentity` →
  `Authenticator`) has no unit test; e2e covers it.
- `ctx` already cancelled before `Do` still writes `command.start` (followed
  by a result recording the cancellation); check `ctx.Err()` first.
- `probe.result` can reach 2N MiB for a group of N probes; consider a
  per-entry cap. `failAll` keeps `Passed=true` outcomes when the
  `probe.result` write fails — callers must treat the error as
  authoritative.

## From the final fix-wave re-review

- `expires-at` is stamped from launch start, but the pod wait starts at pod
  creation and cleanup can take up to 30s afterwards: the reap margin must
  exceed ~1m (5m recommended).
- The capped-token check compares the API server's `ExpirationTimestamp` with
  the gateway clock; more than ~1m of skew fails every kubernetes-provider
  run (fails closed — an ops note).
- A whitespace-only `Actor` is accepted; `command.start.command.providers`
  still records the raw (possibly duplicated) list next to the deduplicated
  top-level `providers`.
- Tests: assert all four `expires-at` values are equal and absent from the
  NetworkPolicy pod selector; assert the granted duration in the capped-token
  error; no test for `classifyAudit` on the executor's `egress.revoked`
  append; no probe-path twin of the slot-release test.

## Sandbox runner

- A same-UID child can forge the result envelope (documented in spec §17.2).
- Signal death (OOM, segfault) yields exit code -1 with an empty `Error`.
- UTF-8 sanitising / JSON escaping can exceed `MaxOutput`; no aggregate
  envelope bound against the 64 MiB scanner limit (cap commands per pod).
- No `recover` around `Run`; a failure `Result` has an empty ID; a marshal
  failure exits without an envelope; files are written in map order.
- A successful command with a background child always costs ~2s
  (`WaitDelay`) before the group kill.

## Credentials, egress, pods

- The whole credentials Secret is mounted at `/last/creds`, so env values
  are also readable as files; mount only the kubeconfig/token keys.
- Provider names containing dots can collide in `<name>.env.<X>` keys.
- No CRD bounds on `tokenTTL`; `APIServerHost` is not validated.
- Provider authors can name any ServiceAccount or Secret in `last-gateway`
  — keep Provider create RBAC admin-only, or add a label allow-list.
- Malformed IP/CIDR input returns the misleading
  `ErrHostEgressUnsupported`; `0.0.0.0`/`::` are accepted.
- `APIServerEgress` silently degrades on a missing or portless
  EndpointSlice and ignores dual-stack ClusterIPs.
- Image digests are not validated (config layer); `emptyDir` `sizeLimit` is
  enforced only by eviction; `PATH`/`TMPDIR` depend on the image.
- A managed object without a run label is never reaped (and not logged);
  `deleteRun` counts a NotFound policy as deleted.
- Fail-fast checks only `ContainerStatuses` (no init containers today).

## Probes

- JSON integers ≥ 2^53 become lossy floats (`json.Decoder.UseNumber`).
- Probes needing documents above `MaxJSONNodes` (10,000) must narrow
  output in the command (`jq`, `-o jsonpath`).

## Chart and tooling

- `helm-lint` render checks are substring greps (Service port, binding
  subject, policyTypes are unproven).
- Pod Security Admission `restricted` labels on `last-sandbox` (the sandbox
  pod already complies) and the gateway namespaces.
- `audit/writer.go` and `gateway/server.go` still use the `"gateway"`
  literal instead of `sandbox.GatewayActor`.

## Test polish

- Scheduler: `popLocked` retains the head pointer; parameter `max` shadows
  the builtin; race test hardcodes `i%4`.
- Pod tests: env order across multiple credential vars, resource overlay
  aliasing, exact-6h timeout, negative deadline, spec mount/volume name
  identity.
- Reaper: no ctx-cancel test.
- Gateway: the token-leak log assertion checks only two formats.

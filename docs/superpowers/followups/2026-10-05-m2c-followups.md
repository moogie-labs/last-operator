# M2c (operator lifecycle) follow-ups

Rulings and deferred items from `docs/superpowers/plans/2026-10-05-m2c-operator-lifecycle.md`.

## Rulings made in M2c

- **Pending.** An Intent with no phase is Pending; the first reconcile moves it straight to Planning.
- **Failed is terminal until a spec edit.** Automatic retry with backoff (spec §7) arrives with Auto modes (M5), along with a cap on replans after step failures. Until then every replan needs a fresh human approval.
- **Restarts during Planning** start the conversation over. Reloading it from `QueryAudit` arrives in M3.
- **Restarts during Executing.** A phase the operator lost track of is ended as failed and the Intent becomes `ExecutionInterrupted`. Phases are never resumed, because step results are not persisted outside the gateway.
- **Unavailable during `RunCommand`** is treated as a failed step without replanning: the gateway may or may not have run it.
- **Plan-declared outputs.** `ManagedResourceSpec.outputs` is part of the approved plan, and the gateway writes exactly those values.
- **ManagedResource naming.** ManagedResources are named `<intent>-<index>`. An Intent name longer than 251 characters fails at ManagedResource creation.
- **Probe schedule.** The schedule is kept in memory. After a restart it resumes from the newest `lastProbeTime` among the current plan's ManagedResources. The gateway runs the executed plan's probes. The operator matches the outcomes to ManagedResources by probe ID from each ManagedResource's spec. It considers only the current plan's ManagedResources (`<intent>-<i>` for each plan resource, controlled by the Intent) and reads them uncached. The matching is not cosmetic: each ManagedResource's `InSync` condition drives the Intent between Ready and Drifted. When a new plan executes, `observed`, `lastProbeTime` and `InSync` are cleared on each of its ManagedResources (status subresource, after the spec write), so no earlier plan's outcome carries over.

## Deferred

- **Intent identity (from the M2b follow-ups).** A deleted and recreated Intent with the same name shares the gateway record and approvals, which are keyed by namespace and name. A recreated Intent whose plan is identical to an executed one is refused with "already opened a phase". Fix it with the "approval replay" item: tie approvals and the record to the Intent UID.
- **Stale ManagedResources.** A ManagedResource that an earlier plan created beyond the current plan's resources stays until the M5 Delete stage, keeping the status of its last probe run. That status can be `InSync=False`. The operator ignores it: it is never probed, never updated, and does not count toward Ready or Drifted. A ManagedResource whose index the current plan reuses is rewritten, and its probe status is cleared. The operator has no delete on ManagedResources by design.
- **Deletion.** Deleting an Intent garbage-collects its ManagedResources and their output Secrets without an audit entry. Finalizers and teardown arrive in M5.
- **Long reconciles.** Planning and execution run inside one reconcile, so a long step holds a worker. Four workers per controller suffice for M2. Revisit with real LLMs (M3).
- **Status size.** The plan is stored in `Intent.status` and is capped at 256 KiB of canonical JSON. Large manifests in `files` count toward that cap.
- **Events.** Events are emitted on phase transitions only. Per-step Events could help `kubectl describe`.
- **CRD upgrades (before the first release).** Helm installs the chart's `crds/` only on first install and never upgrades them, so `helm upgrade` leaves old schemas in place. The API server then prunes new fields such as `ManagedResourceSpec.outputs`, which changes the plan hash. The operator now detects this after storing a plan and fails the Intent with "The API server changed the plan when storing it; are the CRDs current?". `hack/e2e.sh` applies the CRDs explicitly. Document the upgrade step for chart users, or move the CRDs into templates, before the first release.
- **nginx preload in e2e.** `hack/e2e.sh` loads `nginx:1.27-alpine` into kind as a single-platform archive (`docker save --platform` plus `kind load image-archive`). On Docker's containerd image store, `kind load docker-image` fails for a pulled multi-platform image, because the saved index names platforms that were never pulled. On Docker older than 28, which has no `save --platform`, the script falls back to `kind load docker-image`.
- **Approvals from before a replan (with "approval replay").** `awaitApproval` accepts any Approve whose subject hash matches the current plan. A replan that produces an identical plan (same hash) is therefore approved by the earlier approval. The gateway then refuses the phase because that subject is already `Used`, and the Intent fails with a confusing message. Consider skipping approvals created before the plan's Planned transition. Fix it together with the approval-replay item.
- **Phases open for another subject (with "Intent identity").** `abandonPhase` and `endFailed` end the phase using the current plan's hash. A phase open for a different subject stays open until the gateway restarts. That happens, for example, when an Intent is deleted and recreated while the operator is down. Fix it together with the Intent-identity item.
- **Resources without probes.** A plan with resources but no probes reaches Ready unverified. Consider requiring at least one probe per resource, as a correction turn (M3).
- **Step stderr in status.** Failed steps' stderr tails (512 B) reach Intent status, Events and logs. Sandbox redaction covers only Provider credentials, on a best-effort basis. Revisit before real secrets flow through steps.
- **Probe metric cardinality.** The ManagedResource controller's probe metric label series (namespace, Intent) are never deleted when an Intent goes away. Fix before the first release.

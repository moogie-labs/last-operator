# M5a (identity, approvals and Delete) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-18-m5a-identity-approvals-delete.md`.

## Result

**The full e2e passed** on the two-node kind cluster `last-m4a` after the final fix wave: `ok test/e2e 1295.8s`, with every test passing.
- **`TestDeleteIntentTearsDownWhatItCreated`:** an executed Intent is deleted, and the model orders the recorded teardown. The test approves the Delete with `kubectl last approve --hash`. The Deployment, Service, ConfigMap, ManagedResources and Intent are then gone. The audit log shows the Delete `phase.begin` with the approved hash and a matching `phase.end` with outcome `succeeded`, and the chain verifies.

**What the reviews found:**
- **Probes stopped 24 hours after approval.** RunProbes re-checked the standing approval, so the new `approvals.maxAge` would have turned off drift detection for every Intent a day after approval, and at once on upgrade. Probes are now authorized by the gateway's own executed record, and a Deny for the live UID still stops them.
- **A succeeded Delete could run again.** While the finalizer was still held, the approval was still valid, so a Delete or Discovery phase could reopen and recreate the record. An in-memory tombstone now refuses every phase for a torn-down UID. It is set when the Delete's `phase.end` is written.
- **Teardown commands with `${steps…}` could never resolve**, which made an Intent undeletable. `plan.Validate` now refuses them.
- **Gateway records leaked** for every Intent deleted without a succeeded Delete. A leader sweep now removes them.
- **The operator's RBAC.** The first Delete implementation granted the operator `delete` on ManagedResources. That was reverted: ownerRef GC removes them once the finalizer goes. The operator's new `patch` on Intents is limited by the `last-intent-operator` admission policy to the teardown finalizer.
- **The predecessor's open phase.** It was dropped with no `phase.end`. The close blocked on a running step, and pending index entries leaked to a recreated Intent.
- **CLI approvals used the wrong stage.** `kubectl last approve` would have approved a Delete plan as Create. `follow` claimed "torn down" for any deletion.

## For you to decide

- **A partially applied Create is released without teardown.** If a Create ran some steps and then failed or was aborted, deleting the Intent releases it at once (D6, "never executed"). The objects those steps made stay behind. A `PartialCreateNotTornDown` warning now says so. The alternative (D7, "nothing a plan created is orphaned silently") is to offer that Create plan's teardown as an approval-required Delete.

## Probes and approvals

- **A stale Deny can turn probes off for good.** This is the most urgent item here.
  - **Why:** `findDeny` counts a Deny of any age, but BeginPhase ignores a Deny older than `maxAge`.
  - **Example:** a plan is denied, then proposed again after `maxAge`, then approved and executed. Its probes are refused from then on, and the Intent stays `Ready`.
  - **Fix:** count only a Deny created after the executed phase began, and amend main spec §5.2.
- **The partial-Create warning misses two cases:**
  - Creates interrupted by a gateway restart (no `stepsRun`);
  - Creates whose first step failed (`stepsRun` 0).

  Also treat any Create `command.result` for the subject as partial.
- **The operator and gateway age approvals by their own clocks** (documented in main spec §5.2).

## Gateway record and tombstone

- **The tombstone is in memory only.** After a restart in the window between a succeeded Delete and the finalizer's removal, the guard is gone. A Delete could then run again under its approval, which relies on teardown being idempotent. The same window can produce a second `phase.end` if the first save failed before a leader change.
- **Interrupted phase.end after a failed save.** A recreated Intent (new UID), arriving while a succeeded Delete's record drop is pending after a failed first save, closes the old phase as interrupted, which writes a second `phase.end`.
- **The `tornDown` map never expires.** It is bounded by the Intents deleted in one leader term.
- **Pending index entries are keyed by Intent name, not UID.** A recreated Intent's entries written before its first BeginPhase fold below the floor and show as truncated. The sweep can also drop them in a narrow window. The `flushPredecessorIndex` comment overstates their ownership. The audit log loses nothing.
- **The record sweep runs inline in the leader loop,** so with many records it delays index flushes and checkpoints. Run it in its own goroutine, or page it.
- **`StateStore.Delete` has no ResourceVersion precondition.** A sweep that decides before a leadership handover could delete a record the new leader just wrote.

## Delete flow

- **A retry annotation changed while a Delete is in flight** fires on a later failure without a new user action.
- **Plans executed on this branch before 9530eae** could hold `${steps…}` in teardown. They would end in `TeardownFailed` and need the finalizer removed by hand.
- **Kustomize `config/` ships no admission policies** (also true of the approval policy), yet its RBAC now grants `patch` on Intents. Mark `config/` unsupported, or add the policies.
- **The `last-intent-operator` policy compares labels, annotations and ownerReferences wholesale.** Operator-owned metadata would need a policy change.

## Tests

- **Gateway:**
  - `retryTeardown`'s other-UID branch;
  - `afterTeardown`'s NotFound and read-error branches;
  - the Create-approval refusal asserts only the code.
- **Agent:**
  - an undecodable `phase.end` is skipped silently;
  - no out-of-order begin/end fixture;
  - truncation on only the `phase.end` scan is untested.
- **Controller envtest:**
  - no specs for deletion during Clarifying, Planning, AwaitingDiscoveryApproval or Verifying, or for AlreadyExists in stage Delete;
  - the "valid Approve despite stale Deny" spec sleeps 2.1 s;
  - the restored API-rejection spec drives `plan()` by hand;
  - the fake gateway's `Unlock`/`defer Lock` pattern is fragile.

## Carried from M4

The M4a, M4b and M4c follow-ups still apply. Before the next real-model demo, run `ANTHROPIC_API_KEY=… go run ./hack/schemaprobe`.

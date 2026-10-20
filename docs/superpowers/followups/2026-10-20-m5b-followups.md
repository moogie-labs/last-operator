# M5b (Update and Remediate) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-19-m5b-update-remediate.md`.

## Result

**The full e2e passed** on the two-node kind cluster `last-m4a` after the final fix wave: `ok test/e2e 1375.9s`, with all 11 tests passing.
- **`TestEditPlansAnUpdate`:** an edit plans an Update, and approving it runs the Update. The Update drops the ConfigMap. Its steps remove the ConfigMap itself, and the gateway deletes `<intent>-1` with one `managedresource.deleted` entry.
- **`TestDriftIsRemediated`:** the Deployment is scaled to 0, which makes the Intent Drifted. A confirming probe run then leads to a Remediate plan with a nonce. Approving it scales the Deployment back to 2 replicas and returns the Intent to Ready. The executed plan's hash is unchanged.

**What the reviews found:**
- **A truncated audit index disabled Update and Remediate.** Once an Intent's index passed 200 entries, or an Intent was recreated under a name whose record was still there, the operator could no longer find the executed plan. Edits then planned a Create and every drift failed. Now a visible executed pair is always found, and index trimming never drops it.
- **An Update that dropped a resource other than the last one failed after its steps ran.** It failed whenever a resource behind the dropped one had an output Secret, because ManagedResources are positional. An output Secret now moves to the ManagedResource that declares it, audited as `output.moved`.
- **A failed ManagedResource deletion blocked every later phase until restart,** Delete included. The next BeginPhase now finishes the pending cleanup itself.
- **A succeeded phase whose record save failed did the same.** Once the operator gave up on it, every later phase was blocked until restart. After a restart the gateway also recorded a second, interrupted `phase.end`. BeginPhase now finishes the save, and at lead the gateway closes such a phase with its logged outcome.
- **A transient audit read after a failed step turned the replan into `ExecutionInterrupted`.** The replan now keeps its stage without reading the log.
- **A Remediate whose drift cleared after a reload fallback stuck in Planning.** It now returns to Ready.
- **Audit verify could report a live checkpoint as bad.** It now lists the checkpoint index before the chain.
- **The e2e audit helper re-read the whole chain on every poll.** It made each run on a long-lived cluster slower than the last, until the suite timed out.

## For you to decide

- **A denied Remediate fails the Intent with `ApprovalDenied`,** and its probes stop. *Answered in M5c:* the retry annotation now retries a Failed Intent in any stage. For a denied Remediate it starts a new Remediate session, with the denial as context and `status.remediations` restarted at 0. A denied plan is never retried automatically, even in Auto mode.
- **A partially applied Create (from M5a) is released without teardown.** This now also covers an aborted Update that ran steps.

## Gateway

- **Unaudited stale deletion.** A Delete BeginPhase drops pending stale deletions whose audit append failed. Write the pending `managedresource.deleted` entries before removing the phase.
- **`closeEnded` doesn't cross-check its phase.begin against the record.** It should refuse unless the stage and subject match, and unless `sha256(plan)` equals the subject.
- **A deletion that succeeded can lose its audit entry.** If the in-memory state is lost after the delete, the ManagedResource is then skipped as terminating and is never audited. Consider writing an intent-to-delete entry first.
- **R6 compares clocks.** It compares the gateway-clock `Began` with the API server's `creationTimestamp`. Ignoring only Denies already older than `approvals.maxAge` at `Began` would close the skew window; it needs a maxAge snapshot in the record.
- **Records written before M5b have no `BeginSeq`.** Their executed pair stays unpinned until the next succeeded Create or Update.
- **The sweep removes a failed Update's extra ManagedResources,** with their Secrets. An MR created during a phase interrupted at lead can also be swept. Sweep entries carry no `AuthorizedBy`.
- **The lead lookup for a logged `phase.end` is capped at 1000 entries.** Past that, the phase closes as interrupted.
- **A persistently refused stale delete** (Forbidden) makes every non-Delete BeginPhase answer Unavailable.

## Operator

- **The confirming RunProbes re-runs on every transient retry of a fresh Remediate session.** Cache the first message per session.
- **`probedPlanHash` scans the whole index on every scheduled run** for a plan with a nonce. Cache it per `status.plan.hash`.
- **A flapping probe never reaches `RemediationFailed`.** If a probe fails on schedule but passes on the confirming re-run, the cap is never hit; the recurring DriftCleared Warnings are the signal.
- **An oversize Remediate plan is refused only at BeginPhase,** after its approval.
- **`endFailed` after EndPhase(failed):** a failing `ensureFinalizer` or status write still fails the Intent as `ExecutionInterrupted`.
- **`FirstMessage` degrades silently** when the executed plan can't be encoded.
- **Executed plans from before M5a whose teardown fails the M5a checks can't be remediated.** This is documented in the README.

## Tests

- **Edit e2e:**
  - check that `web-1` is gone as soon as the Intent is Ready;
  - tie the `managedresource.deleted` seq to the Update phase.
- **Drift e2e:** filter the executed-plan `phase.end` on `succeeded`.
- **Untested:**
  - the UID precondition on stale deletes (the fake client ignores it);
  - the sweep of an empty-UID record;
  - a multibyte stream at the 2 KiB probe limit.

## Carried from M5a

The M5a follow-ups (`docs/superpowers/followups/2026-10-19-m5a-followups.md`) still apply, except for the stale-Deny probe item, which R6 fixed. Before the next real-model demo, run `ANTHROPIC_API_KEY=… go run ./hack/schemaprobe`. The prompt grew in M5b; the schema did not.

# M5c (Auto modes, retries and adaptive probes) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-20-m5c-auto-retries-probes.md`.

## Result

**Every task and the final fix wave passed review.** The final review found no path where something that needs a human approval runs without one.

**The full e2e suite passed on the final code** (0872dd3) on the two-node kind cluster `last-m4a`: `ok test/e2e 2351.5s`, all 14 tests. That is M5's done-criterion: edit, drift and delete, each in Manual and in Auto, with the audit chain verifying.
- **Earlier attempts:** the first run on the fix wave timed out at the 50-minute limit, because the harness read a long audit chain one entry at a time (2930db4 now reads it concurrently). A second run was lost when the session restarted.
- **Cleanup before the passing run:** the lost run had left OperatorConfig allowing Auto for every key, plus test namespaces, Providers and LLMProviders from earlier runs. These were cleaned up first. After the passing run the cluster had no Intents and empty OperatorConfig defaults.

**What the reviews found:**
- **An Auto Remediate could rewire `${steps…}` references.** An approved step could then run with the output of a different approved command. Remediate steps are now compared by an extended key that includes what each reference points at.
- **The per-step approval re-check skipped the approver refusals.** It accepted an Approve signed by the operator or by a gateway ServiceAccount. One shared `checkApprover` now serves BeginPhase and every re-check.
- **A human Deny on an Auto plan was retried automatically a minute later.** Denials are now matchable (`gatewayv1.Denied`), the Intent fails with `ApprovalDenied`, and it is never retried automatically.
- **Retries reset at a Remediate's Ready,** so the drift → cap → retry loop never backed off. Retries now reset only on a Create or Update Ready, on a passing scheduled probe run, or on a spec edit.
- **Two smaller fixes:**
  - The replan count leaked across drift episodes.
  - A passed probe run that could not be recorded doubled the interval instead of resetting it.

## For you to decide

- **D11 window cap.** `status.remediations` counts consecutive remediations, and a fully passing scheduled probe run resets it, together with `status.retries`. So a resource that something reverts more slowly than one scheduled run never reaches the cap, its retries never back off, and each drift is remediated at once. Should the cap count remediations within a time window instead (for example, 3 per hour), and should the backoff then survive a passing run?

## Gateway

- **Record why Auto applied.** An Auto `phase.begin` doesn't record which OperatorConfig, IntentPolicy or `spec.modes` made the stage Auto. Add their names and resourceVersions to the payload so an auditor can reconstruct the decision.
- **An interrupted phase is stamped with BeginPhase's authorization.** Its `phase.end`, written after a gateway restart, carries that authorization, not the Approve the phase fell back to; the fallback lives only in memory.
- **Refusal wording after a fallback.** After an Auto phase falls back to an Approve, refusals still say "auto authorization withdrawn" or "Auto-authorized".
- **Several Approves for one subject:** only the first by name is checked. This fails safe.
- **The in-memory tombstone (M5a) gap is slightly wider.** During a gateway restart between a succeeded Delete and the finalizer's removal, an Auto discovery command can reopen a phase without approval.

## Operator

- **A plan sent to run by an Approve that has since gone** retries BeginPhase every 5s. The operator should re-check `decision` there and go back to waiting.
  - **Cause:** the M5 requeue can't tell approval-view lag from a deleted or expired Approve.
- **A Deny of an Auto discovery command reaches the model as `DiscoveryFailed`,** not `DiscoveryDenied` with the reason.
- **Retry feedback includes user-facing hint text.**
- **Failed Intents don't watch policies,** so a stage switched to Auto starts its backoff only at the next reconcile.
- **A retried Remediate that finds no drift** keeps `remediations` at 1 until a passing scheduled run.
- **A replan that races a spec edit** still counts as a replan and carries the old step failure.
- **An Event for the discovery NeedsApproval fallback.**
- **`plan.Validate` doesn't refuse `${steps}` references to unknown or later steps.** They fail at run time instead.

## CLI

- **`follow` misses a second consecutive Auto discovery command** within one AwaitingDiscoveryApproval phase.

## Tests and tooling

- **Missing tests:**
  - an Auto Delete outside the permutation reaching AwaitingApproval through `tearDown`;
  - DriftCleared keeping retries;
  - `checkAutoAudit` asserting the initial Create's `auto/create`.
- **The gauge-deletion test** re-creates the series it checks.
- **e2e audit reads:** scope them by key range, and pass the object version to `Get` to halve the requests.
- **`bin/golangci-lint` can't load Go 1.27.3 export data** on this machine (a "typecheck" error). Rebuild it with the current toolchain.

## Carried

The M5a and M5b follow-ups still apply. The schema probe was run on 2026-10-30 against the final code: `ACCEPTED (2157-byte schema); the answer parses as a turn`, so the real-model demo is unblocked.

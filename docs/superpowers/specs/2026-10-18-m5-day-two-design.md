# M5: Day 2 (Update, Remediate, Delete, Auto modes) — Design

**Status:** drafted on 2026-10-18 from the main spec, the code, and the follow-ups earmarked "before M5". The user asked to proceed without a round of questions. Calls made without them are marked *(my call)*.

**Parent spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (the "main spec"): §5.1–§5.3, §6, §7, §8.3 and §10. The amendments in §9 below carry these decisions back into it.

**Inputs:**
- the M2b follow-ups (approval replay; closing interrupted phases at lead);
- the M2c follow-ups (Intent identity, stale ManagedResources, deletion, plan checks on teardown);
- the M3a follow-ups (record lifetime; the replay window);
- the M3b follow-ups (workers held by long turns).

## 1. Goal and done-criterion

M5 lets an Intent live past its creation:
- **Edit:** editing `spec.request` or `spec.answers` plans and runs an **Update**.
- **Drift:** a failing probe leads to a **Remediate** plan.
- **Delete:** deleting the Intent tears down what it created, behind a finalizer.
- **Modes:** each stage runs in Manual or Auto mode, resolved through the policy layers.

Probes follow an adaptive interval with jitter and a floor.

**Done when:** e2e on kind covers the edit, drift and delete flows, each in Manual and in Auto mode, and `kubectl last audit verify` passes (main spec §20).

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Approvals bind to the Intent's UID and expire.** `IntentApproval.spec.intentUID` is required. The gateway rejects an approval whose UID differs from the Intent's, or that is older than `approvals.maxAge` (OperatorConfig, default 24h). `kubectl last approve` fills the UID in. | This closes the replay follow-up: a recreated Intent with the same name can't use old approvals, and an approval can't come back after its subject leaves the record's "used" list. |
| D2 | **The gateway's record is bound to the Intent's UID.** The record ConfigMap keeps its name, a hash of the Intent's namespace and name, and stores the UID; a record for another UID is reset on load. A successful Delete phase deletes the record and the in-memory phase. The output Secrets are not the gateway's to delete: they belong to their ManagedResources and go with them by ownerReference garbage collection. | This closes the identity and record-lifetime follow-ups. A recreated Intent starts clean. |
| D3 | **The executed plan is read from the audit log.** The operator finds the last executed Create or Update plan through `QueryAudit`: the latest `phase.begin` whose `phase.end` outcome is `succeeded`, excluding Discovery and Remediate. It never relies on `status.plan`, which may hold an unexecuted plan. | Remediation context, teardown candidates and Auto checks need the plan that really ran. The gateway already treats its own record as authoritative (main spec §8.3). |
| D4 | **Update is Create with history.** A spec edit after an executed plan plans in stage `Update`. The model gets the executed plan and the new request. The Update plan's resources replace the ManagedResources: those it drops are deleted after the Update succeeds, and its own steps must remove the real objects. A successful Update becomes the executed plan. Before any plan has run, an edit keeps stage `Create`. | It keeps one plan shape. Dropped resources are removed by the steps the approver reviewed, not by a hidden teardown. |
| D5 | **Remediate repairs, it does not redesign.** On drift, the operator asks the model for a Remediate plan, giving it the executed plan and the failing probes' results. The gateway requires the Remediate plan's `resources` to equal the executed plan's, canonically. A Remediate phase never becomes the executed plan. Changing what is managed needs a spec edit (an Update). | It makes "Auto-mode remediation limited to re-running commands from that plan" (main spec §8.3) checkable, and keeps probes and teardown stable. |
| D6 | **Delete reviews the recorded teardown.** On deletion, the operator gives the model the executed plan's teardown commands to order, and the model returns a Delete plan with no resources. Manual mode needs approval. In Auto mode, the gateway allows it only if its steps are a permutation of the executed plan's teardown commands. An Intent that never executed is released at once. | This matches main spec §7 and §8.3. |
| D7 | **A finalizer holds every executed Intent.** The finalizer is `last.moogie-labs.com/teardown`, added before the first Create phase begins. It is removed after a successful Delete phase, or at once when nothing ever executed. If teardown fails, the Intent stays `Failed` (reason `TeardownFailed`). The user retries by annotating `last.moogie-labs.com/retry`, or gives up by removing the finalizer by hand, which is documented and goes unaudited. | Nothing a plan created is orphaned silently. The manual escape hatch is necessary because the gateway cannot force anything. |
| D8 | **Modes resolve through the layers.** For each stage key (discovery, create, which covers Update, reconcile and delete), the layers resolve `OperatorConfig.spec.defaults.modes`, then each `IntentPolicy` in the namespace, then `Intent.spec.modes`. A layer may tighten Auto to Manual. A layer may loosen Manual to Auto only if every policy layer has `allowAutoModes.<key>: true`. The gateway resolves the effective mode for every phase. The operator resolves it only to show in `status.effectiveModes` and to skip waiting for approval. | This is main spec §6 as written. The gateway stays the authority. |
| D9 | **Auto authorizes without approval, within limits:** Create and Update run any valid plan; Discovery runs any valid command; Remediate requires every step to equal a step of the executed plan (compared as written, before `${steps…}` resolution); Delete requires D6's permutation. A Remediate or Delete plan outside those limits needs approval even in Auto mode. | This is main spec §8.3. |
| D10 | **Replans are capped, and Auto retries back off.** A planning trigger allows at most 3 replans after failed steps (`status.replans`), then the Intent fails. In Auto mode, a `Failed` Intent retries with exponential backoff (1m, 2m, 4m, … up to 1h), counted in `status.retries`. In Manual mode it waits for a spec edit or the retry annotation *(my call)*. | Without the cap, an Auto Intent could loop forever against a model that keeps failing. The backoff stops a broken Auto Intent from hammering the LLM and the cluster. |
| D11 | **Remediation backs off too.** Drift that persists after a successful Remediate counts toward `status.remediations`. After 3 consecutive remediations, the Intent fails with `RemediationFailed`, and Auto retry (D10) takes over. | It stops tight drift and remediation loops, for example a resource something else keeps reverting. |
| D12 | **Probes use adaptive intervals** (main spec §10.1), replacing M2's fixed grid. The interval starts at `probes.fastInterval` (default 1m) after execution, remediation or drift. It doubles after each fully passing run, up to `probes.interval` (default 5m). It never drops below `OperatorConfig.probes.minInterval` (floor, default 1m). Each run is offset by ±10% jitter. The current interval lives in the ManagedResource controller's memory, keyed by Intent, and restarts at `fastInterval` after an operator restart *(my call)*. | Fast feedback after a change, quiet steady state, and no lockstep across Intents. |
| D13 | **M5 is three plans:** M5a, identity, approvals and Delete; M5b, Update and Remediate; M5c, Auto modes, retries, adaptive probes and the full e2e matrix. | M5a fixes the safety follow-ups that every later stage relies on. M5b adds the two remaining Manual stages. M5c turns on Auto across all of them. Each plan delivers testable software. |

## 3. M5a — identity, approvals, Delete

### 3.1 Approvals
- **API:** `IntentApprovalSpec.intentUID` (string, required, immutable, like the rest of the spec). `OperatorConfigSpec.approvals.maxAge` (Duration, default 24h, from 1m to 30 days).
- **The gateway's `findApproval`** also requires `a.Spec.IntentUID == intent.UID` and `now - a.CreationTimestamp <= maxAge`. Non-matching approvals are ignored, just as non-matching subjects are today. A Deny with a matching UID still wins.
- **The CLI** sets `intentUID` from the Intent it read when it showed the plan (`approve`, `deny`, and the interactive prompt).
- **The existing admission policy** (`approvedBy == username`) is unchanged.

### 3.2 Record keyed by UID
- **Name:** the record ConfigMap keeps its name, `last-intent-` and a hash of the Intent's namespace and name. The record stores the Intent's UID (`intentUID`), and `Load` resets a record that names another UID, keeping only its index floor, so a recreated Intent starts clean.
- **Old Intents:** a record written before M5a has no UID. `Load` adopts it for the Intent that next uses it, so its used-subject history and executed plan carry over; D1's UID binding of approvals still blocks replays.
- **At lead,** the gateway lists every record and closes each open phase from another epoch as interrupted. This closes the M2b follow-up.

### 3.3 The executed plan from the audit log
- **Audit index:** the gateway keeps indexing `phase.begin` and `phase.end` (M3).
- **`agent.ExecutedPlan(ctx, auditor, ref) (*plan.Body, hash string, found bool, err error)`** reads the latest succeeded Create or Update `phase.begin` payload. It pairs begin and end by `beginSeq` and verifies that the plan's canonical hash equals the entry's subject.
- **A truncated index** without the entry means the executed plan is unknown. Delete then falls back to the ManagedResources' `teardown` with a warning, and needs approval even in Auto mode; Remediate fails with `PlanningFailed`.

### 3.4 Delete
- **Finalizer (D7):** added in `startPlanning` before the first Create phase begins.
- **On a deletion timestamp:**
  - with no executed plan, remove the finalizer. When the audit log holds a Create `phase.end` that did not succeed but records `stepsRun > 0`, the release also emits a Warning event (`PartialCreateNotTornDown`) naming the plan and the steps run, since what those steps created is not torn down;
  - otherwise go to Planning with stage `Delete`.
- **Delete planning:** the model gets the teardown commands (from D3, in resource order) and returns a plan whose steps order them, with `resources: []`.
- **Operator checks:** before recording, the operator verifies that the steps are a permutation of the teardown commands. If they aren't, the model gets a correction (the M4b identical-replan mechanism).
- **Running it:** AwaitingApproval (stage Delete), then Executing, which shows phase `TearingDown`.
- **On success:**
  - the gateway's `EndPhase(Delete, succeeded)` deletes the record (D2);
  - the operator removes the finalizer. The Intent is then deleted, and ownerReference garbage collection removes its ManagedResources and, through them, their output Secrets. The operator itself never deletes a ManagedResource (main spec §4.1);
  - a refusal of a later Delete phase call because the gateway already tore this Intent UID down (`gatewayv1.MsgTornDown`) also counts as success: the answer to the ending EndPhase was lost.
- **On failure:** `Failed` / `TeardownFailed`. The `last.moogie-labs.com/retry` annotation, with any value change, replans the Delete.
- **Gateway:** BeginPhase accepts stage `Delete`. The subject is the plan in `status.plan` (as for Create), and `resources` must be empty. Authorization in M5a is approval only; Auto arrives in M5c.
- **Plan checks:** `plan.Validate` and `agent.Parse` apply the argv-xor-script rule to teardown commands (M2c follow-up). They also refuse a teardown command whose argv or script holds a `${steps.` reference: a Delete phase has no Create step results to resolve it from, so the Intent could never be torn down. The model gets the refusal as a correction.

### 3.5 CLI
- `follow` shows `TearingDown`.
- `approve` and `deny` carry the UID.
- `kubectl last` shows which stage is being approved.

## 4. M5b — Update and Remediate

### 4.1 Update
- **Starting an Update:** a generation change on an Intent with an executed plan, in phase Ready, Drifted or Failed, starts planning in stage `Update`.
- **The first message** includes the executed plan (steps, resources, and probes summarised) and the new request and answers.
- **Approval** is for stage Update.
- **Gateway:** BeginPhase accepts stage `Update`, treated like Create (same record limits). A successful Update becomes the executed plan.
- **At EndPhase(Update, succeeded) (R1):** the gateway deletes the ManagedResources the new plan dropped: those the Intent controls whose names (`<intent>-<i>`) are not among the new plan's. It does this, not the operator after Verifying, because the operator has no delete on ManagedResources: deleting one garbage-collects its output Secret, which main spec §4.1 forbids the operator doing unaudited. The gateway audits each deletion as `managedresource.deleted`. A failed delete keeps the phase, and the operator's EndPhase retry finishes it; if the operator moved on instead (say, to the Intent's Delete), the next BeginPhase finishes it, answering Unavailable while a delete still fails. A gateway restart loses that phase, so the leader's record sweep, at lead and then hourly, also deletes the ManagedResources an Intent controls beyond its executed plan's resources, when the Intent is live with the record's UID and no phase is open in the record or in memory. Their real objects were removed by the plan's steps, which the approver saw (D4).

### 4.2 Remediate
- **Starting:** on `Drifted` (InSync false on any ManagedResource), the operator goes to Planning, stage `Remediate`.
- **The first message** includes the executed plan, the failing probes, their outputs (truncated as in probe results) and the assertion that failed.
- **Confirming the drift (R5).** The session's first message is built after a confirming `RunProbes`, with the outcomes recorded on the ManagedResources. If every probe passes, the Intent returns to Ready with a Warning `DriftCleared` event: no plan, no approval prompt, and the remediation is not counted. Otherwise the failing outcomes, truncated, go into the first message.
- **The plan (R2).** A Remediate's model turn answers `"resources": []` and may not ask questions. The operator copies the executed plan's resources in before recording, so the model need not repeat them and their outputs verbatim. The gateway checks that they equal the executed plan's, canonically (D5), and refuses a Remediate whose resources differ even when the operator sent them. `plan.ValidateRemediate` replaces `plan.Validate` for Remediate, because the copied outputs name the executed plan's steps, not the remediation's.
- **A nonce per remediation (R3).** `plan.Body` and `Plan` gain `nonce` (32 lowercase hex, left out of the hash when empty). The operator sets a fresh one when it records a Remediate plan. The gateway requires a nonce on Remediate and refuses one on every other stage. Remediate stays run-once, so each remediation needs its own approval: an identical repair for a recurring drift is a new subject, as a discovery command is.
- **Gateway:**
  - BeginPhase accepts stage `Remediate`, and requires the plan's `resources` to equal the executed plan's, canonically;
  - EndPhase(Remediate) does not replace the executed plan;
  - RunProbes keeps following the executed plan.
- **Afterwards:** Verifying runs, then Ready. `status.remediations` counts consecutive remediations and is reset by a fully passing probe run (D11).

### 4.3 Edits during Remediate or Delete
- **A spec edit while Remediating** abandons the remediation, ending its phase as aborted if one is open, and starts an Update.
- **A deletion** overrides everything: the Intent aborts any open phase and starts Delete.

## 5. M5c — Auto modes, retries, adaptive probes

### 5.1 Effective modes
- **Policy:** `policy.Resolve` gains `Modes(intentSpecModes) EffectiveModes`, implementing D8. A missing value at a layer inherits the layer above, and the shipped default is Manual.
- **The gateway** computes the effective mode in BeginPhase and RunCommand (re-checked per step, as approvals are), and audits `authorizedBy: "mode:Auto"` (the existing `Auth` field) instead of an approval name.
- **D9's limits** are enforced in BeginPhase.

### 5.2 Operator
- `status.effectiveModes` is shown, and `kubectl last` displays it.
- **In an Auto stage** the operator skips AwaitingApproval and calls BeginPhase directly.
- **If BeginPhase refuses** with PermissionDenied ("needs approval": a Remediate or Delete plan outside D9's limits), the operator falls back to AwaitingApproval.
- **Auto Discovery** runs commands without approval.
- **Retries and replans:** D10 and D11 apply. `status.retries` and `status.replans` reset on a successful execution, and on a generation change for replans.

### 5.3 Adaptive probes
- **The ManagedResource controller** replaces `schedule.go`'s fixed grid with D12.
- **State** is kept per Intent in memory:
  - **interval:** starts at `fastInterval`, doubles on an all-pass run up to `interval`, and resets on any failure, execution or remediation;
  - **next due time:** `last start + interval ± 10%`.
- **Floor:** `minInterval` bounds `fastInterval` and `interval` from below.
- **Overlap:** still none, and skips are still counted.
- **Metrics:** the current interval and skips per Intent, by label.

## 6. Errors

| Condition | Result |
|---|---|
| An approval for a different UID, or older than `maxAge` | Ignored, as if absent, so the Intent keeps waiting. A Deny for the right UID still denies. |
| A Remediate plan whose resources differ from the executed plan's | The operator sends a correction. The gateway refuses with `policy.denied` and FailedPrecondition. |
| An Auto Remediate or Delete plan outside D9's limits | The gateway refuses with PermissionDenied ("needs approval"), and the operator moves to AwaitingApproval. |
| Teardown fails | `Failed` / `TeardownFailed`, with the finalizer kept. Retry with the annotation, or with Auto backoff in Auto delete mode. |
| No executed plan is found for Remediate | `Failed` / `PlanningFailed`. |
| No executed plan is found for Delete | Fall back to the ManagedResources' teardown, with approval always required. |
| More than 3 replans, or more than 3 consecutive remediations | `Failed` (`PlanningFailed` or `RemediationFailed`), then Auto backoff. |

## 7. Security

- **Approvals are bound** to the UID and expire (D1).
- **Auto is bounded by policy** (`allowAutoModes`) and by D9's limits. Auto Remediate re-runs only commands the approver of the executed plan saw. Auto Delete runs only the recorded teardown commands.
- **The operator still writes only status.** Every authority decision stays in the gateway. The executed plan, teardown commands and modes come from the gateway's audit-backed record and the policy CRDs, never from `status`.
- **The finalizer can be removed by hand.** Anyone with `update` on `intents` can do it. Doing so abandons teardown. The ManagedResources and their output Secrets go by ownerReference garbage collection once the Intent is gone. The gateway's record goes in the leader's record sweep, which runs at lead and then hourly: it deletes a record with no open phase when its Intent is gone or a recreated Intent of another UID holds the name. This is a documented operational choice, recorded in §17.2.

## 8. Testing

**Unit and envtest:**
- approval UID and maxAge filtering;
- the record keyed by UID, and closing open phases at lead;
- `ExecutedPlan` from audit entries, including truncation;
- each stage's gateway rules (Update replaces the executed plan, Remediate keeps it and requires equal resources, Delete deletes the record and outputs);
- D9's limits;
- mode resolution across the layers;
- the controller flows for edit, drift and delete, in Manual and Auto;
- the retry, replan and remediation caps and backoff;
- the adaptive interval with a fake clock.

**e2e on kind:** a matrix of edit, drift (the test edits the Deployment to break a probe) and delete, each in Manual (approved through `kubectl last`) and Auto (policy allows it), with a scripted model. The audit chain must verify.

## 9. Main-spec amendments

- **§5.1:** `status.effectiveModes`, `status.replans`, `status.retries`, `status.remediations`.
- **§5.2:** `intentUID`, and the age limit.
- **§5.6:** `approvals.maxAge`.
- **§7:**
  - edits plan in stage Update once a plan has executed;
  - drift plans a Remediate;
  - the Delete flow, the finalizer, the retry annotation, and `TeardownFailed`;
  - the replan cap and Auto backoff;
  - the M2 "Failures" paragraph's "arrives with Auto modes (M5)" now describes the behaviour.
- **§8.3:**
  - the Remediate resources-equal rule, and that Remediate doesn't replace the executed plan;
  - the Delete permutation rule;
  - Auto's limits;
  - approvals' UID and age;
  - the record keyed by UID and deleted after a Delete.
- **§10.1:** the "Fixed interval (M2)" paragraph is replaced by the adaptive scheme now in force. Note that the interval resets after an operator restart.
- **§17.2:** removing the finalizer by hand.

## 10. Left to the builder

- The UID-hash form of the record name.
- The exact first-message wording for Update, Remediate and Delete.
- Event reasons.
- The metric names.
- The order of tasks within each plan.

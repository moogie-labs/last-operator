# M5b: Update and Remediate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- Editing an executed Intent's spec plans, approves and runs an **Update**, which becomes the executed plan.
- Drift plans, approves and runs a **Remediate** that repairs what the executed plan manages without changing it.

**Architecture:**
- **The gateway** opens Update and Remediate phases:
  - **Update** is a Create that needs an executed plan, and it replaces that plan.
  - **Remediate** needs an executed plan, must carry that plan's resources unchanged, has a fresh nonce in every subject, and never replaces the executed plan.
  - After a succeeded Update, the gateway deletes the Intent's ManagedResources the new plan no longer has, and audits each deletion.
- **The operator:**
  - picks the stage from the audit log (`agent.ExecutedPlan`);
  - starts an Update on a spec edit after execution;
  - on drift, re-runs the probes, then plans a Remediate from their results.

  It keeps the session's stage across discovery commands, and counts consecutive remediations.

**Tech Stack:** Go 1.26, controller-runtime, kubebuilder markers, Ginkgo envtest, kind e2e.

**Spec:** `docs/superpowers/specs/2026-10-18-m5-day-two-design.md`: D4, D5, D11, §4, the M5b rows of §6, and §8. Main spec: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §5.1, §7, §8.3 and §10. Follow-ups: `docs/superpowers/followups/2026-10-19-m5a-followups.md`.

## Global Constraints

- **Commits and files:**
  - never add a commit attribution trailer (such as `Co-authored-by:`); never commit `.superpowers/`;
  - never edit generated files (`config/crd/bases`, `config/rbac/role.yaml`, `zz_generated.*`, `PROJECT`);
  - after changing `api/`, run `make manifests generate helm-crds`;
  - after Go changes, run `make lint-fix`, `make lint` (0 issues) and `make test`;
  - after chart changes, run `make helm-lint`.
- **Stages:** `Update` and `Remediate` (the existing `lastv1alpha1.StageUpdate` and `StageRemediate`). Approvals are stage-bound, as today.
- **Phases and reasons:**
  - `PhaseRemediating` while a Remediate phase runs, mirroring `PhaseTearingDown`;
  - Failed reason `RemediationFailed`;
  - Warning event reason `DriftCleared` when the confirming probe run finds no drift.
- **`status.remediations`** is an `int32`, JSON `remediations,omitempty`. It counts consecutive Remediate plans recorded for drift. At 3, the next drift fails the Intent with `RemediationFailed` instead of planning. A scheduled probe run in which every probe passes, finishing after the Intent last became Ready, resets it to 0 (D11).
- **Rulings made while planning,** which refine the design:
  - **R1. Stale ManagedResources are deleted by the gateway, at EndPhase(Update, succeeded).** Design §4.1 says the operator deletes them after Verifying. But the operator has no `delete` on ManagedResources: main spec §4.1 forbids it, because deleting one garbage-collects its output Secret unaudited. ManagedResource names are positional (`<intent>-<i>`), so "stale" means a ManagedResource controlled by the Intent whose name is not among the new plan's names. Each deletion writes audit entry `managedresource.deleted` with payload `{"managedResource": <name>, "uid": <uid>}`.
  - **R2. A Remediate's model turn answers `"resources": []`, and the operator copies the executed plan's resources in before recording.** The gateway then checks that they equal the executed plan's resources canonically (D5). This spares the model from copying the resources and their outputs verbatim. `plan.ValidateRemediate` replaces `plan.Validate` for Remediate, because the copied outputs reference the executed plan's steps, not the remediation's.
  - **R3. Every Remediate subject carries a nonce.** `plan.Body` and `lastv1alpha1.Plan` gain `Nonce string` (JSON `nonce,omitempty`, 32 lowercase hex characters). The operator sets it when recording a Remediate plan, using `agent.NewNonce()`.
    - The gateway requires a nonce on Remediate and refuses one on every other stage, with InvalidArgument through `d.deny`.
    - Run-once stays for Remediate, so each remediation needs its own approval. An identical repair for a recurring drift gets a new subject, as discovery commands do.
  - **R4. `status.stage` holds the session's stage (Create, Update or Remediate) while a discovery command is pending.** It no longer switches to `Discovery`. Phase `AwaitingDiscoveryApproval` and `status.pendingCommand` mark the discovery. `planStage` returns Update, Remediate or Delete as stored, and Create otherwise, so legacy `Discovery` and empty values mean Create.
  - **R5. Remediation starts with a confirming `RunProbes`, run when the session's first message is built.** If every probe passes, the Intent goes back to Ready with a `DriftCleared` event, and no plan is made or counted. Otherwise the failing outcomes, truncated, go into the first message.
  - **R6. The stale-Deny follow-up is fixed here,** because drift detection feeds Remediate. `rec.Executed` gains `Began` (RFC 3339, the executed phase's BeginPhase time). RunProbes' `findDeny` counts only Denies created at or after `Began`. A record without `Began` counts every Deny, as today.
- **The executed plan** comes from the audit log (`agent.ExecutedPlan`), never from `status`. Update and Remediate need it; when it is not found (never executed, or a truncated index), an edit plans Create and drift fails the Intent with `PlanningFailed` ("no executed plan found to remediate").

## Review Focus

1. **A spec edit while a remediation is in flight** (stage Remediate, in Planning, AwaitingDiscoveryApproval, AwaitingApproval or Remediating) abandons it: an open phase is ended as failed with reason "Aborted: the spec changed", and an Update is planned. Task 5 tests each phase.
2. **Drift that clears before the remediation is planned** returns the Intent to Ready with no plan, no approval prompt and no count. Task 5 tests it.
3. **An Update that drops a resource** leaves no ManagedResource behind. Its output Secret then goes by garbage collection, and probes stop covering it. Tasks 2 and 6 test it.
4. **A Remediate answer that names resources, or asks questions,** gets a correction, never an approval prompt for a resource-changing remediation. The gateway refuses a Remediate whose resources differ, even when the operator sends one. Tasks 1 and 5 test it.
5. **The same drift twice:** the second remediation, even with identical steps, has a new subject, and the first approval does not authorize it. Tasks 1 and 5 test it.

---

### Task 1: Gateway Update and Remediate stages

**Files:**
- Modify: `internal/plan/plan.go` (`Body.Nonce`, `FromStatus`, `ValidateRemediate`, `SameResources`)
- Modify: `api/v1alpha1/intent_types.go` (`Plan.Nonce`), then run `make manifests generate helm-crds`
- Modify: `internal/gateway/phases.go`, `internal/gateway/state.go` (`ExecutedPlan.Began`), `internal/gateway/probes.go` (`findDeny`, and a shared decoder for the executed body)
- Tests: `internal/plan/plan_test.go`, `internal/gateway/update_test.go` (new), `internal/gateway/probes_test.go`

**Interfaces:**
- Produces:
  - `plan.Body{Steps, Resources, Nonce}`;
  - `func ValidateRemediate(b, executed Body) error`, which checks the steps as `Validate` does, requires `SameResources(b.Resources, executed.Resources)`, and requires a 32-hex-character nonce;
  - `func SameResources(a, b []lastv1alpha1.ManagedResourceSpec) (bool, error)`, comparing canonical JSON with `IntentRef` cleared;
  - the gateway accepts BeginPhase for Update and Remediate.

- [ ] **Step 1: Write the failing tests**
  - **Plan:**
    - `TestHashIncludesTheNonce`;
    - `TestValidateRemediateRequiresEqualResourcesAndANonce`;
    - `TestSameResourcesIgnoresIntentRef`.
  - **Gateway:**
    - `TestUpdateNeedsAnExecutedPlan`: FailedPrecondition, audited as `policy.denied`.
    - `TestASucceededUpdateReplacesTheExecutedPlan`: RunProbes then follows the new plan's hash.
    - `TestRemediateNeedsTheExecutedResources`: different resources give FailedPrecondition.
    - `TestASucceededRemediateKeepsTheExecutedPlan`.
    - `TestRemediateNeedsANonceAndOtherStagesRefuseOne`.
    - `TestTheSameRemediationTwiceNeedsTwoApprovals` (Review Focus 5): run-once refuses the reused subject, and a new nonce with a new approval runs.
    - `TestUpdateAndRemediateRespectTheRecordLimit`: recordFits applies to Update.
    - `TestAnIntentBeingDeletedOpensNoUpdateOrRemediate`.
    - `TestRunProbesIgnoresADenyOlderThanTheExecutedPhase` (R6): a Deny created before `Began` is ignored, and one created after still stops probes.
- [ ] **Step 2: Run `go test ./internal/plan/ ./internal/gateway/`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Add Update and Remediate to `openableStages`.
  - Update and Remediate load the record before validating. Both need `rec.Executed` ("intent has no executed plan to update" / "… to remediate").
  - Remediate validates with `ValidateRemediate` against the decoded executed body.
  - In EndPhase, exclude Remediate from the `rec.Executed` replacement (phases.go, the `p.outcome == outcomeSucceeded && p.stage != …` condition).
  - Store `Began`: the phase records `g.clock()` at BeginPhase.
- [ ] **Step 4: Run `go test -race ./internal/gateway/ ./internal/plan/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): open Update and Remediate phases"`

---

### Task 2: The gateway removes stale ManagedResources after an Update

**Files:**
- Modify: `api/v1alpha1/managedresource_types.go`: add `func ManagedResourceName(intent string, i int) string`, and use it from `internal/controller/managedresources.go`.
- Modify:
  - `internal/audit/entry.go`: `TypeManagedResourceDeleted = "managedresource.deleted"`.
  - `internal/gateway/phases.go`: EndPhase(Update, succeeded).
- Modify the chart:
  - `charts/last-operator/templates/gateway.yaml`, ClusterRole `last-gateway-lifecycle`: add a rule giving `delete` on `managedresources` only.
  - The `Makefile` `helm-lint` target: grep that the gateway has that verb and the operator ClusterRole still does not.
- Modify the main spec §4 (the gateway's RBAC) and §15 (output Secrets go with their ManagedResource).
- Tests: `internal/gateway/update_test.go`

**Interfaces:**
- Consumes: Task 1's Update stage.
- Produces: once EndPhase(Update, succeeded) has answered, no ManagedResource controlled by the Intent remains outside `ManagedResourceName(intent, 0..n-1)`.

- [ ] **Step 1: Write the failing tests**
  - `TestASucceededUpdateDeletesStaleManagedResources`. The Intent owns `web-0`, `web-1` and `web-2`, and the Update plan has 1 resource. After EndPhase, `web-1` and `web-2` are gone, with one `managedresource.deleted` entry each. An MR controlled by another object is untouched.
  - `TestAFailedUpdateDeletesNothing`.
  - `TestStaleManagedResourceDeletionIsRetried`. The first delete fails, and EndPhase returns Unavailable with the phase kept in memory. The operator's retry deletes the MR, writes no second `phase.end`, and writes exactly one `managedresource.deleted` per MR. A NotFound counts as done, with no entry.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run Stale\|FailedUpdate`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Delete after the record save, under the Intent lock. Reuse EndPhase's existing retry path for a phase whose `phase.end` is written (`endSeq != 0`). List MRs with the uncached `g.crd`, filtered to those whose controller ownerRef UID is the Intent's UID. Write the audit entry after each successful delete.
- [ ] **Step 4: Run `go test -race ./internal/gateway/ && make lint && make helm-lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): delete the ManagedResources an Update dropped, and audit each"`

---

### Task 3: Prompts and audit reads for Update and Remediate

**Files:**
- Modify: `internal/agent/prompt.go`, `internal/agent/audit.go`
- Tests: `internal/agent/prompt_test.go`, `internal/agent/audit_test.go`

**Interfaces:**
- Produces:
  - **The Update first message.** `agent.Request` gains `Executed *plan.Body`. When it is set, `FirstMessage` adds a section after the request:
    - "This Intent already executed this plan:", followed by the plan's JSON (steps, then resources with probes and teardown);
    - the instruction: "Plan an Update: steps that take the cluster from what that plan created to what the request now asks, and resources that list everything the Intent manages afterwards. Your steps must remove the real objects of any resource you drop."
  - **The Remediate first message.** `type Remediation struct { Namespace, Name, Text string; Executed plan.Body; Failures []ProbeFailure; Env Environment; Feedback string }` and `type ProbeFailure struct { Resource, ProbeID, Assert string; Result *gatewayv1.CommandResult; Error string }`. `func RemediateMessage(r Remediation) (string, error)` writes:
    - the executed plan;
    - each failing probe: its assertion, exit code, and stdout/stderr, each cut to 2 KiB with the existing `head`/`writeStream` truncation marks;
    - the environment;
    - the instruction: "Plan a Remediate: steps that bring what the executed plan manages back to its desired state. Answer with \"resources\": []; what is managed does not change. You may run discovery commands; you may not ask questions."
  - **The system prompt** gains one sentence: stages Update and Remediate exist, and a Remediate answer always has `"resources": []`.
  - **`func PhaseSucceeded(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, stage lastv1alpha1.Stage, subject string) (bool, error)`.** It pairs a `phase.begin` of that stage and subject with a `phase.end` whose outcome is `succeeded`. A truncated index returns false with a nil error.

- [ ] **Step 1: Write the failing tests**
  - `TestFirstMessageForAnUpdateShowsTheExecutedPlan`.
  - `TestRemediateMessageTruncatesProbeOutput`: 10 KiB of stdout gives at most 2 KiB plus the truncation mark.
  - `TestRemediateMessageNeverAsksForResources`.
  - `TestPhaseSucceededPairsBeginAndEnd`: succeeded gives true; failed gives false; another subject gives false; truncated gives false with a nil error.
- [ ] **Step 2: Run `go test ./internal/agent/`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Reuse `scan`, as `ExecutedPlan` does.
- [ ] **Step 4: Run `go test ./internal/agent/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(agent): first messages for Update and Remediate, and PhaseSucceeded"`

---

### Task 4: The operator plans Updates

**Files:**
- Modify:
  - `internal/controller/intent_controller.go`: in the dispatcher, Ready and Drifted with `generation != observedGeneration` go to `startPlanning`.
  - `internal/controller/intent_planning.go`:
    - `startPlanning` picks the stage;
    - `planStage` follows R4;
    - `awaitApproval` restarts on a generation change for Update as for Create;
    - `record` keeps `status.stage` across a discovery command (R4).
  - `internal/controller/intent_discovery.go`: send stage Discovery to the gateway from the phase, not from `status.stage`. Grep every read of `StageDiscovery` against `status.stage`.
  - `internal/controller/sessions.go`: `newConversation` sets `Request.Executed` for stage Update.
  - `internal/controller/fakegateway_test.go`: Update, Executed replacement, and the stale MR deletion of Task 2.
- Tests: `internal/controller/intent_update_test.go` (new)

**Interfaces:**
- Consumes:
  - `agent.ExecutedPlan` (M5a);
  - `agent.Request.Executed` (Task 3);
  - the gateway Update stage (Tasks 1 and 2).
- Produces: `startPlanning` sets `status.stage` to Update when `ExecutedPlan` finds a plan, and to Create otherwise. An audit read error requeues and never guesses.

- [ ] **Step 1: Write the failing envtest specs**
  - "plans an Update when the spec is edited after a plan executed", from Ready, from Drifted, and from Failed after execution. The first message holds the executed plan.
  - "keeps stage Create for an edit before any plan executed".
  - "runs an approved Update and makes it the executed plan". The approval is for stage Update. Verifying passes, and the Intent is Ready. A stale ManagedResource is gone through the fake gateway.
  - "replans an Update after a failed step, still in stage Update".
  - "keeps stage Update across a discovery command" (R4). `status.stage` stays Update while the phase is AwaitingDiscoveryApproval. The command's approval is for stage Discovery, and the plan that follows is for stage Update.
  - "an Approve for stage Create does not run an Update plan".
- [ ] **Step 2: Run `make test`, focused with `-ginkgo.focus Update`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): plan and run an Update when an executed Intent's spec changes"`

---

### Task 5: The operator remediates drift

**Files:**
- Modify: `api/v1alpha1/intent_types.go` (`status.remediations`), then run `make manifests generate helm-crds`.
- Create: `internal/controller/intent_remediate.go`.
- Modify:
  - `intent_verify.go`:
    - `followProbes` starts a Remediate on Drifted, and resets `remediations` per the Global Constraints;
    - `verify` compares a Remediate's probe run with the executed plan's hash from `agent.ExecutedPlan`, not with `status.plan.hash`.
  - `intent_controller.go`: the dispatcher sends `PhaseRemediating` to `execute`.
  - `intent_planning.go`:
    - a Remediate check in `agent.Next`, which refuses questions and non-empty resources with a correction naming the problem;
    - `rerunCheck` is skipped for Remediate;
    - `record` copies the executed resources in, sets the nonce, and computes the hash (R2, R3);
    - `awaitApproval` sends Remediate to `PhaseRemediating`.
  - `intent_execution.go`:
    - Remediate skips `ensureManagedResources` and `writeOutputs`;
    - `finishPhase` uses `agent.PhaseSucceeded` for Remediate when EndPhase answers FailedPrecondition.
  - `sessions.go`: `newConversation` for Remediate runs the confirming `RunProbes` (R5), then builds `RemediateMessage`.
  - `intent_delete.go`: `abortOpenPhase` treats `PhaseRemediating` like `PhaseExecuting`, so a deletion during a remediation aborts it (design §4.3).
  - `fakegateway_test.go`: Remediate, nonce, the resources-equal check, and scripted probe outcomes.
- Tests: `internal/controller/intent_remediate_test.go` (new)

**Interfaces:**
- Consumes:
  - `agent.RemediateMessage` and `agent.PhaseSucceeded` (Task 3);
  - `plan.SameResources` and `Body.Nonce` (Task 1);
  - the R4 `planStage` (Task 4).
- Produces: the drift lifecycle Drifted → Planning (stage Remediate) → AwaitingApproval → Remediating → Verifying → Ready.

- [ ] **Step 1: Write the failing envtest specs**
  - "remediates drift through an approved Remediate plan". The first message holds the failing probe's assertion and output. The recorded plan carries the executed resources and a nonce. The approval is for stage Remediate. The Intent goes Remediating, Verifying, then Ready, and `remediations` is 1.
  - "returns to Ready when the confirming probe run passes" (Review Focus 2): a `DriftCleared` event, no plan, and `remediations` unchanged.
  - "corrects a Remediate answer with resources or questions" (Review Focus 4). After `MaxAttempts`, the result is `PlanningFailed`.
  - "fails with PlanningFailed when no executed plan is found".
  - "fails with RemediationFailed at the fourth consecutive drift", then "resets remediations after a fully passing scheduled probe run".
  - "abandons a remediation on a spec edit and plans an Update", in each phase listed in Review Focus 1.
  - "a deletion during Remediating aborts the phase and starts Delete".
  - "the same drift twice gets two subjects and two approvals" (Review Focus 5).
  - "a gateway restart mid-Remediate fails the Intent as interrupted unless the audit log shows the phase succeeded". This uses `PhaseSucceeded`.
- [ ] **Step 2: Run `make test`, focused with `-ginkgo.focus Remediat|drift`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): remediate drift through an approved Remediate plan"`

---

### Task 6: CLI, e2e and docs

**Files:**
- Modify:
  - `internal/cli/decide.go`: add Remediate to `approvablePlanStages`.
  - `internal/cli/follow.go`: print Remediating, and the stage in the prompt.
- Create: `test/e2e/day2_e2e_test.go`, modelled on `test/e2e/delete_e2e_test.go`, which swaps the scripted-model ConfigMap per stage.
- Modify the main spec:
  - §5.1: `status.remediations`, and `Plan.nonce`;
  - §7: Update, Remediate, edits during a remediation, `RemediationFailed`;
  - §8.3: Update and Remediate rules, R1–R3, R6;
  - §10.
- Modify:
  - the design doc §4.1 and §4.2, to record R1, R2, R3 and R5;
  - the README lifecycle section.
- Tests: `internal/cli/*_test.go`

- [ ] **Step 1: Write the CLI tests.**
  - `TestApproveARemediatePlan`: the approval carries stage Remediate and the UID.
  - `TestFollowShowsRemediating`.
- [ ] **Step 2: Write the e2e tests.**
  - **`TestEditPlansAnUpdate`:**
    1. The nginx Intent reaches Ready with 2 resources (Deployment and ConfigMap).
    2. Edit `spec.request`. The Update plan keeps the Deployment, drops the ConfigMap, and its steps delete the ConfigMap.
    3. Approve it with `kubectl last approve --hash`.
    4. Expected: the ConfigMap object is gone, `<intent>-1` is gone, the Intent is Ready, and the audit holds an Update `phase.end` (succeeded) and one `managedresource.deleted`.
  - **`TestDriftIsRemediated`:**
    1. The Intent reaches Ready.
    2. The test scales the Deployment to 0, so a `readyReplicas` probe fails.
    3. The Intent goes Drifted, then the Remediate plan comes (`kubectl scale … --replicas=2`).
    4. Approve it.
    5. Expected: the Intent is Ready, the Deployment has 2 ready replicas, and the audit holds a Remediate `phase.end` (succeeded) whose subject differs from the executed plan's.

  In both tests the audit chain must verify. Set the e2e OperatorConfig probe interval to its floor, so drift is seen within about 1 minute.
- [ ] **Step 3: Run `go test ./internal/cli/` and `make test && make lint`, then the full e2e on the existing cluster:** `KIND_CLUSTER=last-m4a hack/e2e.sh > "$SCRATCH/m5b-e2e.log" 2>&1`. Expected: `ok … test/e2e`, with every test passing.
- [ ] **Step 4: Commit.** `git commit -m "test(e2e): edit and drift flows through Update and Remediate"`

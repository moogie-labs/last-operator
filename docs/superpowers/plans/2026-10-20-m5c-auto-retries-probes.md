# M5c: Auto Modes, Retries and Adaptive Probes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- Stages run without approval where the policy layers allow Auto, within the D9 limits.
- Failed Intents retry with backoff, and replans are capped.
- Probes run on an adaptive, jittered interval.

**Architecture:**
- **Policy:** a new `policy.Modes` resolves the effective mode for each stage key across OperatorConfig, IntentPolicies and the Intent.
- **Gateway:** it resolves modes for every phase. In an Auto stage it authorizes BeginPhase without an approval, but only within D9's limits; it re-checks per step and audits `{kind: auto}`. Outside the limits it answers "needs approval".
- **Operator:**
  - it shows `status.effectiveModes` and skips waiting in Auto;
  - it falls back to waiting for approval when the gateway asks for one;
  - it caps replans and retries failed Intents (with backoff in Auto, through the retry annotation in Manual).
- **ManagedResource controller:** it replaces the fixed probe grid with D12's adaptive interval.

**Tech Stack:** Go 1.26, controller-runtime, kubebuilder markers, Ginkgo envtest, kind e2e.

**Spec:** `docs/superpowers/specs/2026-10-18-m5-day-two-design.md`: D8, D9, D10, D11 (Auto retry), D12, §5, the Auto rows of §6, §8, and §9. Main spec: `docs/superpowers/specs/2026-09-26-last-operator-design.md`, §5.1, §6, §7, §8.3 and §10.1. Follow-ups: `docs/superpowers/followups/2026-10-19-m5a-followups.md` and `2026-10-20-m5b-followups.md`.

## Global Constraints

- **Commits and files:**
  - never add a commit attribution trailer (such as `Co-authored-by:`), and never commit `.superpowers/`;
  - never edit generated files;
  - after changing `api/`, run `make manifests generate helm-crds`;
  - after Go changes, run `make lint-fix`, `make lint` (0 issues) and `make test`;
  - after chart changes, run `make helm-lint`.
- **Stage keys (D8):**
  - `discovery` → Discovery;
  - `create` → Create and Update;
  - `reconcile` → Remediate;
  - `delete` → Delete.

  The shipped default for every key is `Manual`.
- **Rulings made while planning,** which refine the design:
  - **R1. Mode resolution is order-independent.** For a key:
    - `allowed` holds when every policy layer has `allowAutoModes.<key>: true`. The layers are `OperatorConfig.spec.defaults` and every IntentPolicy in the namespace.
    - `policyMode` is Manual if any IntentPolicy sets Manual. Otherwise it is Auto if the defaults or any IntentPolicy set Auto. Otherwise it is Manual.
    - The effective mode is Auto only when `allowed` holds and either the Intent sets Auto, or the Intent sets nothing and `policyMode` is Auto. An Intent that sets Manual always gets Manual.
    - A Manual set by any IntentPolicy is final: the Intent cannot loosen it, even when every layer allows Auto. The Intent's Auto can loosen only the defaults layer's Manual (main spec §6: each layer only tightens the one above it). *(Amended during Task 1's review.)*
  - **R2. Auto authorization in the gateway.** In BeginPhase, when the stage's effective mode is Auto and the plan is within D9's limits:
    - the phase is authorized with `audit.Authorization{Kind: "auto", Name: "<key>"}`, and no `approval.observed` is written;
    - a Deny for the subject (UID-bound, not stale) still refuses, as today;
    - outside the limits, the normal approval lookup runs; with no approval, the refusal is PermissionDenied with exactly `gatewayv1.MsgNeedsApproval`. `gatewayv1.NeedsApproval(err) bool` matches the code and the message, as `TornDown` does.
  - **R3. D9 limits.** Commands are compared by canonical JSON with `id` cleared, as written before `${steps…}` resolution (the M5a `commandKey` rule).
    - Create, Update and Discovery: any valid plan or command.
    - Remediate: every step equals some step of `rec.Executed`.
    - Delete: the steps are a multiset permutation of `rec.Executed`'s teardown commands, in resource order. With no `rec.Executed`, Delete needs approval.

    The gateway moves the operator's `commandKey` into `plan.CommandKey(lastv1alpha1.Command) (string, error)` and both sides use it.
  - **R4. RunCommand re-checks per step.** For a phase authorized by Auto, it requires the effective mode to still be Auto. If it is not, it falls back to the approval re-check, and refuses the step if no approval stands.
  - **R5. Retries.**
    - **Who retries:** a Failed Intent whose failed stage's key is Auto retries after `min(1m·2^retries, 1h)`, counted from the Ready condition's `lastTransitionTime`. `status.retries` increments in the retry's status write. `ApprovalDenied` is never retried automatically.
    - **Manual and annotation:** in Manual, and in Auto at any time, a change to the `last.moogie-labs.com/retry` annotation retries any Failed Intent at once. This generalizes M5a's Delete-only annotation and answers the M5b denied-Remediate follow-up.
    - **What a retry does:** it restarts the failed stage's trigger. Create and Update restart through `startSession(stage, failure)`. Remediate restarts through `startRemediate`, with `remediations` reset to 0. Delete restarts through `beginDelete`.
    - **Resets:** `status.retries` resets to 0 when an execution succeeds (the Intent reaches Ready) and on a generation change.
  - **R6. Replans (D10).** `status.replans` increments in `endFailed`'s replan write. At 3, the next failed step fails the Intent with `PlanningFailed` and the message "The plan failed 3 times; last failure: <reason>" instead of replanning. It resets on a generation change, on a retry and on a successful execution.
  - **R7. Adaptive probes (D12).**
    - `policy.ProbeIntervals(op, policies) (fast, steady time.Duration)`:
      - `fast` is the shortest `fastInterval` set (default 1m);
      - `steady` is the shortest `interval` set (default 5m);
      - both are at least the floor `probes.minInterval` (default 1m), and `fast` is at most `steady`.
    - The controller keeps `{interval, planHash, next}` per Intent in memory:
      - the interval starts at `fast`, doubles after a run where every probe passed, up to `steady`, and resets to `fast` on any failure or when `status.plan.hash` changes;
      - `next` is the run's start plus `interval × (1 + j)`, where `j` is uniform in [−0.1, 0.1] from an injectable source;
      - after an operator restart, the interval restarts at `fast`.
    - Metrics: a gauge `last_probe_interval_seconds{namespace,intent}`; the existing skip counter is kept.
- **New status fields** (`IntentStatus`):
  - `effectiveModes *StageModes` (JSON `effectiveModes,omitempty`);
  - `replans int32` and `retries int32` (`omitempty`).

## Review Focus

1. **A policy edit that turns Auto off during an Auto phase.** The next step needs an approval and is refused without one. Task 2 tests it.
2. **An Auto Remediate with a step outside the executed plan, or an Auto Delete that is not a permutation of the teardown commands.** It waits for approval and never runs unapproved. Tasks 2 and 3 test it.
3. **An Intent that asks for Auto when a policy layer does not allow it** gets Manual, and `status.effectiveModes` shows Manual. Tasks 1 and 3 test it.
4. **A broken Auto Intent does not hammer the cluster.** Its retries come at 1m, 2m, 4m and so on, up to 1h, and are counted. A denied plan is never retried automatically. Task 4 tests it.
5. **A Deny for the subject stops an Auto phase,** both at BeginPhase and when RunCommand re-checks each step. Task 2 tests it.

---

### Task 1: Mode resolution and status fields

**Files:**
- Modify: `api/v1alpha1/intent_types.go` (`EffectiveModes`, `Replans`, `Retries`). Then run `make manifests generate helm-crds`.
- Create: `internal/policy/modes.go`. Tests go in `internal/policy/modes_test.go`.

**Interfaces:**
- Produces:
  - `type ModeKey string` with constants `KeyDiscovery`, `KeyCreate`, `KeyReconcile` and `KeyDelete`;
  - `func KeyFor(stage lastv1alpha1.Stage) ModeKey`;
  - `type EffectiveModes struct{ Discovery, Create, Reconcile, Delete lastv1alpha1.Mode }` with `func (m EffectiveModes) For(stage lastv1alpha1.Stage) lastv1alpha1.Mode` and `func (m EffectiveModes) Status() *lastv1alpha1.StageModes`;
  - `func Modes(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy, intent *lastv1alpha1.StageModes) EffectiveModes`, implementing R1.

- [ ] **Step 1: Write the failing tests.**
  - `TestModesDefaultToManual`.
  - `TestAutoNeedsEveryLayerToAllowIt`: defaults allow, but one of two IntentPolicies does not, so the result is Manual even though the Intent sets Auto.
  - `TestAnyPolicyManualWins`.
  - `TestTheIntentCanLoosenOnlyWhenAllowed`.
  - `TestAnIntentManualAlwaysWins`.
  - `TestUpdateUsesTheCreateKey`.
  - `TestModesIgnorePolicyOrder`: the same result whichever way the policies are permuted.
- [ ] **Step 2: Run `go test ./internal/policy/`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./internal/policy/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(policy): resolve effective modes across the policy layers"`

---

### Task 2: The gateway authorizes Auto stages within D9's limits

**Files:**
- Modify:
  - `internal/gateway/phases.go` (BeginPhase authorization);
  - `internal/gateway/commands.go` (`recheckApproval` becomes a re-check of the phase's authorization);
  - `internal/plan/plan.go` (`CommandKey`, moved from `internal/controller/intent_delete.go`, which then uses it);
  - `internal/gatewayapi/gatewayv1/refusals.go` (`MsgNeedsApproval`, `NeedsApproval`).
- Tests: `internal/gateway/auto_test.go` (new).

**Interfaces:**
- Consumes: `policy.Modes` and `KeyFor` (Task 1).
- Produces:
  - Auto phases audited as `{kind: auto, name: <key>}`;
  - `gatewayv1.NeedsApproval(err) bool`;
  - `plan.CommandKey`.

- [ ] **Step 1: Write the failing tests.**
  - `TestAutoCreateRunsWithoutApproval`: `phase.begin`'s `authorizedBy` is `{auto, create}`, and no `approval.observed` is written.
  - `TestAutoDiscoveryRunsWithoutApproval`.
  - `TestAutoRemediateWithinTheExecutedStepsRuns` and `TestAutoRemediateWithANewStepNeedsApproval`: the second gets PermissionDenied with `MsgNeedsApproval`, and its approval then authorizes it.
  - `TestAutoDeletePermutationRuns`, `TestAutoDeleteOutsideTheTeardownNeedsApproval` and `TestAutoDeleteWithoutAnExecutedPlanNeedsApproval`.
  - `TestADenyStopsAnAutoPhase`, at BeginPhase and at RunCommand (Review Focus 5).
  - `TestTurningAutoOffMidPhaseNeedsAnApproval` (Review Focus 1): the IntentPolicy is edited between steps, so the next RunCommand is refused, and the step is allowed once an Approve exists.
  - `TestManualIsUnchanged`: no approval means FailedPrecondition, as today.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run Auto\|Deny\|Manual`.** Expected: FAIL.
- [ ] **Step 3: Implement R2–R4.** The gateway resolves the Intent's modes from the same OperatorConfig and IntentPolicy reads that `effectivePolicy` uses. Update main spec §8.3 for the Auto limits and the `auto` authorization.
- [ ] **Step 4: Run `go test -race ./internal/gateway/ ./internal/plan/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): authorize Auto stages within D9's limits, and re-check each step"`

---

### Task 3: The operator runs Auto stages

**Files:**
- Modify:
  - `internal/controller/intent_planning.go`: `record` goes straight to the running phase in Auto.
  - `internal/controller/intent_discovery.go`: an Auto discovery command runs without waiting.
  - `internal/controller/intent_execution.go`: on `NeedsApproval`, move to AwaitingApproval (or AwaitingDiscoveryApproval), with the message "The gateway needs an approval for this plan".
  - `internal/controller/status.go`, or the place that refreshes status: write `status.effectiveModes` from `policy.Modes`, in the status write that starts each session and in each phase move that records a plan.
  - `internal/controller/fakegateway_test.go`: mirror R2 and R3, and add the policy objects the fake reads.
- Tests: `internal/controller/intent_auto_test.go` (new).

**Interfaces:**
- Consumes: Tasks 1 and 2.
- Produces: the Auto flow for every stage. The phase sequences are unchanged except that the AwaitingApproval and AwaitingDiscoveryApproval waits are skipped.

- [ ] **Step 1: Write the failing envtest specs.**
  - "runs an Auto Create without an approval".
  - "runs Auto discovery commands without an approval".
  - "plans and runs an Auto Update on an edit".
  - "remediates drift in Auto when the repair re-runs executed steps".
  - "waits for an approval when an Auto Remediate adds a step" (Review Focus 2).
  - "tears down in Auto when the Delete is a permutation".
  - "shows Manual in status.effectiveModes when a layer does not allow Auto" (Review Focus 3).
  - "a Manual stage still waits for an approval".
- [ ] **Step 2: Run `make test`, focused with `-ginkgo.focus Auto|effectiveModes`.** Expected: FAIL.
- [ ] **Step 3: Implement.** The operator's mode is a hint: it skips the wait only when its own resolution says Auto, and the gateway still decides.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): run Auto stages without waiting, and wait when the gateway needs an approval"`

---

### Task 4: Replan cap, retries and backoff

**Files:**
- Modify:
  - `internal/controller/intent_controller.go`: the Failed branch handles the retry annotation and Auto backoff.
  - `internal/controller/intent_execution.go`: `endFailed` applies the replan cap.
  - `internal/controller/intent_remediate.go`: a retry of a remediation resets `remediations`.
  - `internal/controller/intent_delete.go`: the Delete retry goes through the shared retry path.
  - `internal/controller/intent_verify.go`: reaching Ready resets `retries` and `replans`.
  - `internal/cli/follow.go`: the failure hint names the retry annotation for every stage.
- Tests: `internal/controller/intent_retry_test.go` (new), using the injectable `Now`.

**Interfaces:**
- Consumes: `policy.Modes` (Task 1). The existing `AnnotationRetry` and `ObservedRetry`.
- Produces: R5 and R6.

- [ ] **Step 1: Write the failing envtest specs.**
  - "fails with PlanningFailed after 3 replans", with the message "The plan failed 3 times; last failure: …".
  - "resets replans on a spec edit and on success".
  - "retries a failed Auto Intent after 1m, then 2m, then 4m", counting `status.retries`, and with the delay capped at 1h by a unit test on the delay function (Review Focus 4).
  - "never retries ApprovalDenied automatically".
  - "retries a failed Manual Intent when the retry annotation changes", for Create, Update, Remediate and Delete.
  - "a retried RemediationFailed starts with remediations 0".
  - "resets retries when the Intent reaches Ready".
- [ ] **Step 2: Run `make test`, focused with `-ginkgo.focus retr|replan`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Add `func retryDelay(retries int32) time.Duration`. Update main spec §5.1 and §7 for the replan cap, Auto backoff, the retry annotation for every stage, and the reset rules.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): cap replans, retry failed Intents with backoff, and honour the retry annotation for every stage"`

---

### Task 5: Adaptive probe intervals

**Files:**
- Modify:
  - `internal/policy/probes.go`: `ProbeIntervals` replaces `ProbeInterval`; update every caller.
  - `internal/controller/managedresource_controller.go` and `internal/controller/schedule.go`: R7's state and next-run time.
  - `internal/controller/metrics.go`: the interval gauge.
- Tests: `internal/policy/probes_test.go`, `internal/controller/schedule_test.go`, and an envtest spec in `managedresource_controller_test.go`.

**Interfaces:**
- Produces:
  - `func ProbeIntervals(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) (fast, steady time.Duration)`;
  - in the controller, `type probeSchedule struct{ interval time.Duration; planHash string; next time.Time }` and `func (s probeSchedule) after(start, finish time.Time, passed bool, fast, steady time.Duration, jitter float64) (probeSchedule, int)`, which returns the next schedule and the skipped count.

- [ ] **Step 1: Write the failing tests.**
  - `TestProbeIntervalsDefaultsAndFloor`: the defaults are 1m and 5m; a floor of 2m lifts both; a `fastInterval` larger than `interval` is clamped down to `steady`.
  - `TestScheduleDoublesOnPassUpToSteady`: 1m, 2m, 4m, 5m, 5m.
  - `TestScheduleResetsOnFailureAndPlanChange`.
  - `TestJitterStaysWithinTenPercent`: with a jitter of ±0.1, the next run falls in [0.9·i, 1.1·i].
  - `TestSkipsAreCountedWhenARunOverruns`.
  - Envtest: "probes a Ready Intent at the fast interval, then less often", using the fake clock.
- [ ] **Step 2: Run `go test ./internal/policy/ ./internal/controller/ -run 'Probe|Schedule|Jitter|Skip'`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §10.1: replace the "Fixed interval (M2)" paragraph with the adaptive scheme, and note that the interval resets after an operator restart.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(probes): adaptive, jittered probe intervals with a floor"`

---

### Task 6: CLI, the e2e matrix, and docs

**Files:**
- Modify: `internal/cli/render.go` and `internal/cli/follow.go`. They show the effective modes, plus `replans` and `retries` when they are non-zero. An Auto stage's follow output says "running without approval (Auto)" instead of prompting.
- Create: `test/e2e/auto_e2e_test.go`.
- Modify: `hack/e2e.sh`, to raise the timeout if the suite needs it (now 50m).
- Docs:
  - main spec §5.1 (status fields), §6 (R1 resolution) and §7 (Auto flows);
  - the design doc §5, recording R1–R7;
  - the README: modes, retries, and probes.
- Tests: `internal/cli/*_test.go`.

- [ ] **Step 1: Write the CLI tests.**
  - `TestRenderShowsEffectiveModes`.
  - `TestFollowDoesNotPromptInAuto`.
- [ ] **Step 2: Write the e2e tests.** Use a test-namespace IntentPolicy with `modes` and `allowAutoModes` all set to Auto/true. Set `OperatorConfig.spec.defaults.allowAutoModes` to all true, and restore it afterwards the way delete_e2e restores OperatorConfig.
  - `TestAutoEditPlansAndRunsAnUpdate`: no IntentApproval is created, and the audit holds an Update `phase.begin` whose `authorizedBy.kind` is `auto`.
  - `TestAutoDriftIsRemediated`: the Remediate steps re-run an executed step (the Create plan includes `kubectl scale … --replicas=2` and a rollout status), so it runs without approval.
  - `TestAutoDeleteTearsDown`: the Delete runs without approval.

  Every test verifies the audit chain. The Manual half of the matrix is the existing `TestEditPlansAnUpdate`, `TestDriftIsRemediated` and `TestDeleteIntentTearsDownWhatItCreated`.
- [ ] **Step 3: Run `go test ./internal/cli/` and `make test && make lint`, then the full e2e on the existing cluster:** `KIND_CLUSTER=last-m4a hack/e2e.sh > "$SCRATCH/m5c-e2e.log" 2>&1`. Expected: `ok … test/e2e`, with every test passing.
- [ ] **Step 4: Commit.** `git commit -m "test(e2e): the Manual and Auto matrix for edit, drift and delete"`

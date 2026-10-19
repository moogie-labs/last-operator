# M5a: Intent Identity, Approval Binding and the Delete Stage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- An approval works only for the Intent it was made for, and only while it is fresh.
- A recreated Intent starts with a clean gateway record.
- Deleting an executed Intent tears down what it created, behind a finalizer, through an approved Delete plan.

**Architecture:**
- **Approvals:** `IntentApproval` gains a required `intentUID`, and the gateway also ignores approvals older than `OperatorConfig.spec.approvals.maxAge`.
- **The gateway's record:** each Intent's record stores the Intent's UID and is reset when a different UID appears. It is deleted after a successful Delete phase. Open phases left by an earlier gateway are closed eagerly at lead.
- **The operator:**
  - adds a finalizer before the first phase;
  - reads the executed plan from the audit log through `QueryAudit`;
  - on deletion, asks the model to order the recorded teardown commands into a Delete plan, gets it approved, runs it (phase `TearingDown`), deletes the ManagedResources, and releases the finalizer.

**Tech Stack:** Go 1.26, controller-runtime, kubebuilder markers, Ginkgo envtest, kind e2e.

**Spec:** `docs/superpowers/specs/2026-10-18-m5-day-two-design.md` §3, D1–D3, D6, D7, §6, §7, §9 (the M5a parts). The main spec is `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

## Global Constraints

- **Commits and files:**
  - never add a commit attribution trailer (such as `Co-authored-by:`); never commit `.superpowers/`;
  - after changing `api/`, run `make manifests generate helm-crds`;
  - after Go changes, run `make lint-fix`, `make lint` (0 issues) and `make test`;
  - after chart changes, run `make helm-lint`.
- **Rulings made while planning,** which refine design D2:
  - **The record ConfigMap's name stays the hash of `namespace/name`.** The record stores `intentUID` instead. A record whose UID is not the Intent's is reset to empty: open phase, used subjects, executed plan and index entries all go.
  - **Output Secrets are not deleted by the gateway.** They are owned by their ManagedResource (controller owner reference), and deleting the ManagedResources removes them.
- **Approvals:**
  - `IntentApprovalSpec.IntentUID` is a string, JSON `intentUID`, required with `MinLength=1`. It is immutable like the rest of the spec (the existing admission policy).
  - `OperatorConfigSpec.Approvals.MaxAge` is a `*metav1.Duration`, JSON `approvals.maxAge`. The default is 24h; CEL checks it is between 1m and 720h.
  - The gateway ignores an approval whose `intentUID` is not the Intent's UID, or whose `creationTimestamp` is older than `maxAge`. This holds for Deny too: a stale or foreign Deny is ignored.
- **The finalizer** is `last.moogie-labs.com/teardown`.
- **The retry annotation** is `last.moogie-labs.com/retry`. A changed value replans a failed Delete. The operator records the value it acted on in `status.observedRetry`.
- **New phase and reason:**
  - `PhaseTearingDown` while a Delete phase runs;
  - Failed reason `TeardownFailed`.
- **The Delete plan:** `resources: []`, and its steps are a permutation of the executed plan's teardown commands, compared by canonical JSON of the command. The operator checks this before recording, sending a correction otherwise. In M5a the gateway authorizes Delete by approval only.
- **The executed plan comes from the audit log.** It is the plan in the newest `phase.begin` with stage Create or Update whose matching `phase.end` (same `beginSeq`) has outcome `succeeded`. Its canonical hash must equal the entry's subject hash.

## Review Focus

1. **An approval for a deleted and recreated Intent with the same name must not authorize anything.** Task 1 tests a stale-UID Approve that is ignored, and a stale-UID Deny that is ignored too.
2. **Deleting an Intent that never executed must not hang on the finalizer.** Task 5 tests deletion while AwaitingApproval and while Failed before execution.
3. **A Delete whose teardown fails must keep the finalizer and say why.** The retry annotation must re-plan. Task 5 tests both.
4. **A model that reorders the teardown wrongly, adds a step, or drops one gets a correction, never an approval prompt for a foreign plan.** Task 5 tests each case.
5. **A gateway restart mid-teardown must not wedge the Intent.** The open Delete phase is closed as interrupted at lead, and the operator replans the Delete. Tasks 2 and 5 test this.

---

### Task 1: Approvals bound to the Intent's UID and age

**Files:**
- Modify: `api/v1alpha1/intentapproval_types.go`, `api/v1alpha1/operatorconfig_types.go`, then run `make manifests generate helm-crds`
- Modify: `internal/gateway/phases.go` (`findApproval` and its callers)
- Modify: `internal/cli/decide.go`
- Modify: every test or e2e fixture that builds an `IntentApproval` (grep for `IntentApprovalSpec{`)
- Modify: main spec §5.2 and §5.6

**Interfaces:**
- Produces: `func (g *Gateway) findApproval(ctx context.Context, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) (*lastv1alpha1.IntentApproval, error)`. It takes the Intent, for its UID and name, and reads `maxAge` from OperatorConfig, defaulting to 24h.

- [ ] **Step 1: Write the failing tests** (gateway)

  - `TestApprovalForAnotherUIDIsIgnored`: an Approve with the right subject but another UID leaves BeginPhase refusing with "no IntentApproval approves".
  - `TestStaleApprovalIsIgnored`: an Approve older than `maxAge` (set to 1h, approval created 2h ago) is refused the same way.
  - `TestDenyForAnotherUIDIsIgnored`: a foreign-UID Deny alongside a valid Approve. The Approve wins.
  - `TestDenyForThisUIDWins`.
  - The existing approval tests are updated with the right UID.
  - CLI: `TestApproveSetsTheIntentUID`, where the created object carries `in.UID`.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ ./internal/cli/`. Expected: FAIL.

- [ ] **Step 3: Implement**

  Add the API fields, markers and CEL rule, then regenerate. Update `findApproval` and the CLI, and fix the fixtures. Make the spec edits.

- [ ] **Step 4: Run the tests and lint**

  Run `make test && make lint && make helm-lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git commit -m "feat(approvals): bind approvals to the Intent's UID and expire them"
  ```

---

### Task 2: The gateway record knows its Intent, and open phases close at lead

**Files:**
- Modify: `internal/gateway/state.go` (`IntentRecord.IntentUID`; `Load(ctx, intent, uid string)` resets on mismatch; `Delete(ctx, intent)`; `List(ctx)`)
- Modify: callers of `Load` (`phases.go`, `probes.go`, `index.go`, `queryaudit.go`). Where no Intent object is at hand (index flush, QueryAudit), use `LoadAny(ctx, intent)`, which keeps whatever UID is stored.
- Modify: `internal/gateway/runtime.go` `prepareLeading`. List records and close each `Open` from another epoch as interrupted (`phase.end` reason `interrupted`, the existing helper).
- Modify: `charts/last-operator/templates/gateway.yaml`. Add `delete` to the gateway Role's `configmaps` verbs. Add a `helm-lint` grep.

**Interfaces:**
- Produces:
  - `func (s *StateStore) Load(ctx context.Context, intent, uid string) (*LoadedRecord, error)`: a stored record with a different non-empty UID comes back empty, carrying the new UID, and its ConfigMap is kept so the next Save overwrites it;
  - `func (s *StateStore) LoadAny(ctx context.Context, intent string) (*LoadedRecord, error)`;
  - `func (s *StateStore) Delete(ctx context.Context, intent string) error`, where NotFound is nil;
  - `func (s *StateStore) List(ctx context.Context) ([]*LoadedRecord, error)`, filtered by the existing `component=intent-state` label.

- [ ] **Step 1: Write the failing tests**

  - `TestARecordForAnotherUIDStartsClean`: a stored record with UID A, Used subjects and Executed. Loading it for UID B gives an empty record, and BeginPhase for a plan subject that A had used succeeds for B.
  - `TestLeadClosesInterruptedPhases`: two records with `Open` from another epoch. After `prepareLeading`, both have `Open == nil` and each has a `phase.end` audit entry with reason `interrupted`.
  - `TestDeleteRemovesTheRecord`.
  - `make helm-lint` fails until the verb is added.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/`. Expected: FAIL.

- [ ] **Step 3: Implement as specified**

- [ ] **Step 4: Run the tests and lint**

  Run `go test -race ./internal/gateway/ && make lint && make helm-lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git commit -m "feat(gateway): reset a record for a recreated Intent and close interrupted phases at lead"
  ```

---

### Task 3: The executed plan from the audit log, and teardown checks

**Files:**
- Modify: `internal/agent/audit.go` (add `ExecutedPlan`)
- Modify: `internal/plan` (`Validate` checks teardown commands' argv-xor-script), `internal/agent/parse.go` (the same check)
- Tests: `internal/agent/audit_test.go`, `internal/plan/*_test.go`, `internal/agent/parse_test.go`

**Interfaces:**
- Produces: `func ExecutedPlan(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef) (body plan.Body, hash string, found bool, err error)`, per Global Constraints. It scans `phase.end` (success outcomes) and `phase.begin` (stage Create or Update) through `scan`, and pairs them by the end payload's `beginSeq`. A truncated index returns `found == false` with a nil error.

- [ ] **Step 1: Write the failing tests**

  - `TestExecutedPlanIsTheLatestSucceededCreateOrUpdate`, against a fake auditor holding:
    - Create succeeded (plan P1);
    - Discovery succeeded;
    - Create failed (P2);
    - Update succeeded (P3);
    - Remediate succeeded (P4).

    The result is P3 with its hash.
  - `TestExecutedPlanNotFound`: no succeeded entries gives `found == false`.
  - `TestExecutedPlanRejectsAHashMismatch`: the begin payload's plan hashes to something other than its subject, so the result is an error.
  - `TestValidateRefusesATeardownWithBothArgvAndScript`, and the same in `agent.Parse`.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/agent/ ./internal/plan/`. Expected: FAIL.

- [ ] **Step 3: Implement**

  The `phase.begin` payload is `{"plan": "<canonical JSON>"}` (gateway `phaseBegin`), and `phase.end` carries `beginSeq` and `outcome` (read `internal/gateway/phases.go` for the exact fields). Decode the plan with `plan` package helpers and check its hash with `plan.Hash` (or the canonical sha256).

- [ ] **Step 4: Run the tests and lint**

  Run `go test ./internal/agent/ ./internal/plan/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git commit -m "feat(agent): read the executed plan from the audit log, and check teardown commands"
  ```

---

### Task 4: The gateway's Delete stage

**Files:**
- Modify: `internal/gateway/phases.go`:
  - BeginPhase accepts `StageDelete`, refusing a plan whose `resources` is non-empty with FailedPrecondition through `d.deny`, and authorizing by approval;
  - EndPhase on a succeeded Delete deletes the record and forgets the in-memory phase and index entries for the Intent.
- Modify: the main spec §8.3, for Delete and the record's lifetime.

**Interfaces:**
- Consumes: Task 2's `StateStore.Delete`.
- Produces: Delete phases end to end in the gateway.

- [ ] **Step 1: Write the failing tests**

  - `TestDeletePhaseRunsAnApprovedTeardown`: BeginPhase(Delete), then RunCommand for each step, then EndPhase(succeeded). The record ConfigMap is gone, and RunProbes then answers "no executed plan".
  - `TestDeleteRefusesAPlanWithResources`.
  - `TestDeleteNeedsApproval`.
  - `TestAFailedDeleteKeepsTheRecord`: EndPhase(failed) keeps the record and Executed.

- [ ] **Step 2: Run the tests and confirm they fail**

  Run `go test ./internal/gateway/ -run Delete`. Expected: FAIL.

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run the tests and lint**

  Run `go test -race ./internal/gateway/ && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git commit -m "feat(gateway): run approved Delete phases and drop the Intent's record after teardown"
  ```

---

### Task 5: The operator's Delete flow and finalizer

**Files:**
- Modify: `api/v1alpha1/intent_types.go`. Add `status.observedRetry` and the `TearingDown` phase if it is not printed yet. Run `make manifests generate helm-crds` if fields change.
- Create: `internal/controller/intent_delete.go`, with tests in `internal/controller/intent_delete_test.go`.
- Modify: `internal/controller/intent_controller.go`:
  - the deletion branch;
  - add the finalizer in `startPlanning`, before Create;
  - Watch metadata changes for the retry annotation. The default `For` watch includes them; confirm that.
- Modify: `internal/controller/intent_planning.go`, `intent_execution.go`. Handle stage Delete: the first message, the permutation check as an `agent.Next` check (alongside `answeredCheck` and `rerunCheck`), phase `TearingDown` while executing, no Verifying after Delete.
- Modify: `internal/agent/prompt.go`, for the Delete first message.
- Modify: `internal/controller/fakegateway_test.go`, if Delete needs fake support.

**Interfaces:**
- Consumes: `agent.ExecutedPlan` (Task 3), the gateway's Delete stage (Task 4), Task 1's UID-bound approvals.
- Produces: the deletion lifecycle, per design §3.4.

- [ ] **Step 1: Write the failing envtest specs**

  - **"adds the teardown finalizer before the first phase begins".**
  - **"releases an Intent that never executed at once"** (Review Focus 2): deleted while AwaitingApproval, and deleted while Failed before execution. The Intent is gone, with no Delete planning.
  - **"tears down an executed Intent through an approved Delete plan":**
    - executed Intent, then delete;
    - the model's first message lists the teardown commands;
    - the model answers their permutation;
    - the Intent goes to AwaitingApproval with stage Delete;
    - an approval with the UID moves it to TearingDown, and the fake gateway runs the steps;
    - the ManagedResources are deleted, the finalizer is removed and the Intent is gone.
  - **"corrects a Delete plan that is not a permutation of the teardown"** (Review Focus 4): a reordering that adds, drops or replaces a command gets a correction naming the problem. After `MaxAttempts`, `PlanningFailed` keeps the finalizer.
  - **"keeps the finalizer and reports TeardownFailed when a teardown step fails"** (Review Focus 3). Then setting the retry annotation replans the Delete.
  - **"replans the Delete after the gateway closes its phase as interrupted"** (Review Focus 5): the fake gateway's RunCommand returns FailedPrecondition "no open phase". The result is `Failed`/`TeardownFailed`, and the retry annotation recovers.

- [ ] **Step 2: Run the specs and confirm they fail**

  Run `make test`, focused on `internal/controller` with `-ginkgo.focus Delete|finalizer`. Expected: FAIL.

- [ ] **Step 3: Implement as specified**

  If the executed plan is not found but ManagedResources exist, fall back to their `teardown` (design §3.3).

- [ ] **Step 4: Run the tests and lint**

  Run `make test && make lint`. Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git commit -m "feat(operator): tear down deleted Intents behind a finalizer"
  ```

---

### Task 6: CLI, e2e and docs

**Files:**
- Modify: `internal/cli/follow.go`, `internal/cli/render.go`. Show `TearingDown` and the stage being approved. Following an Intent through deletion ends when it is gone, with "Intent … torn down".
- Create: `test/e2e/delete_e2e_test.go`, modelled on `TestOperatorWebServer`:
  - an Intent with the nginx plan whose resources carry teardown commands (`kubectl delete …`) reaches Ready;
  - `kubectl delete intent`;
  - the scripted model answers the Delete plan;
  - approve with `kubectl last approve --hash` (or `y` on the follow prompt);
  - the Deployment, Service and ConfigMap are gone, the Intent is gone, and the audit chain verifies.
- Modify: the main spec §7 (the Delete flow, the finalizer, the retry annotation, TeardownFailed) and §17.2 (removing the finalizer by hand), and the README's lifecycle section.

- [ ] **Step 1: Write the CLI unit tests and the e2e**

  - CLI: `TestFollowEndsWhenTheIntentIsTornDown`; `TestRenderShowsTheStageBeingApproved`.

- [ ] **Step 2: Run the CLI tests and confirm they fail**

  Run `go test ./internal/cli/`. Expected: FAIL.

- [ ] **Step 3: Implement the CLI, then run the full e2e on the existing cluster**

  Run `KIND_CLUSTER=last-m4a hack/e2e.sh > "$SCRATCH/m5a-e2e.log" 2>&1`. Expected: `ok … test/e2e`, with every test passing.

- [ ] **Step 4: Commit**

  ```bash
  git commit -m "test(e2e): delete an Intent and tear down what it created"
  ```

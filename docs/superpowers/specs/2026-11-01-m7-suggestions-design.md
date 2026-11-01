# M7: Rule suggestions — Design

**Status:** drafted on 2026-11-01 from the main spec, the code, and the M6 follow-ups. The user asked to skip the question round for milestone designs. Calls I made without asking are marked *(my call)*.

**Parent spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (the "main spec"): §4.1, §5.5, §5.6, §9.4, §9.5 and §20.

**Inputs:**
- the `CommandRuleSuggestion` CRD from M0 (`api/v1alpha1/commandrulesuggestion_types.go`) and its two CEL rules: `decidedBy` is required once decided, and the decision can't change after that;
- `OperatorConfig.spec.suggestions` (`repeatThreshold`, default 3; `reviewInterval`), the unused `status.suggestedRules`, and the `suggestion.decided` audit type;
- M6's `internal/rules` package (`Match`, `Covers`, `PlanCoverage`, `Generalize`, `StartsCode`, `LintFindings`) and its `approve --always-allow` flow;
- M6 final review I2: the CLI ignores `status.plan.generalizations` and `pendingCommand.generalization`, because the untrusted operator writes them outside the plan hash. M7 has to say how an operator proposal can reach an approver safely;
- the M6 follow-ups "who wrote a rule" and "generalized teardown rules are broad".

## 1. Goal and done-criterion

Rules let Manual mode scale, but only if people write them. Suggestions close that gap. The operator notices which commands keep needing approval, proposes a rule for them, and a human accepts or denies it. A denial is remembered, so the same proposal, or a broader one, never comes back.

**Done when:** e2e on kind shows that a denied suggestion is never suggested again, that a narrower one is, and that `kubectl last audit verify` passes (main spec §20).

## 2. The trust model *(my call; this answers M6 I2)*

The operator is untrusted (main spec §3). Anything it writes can be adversarial: a suggestion's pattern, its stages, its rationale, its examples, its lint. Signing or hashing operator text wouldn't help, because the operator would be signing its own words. So M7 trusts no operator text. Instead it makes sure a person sees exactly what they are agreeing to, and that nothing the operator wrote turns into a rule without that person's explicit act.

- **A suggestion is a proposal for a person to read, never input to an automated step.**
  - `approve --always-allow=generalized` and `=exact` never use suggestions. Batch mode still generalizes only with `rules.Generalize`.
  - There is no "accept all".
  - Every accept names one suggestion and goes through the CLI, which shows the full proposal first.
- **The CLI re-derives everything it can and never shows the operator's own claims as fact.**
  - It runs lint itself and ignores `spec.breadthWarnings`.
  - It works out which commands the pattern matches.
  - It compares the proposal with what the CLI's own `g` choice would create, and highlights every place the proposal is broader.
  - It strips control characters from the rationale and the examples before printing them, so operator text can't drive the terminal.
- **The CLI refuses outright any proposal whose pattern matches nothing or lets code vary** (lint `Nothing` or a code finding). A person who really wants such a rule can still write it by hand.
- **`status.plan.generalizations`, `status.pendingCommand.generalization` and the `Generalization` type are removed** from the API. No release ever wrote them, and an unused, operator-written "advice" field invites someone to trust it. Proposals live only in `CommandRuleSuggestion` objects.

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| S1 | **Admission (`last-suggestions`, a new ValidatingAdmissionPolicy) on CREATE and UPDATE.** Requests from gateway-namespace ServiceAccounts are refused. The operator's ServiceAccount may create but not update. What it creates must have `decision: Pending`, no `decidedBy` or `decisionReason`, and no `proposed.source`. A `Cluster` target must be in `last-system`. For any requester, a request that sets the decision must set `decidedBy == request.userInfo.username`, and the operator may never do it. Setting the decision means a create whose decision isn't Pending, or an update that moves it away from Pending. Other updates to an already decided suggestion, such as a label change, don't re-check `decidedBy`. | Main spec §9.5 and §4.1. A ValidatingAdmissionPolicy can't mutate, so "stamps `decidedBy`" is enforced the way `IntentApproval.approvedBy` already is: the field must equal the requester. |
| S2 | **Suggestions are immutable apart from the decision.** On update, every spec field other than `decision`, `decidedBy` and `decisionReason` must stay the same. Once decided, those three are frozen too. This extends the CRD's existing transition rule. | A decision then always refers to the text the person saw. The builder chooses whether to enforce this with CRD CEL or with the policy; both must be tested. |
| S3 | **RBAC.** The operator gets get, list, watch and create on suggestions, and no update, patch or delete. The gateway gets get, list and watch. The chart ships a ClusterRole, `last-suggestion-decider`, with get, list, watch, update and patch on suggestions, for admins to bind. Accepting a suggestion also needs create on the rule kind, as in M6. | Main spec §4.1. The operator can't delete, so denials are kept forever (§9.5). |
| S4 | **Rule provenance: `source.createdBy`** *(my call; M6 follow-up "who wrote a rule")*. `RuleSource` gains `createdBy`. `last-rules` refuses a create that carries `spec.source` unless `source.createdBy == request.userInfo.username`. It also refuses an update that changes `spec.source`. `createdFrom: suggestion` requires a `suggestionRef`. A hand-written rule may leave `source` out. Because the source is part of the spec, `rule.observed` records it automatically. The M6 rule name hash, and its "identical spec counts as done" check, both leave `source` out, so two approvers creating the same rule don't clash. | An auditor can tell who created a rule the CLI made. The rule-author decision was open with the user, and this is the smallest form of it. |
| S5 | **The vetting gate (`suggest.Vet`), run by the operator before it creates anything.** A proposal is dropped if any of these hold: lint finds a pattern that matches nothing; lint finds code that can vary; lint finds a wildcard after a destructive verb; `argv[0]` differs from its examples' `argv[0]`; the match pattern fails to match every example; or `proposed.source` is set. It is also skipped, with no suggestion made, if existing rules already cover every example at the proposed stages, or if a Denied suggestion suppresses it (S8). | This is noise control, not security: §2 is the security. It does keep an honest operator from proposing the dangerous shapes M6 fought (C1). It also settles the teardown-breadth follow-up for suggestions *(my call)*: the operator never proposes a wildcard after `delete` or another destructive verb. |
| S6 | **Names and dedupe.** A suggestion is named `sugg-<hash10>`, where `hash10` is the first 10 hex characters of the SHA-256 of the canonical JSON of `{targetScope, namespace, proposed}`. Creating a name that already exists counts as done. Each namespace may hold at most 100 Pending suggestions *(my call)*. Beyond that the operator creates none and emits a `SuggestionLimit` event. The label `last.moogie-labs.com/intent` names the Intent that prompted the suggestion. | The same proposal from any trigger is one object. With no delete permission, the cap bounds pile-up. People can delete suggestions they don't want. |
| S7 | **Which proposals an approver is offered is worked out from coverage, not from labels.** For each uncovered entry, the CLI lists the Pending suggestions that would cover it at the plan's stage: `rules.Covers` over the namespace's suggestions plus the `Cluster` ones in `last-system`. | This works for every trigger, and it can't be misdirected by a label the operator chose. |
| S8 | **Suppression (main spec §9.5).** A new proposal N is dropped when some Denied suggestion D would let through nothing that N doesn't. That means D's match is contained in N's match; D's stages are a subset of N's, with Create and Update as one class; D's providers are a subset of N's; N's egress covers D's; and each of D's install patterns is contained in one of N's. Any comparison that can't be decided counts as contained, so N is dropped. Scope: a `Namespace` N is compared with the Denied `Namespace` suggestions in its own namespace. A `Cluster` N is compared with every Denied suggestion, of either scope. A narrower N is still offered, including one narrower only in stages or providers *(my call)*. | Main spec §9.5: "equal to or narrower than", and undecidable means broader. A cluster rule reaches every namespace, so any denial counts against it. A namespace rule is narrower than a denied cluster-wide one. |
| S9 | **Pattern containment (`rules.Contains`).** Positions are compared pairwise. A `...` in the broad pattern covers every remaining position of the narrow one, including the narrow pattern's own `...`. A `...` in the narrow pattern is contained only by a `...`. Per position: a narrow position that matches exactly one string (`literalOf`) is decided by `Match`. Anything is inside `*`. `[!-]*` contains any position that can't start with `-`. Identical positions are equal. Glob inside glob is decided exactly where the builder implements it; otherwise it, and any `re:` position that isn't a literal, is undecided. The result is three-valued: contained, not contained, or undecided. | The spec's "literal ⊂ glob ⊂ `...`", and failing closed wherever it can't be decided. |
| S10 | **The LLM proposes match patterns only, through `CallLLM` with `purpose: suggest`.** It uses a separate small JSON schema, `{suggestions: [{entry, argv, rationale}]}`, with no regex keywords, and a separate system prompt. The operator fills in everything else in the rule, as M6 R13 does. `CallLLMRequest` gains a `purpose` field (`turn`, the default, or `suggest`). The gateway records it in the `llm.request` payload. Suggest calls are audited like any other LLM call but are not added to the Intent's audit index, so they never displace or confuse a planning session (`agent.Reload`). The scripted provider answers a suggest call from the key `_suggest.<namespace>.<intent>`; a missing key means no proposal. A suggest call that fails or times out produces no suggestion, raises an event, and is not retried. | The turn schema stays as it is, which avoids the "compiled grammar too large" risk. Existing scripted e2e tests keep working, because they have no `_suggest.` keys. A lost suggestion costs nothing: the next trigger can try again. |

## 4. The three triggers

All three run in the operator. Each one builds proposals, runs them through `suggest.Vet`, and creates whatever survives.

### 4.1 PlanTime

- **When:**
  - While a plan waits in AwaitingApproval, the reconcile runs one PlanTime step for it.
  - The step runs once per plan. `status.plan.suggested: true` (a new field) marks it done, even when it produced nothing, and a new plan resets it.
  - Plans that run without approval (Auto, or fully covered by rules) are skipped *(my call)*: there is no person waiting to read anything.
- **What:**
  - One `CallLLM` (`purpose: suggest`) proposes a pattern for each uncovered entry.
  - Stages *(my call, following the spec's "remediation and teardown")*:
    - the steps and probes of a Create or Update plan get `[the plan's stage, Remediate]`;
    - the steps and probes of any other plan get `[the plan's stage]`;
    - teardown entries get `[Delete]`.
  - Providers, egress and install follow M6 R13, including the provider union.
  - Examples: the entry's own command.
  - Target: `Namespace`.
- **Recorded:** the step writes `status.suggestedRules` with the names of the suggestions it created or found for this plan.
- **Not covered:** discovery commands get no PlanTime suggestions *(my call)*. RepeatedApproval covers them.

### 4.2 RepeatedApproval

- **Input:**
  - `QueryAudit` reads `command.start` entries for each Intent in the namespace.
  - To make this possible, `AuditEntry` gains `authorized_by {kind, name}` from the entry's `AuthorizedBy`.
  - A command counts as manually approved when its `authorizedBy.kind` is `approval`.
- **Group key** (main spec §9.5): the stage class (Create and Update as one), the sorted provider set, and the binary plus its first non-flag arguments, up to 3 in all. That is the same three literals `Generalize` keeps.
  - An argv with a `StartsCode` position is keyed by its whole argv, so only exact repeats group.
  - Scripts, commands with `${steps.` references, and commands already covered by a rule are left out.
- **Threshold:** a group fires when it holds commands from at least `repeatThreshold` distinct IntentApprovals (distinct `authorizedBy.name`).
- **Proposal:** the LLM proposes one pattern that must match every example. The examples are the group's latest commands, at most 20. If the LLM gives no valid answer, a deterministic merge is used instead, when the examples have the same length and their flags line up:
  - an equal position stays literal;
  - a differing position becomes `[!-]*`, if none of its values starts with `-`;
  - any differing position at or after a `StartsCode` position means no merge.

  If neither works, there is no suggestion and the operator emits an event.
  - Stages: the group's stage class. Providers: the group's. Target: `Namespace`. The label names the newest Intent among the examples.
  - The LLM call is made on that Intent's behalf.
- **When:**
  - after the operator ends a phase that an IntentApproval authorized, for the groups of the commands that phase ran;
  - at most once a minute per namespace;
  - again in full on each PeriodicReview.
- **Limit:** Intents that have been deleted no longer have an index, so their approvals stop counting. This is recorded as a limitation, not designed around.

### 4.3 PeriodicReview

Every `reviewInterval` (default 24h, from 1h to 720h *(my call)*), with ±10% jitter, from a timer in operator memory. A restart starts the timer again; that is harmless because creates are idempotent (S6).

The review runs three passes in each namespace that has Intents or rules:

1. **The full RepeatedApproval scan.** It catches groups whose trigger was missed.
2. **Consolidation.** Two or more CommandRules in the namespace are merged into one proposal when all of these hold:
   - they share `argv[0]`, pattern length, stages, providers, egress and install;
   - their match patterns differ only at positions where none of them has a `StartsCode` word.

   The proposal uses the deterministic merge from §4.2. The LLM is not used here *(my call)*. The proposal must contain each merged rule's match (S9, decided), in place of matching examples. Its rationale names the merged rules. Examples are audited commands those rules covered, while the index still holds them.
3. **Promotion to cluster scope.** When two or more namespaces hold CommandRules with identical specs (canonical JSON, ignoring `source`), the review proposes one `ClusterCommandRule` with that spec. The suggestion is created in `last-system` with target `Cluster`, and its rationale names the rules.

## 5. Deciding

`kubectl last rules suggestions` covers the decision commands:

- **`kubectl last rules suggestions [-n ns | -A] [--all]`** lists Pending suggestions, or every suggestion with `--all`. It shows name, scope, trigger, match, stages, providers, age, and the CLI's own lint count.
- **`kubectl last rules suggestions show <name>`** prints the full proposal, as described in §2: the CLI's lint, the examples and whether the pattern matches each, and every place it is broader than `g` would be. Those places are pattern positions (by `rules.Contains`), extra stages, extra providers, extra egress, and install entries that are not exact. The rationale and examples are labelled "operator-proposed".
- **`kubectl last rules suggestions accept <name> [--edit] [--yes]`**
  1. Shows `show`'s output and asks for confirmation; `--yes` skips the question but still prints everything.
  2. `--edit` lets the person type a new match as space-separated argv, as in M6, which is linted again.
  3. Creates the rule as the person: a CommandRule in the suggestion's namespace, or a ClusterCommandRule for `Cluster`. The rule is named after the suggestion, and its spec is the proposal plus `source {createdFrom: suggestion, suggestionRef, createdBy}`.
  4. Updates the suggestion to `decision: Accepted`, with `decidedBy` set to the user that SelfSubjectReview returns, using the resourceVersion it read. An edited accept sets `decisionReason: "accepted with an edited match: <argv>"`.

  An existing rule with an identical spec (ignoring `source`) counts as created, so a second run after a failed update finishes the job.
- **`kubectl last rules suggestions deny <name> [--reason text]`** sets `decision: Denied`, `decidedBy` and `decisionReason`.
- **`kubectl last approve` (interactive and `--always-allow` without `=`)** gains a fourth choice per entry, **`s: suggested`**, offered when S7 finds a covering Pending suggestion. Choosing it shows the same display as `show` and accepts that suggestion, as `accept` does. The `IntentApproval` follows once every entry is decided, as in M6. If an entry has several covering suggestions, the CLI lists them and the person picks one. The batch forms never offer `s` (§2).
- **`kubectl last plan`** lists, for each uncovered entry, the names of the Pending suggestions that cover it.

## 6. Audit: `suggestion.decided`

The gateway watches suggestions through an informer. When it sees one with `decision` other than Pending that it hasn't recorded, it writes a `suggestion.decided` entry. The payload holds:
- `{targetScope, namespace, name, uid, resourceVersion, trigger}`;
- `{decision, decidedBy, decisionReason}`;
- `proposed`, as canonical JSON;
- `proposedHash`, as `sha256:<hex>`.

The entry has no Intent. The gateway keeps the UIDs it has recorded in a ConfigMap in its own namespace, `last-suggestion-decisions`, and checks it again at startup. If the gateway crashes between writing the entry and saving the ConfigMap, the entry is written a second time. That duplicate is harmless and is recorded as such.

## 7. Components

- **`internal/rules`:**
  - `Contains(broad, narrow []string) Verdict`, with three values (S9);
  - `Subsumes(n, d CommandRuleSpec) Verdict` (S8);
  - `GroupKey(stage, cmd) string` (§4.2);
  - `Merge(argvs [][]string) ([]string, bool)` (§4.2);
  - `Finding` gains a `Kind` (`Nothing`, `Code`, `Destructive`, `Breadth`) to replace the `Nothing` bool, so the gate and the CLI pick findings by kind.
- **`internal/suggest`** (new), with no Kubernetes client, shared by the operator and the CLI:
  - `Vet`, the S5 gate;
  - `Name`, S6;
  - the system prompt, schema and parser for suggest calls;
  - `Broader`, the comparison with `g` used for the display in §5;
  - `Sanitize`, which strips control characters for display.
- **Operator:**
  - RBAC (S3);
  - the PlanTime step in the AwaitingApproval reconcile;
  - a `suggestions` Runnable that runs the RepeatedApproval queue and the PeriodicReview timer;
  - `status.plan.suggested`;
  - the fake gateway serves `authorized_by` and the suggest purpose.
- **Gateway:**
  - `CallLLM` `purpose`, the index skip, and the scripted `_suggest.` key;
  - `AuditEntry.authorized_by`;
  - the suggestion informer and `suggestion.decided` (§6).
- **CLI:**
  - `rules suggestions` list, `show`, `accept` and `deny`;
  - `approve`'s `s` choice;
  - suggestions shown in `plan`;
  - removal of the dead generalization fields and of their test.
- **API:**
  - S2 immutability;
  - `RuleSource.createdBy`;
  - `Plan.suggested`;
  - the generalization fields removed;
  - `reviewInterval` bounds.
- **Chart:**
  - the `last-suggestions` policy and binding;
  - the `last-rules` additions (S4);
  - RBAC, including `last-suggestion-decider`;
  - helm-lint checks that the operator can't update or delete suggestions, and that the gateway can't write them.

## 8. Errors

| Condition | Result |
|---|---|
| A suggest LLM call fails, times out, or returns something unparseable | No suggestion from that call; a `SuggestionFailed` event; no retry. The next trigger can try again. |
| A proposal fails `Vet` | Dropped, with a `SuggestionDropped` event naming the reason. |
| A proposal is suppressed by a denial | No suggestion; a `SuggestionSuppressed` event naming the Denied suggestion. |
| The namespace already holds 100 Pending suggestions | No suggestion; a `SuggestionLimit` event. |
| `accept` creates the rule but the decision update fails | The rule stays. Running `accept` again finds the identical rule and finishes. |
| The suggestion changed between `show` and `accept` | It can't change (S2). The update's resourceVersion check catches a concurrent decision. |
| `accept` for a pattern that matches nothing or lets code vary | Refused (§2), with a pointer to writing the rule by hand. |
| The operator tries to update, delete or decide a suggestion | Refused by RBAC, and the decision also by `last-suggestions`. |
| A suggestion is decided and then deleted while the gateway is down | Its decision is not audited. Any rule it produced is audited when it is used (`rule.observed`, including `source`). |

## 9. Security

- **No operator text authorizes anything, and nothing becomes a rule without a person's explicit act on that one suggestion** (§2). Batch approval never uses suggestions.
- **Only humans decide, and the decider is the requester** (S1). Only humans create rules (M6 R8). Rules from suggestions record `createdBy` (S4).
- **The CLI recomputes lint, matches and breadth, and refuses code-varying proposals.** It sanitizes operator text before printing it.
- **Denials are permanent memory.** The operator can't delete or change a suggestion. Suppression fails closed.
- **The LLM sees only commands the operator already holds:** the plan's entries and audited argv. Suggest calls are audited.

## 10. Testing

**Unit and envtest:**
- `rules.Contains` and `Subsumes`, table-driven:
  - every row of S9;
  - the spec's example: a denied `aws ec2 ...` does not suppress `aws ec2 describe-* ...`, and does suppress `aws ...`;
  - undecided cases count as contained;
  - stage, provider, egress and install containment.
- `rules.GroupKey` and `Merge`, including `StartsCode` argvs.
- `suggest.Vet`: each drop reason, plus a property test that `Generalize` output of generated argvs always passes the lint parts.
- Admission:
  - the operator can create Pending suggestions only;
  - the operator can't update, decide or delete;
  - `decidedBy` must equal the requester;
  - S2 immutability;
  - a `Cluster` target outside `last-system` is refused;
  - `source.createdBy` on rules.
- Operator, with the fake gateway:
  - the PlanTime step runs once per plan, with its stages and `suggestedRules`;
  - RepeatedApproval fires at the threshold and not before, counting distinct approvals;
  - PeriodicReview consolidation and promotion;
  - suppression and dedupe;
  - the pending cap;
  - an LLM failure.
- Gateway:
  - `purpose` is recorded, and suggest calls stay out of the index;
  - `agent.Reload` is unaffected by suggest calls;
  - `authorized_by` in `QueryAudit`;
  - `suggestion.decided`, written once, including after a restart.
- CLI:
  - list, `show`, `accept` (plain, `--edit`, and a re-run after a failed update), and `deny`;
  - the `s` choice in `approve`;
  - batch modes never offer `s`;
  - code-varying proposals are refused;
  - control characters are stripped.

**e2e on kind:**
- **`TestDeniedSuggestionIsNotRepeated`.** Three Intents with the same plan shape, each with a `_suggest.` script key.
  1. Intent A's PlanTime suggestion has a broad pattern P. The test denies it with `rules suggestions deny --reason`.
  2. Intent B's script proposes P and a broader P⁺. Neither is created, and B gets `SuggestionSuppressed` events naming A's suggestion.
  3. Intent C's script proposes N, which is narrower than P (a literal at a position where P has `[!-]*`). N is created.
  4. The test accepts N with `rules suggestions accept --yes`. The rule exists, with `source {createdFrom: suggestion, suggestionRef, createdBy: <kind user>}`, and the suggestion is Accepted.
  5. The audit holds a `suggestion.decided` entry for each of the two decisions, with `decidedBy` set to the kind user, and the chain verifies.
- RepeatedApproval and PeriodicReview are covered by envtest only *(my call)*. Each would need several full Intent runs in e2e.

## 11. Main-spec amendments

- **§4.1:** RBAC on suggestions for the operator, the gateway and `last-suggestion-decider`; the `last-suggestions` policy; `last-rules` and `createdBy`.
- **§5.5:** the immutability rule (S2); `RuleSource.createdBy`.
- **§5.6:** `reviewInterval` bounds.
- **§9.4:** the `s` choice. The note about generalizations (removed) is replaced with a pointer to §2 of this design.
- **§9.5:**
  - the trust model;
  - the trigger details (§4);
  - suppression scope and dimensions (S8);
  - containment (S9);
  - names and the cap (S6);
  - deciding (§5).
- **§12 and §14:** `CallLLM` purpose; `AuditEntry.authorized_by`; the `suggestion.decided` payload.

## 12. Upgrading to M7 (for the README)

- Apply the CRDs by hand: the generalization fields are removed, and `createdBy` and `suggested` are new.
- Bind `last-suggestion-decider` to the people who review suggestions.
- Rules created before M7 have no `createdBy`.

## 13. Left to the builder

- How M7 splits into tasks.
- Whether S2 lives in CRD CEL or in the policy.
- How far glob-in-glob containment goes beyond the decided cases in S9.
- The suggest system prompt's wording, and the exact CLI display and event wording.
- How the RepeatedApproval scan pages `QueryAudit`, and how it remembers high-water marks between scans, in memory.
- Metric names.

## 14. Still open with the user (from M6), not changed here

- An escape hatch for teardown written before M6 that fails on a missing object.
- The breadth of generalized teardown rules from the CLI's `g` choice. M7 only stops the operator from suggesting such rules (S5).

# M7 (rule suggestions) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-11-01-m7-suggestions.md`.

## Result

**The full e2e suite passed** on the two-node kind cluster `last-m4a` after the final fix wave: `ok test/e2e 3354.4s`, 17 tests.
- **`TestDeniedSuggestionIsNotRepeated`** (104s): Intent A's suggestion is denied; Intent B's identical and broader proposals are suppressed, with `SuggestionSuppressed` events naming A's suggestion; Intent C's narrower proposal is created and accepted. The rule records `source.createdBy`, both decisions are in the audit log as `suggestion.decided` with `decidedBy` set to the kind user, and the chain verifies.
- `kubectl last audit log` gained `--type`, `--from-seq` and `--json`, because `suggestion.decided` belongs to no Intent and the default output truncates payloads.

**What the reviews found:**
- **A literal never contained a glob.** A narrower proposal (a literal where the denied pattern has `[!-]*`) counted as "undecided", so it was suppressed. That broke the done-criterion's "a narrower one is suggested". Containment now says No when the narrow side provably matches a string the broad side doesn't. That also lets glob refinements such as `web-*` survive a denied `[!-]*`.
- **A merge could hide a destructive verb.** Merging `kubectl get pods` with `kubectl delete pods` gave `kubectl [!-]* pods`, and the destructive-verb lint only fires on a literal verb. Neither the deterministic merge nor an LLM answer can now put a wildcard where any example has a destructive verb.
- **Operator text could pose as CLI output.** Tabs in a provider faked the `list` table's lint column, and Namespace suggestions in `last-system` were offered to other namespaces. Operator text is now escaped in single-line cells and indented in multi-line ones, and only Cluster suggestions come from `last-system`.
- **"Nowhere broader" ignored scope.** A Cluster suggestion equal to the CLI's own `g` rule was called nowhere broader. The CLI now says a ClusterCommandRule applies in every namespace.
- **A permanent create error looped.** The RepeatedApproval runner retried the LLM every minute and hid the namespace's other groups. Invalid, too-large and forbidden creates are now final, examples drop `files`, and an Intent name that is not a valid label value is left off the label.
- **PlanTime blocked approvals.** The suggest call ran inside the reconcile, so an approval could wait up to 5 minutes. PlanTime now runs on the suggestions runner.
- **Unbounded state:** the runner's audit cache is capped at 200 commands per Intent and pruned; the gateway's decisions record is paginated and pruned.

## For you to decide

- **Still open from M6:** the escape hatch for pre-M6 teardown that fails on a missing object, and the breadth of generalized teardown rules from the CLI's `g` choice. M7 only stops the operator from suggesting such rules.
- **Restoring rules.** A rule with `source` can only be re-created by the user named in `source.createdBy`, so Velero restores and GitOps re-applies are refused unless `source` is stripped or rewritten. The README says so; a different author model (for example, allowing a named restore identity) is your call.
- **Configurable system namespace.** The operator has `--system-namespace`, but the CLI and both admission policies hard-code `last-system` (and `last-gateway`), like the earlier policies. Templating them is a chart change.

## Operator

- **PlanTime:**
  - A job dequeued after an approval but before the reconcile moves on makes a needless suggest call.
  - A failed step, or a leader change, waits for the Intent's next watch event to be queued again.
  - The "no script" case is recognized by the gateway's error text.
- **RepeatedApproval and PeriodicReview:**
  - A scan is queued only after a successful EndPhase; approved commands in a failed phase wait for the review.
  - Consolidation is all-or-nothing per set, ignores `Install` order, and promotion also ignores `description` and reads rules in the system namespace.
  - Examples are deduplicated by the whole command, so the same argv under two step IDs uses two slots.
  - Groups whose commands name no provider are skipped.
- **Events:** `SuggestionDropped` and `SuggestionLimit` have no related object, so several for one plan collapse into one event series.
- **Cost:** each proposal does two or three uncached cluster-wide Lists, and rules are read twice.

## Gateway

- `recordDecisions` stops at the first suggestion whose spec can't be canonicalized, and a save conflict mid-loop leaves the rest for the next tick (each case writes a harmless duplicate).
- The first decisions scan waits 30 seconds after gaining leadership.
- A suggestion decided and deleted within about 30 seconds is never audited.

## CLI

- An edited accept racing another accept reports "still Pending" when the suggestion may already be Accepted.
- `opPattern` does not quote elements containing `"` or `\`, so `"x` and `*"` can read as one element.
- `plan` makes two Lists per uncovered entry, and an unreadable suggestions list is reported once per entry.
- `approve` calls WhoAmI twice.
- `accept` relies on admission to keep Cluster suggestions in `last-system`.
- The CLI does not check that a suggestion's name equals `suggest.Name(...)`, so the operator can choose the rule's name.

## Rules and suggest

- `Verdict`'s zero value is `No`; every caller compares explicitly today.
- Egress containment returns Unknown where it could decide exactly (it over-suppresses, which the design allows).
- `WidenedDestructive` does not flag a glob or `re:` at a widened position in a merged rule's pattern (such a rule was already written by a person).
- `Name` hashes nil and empty `stages`/`providers` differently; proposals always use non-nil slices today.
- Cross-package string matches: Vet's "already covered" reason and the gateway's "no script" text.

## Tests

- S1 and S2 admission gaps: an operator create with only `decisionReason`, the per-field rules on `examples` and `breadthWarnings`, and adding a `decisionReason` after a decision.
- `reviewInterval`'s 1h boundary; `authorized_by` for the `rule` and `auto` kinds; the decisions duplicate window.
- PlanTime stages for Update, Remediate and Delete plans; the found-existing path; the Cluster branch of the creator.
- The e2e test asserts exactly two `SuggestionSuppressed` events right after waiting for two.
- The helm-lint rule-write grep was narrowed to avoid matching `commandrulesuggestions`; `TestOperatorRBAC` still covers the operator.

## Carried

The M5 and M6 follow-ups still apply. M7 changed the prompts with a separate suggest schema, not the turn schema, so the 2026-10-30 schema probe still stands.

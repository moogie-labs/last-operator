# M6 (allow rules) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-30-m6-allow-rules.md`.

## Result

**The full e2e suite passed** on the two-node kind cluster `last-m4a` after the final fix wave: `ok test/e2e 2912.3s`, 17 tests. Two commits followed, both changing only how the CLI generalizes rules and what lint reports. After the first, the allow-rules test passed again on its own (761s). The second changes nothing that the test's commands use.
- **`TestAllowRulesLetASimilarIntentRun`:** Intent A is approved with `--always-allow=generalized`, and a similar Intent B then runs with no approval. Its audit shows `authorizedBy {rule}`, a `rules` map that covers every step, its probe and its teardown, and one `rule.observed` per rule. The chain verifies.
- **`TestDeletingAPartialCreateTearsItDown`:** a Create makes a ConfigMap and then fails at its next step. Deleting the Intent removes the ConfigMap once the Delete is approved.

**What the reviews found:**
- **`re:` patterns could escape their anchors.** A pattern with unbalanced parentheses could match almost any argument. Each `re:` is now parsed on its own before it is anchored.
- **A rule covering one step authorized the whole plan.** That included probes, which run on every cycle, and teardown, which Auto can run unattended. Neither had been matched or seen by a human. Rules now authorize a Create or Update only when every step, probe and teardown command is covered (R14), and Create and Update share rule stages (R15).
- **Generalized rules turned scripts into wildcards.** `bash -c "…"`, `ssh host …`, `watch …`, `su -lc …`, wrappers such as `timeout` and `sudo`, and `docker exec ctr …` all produced patterns that let the code, the remote command or the program vary. Everything from such a word on, and after `--`, now stays literal. A separate flag value can no longer stand for a flag, and lint warns when a hand-written pattern lets code vary.
- **The CLI used the operator's generalizations.** It took operator-written generalizations from the Intent's status, outside the plan hash. It now ignores them and generalizes each command itself.
- **Older partial plans and interrupted steps were missed.** A partial plan older than the executed plan, and a step cut off mid-run, were not torn down. Delete now covers every partial plan in the history and counts a step that only started.
- **The teardown prompt asked for tolerance too late.** It asked for tolerant commands at Delete time, when the model can only reorder them. The request moved to where teardown commands are first written.
- **`rule.observed` now records the rule's canonical spec and its hash,** and step IDs may not contain `:`.

## For you to decide

- **Who wrote a rule.** The gateway cannot attribute a rule to its author: `rule.observed` records what the rule said, not who created it. An admission policy could stamp `source.createdBy` from the request's user, as approvals record `approvedBy`.
- **Generalized teardown rules are broad.** A generalized teardown rule at stage Delete, such as `kubectl delete deployment [!-]* -n [!-]*`, lets any Intent being deleted in that namespace delete any matching object. Lint flags destructive verbs, but nothing enforces narrower teardown rules.
- **Teardown failing on a missing object.** Pre-M6 partial plans may have teardown that fails on a missing object. Each retry rebuilds the same set of commands, so the only way out is removing the finalizer by hand. An approved Delete that may drop commands that came from partial plans would be an escape hatch.

## Gateway

- **`MsgNeedsApproval` still says only an IntentApproval can authorize the plan.** `NeedsApproval` matches it exactly, so switch to a prefix match before changing the text.
- **`approval.observed` can repeat** after a phase alternates between rule and approval authorization (as Auto does).
- **No author check on rules.** Rules have no gateway-side author check, unlike approvals' `checkApprover`; RBAC is the control.
- **Hard-coded namespace.** The admission policies hard-code the `last-gateway` namespace.

## Operator

- **Labels:** a rule-covered Remediate or Delete in an Auto stage is labelled "(Auto)".
- **Plans from before M6** have no `uncoveredSteps`.
- **Partial plans:**
  - A superseded plan that succeeded, but whose `phase.end` index entry was lost, counts as partial. It over-tears, which is harmless with tolerant teardown.
  - A partial plan whose `phase.begin` left the index is dropped without an event.
  - Each Delete reconcile scans the index four times.
- **The window-cap message becomes the remediation feedback** on a later retry.

## Rules and lint

- **Lint false positives.** A runner or interpreter word used as a resource name (`kubectl get node`, `kubectl set env`) stops generalization and makes lint warn on hand-written patterns.
- **The `/work/` lint misses some forms:** `re:^/work/…`, attached short flags (`-f/work/x`) and relative paths.
- **Command-valued `--flag=value` options** (`rsync --rsh=…`, `helm --post-renderer=…`) generalize to `--flag=*`. The README documents this.
- **Noise on dead patterns:** patterns that match nothing also get breadth warnings.

## Tests

- **Operator specs** for a rule-skipped Delete, a Delete or Remediate the gateway disagrees on, and a real Update-stage plan.
- **e2e:** assert that B's `rule.observed` refs are A's labelled rules.
- **Admission tests:** warm-up rules are not cleaned up; the helm-lint checks are line-based.

## Carried

The M5a, M5b and M5c follow-ups still apply. The schema probe passed on 2026-10-30, and M6 changed the prompts but not the turn schema.

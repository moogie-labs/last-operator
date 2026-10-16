# M3b (operator turns, Clarifying, Discovery) follow-ups

Deferred items from the task reviews and final review of `docs/superpowers/plans/2026-10-13-m3b-operator-turns-discovery.md`.

## Before M3c builds on this

- **Run the kind e2e once.** Done on 2026-10-16 (see the M3c follow-ups): the full suite passed, including the chart's Provider RBAC and the encoded turns.
- **Check the schema against real providers.** The turn schema inlines the command object 4 times and has no `$defs`. The M3c demo should confirm that Anthropic structured outputs accept it, and OpenAI strict mode for anyone using that adapter.
- **The OpenAI adapter caps a turn at 10 minutes.** `internal/gateway/llm/openai.go` sets an `http.Client` timeout of 10 minutes. That cuts long turns short whatever the operator's per-call bound (`agent.CallTimeout`, 30 minutes). Anthropic has no such cap. A related issue is still open: OpenAI-compatible models with a smaller output limit reject `max_tokens` 64000 (see the M3a follow-ups, Token budget).

## Open edges

- **Approve and Deny inside the open window leak a Discovery phase.** The window runs from `BeginPhase(Discovery)` to the `PhaseOpen` status write.
  - The trigger: an operator crash or a failed status write in that window, then a Deny filed for the same command.
  - The leak happens in two places:
    - the denial branch of `awaitDiscovery` reports the denial without ending the phase;
    - `endApprovedPhase`, after a spec edit, also skips it, because a Deny wins in `decision`.
  - Later `BeginPhase` calls then return `AlreadyExists` until the gateway restarts.
  - Fix, about 10 lines:
    - end the phase when *any* Approve exists for the subject, whatever the Deny says, in both branches;
    - tolerate `FailedPrecondition`;
    - add one spec.
- **A RunCommand refusal is missing from the M3 spec §6 table.** Done in M3c.
- **`Internal` from `QueryAudit` retries forever.** It means a verification failure or a missing indexed entry. The Intent stays in Planning and only the logs say why. Consider failing it with `PlanningFailed`.
- **A repeated identical command can confuse a reload.** If the model asks for the same command twice and the operator crashes before recording the second, the reload predicate matches both turns. It then appends the first run's result after the second turn. The impact is low, because the output is for the same command.
- **A new session after a failed reload shows answers without their questions.** The first message lists `- q1: web` without the question text, although `status.questions` still holds it.
- **A conversation over gRPC's 4 MiB limit retries forever.** It fails with `ResourceExhausted`, which is retried. This is unlikely within the caps. Consider making it terminal.
- **Workers held for long turns.** A turn can now hold a reconcile worker for up to 91 minutes, and the Intent controller runs 4 workers. Revisit at M5 scale.
- **Leadership regained without a restart.** If a process ever regains leadership without restarting, its in-memory session could be stale. controller-runtime exits on leader loss today. Applying `recordedTurn` to memory hits as well as reloads would be cheap insurance.

## Test gaps

- **A spec that depends on cache order.** The spec "ends an approved discovery phase when the spec changes" assumes the approval reaches the manager cache before the status event. It passed 12 of 12 runs. To make it certain, wait until the cached client lists the approval before `setStatus`.
- **Agent package:**
  - no test of `Encode`'s error paths;
  - no test of a `check` refusal using up `MaxAttempts`;
  - no test of `ErrNoSeq` on a later attempt;
  - no test of `MaxModelTurns` being reached during corrections.
- **Controller:**
  - `pendingTurn`'s `QueryAudit` branches are untested, both the transient and the terminal one;
  - the order in which allowed Providers are listed is untested;
  - no spec covers a session cap ending in `PlanningFailed` through the turn cap; only the command cap is covered.

## Polish

- **Hard-coded caps in the prompt.** `systemPrompt` writes the caps ("10 commands and 20 answers") as literals. A test should pin them to `MaxCommands` and `MaxModelTurns`.
- **Wording:**
  - "answer 1 questions" in the Clarifying message;
  - `FirstMessage` repeats the namespace;
  - the model-turn prefixes differ slightly between a refused discovery command and one that could not run.
- **Code tidy-ups:**
  - the `session` type is shadowed by local `session` variables;
  - `intentpolicies;operatorconfigs` RBAC markers appear on both reconcilers;
  - no log line on the denied and failed discovery paths.

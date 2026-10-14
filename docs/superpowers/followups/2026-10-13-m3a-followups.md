# M3a (gateway for real LLMs) follow-ups

Deferred items from the final review of `docs/superpowers/plans/2026-10-05-m3a-gateway-llms-discovery.md`.

## Contracts for M3b

- **QueryAudit.** A read is read-your-writes or it fails with `Unavailable`: the gateway saves the Intent's collected index entries before reading, and refuses the query when it cannot. `truncated` reports only entries the index dropped. It does not report entries lost because the gateway crashed before a flush.
- **Discovery error codes.**
  - `PermissionDenied`: the subject does not match the pending command.
  - `FailedPrecondition`: the subject was already used, or no approval approves it.
  - `InvalidArgument`: size, step ID or nonce problems.
- **EndPhase while a step runs.** `EndPhase` now returns `Aborted` ("a step of this phase is still running; retry") instead of waiting for the step, as `RunCommand` does. Today the operator retries only `Unavailable` in-loop and leaves an `Aborted` `EndPhase` to the next reconcile. M3b may want to retry it in-loop.
- **Union schema compatibility.** M3b's turn schema must work with OpenAI strict mode (`additionalProperties: false`, every property required) and with Anthropic's structured-output subset.

## Deferred

- **Fallback text joining.** The Anthropic adapter joins the text blocks on either side of a mid-stream fallback. We believe this is correct, because the fallback continues from the partial text, but no test pins it. Confirm it in the M3c real-model demo and add a pinning test.
- **Discovery refusal wording.** Discovery refusals reuse the Create wording ("this plan already opened a phase", "approval … denies this plan"). Tune the wording before M3c shows it to users.
- **Token budget.** The operator's `max_tokens` (16000, M3 spec §3.2) shares its budget with always-on thinking at effort `high`, so truncated answers are likely. M3b or M3c should measure this, consider about 64000 with streaming, and amend the spec.
- **Pending index cap.** During API outages the per-Intent pending index entries grow without bound. Cap them at `maxIndexEntries`.
- **ObserveQuery placement.** `QueryAudit` counts only successful calls. Count every call.
- **Record ConfigMap lifetime.** Record ConfigMaps are never garbage-collected, and CallLLM-only Intents now create them too. This belongs to M5 teardown.
- **Approval replay window.** Approval freshness and UID binding stay with the approval-replay item, to be fixed before M5. So does the replay window that opens after 200 discovery runs on one Intent.
- **Remaining test gaps.**
  - The `DiscoveryHash` error path.
  - `pc.Hash != subject` on its own.
  - Deny wins for Discovery.
  - OpenAI 403 and other 4xx cases.
  - The Anthropic network and cancel paths.
  - The 3 MiB `more` cutoff.
  - The LLM token metrics.

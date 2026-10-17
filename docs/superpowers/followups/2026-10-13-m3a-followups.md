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

- **Fallback text joining.** Pinned in M3c: `TestAnthropicJoinsTheTextOnEitherSideOfAFallback` streams text, a fallback block and more text, and expects the two texts joined in order. The real-model demo could not confirm it, because a benign request never triggers a fallback. If the API ever restarts the answer instead of continuing it, the joined text fails `ParseTurn` and the operator's correction turn recovers.
- **Discovery refusal wording.** Done in M3c: BeginPhase and RunCommand name "this discovery command" for stage Discovery, and a replayed discovery subject says it already ran.
- **Token budget.** Done in M3b. The operator sends `max_tokens` 64000, and M3 spec §3.2 is amended. Still open: OpenAI-compatible models with a smaller output limit reject 64000 with a 400 (`InvalidArgument`), which fails planning. A per-provider limit (`LLMProvider.spec.maxTokens`, or clamping in the adapter) belongs with the first OpenAI-compatible user.
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

## From the final fix-wave re-review

- **Proxy username in network errors.** For both adapters, Go's HTTP client redacts a proxy password in an endpoint URL to `user:***`, but the username can still reach the `llm.response` audit entry. Admins set endpoints, and usernames are not credentials under the constraints, so this is accepted for now.
- **OpenAI proxy password on a malformed endpoint.** If `LLMProviderSpec.endpoint` is malformed (for example a bad port), `http.NewRequestWithContext` fails with a `url.Parse` error that quotes the raw URL, password included. The OpenAI adapter wraps that error into `ErrBadRequest`, so the text can reach the WORM `llm.response` entry. Use a fixed message for this error, and validate `endpoint` at admission (a URL with no user-info). Also check how the Anthropic SDK handles a malformed base URL. Fix before the first release.

# M3: Real LLMs, Clarifying and Discovery — Design

**Status:** approved in brainstorming on 2026-10-05.

**Parent spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (the "main spec"). This document designs milestone M3 (main spec §20). It decides what the main spec leaves open, and the amendments in §9 below carry its decisions back into the main spec. Where the two disagree, the amended main spec is authoritative.

**Inputs:** the M2b and M2c follow-ups (`docs/superpowers/followups/2026-10-05-m2b-followups.md`, `…/2026-10-05-m2c-followups.md`).

## 1. Goal and done-criterion

M3 makes last-operator work against a real model. It adds:

- Anthropic and OpenAI-compatible LLM adapters in the gateway.
- The Clarifying and Discovery phases in the operator.
- The `kubectl last` commands `create` (the bare request form), `answer`, `plan`, `approve` and `deny`.

**Done when:** `kubectl last "run an nginx web server serving a hello page"` reaches Ready on kind against Claude, with the user answering and approving at the CLI's prompts, and `kubectl last audit verify` passes (main spec §20).

**Primary model:** Anthropic Claude. The `anthropic` adapter is built and hardened first. `openai` and `openai-compatible` share one adapter with the same interface and are tested only against fake servers.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Structured turns.** Every model answer is one JSON object, `{"kind": "plan" \| "questions" \| "command", …}`, enforced by a JSON Schema the operator sends through `CallLLM` and validated again by `agent.Parse`. | It is the smallest change to the M2 code and is provider-neutral, and the scripted provider keeps working. Native tool use would need provider-neutral tool-call blocks in `CallLLM`, the audit payloads, the scripted provider and every adapter. Claude Opus 5.5 also rejects forced `tool_choice`, so plain-text answers would need handling anyway. |
| D2 | **The agent loop stays in the operator.** | The trust boundary (main spec §3): the LLM is on the untrusted side, so prompt-injected output can only propose. Running the loop in the gateway was rejected. |
| D3 | **A discovery command is a one-step phase.** `BeginPhase(stage Discovery)` → `RunCommand` → `EndPhase`, reusing the M2b approval, policy, audit and sandbox enforcement. | No new execution path. |
| D4 | **Discovery stays Manual in M3.** Auto modes stay in M5 and allow rules in M6. `kubectl last` prompts for each discovery command inline. | It keeps the main spec's milestone order, and the inline prompt keeps the demo to one interactive session. |
| D5 | **Discovery approvals are single-use.** The subject is `sha256(canonical {command, nonce})`, with a fresh nonce per request. | The gateway's `Used` list then refuses a replayed approval, even for an identical command later in the conversation. |
| D6 | **`QueryAudit` is backed by a per-Intent sequence index** kept in the gateway's per-Intent record. It never scans the chain. | Conversation reload has to be cheap, and the same RPC serves rule suggestions in M7. |
| D7 | **Operator-side caps.** At most 20 model turns and 10 discovery commands per planning session. | They bound runaway agents and cost. They amend main spec §17.2 ("no hard caps"). |
| D8 | **M3 is three plans:** M3a gateway, M3b operator, M3c CLI and demo. | It matches the M2a/b/c split. Each plan delivers testable software, and the interfaces between them are fixed here. |

## 3. Gateway (M3a)

### 3.1 `CallLLM` structured output

`CallLLMRequest` gains `response_schema` (bytes, a JSON Schema document; optional). `CallLLMResponse` gains `request_seq`, the sequence number of the `llm.request` entry for the call; the operator uses it to anchor a planning session (§4.3).

- **The gateway never trusts the result.** It forwards the schema to the adapter and returns the content unchanged. The operator still validates every answer.
- **Audit.** The schema is part of the audited `llm.request` payload, as a SHA-256 hex digest plus its byte length rather than the full text. It is the same for every turn and would bloat the chain.
- **Adapter translation:**
  - `anthropic`: structured outputs (`output_config.format`, JSON-schema type).
  - `openai`, `openai-compatible`: `response_format: {type: json_schema, json_schema: {name, schema, strict: true}}`.
  - `scripted`: ignores the schema.

### 3.2 Anthropic adapter

- **Client.** The official Go SDK (`github.com/anthropics/anthropic-sdk-go`).
- **Model.** `LLMProvider.spec.model`, passed as given. Examples and docs use `claude-opus-5-5`.
- **API key.** Read from the Secret named by `LLMProvider.spec.secretRef` in the gateway namespace, key `apiKey`. It is never logged and never placed in errors or audit payloads.
- **Thinking and effort.** Thinking is left at the model default; on Claude Opus 5.5 it is always on. New optional field `LLMProvider.spec.effort` (`low` | `medium` | `high` | `xhigh` | `max`), sent as `output_config.effort`. The adapter defaults it to `high`: planning is intelligence-sensitive, and Claude Opus 5.5's own default is `medium`.
- **Prompt caching.** The request's `system` message, which is the long and stable planning instructions, is sent as a system block with `cache_control: ephemeral`. Cache read and creation tokens are added to the audited token counts.
- **Streaming.** Requests use the SDK's streaming API and wait for the final message, so long turns don't hit HTTP timeouts. `max_tokens` comes from the request (the operator sends 64000: thinking, always on, shares the budget, so 16000 risked truncated answers at effort `high`).
- **Refusals.** The adapter enables the API's server-side refusal fallback (`fallbacks: "default"` with the `server-side-fallback-2026-07-01` beta), so a false-positive safety decline is re-served by another model in the same call. A refusal that still stands (`stop_reason: refusal`) is an adapter error: "the model declined (category <c>)".
- **Error mapping**, as gRPC codes the operator already classifies:

  | Failure | Code |
  |---|---|
  | 401 or 403 (bad key) | `FailedPrecondition` "authentication failed" |
  | 404 (unknown model) | `FailedPrecondition` |
  | 400 (invalid request) | `InvalidArgument` |
  | Refusal | `FailedPrecondition` |
  | 429, 5xx, network errors (after the SDK's 2 retries) | `Unavailable` |

- **Metrics.** Input and output tokens per provider and model are exported (main spec §16).

### 3.3 OpenAI adapter

One adapter serves both `openai` (fixed endpoint `https://api.openai.com/v1`) and `openai-compatible` (`spec.endpoint`).

- **Transport.** Chat Completions over plain `net/http` with a small typed client. No vendor SDK, which keeps the dependency surface small.
- **Requests.** `response_format` as in §3.1. Token counts come from `usage`.
- **Errors.** The same mapping as §3.2.

### 3.4 Discovery stage

`BeginPhase` accepts stage `Discovery` in addition to `Create`:

- **What the gateway checks.**
  - It reads `Intent.status.pendingCommand`. It requires `pendingCommand.hash == subject` and `sha256(canonical {command: pendingCommand.command, nonce: pendingCommand.nonce}) == subject`.
  - The phase body is a one-step plan whose step is `pendingCommand.command`, with `id` set to `"discovery"`.
  - An `IntentApproval` with stage `Discovery`, this subject, and decision Approve is required. Deny wins.
  - The approver checks and the layered policy are the same as for Create.
  - `image.install` is refused, as for Create until M4.
- **What the operator does next.** `RunCommand` then runs step `"discovery"`, and `EndPhase` closes the phase. A discovery command that exits non-zero is still a completed phase: the operator ends it with `succeeded=false` and feeds the result to the model.
- **What the gateway records.**
  - Discovery phases never set the record's `Executed` plan, so probes are unaffected.
  - Discovery phases enter their own bounded list of used subjects (the last 200), separate from plan subjects (the last 50), so discovery never evicts a plan subject. `phase.begin` records the nonce, so an auditor can recompute the subject.
- **Sandbox priority.** `PriorityDiscovery`, which already exists in the scheduler.
- **Size.** The `pendingCommand` canonical JSON is bounded at 64 KiB. Larger commands are refused with `InvalidArgument`.

### 3.5 `QueryAudit`

New RPC:

```proto
// QueryAudit returns audit entries for one Intent, oldest first (spec §4.2).
rpc QueryAudit(QueryAuditRequest) returns (QueryAuditResponse);

message QueryAuditRequest {
  IntentRef intent = 1;
  repeated string types = 2;   // e.g. "llm.request"; empty means every indexed type
  uint64 after_seq = 3;        // return only entries with seq > after_seq
  int32 limit = 4;             // 1..200; default 50
}
message AuditEntry {
  uint64 seq = 1;
  string type = 2;
  string ts = 3;               // RFC 3339
  string stage = 4;
  string subject_hash = 5;
  bytes payload = 6;           // the entry's payload, canonical JSON
}
message QueryAuditResponse {
  repeated AuditEntry entries = 1;
  bool truncated = 2;          // older matching entries fell out of the index
}
```

- **Index.** In its audit-append path, the gateway appends `{seq, type}` to a per-Intent index in its existing per-Intent record (the ConfigMap in the gateway namespace) after writing an `llm.request`, `llm.response`, `command.start`, `command.result`, `phase.begin` or `phase.end` entry.
  - The index keeps the last 200 entries.
  - If the record would exceed `MaxRecordBytes`, the oldest index entries are dropped first.
  - Index writes are best-effort. A failed index save never fails the audited action, which already happened. The cost is that `QueryAudit` reports `truncated`.
- **Reads.** `QueryAudit` reads each indexed entry from the audit sink by sequence number (the object key is derived from the sequence number) and verifies its hash and signature before returning it. An entry that fails verification fails the RPC with `Internal`. The gateway never returns unverified history.
- **Scope.** A caller may only query the Intent it names. The operator is the only caller (TokenReview), and the index is per-Intent, so there is no cross-Intent path. The payloads are what the audit log already holds: credentials are never in it, and command output is redacted before it is audited (main spec §11).
- **Not audited.** `QueryAudit` is read-only, so it writes no audit entry. It is counted in metrics.

## 4. Operator (M3b)

### 4.1 Turn protocol

The planner's system prompt and `agent.Parse` move from "answer with a plan" to "answer with one turn object":

```json
{"kind": "plan",      "summary": "…", "steps": [ … ], "resources": [ … ]}
{"kind": "questions", "questions": [{"id": "q1", "text": "…", "options": ["…"]}]}
{"kind": "command",   "reason": "…", "command": { …§8.1 command, no id… }}
```

- **`plan`** is validated exactly as in M2 (`checkCommand`, `plan.Validate`, size, hash).
- **`questions`** needs 1–5 questions with unique, non-empty `id`s and non-empty text. `options` are optional.
- **`command`** must pass `checkCommand`, and argv commands may not use `${steps…}` references. It gets `id: "discovery"` before hashing.
- **The schema.** The operator builds the JSON Schema for this union once and sends it as `response_schema`.
  - **One closed object.** OpenAI strict mode needs an object root with every property required, and both providers need closed objects. So the schema is one closed object: `kind` plus every field of every kind. The fields a kind does not use are left empty, and `command` is null.
  - **A wire format of its own.** The JSON the model writes differs from the CRD's:
    - `run` is flattened to `argv` and `script`;
    - the maps `files` and `outputs` are arrays of `{path, content}` and `{key, value}`;
    - the free-form `identity` and `desired` are JSON objects written as strings;
    - `outputsSecretRef` is the string `outputsSecret`.
  - **Lenient parsing.** The parser accepts omitted fields and `null`, for models that do not enforce the schema.
- **Malformed answers.** They remain correction turns, at most 3 in a row (`agent.MaxTurns` becomes the per-answer correction budget).

### 4.2 Planning session

A **planning session** is one conversation. It starts on a new Intent, on a `spec.request` edit, or on a replan after a step failure (with the failure as feedback, as in M2), and it ends when a plan is written. In it, the operator:

1. Sends the conversation and receives one turn.
2. On `plan`, writes `status.plan` and moves to AwaitingApproval. This is unchanged from M2.
3. On `questions`, writes `status.questions` and moves to **Clarifying**.
   - When `spec.answers` holds an answer for every question `id`, the operator appends a user turn, "Answers: q1: …", clears `status.questions`, and returns to Planning in the same session.
   - The `generation` bump from the answers does not restart the session. Only a changed `spec.request` does. The operator keeps the last `spec.request` it planned for, in `status.observedRequestHash`.
  - Outside Clarifying, any `generation` change behaves as in M2: in Planning and AwaitingApproval it restarts planning with a new session, while Executing and Ready ignore it until M5, and Failed retries.
4. On `command`, picks a random 128-bit nonce, writes `status.pendingCommand` `{hash, command, reason, nonce}` with `stage: Discovery`, and moves to **AwaitingDiscoveryApproval**. Then:
   - **Approve** (matching subject, stage Discovery): the operator runs the one-step phase (§3.4). It uses the same `Executed`-condition checkpoints as M2 (never resume; abandon a lost phase). It appends the result as a user turn (exit code, stdout and stderr clipped to 16 KiB each, with truncation flagged), clears `pendingCommand`, and returns to Planning.
   - **Deny:** the operator appends "The command was denied: <reason>" and returns to Planning. The model may try something else or ask a question (main spec §5.2).
5. **Caps.** On the 21st model turn or the 11th discovery command in one session, the Intent fails with `PlanningFailed` ("planning exceeded N turns").

**Between turns.**
- **Owed turns.** Returning to Planning keeps `status.questions` or `status.pendingCommand` until the model's next turn replaces them. How a discovery command ended is recorded in the `Executed` condition: `DiscoveryRan`, `DiscoveryDenied` or `DiscoveryFailed`. A restart can then re-derive the user turn the model is owed (§4.3).
- **Reused question ids.** A question whose `id` is already in `spec.answers` is refused with a correction, because its old answer would answer it at once.
- **Lost discovery phases.** A Discovery phase found open after a restart is ended as failed and reported to the model as lost.
- **Refusals.** A gateway refusal of an approved command is reported as "The command did not run: …".

### 4.3 Conversation memory and reload

- **In memory.** Conversations live in memory, keyed by Intent UID and session.
- **Reload.** On a miss (an operator restart or a leader change), the operator rebuilds the conversation from `QueryAudit`:
  - the session's latest `llm.request` whose `llm.response` holds a valid turn gives the message list;
  - that `llm.response` gives the assistant turn;
  - the pending user turn follows (below). A discovery result is read from the command's `command.result` entry.

  The session's start sequence number (the `request_seq` of its first `CallLLM`) is kept in `status.planningSessionSeq`, so the operator never mixes in an earlier session.
- **The pending user turn.** The turn not yet sent to the model is re-derived from the Intent's state: answers from `spec.answers` with `status.questions`, a denial or failure from the `Executed` condition's message, or a command result from its `command.result` entry.
- **Unrecoverable reload.** If the conversation can't be rebuilt (`truncated`, or entries missing), the operator starts a new session, and the transcript notes the restart. The session's first `llm.request` must still be in the index, which catches the loss of the session's start. The rebuilt conversation must also end with the turn the Intent's status records (the same questions, or the pending command with its nonce); a later answer the operator never recorded, a lost index tail, or an answer the operator refused all fall back to the latest matching answer, or to a new session when none matches.

### 4.4 Context in the first message

The first user message of a session lists:

- the request and any answers;
- the Providers the namespace may use (the effective policy intersected with the cluster's `Provider` objects), each with its `description`;
- the effective egress allowances.

This needs operator RBAC `get`, `list` and `watch` on `providers`, the only RBAC change in M3.

### 4.5 Status fields

New `IntentStatus` fields:

- `observedRequestHash` (string): the SHA-256 of `spec.request` for the current session.
- `planningSessionSeq` (uint64): the audit sequence number where the session's first `llm.request` landed.

`PendingCommand` gains `nonce` (string).

`Clarifying` and `AwaitingDiscoveryApproval`, and the `questions` and `pendingCommand` fields, already exist in the API.

## 5. CLI (M3c)

Every command acts as the user, with their kubeconfig identity. The CLI never talks to the gateway.

- **`kubectl last "<request>" [-n NS] [--name NAME] [--no-follow]`**
  - Creates an Intent. The default name is a DNS-safe slug of the first words of the request plus a 4-character random suffix.
  - Then follows it with a watch:
    - **Clarifying:** prints each question; a numbered menu when it has `options`, otherwise a free-text line. Then patches `spec.answers`.
    - **AwaitingDiscoveryApproval:** prints the command (argv or script, files listed by path with their size, Providers, egress, the model's reason) and asks `Approve? [y/N/d]`. `y` approves, `d` denies after asking for a reason, and `N` or Enter leaves it pending and keeps following.
    - **AwaitingApproval:** prints the plan summary, each step (reason, argv or script, files by path, Providers, egress) and each resource with its probes and outputs, then asks the same prompt.
    - **Executing and Verifying:** prints phase changes.
    - **Ready:** prints the ManagedResources and output Secret names, then exits 0.
    - **Failed:** prints the reason and message, then exits 1.
  - Ctrl-C stops following; it never cancels the Intent. `--no-follow` prints the Intent's name and exits.
- **`kubectl last answer NAME`** prompts for the open questions.
- **`kubectl last plan NAME`** prints the pending command or the plan, read-only.
- **`kubectl last approve NAME`** and **`kubectl last deny NAME --reason R`** approve or deny exactly what `plan` shows.

**How approvals are made.** `approve` and `deny` create an `IntentApproval` for **the hash they displayed**: the `pendingCommand` hash with stage Discovery, or the plan hash with stage Create.
- `approvedBy` is the user's name from a `SelfSubjectReview`.
- If the subject changed after it was displayed, the approval matches nothing and the CLI says so.
- They print the hash they acted on.

**Output** is plain text, with no TUI library. The prompts read lines from stdin, so the scripted e2e can drive them.

**Not in M3:** `approve --always-allow` (M6) and `rules suggestions` (M7).

## 6. Errors

| Condition | Result |
|---|---|
| Model API transient error (429, 5xx, network, after the SDK's retries) | `Unavailable`. The operator retries with backoff, and the Intent stays in Planning. |
| Bad key, unknown model, refusal | `FailedPrecondition`. The Intent fails with `PlanningFailed` and the adapter's message (never the key). |
| Malformed turn | A correction turn, at most 3 in a row. |
| Discovery command exits non-zero | The phase ends `succeeded=false`. The result is fed to the model, and the Intent stays in Planning. |
| Discovery phase refused (policy, approval withdrawn) | The phase ends failed. The refusal is fed to the model as a denial. |
| Turn or command cap reached | The Intent fails with `PlanningFailed`. |
| `QueryAudit` reload incomplete | A new session starts, noted in the transcript. |

## 7. Security

- The LLM stays on the untrusted side. In M3, model output only becomes a proposed command or plan that a human approves, and the gateway re-checks every phase.
- Prompt injection through discovery output (for example a malicious ConfigMap) can only make the model propose something.
- The API key stays in the gateway.
- Every turn is audited (`llm.request`/`llm.response`).
- `QueryAudit` returns only the named Intent's verified entries, and only to the operator.
- The new operator RBAC is read-only on Providers.

## 8. Testing

- **Adapters.** `httptest` fake servers check:
  - the request shape: schema, cache control, effort, model, auth header, and the fallback beta;
  - response and usage parsing;
  - the refusal path;
  - every error mapping.

  No real API calls in CI.
- **Gateway** (following the M2b test patterns):
  - Discovery phases: subject and nonce checks, approval stage, single use through `Used`, `Executed` untouched.
  - `QueryAudit`: index bound and truncation, ordering, signature verification, isolation between Intents, best-effort index saves.
- **Operator** (envtest with the fake gateway):
  - each turn kind;
  - the Clarifying round trip, where the answers' generation bump doesn't restart the session;
  - discovery approve, deny and failure;
  - the caps;
  - reload from a fake `QueryAudit` after a simulated restart;
  - Provider context in the first message.
- **CLI.** Unit tests for rendering, prompt handling (scripted stdin), approval creation for the displayed hash, and slugging.
- **e2e (CI, kind, scripted LLM).** The web-server e2e is driven through `kubectl last` with scripted stdin. The script asks one clarifying question, runs one discovery command, then plans. The e2e checks the answers, the discovery approval, Ready, and `audit verify`.
- **Demo (opt-in, never in CI).** `hack/demo.sh` installs on kind with an `anthropic` LLMProvider. The key comes from the developer's environment (`ANTHROPIC_API_KEY`) into a gateway Secret. It then runs the done-criterion interactively. The first successful transcript is recorded in the M3 follow-ups.

## 9. Main-spec amendments

The M3 plans apply these to `docs/superpowers/specs/2026-09-26-last-operator-design.md`:

- **§4.1 RBAC:** add `get/list/watch` on `Provider`.
- **§4.2 API table:** `QueryAudit` takes an Intent, types and a sequence cursor, and returns verified entries from a per-Intent index. It is also used for conversation reload.
- **§5.1 Intent:**
  - `pendingCommand.nonce`;
  - `status.observedRequestHash`;
  - `status.planningSessionSeq`;
  - answers bump `generation` without restarting a planning session.
- **§5.6 LLMProvider:** `spec.effort`; the `secretRef` key name `apiKey`; `claude-opus-5-5` in the example.
- **§7:** the turn protocol, Clarifying and Discovery as designed in §4.2, the caps, and reload through `QueryAudit` replacing the M2 "starts over" note.
- **§8.3:** Discovery subjects are `sha256(canonical {command, nonce})` and are single-use.
- **§12:** `CallLLM` carries `response_schema`; adapter behaviour as in §3.2–3.3.
- **§17.2:** replace "No hard caps on agent turns or cost" with the operator-side caps (D7).
- **§20:** M3 is delivered as M3a, M3b and M3c.

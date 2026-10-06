# M3a: Gateway for real LLMs, Discovery and QueryAudit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the gateway everything M3's operator needs:
- real model calls, through an Anthropic adapter and an OpenAI/OpenAI-compatible adapter;
- schema-constrained answers;
- one-step Discovery phases with single-use approvals;
- a read-only `QueryAudit` RPC so the operator can rebuild a planning conversation after a restart.

**Architecture:**
- **`CallLLM`:** it gains `response_schema` and `request_seq`. Adapters turn the schema into the provider's structured-output mode, and the operator still validates every answer.
- **Discovery:** `BeginPhase` accepts stage `Discovery`. The subject is `sha256(canonical {command, nonce})` of `Intent.status.pendingCommand`. It reuses the M2b approval, policy, sandbox and audit path, and never touches the executed plan.
- **`QueryAudit`:** it serves verified entries from a per-Intent sequence-number index. A hook on the audit writer collects index entries in memory, and a flusher persists them into the gateway's existing per-Intent record.

**Tech Stack:**
- Go 1.26
- `github.com/anthropics/anthropic-sdk-go` v1.78.0 (beta Messages API, streaming)
- `net/http` for OpenAI
- gRPC + buf
- client-go fake clientset, `httptest`

**Spec:**
- `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md` (the "M3 spec"), §3 and §9.
- Main spec: `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

**Scope.** This is the first of three M3 plans:
- **M3a (this plan):** the gateway.
- **M3b:** the operator's turn protocol, Clarifying, Discovery and reload.
- **M3c:** `kubectl last` and the real-model demo.

Nothing in M3a changes operator behaviour. It ships API fields and RPCs that M3b uses.

**Rulings this plan makes:**
1. **`QueryAuditResponse` gains `bool more = 3`.** This is not in the M3 spec. Responses stop at `limit` entries or about 3 MiB of payload, and `more` tells the caller to page with `after_seq` set to the last sequence number returned. Without it, a few large `llm.request` entries could exceed the 4 MiB default gRPC receive size.
2. **`LLMProvider.spec.endpoint` also overrides the Anthropic API base URL when set.** The spec mentions `endpoint` only for `openai-compatible`. This allows a proxy, and it lets the gateway tests point the adapter at an `httptest` server.
3. **Truncation reporting.** The index records `IndexFloor`, the highest sequence number ever dropped from it, and `QueryAudit` reports `truncated` when `after_seq < IndexFloor`. Index entries lost by a crash before a flush are not detectable. The operator treats missing entries as an incomplete reload (M3 spec §4.3).
4. **The OpenAI adapter does not retry.** A transient failure maps to `Unavailable`, and the operator retries with backoff. The Anthropic adapter keeps the SDK's 2 retries, which the spec mandates.

## Global Constraints

- Module `github.com/moogie-labs/last-operator`; API group `last.moogie-labs.com/v1alpha1`.
- **Fail closed.** Every side effect is preceded by its audit entry. `CallLLM` writes `llm.request` before calling the provider (the existing `w.Do`). If the writer is missing or broken, the RPC returns `Unavailable` and does nothing.
- **API keys:**
  - read only from the LLMProvider's Secret in the gateway namespace, key `apiKey`, with surrounding whitespace trimmed;
  - never logged;
  - never in errors, audit payloads or metrics labels.
- **The gateway trusts no model output.** Content is returned unchanged; the schema is forwarded only to constrain the provider.
- **gRPC status codes (M2b table plus these rows):**

  | Condition | Code |
  |---|---|
  | Provider auth failure (401/403), unknown model (404), refusal, missing key | `FailedPrecondition` |
  | Provider rejected the request (other 4xx) | `InvalidArgument` |
  | Provider 429, 5xx, network error | `Unavailable` |
  | Bad `response_schema` (not JSON, or over 64 KiB) | `InvalidArgument` |
  | Unknown QueryAudit type, `limit` outside 0..200 | `InvalidArgument` |
  | A stored entry fails verification or doesn't match the index | `Internal` |
  | Audit sink unreachable during QueryAudit | `Unavailable` |
- **Anthropic requests:**
  - beta Messages API, streaming;
  - `max_tokens` from the request, defaulting to 16000 when 0;
  - `output_config.effort` from `LLMProvider.spec.effort`, defaulting to `high`;
  - `output_config.format` = `{type: json_schema, schema}` when a schema is given;
  - the last system block carries `cache_control: ephemeral`;
  - `fallbacks: "default"` with beta `server-side-fallback-2026-07-01`.
- **Model IDs are passed as given.** Docs and examples use `claude-opus-5-5`.
- **Discovery:**
  - stage `Discovery`;
  - step ID `discovery`;
  - subject `sha256(canonical {command, nonce})`;
  - a pending command's canonical JSON is at most 64 KiB;
  - Discovery phases never set `Executed`, but they do enter `Used`.
- **QueryAudit:**
  - indexed types are `llm.request`, `llm.response`, `command.start`, `command.result`, `phase.begin` and `phase.end`;
  - at most 200 index entries per Intent;
  - `limit` defaults to 50 and allows 1..200;
  - entries are oldest first;
  - every entry is verified (strict decode and signature) and must match the index's sequence number, the requested Intent and the type.
- **Logging.** Kubernetes message style: capital letter, no trailing period, past tense, balanced key/value pairs (`slog`).
- **Tests.** Plain `testing`. No real API calls; adapters are tested against `httptest` servers.
- **After each task:**
  - run `make lint-fix && make test && make lint`;
  - after API or proto edits, also run `make manifests generate helm-crds` and `make proto`, then commit the regenerated files.
- **Commits.** No `Co-authored-by` or other attribution trailers. Never commit `.superpowers/`.

## Review Focus

Each item names the task whose tests pin it:

1. **Truncated output.** The model stops at `max_tokens` with partial JSON. The adapter returns the partial content without an error, so the operator's parser turns it into a correction turn rather than an outage. *Task 3: "returns partial content at max_tokens".*
2. **Untidy keys.** An API key stored with a trailing newline (`kubectl create secret --from-file`, `echo`) works, because it is trimmed. A missing or empty key is `FailedPrecondition`, and the error never contains the key. *Task 3: "the API key is read from the Secret and trimmed", "a missing key never appears in the error".*
3. **Reused approvals.** An approval for one discovery nonce can't be reused for the same command under a new nonce, and the same subject can't run twice. *Task 5: "approvals are single-use".*
4. **Odd QueryAudit queries.** A cursor beyond the head returns nothing (no error). An unknown type is `InvalidArgument`. Large results page with `more`. *Task 6: "after_seq beyond the head is empty", "limit pages with more".*
5. **Restarts and isolation.** After a gateway restart, QueryAudit serves the persisted index and never errors. Another Intent's entries are never returned. *Task 6: "the index survives a restart", "other intents' entries are never returned".*

---

### Task 1: API fields and the discovery subject

**Files:**
- Modify: `api/v1alpha1/llmprovider_types.go`
- Modify: `api/v1alpha1/intent_types.go`
- Create: `internal/plan/discovery.go`
- Test: `internal/plan/discovery_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§5.1, §5.6, §20)
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*`, `charts/last-operator/crds/*`

**Interfaces:**
- Produces the API types and fields:
  - `lastv1alpha1.LLMEffort` (string enum `low|medium|high|xhigh|max`) and the field `LLMProviderSpec.Effort LLMEffort` (JSON `effort`);
  - `PendingCommand.Nonce string` (JSON `nonce`);
  - `IntentStatus.ObservedRequestHash string` (JSON `observedRequestHash`) and `IntentStatus.PlanningSessionSeq int64` (JSON `planningSessionSeq`).
- Produces the discovery subject helpers in `internal/plan`:
  - `const plan.DiscoveryStepID = "discovery"`;
  - `const plan.MaxPendingCommandBytes = 64 << 10`;
  - `func plan.DiscoveryHash(c lastv1alpha1.Command, nonce string) (hash string, size int, err error)`, where `hash` is `"sha256:<hex>"` of the RFC 8785 canonical JSON of `{"command": c, "nonce": nonce}` and `size` is that JSON's length.

- [ ] **Step 1: Write the failing test**

Create `internal/plan/discovery_test.go`:

```go
package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
)

func discovery() lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: plan.DiscoveryStepID, Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "ns"}}}
}

func TestDiscoveryHashIsSHA256OfCommandAndNonce(t *testing.T) {
	h, size, err := plan.DiscoveryHash(discovery(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := audit.Canonical(map[string]any{"command": discovery(), "nonce": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if h != "sha256:"+hex.EncodeToString(sum[:]) || size != len(canonical) {
		t.Fatalf("DiscoveryHash = %s, %d; want the hash and size of %s", h, size, canonical)
	}
}

func TestDiscoveryHashDependsOnNonceAndCommand(t *testing.T) {
	a, _, _ := plan.DiscoveryHash(discovery(), "n1")
	b, _, _ := plan.DiscoveryHash(discovery(), "n2")
	other := discovery()
	other.Run.Argv = []string{"kubectl", "get", "pods"}
	c, _, _ := plan.DiscoveryHash(other, "n1")
	again, _, _ := plan.DiscoveryHash(discovery(), "n1")
	if a == b || a == c || a != again || !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("hashes: same=%s nonce=%s command=%s again=%s", a, b, c, again)
	}
}
```

Run: `go test ./internal/plan/ -run Discovery`
Expected: FAIL to compile, with `undefined: plan.DiscoveryStepID`.

- [ ] **Step 2: Implement the helper**

Create `internal/plan/discovery.go`:

```go
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

// DiscoveryStepID is the step ID every discovery command carries (M3 spec §3.4).
const DiscoveryStepID = "discovery"

// MaxPendingCommandBytes bounds a discovery command's canonical JSON.
const MaxPendingCommandBytes = 64 << 10

type discoverySubject struct {
	Command lastv1alpha1.Command `json:"command"`
	Nonce   string               `json:"nonce"`
}

// DiscoveryHash returns the approval subject of a discovery command,
// "sha256:<hex>" of the RFC 8785 canonical JSON of {command, nonce}, and that
// JSON's size. A fresh nonce per request makes every request a new subject, so
// an approval is good for exactly one run.
func DiscoveryHash(c lastv1alpha1.Command, nonce string) (string, int, error) {
	b, err := audit.Canonical(discoverySubject{Command: c, Nonce: nonce})
	if err != nil {
		return "", 0, fmt.Errorf("plan: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), len(b), nil
}
```

Run: `go test ./internal/plan/ -v`
Expected: PASS.

- [ ] **Step 3: Add the API fields**

In `api/v1alpha1/llmprovider_types.go`, after the `LLMProviderType` constants, add:

```go
// LLMEffort is how much reasoning effort a model spends on each answer.
// +kubebuilder:validation:Enum=low;medium;high;xhigh;max
type LLMEffort string
```

In `LLMProviderSpec`, replace the `SecretRef` comment and add `Effort` after `Scripted`:

```go
	// SecretRef names a Secret in last-gateway holding the API key under the key "apiKey".
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// +optional
	Scripted *ScriptedLLM `json:"scripted,omitempty"`
	// Effort is the model's reasoning effort, for adapters that support it
	// (anthropic). Empty means high: planning is intelligence-sensitive.
	// +optional
	Effort LLMEffort `json:"effort,omitempty"`
```

Also extend the `Endpoint` comment to: `// Endpoint is the API base URL: required for openai-compatible; optional for openai and anthropic (a proxy).`

In `api/v1alpha1/intent_types.go`, add to `PendingCommand` after `Command`:

```go
	// Nonce makes this request its own approval subject: Hash is the SHA-256 of
	// the canonical JSON of {command, nonce}, so an approval is single-use.
	// +optional
	Nonce string `json:"nonce,omitempty"`
```

Add to `IntentStatus` after `ObservedGeneration`:

```go
	// ObservedRequestHash is the SHA-256 of spec.request for the current planning
	// session; answers to clarifying questions do not start a new session.
	// +optional
	ObservedRequestHash string `json:"observedRequestHash,omitempty"`
	// PlanningSessionSeq is the audit sequence number of the current planning
	// session's first llm.request; conversation reload reads from there.
	// +optional
	PlanningSessionSeq int64 `json:"planningSessionSeq,omitempty"`
```

Run: `make manifests generate helm-crds`
Expected: deepcopy, the config CRDs and the chart CRDs regenerate with `effort`, `nonce`, `observedRequestHash` and `planningSessionSeq`.

- [ ] **Step 4: Amend the main spec**

In §5.1, in the YAML example, replace the `pendingCommand:` block with:

```yaml
  pendingCommand:                # set while a discovery command awaits approval
    hash: "sha256:…"             # sha256 of canonical {command, nonce}: single-use
    nonce: "…"
    command: { … §8.1 command, id: discovery … }
    reason: "Find the cluster's default IngressClass"
```

and add after `observedGeneration: 3`:

```yaml
  observedRequestHash: "sha256:…" # spec.request of the current planning session
  planningSessionSeq: 1042        # audit seq of the session's first llm.request
```

In §5.6, in the `LLMProvider` YAML, replace the `model:` line and `secretRef:` line with:

```yaml
  model: "claude-opus-5-5"
  effort: high                  # optional: low | medium | high | xhigh | max (anthropic)
  endpoint: "…"                 # required for openai-compatible; optional proxy for openai/anthropic
  secretRef: { name: anthropic-key }   # Secret in last-gateway, key "apiKey"
```

(Remove the old `endpoint:` line, so `endpoint` appears once.)

In §20, change the M3 row's Scope cell to begin with "Delivered as M3a (gateway), M3b (operator), M3c (CLI and demo); design: `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`." and keep the rest of the cell.

- [ ] **Step 5: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add api/ config/crd/ charts/last-operator/crds/ internal/plan/discovery.go internal/plan/discovery_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(api): add the M3 status fields, LLM effort, and the discovery subject"
```

---

### Task 2: `CallLLM` structured output, request sequence, and error mapping

**Files:**
- Modify: `proto/last/gateway/v1/gateway.proto`; regenerate `internal/gatewayapi/gatewayv1/*`
- Modify: `internal/gateway/llm/llm.go`
- Modify: `internal/gateway/llmrpc.go`
- Modify: `internal/gateway/metrics.go`
- Test: `internal/gateway/llmrpc_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§12)

**Interfaces:**
- Produces the proto fields `CallLLMRequest.response_schema` (bytes, field 4) and `CallLLMResponse.request_seq` (uint64, field 4).
- Produces in `llm`:
  - `Request.Schema []byte`;
  - `Response.CacheReadTokens int` and `Response.CacheCreationTokens int`;
  - the errors `ErrAuth`, `ErrModel`, `ErrBadRequest`, `ErrRefused` and `ErrTransient`. Adapters wrap them with `%w`.
- Produces in `gateway`:
  - `func llmStatus(err error) error`, which maps adapter errors to gRPC status;
  - `(*Metrics).ObserveLLM(provider, model string, r llm.Response, err error)`;
  - `maxSchemaBytes = 64 << 10`.

- [ ] **Step 1: Extend the proto**

In `proto/last/gateway/v1/gateway.proto`, replace `CallLLMRequest` and `CallLLMResponse` with:

```proto
// CallLLMRequest is a provider-neutral LLM request.
message CallLLMRequest {
  IntentRef intent = 1;
  repeated Message messages = 2;
  int32 max_tokens = 3;
  // JSON Schema the answer must match. Adapters that support structured output
  // enforce it; the caller validates the answer either way. Optional.
  bytes response_schema = 4;
}

// CallLLMResponse is the model's reply.
message CallLLMResponse {
  string content = 1;
  int32 input_tokens = 2;
  int32 output_tokens = 3;
  // Sequence number of this call's llm.request audit entry.
  uint64 request_seq = 4;
}
```

Run: `make proto`
Expected: buf lint passes and `internal/gatewayapi/gatewayv1/gateway.pb.go` regenerates with `ResponseSchema` and `RequestSeq`.

- [ ] **Step 2: Write the failing tests**

Append to `internal/gateway/llmrpc_test.go` (add `crypto/sha256`, `encoding/hex`, `errors`, `fmt`, `github.com/prometheus/client_golang/prometheus/testutil`, and `github.com/moogie-labs/last-operator/internal/gateway/llm` to its imports):

```go
const answerSchema = `{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"],"additionalProperties":false}`

func TestCallLLMAuditsTheSchemaDigestAndReturnsTheRequestSeq(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	resp, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), MaxTokens: 1024, ResponseSchema: []byte(answerSchema),
		Messages: []*gatewayv1.Message{{Role: roleUser, Content: "run nginx"}}})
	if err != nil {
		t.Fatal(err)
	}
	es := env.entries()
	req := es[len(es)-2]
	if req.Type != audit.TypeLLMRequest || resp.GetRequestSeq() != req.Seq {
		t.Fatalf("request_seq = %d, want the llm.request seq %d", resp.GetRequestSeq(), req.Seq)
	}
	var p llmRequestPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(answerSchema))
	if p.SchemaSHA256 != hex.EncodeToString(sum[:]) || p.SchemaBytes != len(answerSchema) {
		t.Fatalf("llm.request schema digest = %q (%d bytes)", p.SchemaSHA256, p.SchemaBytes)
	}
	if strings.Contains(string(req.Payload), `"additionalProperties"`) {
		t.Fatal("the full schema was written to the audit log")
	}
}

func TestCallLLMRefusesABadSchemaBeforeAuditing(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	before := len(env.entries())
	for name, schema := range map[string][]byte{
		"not json": []byte("{"),
		"too big":  []byte(`"` + strings.Repeat("x", maxSchemaBytes) + `"`),
	} {
		_, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), ResponseSchema: schema,
			Messages: []*gatewayv1.Message{{Role: roleUser, Content: "x"}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if len(env.entries()) != before {
		t.Fatal("a refused request was audited")
	}
}

func TestLLMStatusMapsAdapterErrors(t *testing.T) {
	cases := map[error]codes.Code{
		fmt.Errorf("%w (HTTP 401)", llm.ErrAuth):       codes.FailedPrecondition,
		fmt.Errorf("%w (HTTP 404)", llm.ErrModel):      codes.FailedPrecondition,
		fmt.Errorf("%w (category cyber)", llm.ErrRefused): codes.FailedPrecondition,
		llm.ErrNoScript:                                codes.FailedPrecondition,
		llm.ErrScriptExhausted:                         codes.FailedPrecondition,
		fmt.Errorf("%w: bad schema", llm.ErrBadRequest): codes.InvalidArgument,
		fmt.Errorf("%w (HTTP 529)", llm.ErrTransient):  codes.Unavailable,
		errors.New("connection reset"):                 codes.Unavailable,
	}
	for err, want := range cases {
		if got := status.Code(llmStatus(err)); got != want {
			t.Errorf("llmStatus(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestCallLLMCountsCallsAndTokens(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(env.g.metrics.llmCalls.WithLabelValues("fixture", "m", "ok")); got != 1 {
		t.Fatalf("llm calls = %v, want 1", got)
	}
}
```

Run: `go test ./internal/gateway/ -run 'CallLLM|LLMStatus'`
Expected: FAIL to compile, with `p.SchemaSHA256 undefined`, `undefined: maxSchemaBytes`, `undefined: llmStatus`, `undefined: llm.ErrAuth`, and `env.g.metrics.llmCalls undefined`.

- [ ] **Step 3: Extend the `llm` package**

Replace `internal/gateway/llm/llm.go` with:

```go
// Package llm is the gateway's provider-neutral LLM interface and its adapters (spec §12).
package llm

import (
	"context"
	"errors"
)

// Message is one conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a provider-neutral completion request.
type Request struct {
	Intent    string // "<namespace>/<name>"
	Messages  []Message
	MaxTokens int
	// Schema, when set, is a JSON Schema the answer must match. Adapters that
	// support structured output enforce it; the caller validates the answer anyway.
	Schema []byte
}

// Response is the model's reply.
type Response struct {
	Content             string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// Provider completes conversations; adapters translate for each vendor.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Errors adapters wrap (with %w) so the gateway can map them to gRPC codes.
// Messages never include credentials.
var (
	ErrAuth       = errors.New("llm: the provider rejected the credentials")
	ErrModel      = errors.New("llm: the provider does not know the model")
	ErrBadRequest = errors.New("llm: the provider rejected the request")
	ErrRefused    = errors.New("llm: the model declined")
	ErrTransient  = errors.New("llm: the provider is unavailable")
)
```

- [ ] **Step 4: Plumb the schema, sequence number and metrics**

In `internal/gateway/llmrpc.go`:

(a) Add the fields to the payloads:

```go
type llmRequestPayload struct {
	Provider  string        `json:"provider"`
	Model     string        `json:"model"`
	Messages  []llm.Message `json:"messages"`
	MaxTokens int           `json:"maxTokens,omitempty"`
	// The response schema is the same on every turn; the log keeps its digest only.
	SchemaSHA256 string `json:"schemaSha256,omitempty"`
	SchemaBytes  int    `json:"schemaBytes,omitempty"`
}

type llmResponsePayload struct {
	RequestSeq          uint64 `json:"requestSeq"`
	Content             string `json:"content"`
	InputTokens         int    `json:"inputTokens"`
	OutputTokens        int    `json:"outputTokens"`
	CacheReadTokens     int    `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int    `json:"cacheCreationTokens,omitempty"`
	Error               string `json:"error,omitempty"`
}

// maxSchemaBytes bounds CallLLMRequest.response_schema.
const maxSchemaBytes = 64 << 10
```

(b) In `callLLM`, right after the messages loop (before reading the Intent), validate the schema:

```go
	schema := req.GetResponseSchema()
	if len(schema) > maxSchemaBytes {
		return nil, status.Errorf(codes.InvalidArgument, "response_schema is %d bytes; the limit is %d", len(schema), maxSchemaBytes)
	}
	if len(schema) > 0 && !json.Valid(schema) {
		return nil, status.Error(codes.InvalidArgument, "response_schema is not JSON")
	}
```

(c) Build the request and payload with the schema, capture the request sequence number, and record metrics. Replace everything from `lreq := llm.Request{…}` to the end of the function with:

```go
	lreq := llm.Request{Intent: key, Messages: msgs, MaxTokens: int(req.GetMaxTokens()), Schema: schema}
	reqPayload := llmRequestPayload{Provider: lp.Name, Model: lp.Spec.Model, Messages: msgs, MaxTokens: lreq.MaxTokens}
	if len(schema) > 0 {
		sum := sha256.Sum256(schema)
		reqPayload.SchemaSHA256, reqPayload.SchemaBytes = hex.EncodeToString(sum[:]), len(schema)
	}
	var resp llm.Response
	var requestSeq uint64
	err = w.Do(ctx,
		audit.Draft{Type: audit.TypeLLMRequest, Actor: actor, Intent: key, Payload: reqPayload},
		func(ctx context.Context) (any, error) {
			r, err := provider.Complete(ctx, lreq)
			resp = r
			return r, err
		},
		func(before audit.Entry, _ any, actionErr error) audit.Draft {
			requestSeq = before.Seq
			p := llmResponsePayload{RequestSeq: before.Seq, Content: resp.Content, InputTokens: resp.InputTokens,
				OutputTokens: resp.OutputTokens, CacheReadTokens: resp.CacheReadTokens, CacheCreationTokens: resp.CacheCreationTokens}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			return audit.Draft{Type: audit.TypeLLMResponse, Actor: actor, Intent: key, Payload: p}
		})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		g.metrics.ObserveLLM(lp.Name, lp.Spec.Model, resp, err)
	}
	switch {
	case err == nil:
		return &gatewayv1.CallLLMResponse{Content: resp.Content, InputTokens: int32(resp.InputTokens), //nolint:gosec // token counts fit
			OutputTokens: int32(resp.OutputTokens), RequestSeq: requestSeq}, nil //nolint:gosec // token counts fit
	case errors.Is(err, audit.ErrAuditUnavailable):
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	default:
		return nil, llmStatus(err)
	}
}

// llmStatus maps an adapter error to a gRPC status the operator classifies:
// errors retrying cannot fix are FailedPrecondition or InvalidArgument;
// everything else is Unavailable and retried with backoff.
func llmStatus(err error) error {
	switch {
	case errors.Is(err, llm.ErrNoScript), errors.Is(err, llm.ErrScriptExhausted),
		errors.Is(err, llm.ErrAuth), errors.Is(err, llm.ErrModel), errors.Is(err, llm.ErrRefused):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, llm.ErrBadRequest):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	default:
		return status.Errorf(codes.Unavailable, "LLM call failed: %v", err)
	}
}
```

Add `crypto/sha256`, `encoding/hex` and `encoding/json` to the imports.

In `internal/gateway/metrics.go`, add two counter vectors to `Metrics`, register them in `NewMetrics`, and add `ObserveLLM`:

```go
	llmCalls  *prometheus.CounterVec
	llmTokens *prometheus.CounterVec
```

```go
		llmCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_llm_calls_total", Help: "LLM calls by provider, model and result (ok or error).",
		}, []string{"provider", "model", "result"}),
		llmTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_llm_tokens_total", Help: "LLM tokens by provider, model and kind (input, output, cache_read, cache_creation).",
		}, []string{"provider", "model", "kind"}),
```

Change `m.registry.MustRegister(m.writes, m.failures, m.seq)` to `m.registry.MustRegister(m.writes, m.failures, m.seq, m.llmCalls, m.llmTokens)`, then add:

```go
// ObserveLLM counts one provider call and the tokens it used (spec §16).
func (m *Metrics) ObserveLLM(provider, model string, r llm.Response, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.llmCalls.WithLabelValues(provider, model, result).Inc()
	for kind, n := range map[string]int{"input": r.InputTokens, "output": r.OutputTokens,
		"cache_read": r.CacheReadTokens, "cache_creation": r.CacheCreationTokens} {
		if n > 0 {
			m.llmTokens.WithLabelValues(provider, model, kind).Add(float64(n))
		}
	}
}
```

Import `github.com/moogie-labs/last-operator/internal/gateway/llm` in metrics.go.

Run: `go test ./internal/gateway/ -run 'CallLLM|LLMStatus' -v`
Expected: PASS, including the existing `TestCallLLMAuditsRequestAndResponse` and `TestCallLLMRefusals`.

- [ ] **Step 5: Amend the main spec**

In §12, replace the first paragraph with:

```markdown
`CallLLM` takes a provider-neutral request: messages, max tokens, and an
optional `response_schema` (a JSON Schema the answer must match). Adapters
in the gateway translate it for each provider: `anthropic`, `openai`, and
`openai-compatible` (for local and self-hosted models). Adapters that
support structured output enforce the schema; the operator validates every
answer either way. Every request and response is audited
(`llm.request` / `llm.response`; the schema is recorded as a SHA-256 digest
and size), so the audit log also serves as the complete transcript store.
`CallLLM` returns the `llm.request` entry's sequence number, which anchors
conversation reload (§7).
```

- [ ] **Step 6: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add proto/ internal/gatewayapi/ internal/gateway/llm/llm.go internal/gateway/llmrpc.go internal/gateway/llmrpc_test.go internal/gateway/metrics.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(gateway): carry a response schema through CallLLM and return the request seq"
```

---

### Task 3: Anthropic adapter

**Files:**
- Create: `internal/gateway/llm/anthropic.go`
- Create: `internal/gateway/llm/httperr.go` (`classifyHTTP`, shared with Task 4)
- Test: `internal/gateway/llm/anthropic_test.go`
- Modify: `internal/gateway/llmrpc.go` (`llmProvider` builds the adapter; new `llmAPIKey`)
- Modify: `internal/gateway/llmrpc_test.go` (gateway-level tests; update `TestCallLLMRefusals`)
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `llm.Request`, `llm.Response` and the `llm.Err*` errors (Task 2); `LLMProviderSpec.Effort` (Task 1).
- Produces the adapter:
  - `type llm.AnthropicConfig struct { APIKey, Model, Effort, BaseURL string; HTTPClient *http.Client; DisableRetries bool }`;
  - `func llm.NewAnthropic(cfg AnthropicConfig) *Anthropic`, which implements `llm.Provider`;
  - `const llm.DefaultEffort = "high"` and `const llm.DefaultMaxTokens = 16000`.
- Produces in `gateway`:
  - `func (g *Gateway) llmProvider(ctx context.Context, lp *lastv1alpha1.LLMProvider) (llm.Provider, error)`;
  - `func (g *Gateway) llmAPIKey(ctx context.Context, lp *lastv1alpha1.LLMProvider) (string, error)`;
  - `const llmAPIKeyField = "apiKey"`.

- [ ] **Step 1: Add the SDK**

```bash
go get github.com/anthropics/anthropic-sdk-go@v1.78.0
```

- [ ] **Step 2: Write the failing adapter tests**

Create `internal/gateway/llm/anthropic_test.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sse renders server-sent events: each pair is an event name and its JSON data.
func sse(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", pairs[i], pairs[i+1])
	}
	return b.String()
}

// textStream is a streamed answer with the given text, stop reason, and stop details.
func textStream(text, stopReason, stopDetails string) string {
	quoted, _ := json.Marshal(text)
	return sse(
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(quoted)+`}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null,"stop_details":`+stopDetails+`},"usage":{"output_tokens":9}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
}

type captured struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   map[string]any
}

// fakeAnthropic answers every request with status and body, recording the request.
func fakeAnthropic(t *testing.T, statusCode int, body string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path, c.header = r.URL.Path, r.Header.Clone()
		_ = json.Unmarshal(raw, &c.body)
		c.mu.Unlock()
		if statusCode == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func anthropicFor(srv *httptest.Server, effort string) *Anthropic {
	return NewAnthropic(AnthropicConfig{APIKey: "secret-key", Model: "claude-opus-5-5", Effort: effort,
		BaseURL: srv.URL, DisableRetries: true})
}

var conversation = Request{Intent: "team-x/web", MaxTokens: 1000, Schema: []byte(`{"type":"object"}`), Messages: []Message{
	{Role: "system", Content: "You plan."}, {Role: "user", Content: "run nginx"},
	{Role: "assistant", Content: "{}"}, {Role: "user", Content: "Answer again."}}}

func TestAnthropicSendsTheRequestTheSpecRequires(t *testing.T) {
	srv, c := fakeAnthropic(t, http.StatusOK, textStream(`{"kind":"plan"}`, "end_turn", "null"))
	resp, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"kind":"plan"}` || resp.InputTokens != 12 || resp.OutputTokens != 9 ||
		resp.CacheReadTokens != 100 || resp.CacheCreationTokens != 7 {
		t.Fatalf("response = %+v", resp)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "/v1/messages" || c.header.Get("X-Api-Key") != "secret-key" ||
		!strings.Contains(c.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("path %s, headers %v", c.path, c.header)
	}
	b := c.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] != float64(1000) || b["stream"] != true || b["fallbacks"] != "default" {
		t.Fatalf("body = %v", b)
	}
	oc, _ := b["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != DefaultEffort || format["type"] != "json_schema" || format["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("output_config = %v", oc)
	}
	system, _ := b["system"].([]any)
	last, _ := system[len(system)-1].(map[string]any)
	if cc, _ := last["cache_control"].(map[string]any); last["text"] != "You plan." || cc["type"] != "ephemeral" {
		t.Fatalf("system = %v", system)
	}
	msgs, _ := b["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles = %v (the system message must not be sent as a turn)", roles)
	}
}

func TestAnthropicHonoursEffortAndOmitsAnAbsentSchema(t *testing.T) {
	srv, c := fakeAnthropic(t, http.StatusOK, textStream("hi", "end_turn", "null"))
	req := conversation
	req.Schema, req.MaxTokens = nil, 0
	if _, err := anthropicFor(srv, "medium").Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oc, _ := c.body["output_config"].(map[string]any)
	if _, ok := oc["format"]; ok || oc["effort"] != "medium" || c.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Fatalf("body = %v", c.body)
	}
}

func TestAnthropicReturnsPartialContentAtMaxTokens(t *testing.T) {
	srv, _ := fakeAnthropic(t, http.StatusOK, textStream(`{"kind":"pl`, "max_tokens", "null"))
	resp, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if err != nil || resp.Content != `{"kind":"pl` {
		t.Fatalf("Complete = %+v, %v; want the partial content and no error", resp, err)
	}
}

func TestAnthropicReportsARefusal(t *testing.T) {
	srv, _ := fakeAnthropic(t, http.StatusOK, textStream("", "refusal", `{"type":"refusal","category":"cyber","explanation":"x"}`))
	_, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cyber") {
		t.Fatalf("Complete = %v, want ErrRefused naming the category", err)
	}
}

func TestAnthropicMapsHTTPErrors(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"x","message":"nope"}}`
	cases := map[int]error{401: ErrAuth, 403: ErrAuth, 404: ErrModel, 400: ErrBadRequest, 429: ErrTransient, 500: ErrTransient, 529: ErrTransient}
	for code, want := range cases {
		srv, _ := fakeAnthropic(t, code, errBody)
		_, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", code, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secret-key") {
			t.Errorf("HTTP %d: the API key leaked into the error", code)
		}
	}
}
```

Run: `go test ./internal/gateway/llm/ -run Anthropic`
Expected: FAIL to compile, with `undefined: NewAnthropic`.

- [ ] **Step 3: Implement the adapter**

Create `internal/gateway/llm/anthropic.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

const (
	// DefaultEffort is the reasoning effort when the LLMProvider sets none:
	// planning is intelligence-sensitive (M3 spec §3.2).
	DefaultEffort = "high"
	// DefaultMaxTokens caps an answer when the request sets no limit.
	DefaultMaxTokens = 16000
)

// AnthropicConfig configures the anthropic adapter. APIKey is a credential:
// it is never logged or placed in an error.
type AnthropicConfig struct {
	APIKey string
	Model  string
	Effort string // empty means DefaultEffort
	// BaseURL overrides the API endpoint (a proxy, or tests); empty means the SDK default.
	BaseURL    string
	HTTPClient *http.Client
	// DisableRetries turns off the SDK's retries (tests).
	DisableRetries bool
}

// Anthropic calls Claude through the Messages API with structured output,
// prompt caching, and the server-side refusal fallback.
type Anthropic struct {
	client anthropic.Client
	model  string
	effort string
}

// NewAnthropic builds the adapter.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	retries := 2
	if cfg.DisableRetries {
		retries = 0
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey), option.WithMaxRetries(retries)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	effort := cfg.Effort
	if effort == "" {
		effort = DefaultEffort
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: cfg.Model, effort: effort}
}

// Complete streams one answer and returns its text. A refusal is ErrRefused;
// an answer cut off at max_tokens is returned as it is, for the caller to judge.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	params := anthropic.BetaMessageNewParams{
		Model:        a.model,
		MaxTokens:    int64(maxTokens),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(a.effort)},
	}
	if len(req.Schema) > 0 {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: json.RawMessage(req.Schema)}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			params.System = append(params.System, anthropic.BetaTextBlockParam{Text: m.Content})
		case "assistant":
			params.Messages = append(params.Messages, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)}})
		default:
			params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(m.Content)))
		}
	}
	// The system prompt is long and stable across turns: cache everything up to its end.
	if n := len(params.System); n > 0 {
		params.System[n-1].CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}

	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Response{}, fmt.Errorf("%w: %v", ErrTransient, err)
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, classifyAnthropic(err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		category := string(msg.StopDetails.Category)
		if category == "" {
			category = "unspecified"
		}
		return Response{}, fmt.Errorf("%w (category %s)", ErrRefused, category)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	u := msg.Usage
	return Response{Content: text.String(), InputTokens: int(u.InputTokens), OutputTokens: int(u.OutputTokens),
		CacheReadTokens: int(u.CacheReadInputTokens), CacheCreationTokens: int(u.CacheCreationInputTokens)}, nil
}

// classifyAnthropic wraps an SDK error in the llm error the gateway maps to a
// gRPC code. The API's error text names the problem, never the key.
func classifyAnthropic(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%w: %v", ErrTransient, err)
	}
	return classifyHTTP(apiErr.StatusCode, apiErr.Error())
}
```

Create `internal/gateway/llm/httperr.go`. Task 4's OpenAI adapter uses it too:

```go
package llm

import (
	"fmt"
	"net/http"
)

// classifyHTTP maps a provider's HTTP error status to the llm error the gateway
// maps to a gRPC code. detail is shown only for requests the provider rejected
// as invalid. It is never shown for authentication failures, whose text may
// quote part of the key.
func classifyHTTP(code int, detail string) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", ErrAuth, code)
	case code == http.StatusNotFound:
		return fmt.Errorf("%w (HTTP %d)", ErrModel, code)
	case code == http.StatusTooManyRequests || code >= http.StatusInternalServerError:
		return fmt.Errorf("%w (HTTP %d)", ErrTransient, code)
	default:
		return fmt.Errorf("%w (HTTP %d): %s", ErrBadRequest, code, detail)
	}
}
```

If the compiler reports a different SDK name, find the right one by grepping the module source at `$(go env GOMODCACHE)/github.com/anthropics/anthropic-sdk-go@v1.78.0/`, and list each substitution in your report. The names above were checked against `betamessage.go`, `betamessageutil.go`, `beta.go`, `aliases.go` and `shared/constant/constants.go` there.

Run: `go test ./internal/gateway/llm/ -v`
Expected: PASS (the scripted tests and the five Anthropic tests).

- [ ] **Step 4: Write the failing gateway tests**

In `internal/gateway/llmrpc_test.go`, change `TestCallLLMRefusals`'s first check from "anthropic = Unimplemented" to "anthropic without a key Secret = FailedPrecondition":

```go
	if _, err := newLLMEnv(t, lastv1alpha1.LLMAnthropic).callLLM(user); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("anthropic without its key Secret = %v, want FailedPrecondition", err)
	}
```

Append these tests (add `net/http`, `net/http/httptest` and `io` to the imports):

```go
// anthropicEnv is an env whose LLMProvider is anthropic at a fake API that checks
// the key and answers with text.
func anthropicEnv(t *testing.T, storedKey string) (*testEnv, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Api-Key") != "sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"real answer\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null,\"stop_details\":null},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	op, lp := llmConfig(lastv1alpha1.LLMAnthropic)
	lp.Spec.Endpoint = srv.URL
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if storedKey != "" {
		if _, err := env.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: ns},
			Data:       map[string][]byte{llmAPIKeyField: []byte(storedKey)},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return env, srv
}

func TestCallLLMAnthropicUsesTheKeyFromTheSecretTrimmed(t *testing.T) {
	env, _ := anthropicEnv(t, "  sk-test\n")
	resp, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if err != nil || resp.GetContent() != "real answer" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
}

func TestCallLLMAnthropicKeyProblemsAreFailedPreconditionWithoutTheKey(t *testing.T) {
	missing, _ := anthropicEnv(t, "")
	_, err := missing.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "key") {
		t.Fatalf("missing Secret = %v", err)
	}
	wrong, _ := anthropicEnv(t, "sk-wrong-value")
	_, err = wrong.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "sk-wrong-value") {
		t.Fatalf("rejected key = %v; want FailedPrecondition that never repeats the key", err)
	}
	for _, e := range wrong.entries() {
		if strings.Contains(string(e.Payload), "sk-wrong-value") {
			t.Fatalf("entry %s carries the API key", e.Type)
		}
	}
}
```

Run: `go test ./internal/gateway/ -run CallLLM`
Expected: FAIL. The anthropic tests get `Unimplemented`, and `llmAPIKeyField` is undefined.

- [ ] **Step 5: Build the adapter in the gateway**

In `internal/gateway/llmrpc.go`, replace `llmProvider` with:

```go
// llmAPIKeyField is the Secret key holding an LLMProvider's API key.
const llmAPIKeyField = "apiKey"

// llmAPIKey reads an LLMProvider's API key from its Secret in the gateway
// namespace. Errors name the Secret, never the value.
func (g *Gateway) llmAPIKey(ctx context.Context, lp *lastv1alpha1.LLMProvider) (string, error) {
	if lp.Spec.SecretRef == nil {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no secretRef", lp.Name)
	}
	s, err := g.kube.CoreV1().Secrets(g.cfg.Namespace).Get(ctx, lp.Spec.SecretRef.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s: Secret %s not found", lp.Name, lp.Spec.SecretRef.Name)
	}
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "read LLMProvider %s key Secret: %v", lp.Name, err)
	}
	key := strings.TrimSpace(string(s.Data[llmAPIKeyField]))
	if key == "" {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s: Secret %s has no %q key", lp.Name, lp.Spec.SecretRef.Name, llmAPIKeyField)
	}
	return key, nil
}

func (g *Gateway) llmProvider(ctx context.Context, lp *lastv1alpha1.LLMProvider) (llm.Provider, error) {
	switch lp.Spec.Type {
	case lastv1alpha1.LLMScripted:
		if lp.Spec.Scripted == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no scripted config", lp.Name)
		}
		return llm.NewScripted(g.kube, g.cfg.Namespace, lp.Spec.Scripted.ConfigMapRef.Name), nil
	case lastv1alpha1.LLMAnthropic:
		key, err := g.llmAPIKey(ctx, lp)
		if err != nil {
			return nil, err
		}
		return llm.NewAnthropic(llm.AnthropicConfig{APIKey: key, Model: lp.Spec.Model, Effort: string(lp.Spec.Effort),
			BaseURL: lp.Spec.Endpoint}), nil
	default:
		return nil, status.Errorf(codes.Unimplemented, "LLM provider type %q is not supported yet", lp.Spec.Type)
	}
}
```

In `callLLM`, change `provider, err := g.llmProvider(&lp)` to `provider, err := g.llmProvider(ctx, &lp)`. Add `strings` and `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` to the imports.

Run: `go test ./internal/gateway/ -run CallLLM -v`
Expected: PASS.

- [ ] **Step 6: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add go.mod go.sum internal/gateway/llm/anthropic.go internal/gateway/llm/httperr.go internal/gateway/llm/anthropic_test.go internal/gateway/llmrpc.go internal/gateway/llmrpc_test.go
git commit -m "feat(gateway): call Claude through the Anthropic adapter"
```

---
### Task 4: OpenAI and OpenAI-compatible adapter

**Files:**
- Create: `internal/gateway/llm/openai.go`
- Test: `internal/gateway/llm/openai_test.go`
- Modify: `internal/gateway/llmrpc.go` (`llmProvider` builds it)
- Modify: `internal/gateway/llmrpc_test.go`

**Interfaces:**
- Consumes: `classifyHTTP`, `DefaultMaxTokens` and the `llm.Err*` errors (Tasks 2–3); `llmAPIKey` (Task 3).
- Produces the adapter:
  - `type llm.OpenAIConfig struct { APIKey, Model, Endpoint string; Compatible bool; HTTPClient *http.Client }`;
  - `const llm.DefaultOpenAIEndpoint = "https://api.openai.com/v1"`;
  - `func llm.NewOpenAI(cfg OpenAIConfig) *OpenAI`, which implements `llm.Provider`.

- [ ] **Step 1: Write the failing tests**

Create `internal/gateway/llm/openai_test.go`:

```go
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type openAISeen struct {
	mu     sync.Mutex
	path   string
	auth   string
	body   map[string]any
}

func fakeOpenAI(t *testing.T, statusCode int, body string) (*httptest.Server, *openAISeen) {
	t.Helper()
	seen := &openAISeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.path, seen.auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &seen.body)
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

const openAIAnswer = `{"choices":[{"message":{"role":"assistant","content":"{\"kind\":\"plan\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`

func TestOpenAISendsChatCompletionsWithAJSONSchema(t *testing.T) {
	srv, seen := fakeOpenAI(t, http.StatusOK, openAIAnswer)
	o := NewOpenAI(OpenAIConfig{APIKey: "sk-secret", Model: "gpt-x", Endpoint: srv.URL + "/v1"})
	resp, err := o.Complete(context.Background(), conversation)
	if err != nil || resp.Content != `{"kind":"plan"}` || resp.InputTokens != 20 || resp.OutputTokens != 5 {
		t.Fatalf("Complete = %+v, %v", resp, err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.path != "/v1/chat/completions" || seen.auth != "Bearer sk-secret" {
		t.Fatalf("path %s auth %q", seen.path, seen.auth)
	}
	b := seen.body
	rf, _ := b["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if b["model"] != "gpt-x" || b["max_completion_tokens"] != float64(1000) || b["max_tokens"] != nil ||
		rf["type"] != "json_schema" || js["strict"] != true || js["name"] != "answer" || js["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("body = %v", b)
	}
	msgs, _ := b["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestOpenAICompatibleSendsMaxTokensAndNoFormatWithoutASchema(t *testing.T) {
	srv, seen := fakeOpenAI(t, http.StatusOK, openAIAnswer)
	req := conversation
	req.Schema = nil
	o := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "llama", Endpoint: srv.URL + "/v1/", Compatible: true})
	if _, err := o.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.path != "/v1/chat/completions" || seen.body["max_tokens"] != float64(1000) ||
		seen.body["max_completion_tokens"] != nil || seen.body["response_format"] != nil {
		t.Fatalf("path %s body %v", seen.path, seen.body)
	}
}

func TestOpenAIReportsARefusalAndAnEmptyAnswer(t *testing.T) {
	srv, _ := fakeOpenAI(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`)
	_, err := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "m", Endpoint: srv.URL}).Complete(context.Background(), conversation)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("refusal = %v, want ErrRefused", err)
	}
	empty, _ := fakeOpenAI(t, http.StatusOK, `{"choices":[]}`)
	if _, err := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "m", Endpoint: empty.URL}).Complete(context.Background(), conversation); !errors.Is(err, ErrTransient) {
		t.Fatalf("no choices = %v, want ErrTransient", err)
	}
}

func TestOpenAIMapsHTTPErrorsWithoutTheKey(t *testing.T) {
	cases := map[int]error{401: ErrAuth, 404: ErrModel, 400: ErrBadRequest, 429: ErrTransient, 503: ErrTransient}
	for code, want := range cases {
		srv, _ := fakeOpenAI(t, code, `{"error":{"message":"Incorrect API key provided: sk-sec***ret"}}`)
		_, err := NewOpenAI(OpenAIConfig{APIKey: "sk-secret", Model: "m", Endpoint: srv.URL}).Complete(context.Background(), conversation)
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", code, err, want)
		}
		if code == 401 && err != nil && strings.Contains(err.Error(), "sk-sec") {
			t.Errorf("HTTP 401: the error quotes the provider's key hint: %v", err)
		}
	}
}

func TestOpenAIDefaultsToTheOpenAIEndpoint(t *testing.T) {
	if o := NewOpenAI(OpenAIConfig{}); o.cfg.Endpoint != DefaultOpenAIEndpoint {
		t.Fatalf("endpoint = %q", o.cfg.Endpoint)
	}
}
```

Run: `go test ./internal/gateway/llm/ -run OpenAI`
Expected: FAIL to compile, with `undefined: NewOpenAI`.

- [ ] **Step 2: Implement the adapter**

Create `internal/gateway/llm/openai.go`:

```go
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenAIEndpoint is the OpenAI API base URL.
const DefaultOpenAIEndpoint = "https://api.openai.com/v1"

const (
	openAITimeout       = 10 * time.Minute
	maxOpenAIErrorBytes = 4 << 10
	maxOpenAIBodyBytes  = 32 << 20
)

// OpenAIConfig configures the Chat Completions adapter, used for both the
// "openai" and "openai-compatible" provider types. APIKey is a credential: it
// is never logged or placed in an error.
type OpenAIConfig struct {
	APIKey   string
	Model    string
	Endpoint string // API base URL; empty means DefaultOpenAIEndpoint
	// Compatible sends max_tokens, which self-hosted servers expect, instead of
	// OpenAI's max_completion_tokens.
	Compatible bool
	HTTPClient *http.Client
}

// OpenAI calls an OpenAI-style Chat Completions API. It does not retry: a
// transient failure is ErrTransient, and the operator retries with backoff.
type OpenAI struct {
	cfg    OpenAIConfig
	client *http.Client
}

// NewOpenAI builds the adapter.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultOpenAIEndpoint
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: openAITimeout}
	}
	return &OpenAI{cfg: cfg, client: c}
}

type openAIRequest struct {
	Model               string                `json:"model"`
	Messages            []Message             `json:"messages"`
	MaxCompletionTokens int                   `json:"max_completion_tokens,omitempty"`
	MaxTokens           int                   `json:"max_tokens,omitempty"`
	ResponseFormat      *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete sends one Chat Completions request and returns the first choice.
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	body := openAIRequest{Model: o.cfg.Model, Messages: req.Messages}
	if o.cfg.Compatible {
		body.MaxTokens = maxTokens
	} else {
		body.MaxCompletionTokens = maxTokens
	}
	if len(req.Schema) > 0 {
		body.ResponseFormat = &openAIResponseFormat{Type: "json_schema",
			JSONSchema: openAIJSONSchema{Name: "answer", Schema: json.RawMessage(req.Schema), Strict: true}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request: %v", ErrBadRequest, err)
	}
	url := strings.TrimSuffix(o.cfg.Endpoint, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	res, err := o.client.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, maxOpenAIErrorBytes))
		return Response{}, classifyHTTP(res.StatusCode, strings.TrimSpace(string(detail)))
	}
	var out openAIResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxOpenAIBodyBytes)).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrTransient, err)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: the response has no choices", ErrTransient)
	}
	msg := out.Choices[0].Message
	if msg.Refusal != "" {
		return Response{}, fmt.Errorf("%w: %s", ErrRefused, msg.Refusal)
	}
	return Response{Content: msg.Content, InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}, nil
}
```

Run: `go test ./internal/gateway/llm/ -v`
Expected: PASS.

- [ ] **Step 3: Write the failing gateway test**

Append to `internal/gateway/llmrpc_test.go`:

```go
func TestCallLLMOpenAICompatibleUsesItsEndpointAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-local" || r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	op, lp := llmConfig(lastv1alpha1.LLMOpenAICompatible)
	lp.Spec.Endpoint = srv.URL + "/v1"
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if _, err := env.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: ns}, Data: map[string][]byte{llmAPIKeyField: []byte("sk-local")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	resp, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if err != nil || resp.GetContent() != "local answer" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
}
```

Run: `go test ./internal/gateway/ -run OpenAICompatible`
Expected: FAIL with `Unimplemented`.

- [ ] **Step 4: Build the adapter in the gateway**

In `llmProvider` (`internal/gateway/llmrpc.go`), add this case before `default`:

```go
	case lastv1alpha1.LLMOpenAI, lastv1alpha1.LLMOpenAICompatible:
		key, err := g.llmAPIKey(ctx, lp)
		if err != nil {
			return nil, err
		}
		return llm.NewOpenAI(llm.OpenAIConfig{APIKey: key, Model: lp.Spec.Model, Endpoint: lp.Spec.Endpoint,
			Compatible: lp.Spec.Type == lastv1alpha1.LLMOpenAICompatible}), nil
```

Run: `go test ./internal/gateway/ -run CallLLM -v`
Expected: PASS.

- [ ] **Step 5: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add internal/gateway/llm/openai.go internal/gateway/llm/openai_test.go internal/gateway/llmrpc.go internal/gateway/llmrpc_test.go
git commit -m "feat(gateway): add the OpenAI and OpenAI-compatible adapter"
```

---

### Task 5: Discovery-stage phases

**Files:**
- Modify: `internal/gateway/phases.go` (`beginPhase`, new `verifyDiscovery`, `endPhase`)
- Modify: `internal/gateway/commands_test.go` (extract `newProviderEnv` from `newRunEnv`)
- Create: `internal/gateway/discovery_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§8.3)

**Interfaces:**
- Consumes: `plan.DiscoveryHash`, `plan.DiscoveryStepID` and `plan.MaxPendingCommandBytes`; `PendingCommand.Nonce` (Task 1).
- Produces the gateway behaviour: `BeginPhase` with `stage: "Discovery"` and `subject_hash` = `status.pendingCommand.hash` opens a one-step phase whose step ID is `discovery`. `RunCommand` and `EndPhase` work on it unchanged, except that `EndPhase(succeeded)` never sets the executed plan.
- Produces the test helpers `newProviderEnv(t, objs...)`, `discoveryCommand()`, `intentWithDiscovery(t, c, nonce)`, `discoveryApproval(name, subject, decision)` and `(*testEnv).beginStage(stage, subject)`.

- [ ] **Step 1: Extract the provider env**

In `internal/gateway/commands_test.go`, replace `newRunEnv` with:

```go
// newProviderEnv is an env with provider "cluster" (and its credential Secret),
// a policy allowing it, and objs. Sandboxed commands answer like kubectl.
func newProviderEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	provider := &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: testProvider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "cluster-creds"}, Env: map[string]string{"CLUSTER_TOKEN": "token"}}}
	env := newTestEnv(t, append([]client.Object{allowAll(), provider}, objs...)...)
	if _, err := env.kube.CoreV1().Secrets("last-gateway").Create(env.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: "last-gateway"},
		Data:       map[string][]byte{"token": []byte("probe-token-value")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env.result = kubectlJSON
	return env
}

func newRunEnv(t *testing.T, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) (*testEnv, *lastv1alpha1.Intent) {
	t.Helper()
	in := intentWithPlan(t, steps, resources...)
	return newProviderEnv(t, in, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)), in
}
```

Add `sigs.k8s.io/controller-runtime/pkg/client` to its imports if it is not already there.

Run: `go test ./internal/gateway/ -count=1`
Expected: PASS. The refactor changes nothing.

- [ ] **Step 2: Write the failing discovery tests**

Create `internal/gateway/discovery_test.go`:

```go
package gateway

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

func discoveryCommand() lastv1alpha1.Command {
	c := argvStep(plan.DiscoveryStepID, kubectlBin, "get", "namespace", envNS, "-o", "json")
	c.Providers = []string{testProvider}
	return c
}

func pendingCommand(t *testing.T, c lastv1alpha1.Command, nonce string) *lastv1alpha1.PendingCommand {
	t.Helper()
	h, _, err := plan.DiscoveryHash(c, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return &lastv1alpha1.PendingCommand{Hash: h, Command: c, Nonce: nonce, Reason: "Look at the namespace"}
}

// intentWithDiscovery is an Intent waiting for approval of one discovery command.
func intentWithDiscovery(t *testing.T, c lastv1alpha1.Command, nonce string) *lastv1alpha1.Intent {
	t.Helper()
	return &lastv1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: envIntent, Namespace: envNS, UID: envIntentUID},
		Spec:       lastv1alpha1.IntentSpec{Request: "run a web server"},
		Status: lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingDiscoveryApproval, Stage: lastv1alpha1.StageDiscovery,
			PendingCommand: pendingCommand(t, c, nonce)},
	}
}

func discoveryApproval(name, subject string, decision lastv1alpha1.Decision) *lastv1alpha1.IntentApproval {
	a := approvalFor(name, subject, decision, approver)
	a.Spec.Stage = lastv1alpha1.StageDiscovery
	return a
}

func (e *testEnv) beginStage(stage lastv1alpha1.Stage, subject string) (*gatewayv1.BeginPhaseResponse, error) {
	return e.g.beginPhase(e.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: string(stage), SubjectHash: subject})
}

// runDiscovery opens, runs, and closes the pending discovery command's phase.
func (e *testEnv) runDiscovery(c lastv1alpha1.Command, subject string) {
	e.t.Helper()
	if resp, err := e.beginStage(lastv1alpha1.StageDiscovery, subject); err != nil || resp.GetSteps() != 1 {
		e.t.Fatalf("BeginPhase(Discovery) = %v, %v", resp, err)
	}
	if r, err := e.run(subject, c); err != nil || r.GetResult().GetExitCode() != 0 {
		e.t.Fatalf("RunCommand(discovery) = %v, %v", r, err)
	}
	if _, err := e.end(subject, true); err != nil {
		e.t.Fatalf("EndPhase = %v", err)
	}
}

// setPending replaces the stored Intent's pending command, as the operator does.
func (e *testEnv) setPending(pc *lastv1alpha1.PendingCommand) {
	e.t.Helper()
	var in lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		e.t.Fatal(err)
	}
	in.Status.Stage, in.Status.PendingCommand = lastv1alpha1.StageDiscovery, pc
	if err := e.crd.Update(e.ctx, &in); err != nil {
		e.t.Fatal(err)
	}
}

func TestDiscoveryRunsAnApprovedCommandAsAOneStepPhase(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	env.runDiscovery(c, subject)

	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || rec.Executed != nil || !slices.Contains(rec.Used, subject) {
		t.Fatalf("record = %+v, %v; want no executed plan and the subject used", rec, err)
	}
	var types []string
	for _, e := range env.entries() {
		if e.Intent != envKey {
			continue
		}
		types = append(types, e.Type)
		if e.Type == audit.TypePhaseBegin && e.Stage != string(lastv1alpha1.StageDiscovery) {
			t.Fatalf("phase.begin stage = %q", e.Stage)
		}
	}
	want := []string{audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandStart, audit.TypeCommandResult, audit.TypePhaseEnd}
	if !slices.Equal(types, want) {
		t.Fatalf("entries = %v, want %v", types, want)
	}
}

func TestDiscoveryRefusesASubjectThatIsNotThePendingCommand(t *testing.T) {
	c := discoveryCommand()
	other, _, _ := plan.DiscoveryHash(c, "other-nonce")
	cases := map[string]struct {
		pending func(*lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand
		subject func(*lastv1alpha1.PendingCommand) string
		code    codes.Code
	}{
		"another subject": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { return pc },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.PermissionDenied},
		"a hash that does not match the command": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Hash = other; return pc },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.PermissionDenied},
		"no nonce": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Nonce = ""; return pc },
			func(pc *lastv1alpha1.PendingCommand) string { return pc.Hash }, codes.InvalidArgument},
		"not the discovery step": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Command.ID = "s1"; return pc },
			func(pc *lastv1alpha1.PendingCommand) string { return pc.Hash }, codes.InvalidArgument},
		"no pending command": {func(*lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { return nil },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.FailedPrecondition},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := intentWithDiscovery(t, c, "n1")
			subject := tc.subject(in.Status.PendingCommand)
			in.Status.PendingCommand = tc.pending(in.Status.PendingCommand)
			env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
			_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
			env.wantDenied(err, tc.code)
		})
	}
}

func TestDiscoveryNeedsADiscoveryStageApproval(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, approvalFor("create", subject, lastv1alpha1.DecisionApprove, approver))
	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestDiscoveryApprovalsAreSingleUse(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	env.runDiscovery(c, subject)

	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "already opened") {
		t.Fatalf("rerun = %v, want the subject refused as used", err)
	}
	// The same command under a new nonce is a new subject; the old approval does not cover it.
	again := pendingCommand(t, c, "n2")
	env.setPending(again)
	_, err = env.beginStage(lastv1alpha1.StageDiscovery, again.Hash)
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestDiscoveryNeverReplacesTheExecutedPlan(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	c := discoveryCommand()
	pc := pendingCommand(t, c, "n1")
	env.setPending(pc)
	if err := env.crd.Create(env.ctx, discoveryApproval("look", pc.Hash, lastv1alpha1.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	env.runDiscovery(c, pc.Hash)

	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || rec.Executed == nil || rec.Executed.PlanHash != in.Status.Plan.Hash {
		t.Fatalf("executed plan = %+v, %v; want the Create plan kept", rec.Executed, err)
	}
	probes, err := env.g.runProbes(env.ctx, &gatewayv1.RunProbesRequest{Intent: ref()})
	if err != nil || probes.GetPlanHash() != in.Status.Plan.Hash {
		t.Fatalf("RunProbes = %v, %v", probes, err)
	}
}

func TestDiscoveryRefusesAnOversizedCommand(t *testing.T) {
	c := discoveryCommand()
	c.Files = map[string]string{"/work/big": strings.Repeat("x", plan.MaxPendingCommandBytes)}
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.InvalidArgument)
}

func TestBeginPhaseStillRefusesOtherStages(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, in.Status.Plan.Hash)
	env.wantDenied(err, codes.FailedPrecondition)
}
```

Run: `go test ./internal/gateway/ -run 'Discovery|OtherStages'`
Expected: FAIL. Every Discovery begin is refused with "stage \"Discovery\" is not supported yet".

- [ ] **Step 3: Accept the Discovery stage**

In `internal/gateway/phases.go`:

(a) Add `verifyDiscovery` after `verifyPlan`:

```go
// verifyDiscovery checks the Intent's pending discovery command against the
// subject and returns it as a one-step plan. Every refusal is audited through d.
func verifyDiscovery(d denial, intent *lastv1alpha1.Intent, subject string) (plan.Body, []byte, error) {
	pc := intent.Status.PendingCommand
	if pc == nil {
		return plan.Body{}, nil, d.deny(codes.FailedPrecondition, "intent has no pending discovery command")
	}
	if pc.Command.ID != plan.DiscoveryStepID {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "a discovery command's id must be %q", plan.DiscoveryStepID)
	}
	if pc.Nonce == "" {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "the pending discovery command has no nonce")
	}
	hash, size, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command cannot be encoded: %v", err)
	}
	if size > plan.MaxPendingCommandBytes {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command is %d bytes; the limit is %d", size, plan.MaxPendingCommandBytes)
	}
	if hash != subject || pc.Hash != subject {
		return plan.Body{}, nil, d.deny(codes.PermissionDenied, "pending command hash %s does not match the subject", hash)
	}
	body := plan.Body{Steps: []lastv1alpha1.Command{pc.Command}}
	if err := plan.Validate(body); err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "%v", err)
	}
	canonical, err := plan.Canonical(body)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command cannot be encoded: %v", err)
	}
	return body, canonical, nil
}
```

(b) In `beginPhase`, replace the stage check and the plan verification:

```go
	if stage != lastv1alpha1.StageCreate {
		return nil, d.deny(codes.FailedPrecondition, "stage %q is not supported yet; this gateway opens Create phases only", stage)
	}
```

with:

```go
	if stage != lastv1alpha1.StageCreate && stage != lastv1alpha1.StageDiscovery {
		return nil, d.deny(codes.FailedPrecondition, "stage %q is not supported yet; this gateway opens Create and Discovery phases only", stage)
	}
```

and replace:

```go
	if intent.Status.Plan == nil {
		return nil, d.deny(codes.FailedPrecondition, "intent has no plan")
	}
	body, canonical, err := verifyPlan(d, &intent, subject)
	if err != nil {
		return nil, err
	}
```

with:

```go
	var body plan.Body
	var canonical []byte
	if stage == lastv1alpha1.StageDiscovery {
		body, canonical, err = verifyDiscovery(d, &intent, subject)
	} else {
		if intent.Status.Plan == nil {
			return nil, d.deny(codes.FailedPrecondition, "intent has no plan")
		}
		body, canonical, err = verifyPlan(d, &intent, subject)
	}
	if err != nil {
		return nil, err
	}
```

(The `err` variable already exists in `beginPhase` from `intentKey`.)

(c) Discovery phases never become the executed plan, so `recordFits` is only needed for Create. Wrap the existing `recordFits` call:

```go
	if stage == lastv1alpha1.StageCreate {
		if err := recordFits(rec, stage, subject, canonical); err != nil {
			return nil, d.deny(codes.InvalidArgument, "plan too large to record: %v", err)
		}
	}
```

(d) In `endPhase`, change `if p.outcome == outcomeSucceeded {` to:

```go
	// A Discovery phase ran one inspection command; probes and later stages keep
	// following the last executed Create plan.
	if p.outcome == outcomeSucceeded && p.stage != lastv1alpha1.StageDiscovery {
```

If an existing test asserts the old refusal text ("Create phases only"), update the expected text to "Create and Discovery phases only".

Run: `go test ./internal/gateway/ -count=1 -v -run 'Discovery|OtherStages|BeginPhase|EndPhase'`
Expected: PASS. Then run `go test ./internal/gateway/ -count=1` and expect PASS.

- [ ] **Step 4: Amend the main spec**

In §8.3, after the numbered list (before "Any mismatch produces…"), add:

```markdown
**Discovery commands** are one-step phases (stage `Discovery`). The subject
is `status.pendingCommand.hash`, the SHA-256 of the canonical JSON of
`{command, nonce}`. The operator picks a fresh nonce for every request, and
the gateway records each subject as used, so an approval is good for
exactly one run, even of an identical command. The gateway checks that the
hash matches the pending command and its nonce, that the command's step ID
is `discovery`, and that it is at most 64 KiB. A Discovery phase never
becomes the executed plan: probes and later stages keep following the last
executed Create plan.
```

- [ ] **Step 5: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add internal/gateway/phases.go internal/gateway/commands_test.go internal/gateway/discovery_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(gateway): run approved discovery commands as one-step phases"
```

---

### Task 6: `QueryAudit`

**Files:**
- Modify: `proto/last/gateway/v1/gateway.proto`; regenerate `internal/gatewayapi/gatewayv1/*`
- Modify: `internal/audit/verify.go` (export `DecodeEntry`)
- Test: `internal/audit/verify_test.go`
- Modify: `internal/gateway/state.go` (index fields and methods; `errRecordTooLarge`)
- Test: `internal/gateway/state_test.go`
- Create: `internal/gateway/index.go`
- Create: `internal/gateway/queryaudit.go`
- Modify: `internal/gateway/rpc.go`, `internal/gateway/server.go` (`lead`), `internal/gateway/metrics.go`
- Modify: `internal/gateway/testenv_test.go` (install the hooks in `newGateway`)
- Test: `internal/gateway/queryaudit_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§4.2)

**Interfaces:**
- Produces the RPC: `rpc QueryAudit(QueryAuditRequest) returns (QueryAuditResponse)`, with the messages `QueryAuditRequest{intent, types, after_seq, limit}`, `AuditEntry{seq, type, ts, stage, subject_hash, payload}` and `QueryAuditResponse{entries, truncated, more}`.
- Produces in `audit`: `func DecodeEntry(body []byte) (Entry, error)`.
- Produces in `gateway`:
  - `type IndexEntry struct{ Seq uint64; Type string }`;
  - the record fields `IntentRecord.Index []IndexEntry` and `IntentRecord.IndexFloor uint64`;
  - the methods `(*IntentRecord).AddIndex(es []IndexEntry)` and `(*IntentRecord).DropIndex(n int)`;
  - `(*Gateway).observeWrites(w *audit.Writer)`, `(*Gateway).flushIndex(ctx, intent) error` and `(*Gateway).flushIndexes(ctx)`;
  - `const maxIndexEntries = 200`.

- [ ] **Step 1: Extend the proto**

In `proto/last/gateway/v1/gateway.proto`, add to `service GatewayService` after `CallLLM`:

```proto
  // QueryAudit returns verified audit entries for one Intent, oldest first (M3 spec §3.5).
  rpc QueryAudit(QueryAuditRequest) returns (QueryAuditResponse);
```

and add these messages at the end of the file:

```proto
// QueryAuditRequest selects one Intent's indexed audit entries.
message QueryAuditRequest {
  IntentRef intent = 1;
  // Entry types to return, e.g. "llm.request"; empty means every indexed type.
  repeated string types = 2;
  // Return only entries with seq > after_seq.
  uint64 after_seq = 3;
  // At most this many entries: 1..200; 0 means 50.
  int32 limit = 4;
}

// AuditEntry is one verified audit entry.
message AuditEntry {
  uint64 seq = 1;
  string type = 2;
  // RFC 3339 timestamp.
  string ts = 3;
  string stage = 4;
  string subject_hash = 5;
  // The entry's payload, canonical JSON.
  bytes payload = 6;
}

// QueryAuditResponse lists entries oldest first.
message QueryAuditResponse {
  repeated AuditEntry entries = 1;
  // Older matching entries fell out of the index.
  bool truncated = 2;
  // More matching entries follow; ask again with after_seq set to the last seq.
  bool more = 3;
}
```

Run: `make proto`
Expected: buf lint passes and the generated code gains `QueryAudit`. `go build ./...` still succeeds: `rpcServer` embeds `UnimplementedGatewayServiceServer`, which supplies a stub until Step 6.

- [ ] **Step 2: Export the strict entry decoder**

Append to `internal/audit/verify_test.go`:

```go
func TestDecodeEntryAcceptsOnlyCanonicalVersion1Entries(t *testing.T) {
	e := Entry{V: EntryVersion, Seq: 3, ChainID: "c", PrevHash: GenesisHash, TS: "2026-10-05T00:00:00.000000000Z", Type: TypeLLMRequest, Actor: "op"}
	body, err := Canonical(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeEntry(body)
	if err != nil || got.Seq != 3 || got.Type != TypeLLMRequest {
		t.Fatalf("DecodeEntry = %+v, %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"not canonical": append([]byte(" "), body...),
		"unknown field": []byte(`{"extra":1,` + string(body[1:])),
		"not json":      []byte("{"),
	} {
		if _, err := DecodeEntry(bad); err == nil {
			t.Errorf("%s: DecodeEntry accepted it", name)
		}
	}
}
```

Then, in `internal/audit/verify.go`, add above `decodeEntry`:

```go
// DecodeEntry strictly decodes a stored entry body: it must be exactly the RFC
// 8785 canonical JSON of a version-1 entry with no unknown fields.
func DecodeEntry(body []byte) (Entry, error) {
	e, problem := decodeEntry(body)
	if problem != "" {
		return Entry{}, fmt.Errorf("audit: %s", problem)
	}
	return e, nil
}
```

Run: `go test ./internal/audit/ -run DecodeEntry -v`
Expected: PASS.

- [ ] **Step 3: Write the failing record-index tests**

Append to `internal/gateway/state_test.go`:

```go
func TestRecordIndexKeepsTheNewestEntriesAndRecordsTheFloor(t *testing.T) {
	var r IntentRecord
	var es []IndexEntry
	for seq := uint64(1); seq <= maxIndexEntries+5; seq++ {
		es = append(es, IndexEntry{Seq: seq, Type: audit.TypeLLMRequest})
	}
	r.AddIndex(es)
	r.AddIndex([]IndexEntry{{Seq: 100, Type: audit.TypeLLMRequest}}) // a duplicate is ignored
	if len(r.Index) != maxIndexEntries || r.Index[0].Seq != 6 || r.IndexFloor != 5 {
		t.Fatalf("index has %d entries from seq %d, floor %d", len(r.Index), r.Index[0].Seq, r.IndexFloor)
	}
	r.DropIndex(10)
	if len(r.Index) != maxIndexEntries-10 || r.IndexFloor != 15 {
		t.Fatalf("after DropIndex: %d entries, floor %d", len(r.Index), r.IndexFloor)
	}
}
```

Run: `go test ./internal/gateway/ -run RecordIndex`
Expected: FAIL to compile, with `undefined: IndexEntry`.

- [ ] **Step 4: Add the index to the record**

In `internal/gateway/state.go`:

(a) Add the constant and type:

```go
// maxIndexEntries bounds an Intent's QueryAudit index (M3 spec §3.5).
const maxIndexEntries = 200

// IndexEntry is one audit entry QueryAudit can serve for the Intent.
type IndexEntry struct {
	Seq  uint64 `json:"seq"`
	Type string `json:"type"`
}

// errRecordTooLarge means a record would not fit its ConfigMap.
var errRecordTooLarge = errors.New("gateway: intent record too large")
```

(b) Add the fields to `IntentRecord`:

```go
	// Index lists the Intent's recent indexed audit entries, oldest first.
	Index []IndexEntry `json:"index,omitempty"`
	// IndexFloor is the highest seq ever dropped from Index; QueryAudit reports
	// truncation for cursors below it.
	IndexFloor uint64 `json:"indexFloor,omitempty"`
```

(c) Add the methods:

```go
// AddIndex merges entries into the index, keeping it sorted, unique by seq,
// and at most maxIndexEntries long.
func (r *IntentRecord) AddIndex(es []IndexEntry) {
	r.Index = append(r.Index, es...)
	slices.SortFunc(r.Index, func(a, b IndexEntry) int { return cmp.Compare(a.Seq, b.Seq) })
	r.Index = slices.CompactFunc(r.Index, func(a, b IndexEntry) bool { return a.Seq == b.Seq })
	if n := len(r.Index) - maxIndexEntries; n > 0 {
		r.DropIndex(n)
	}
}

// DropIndex drops the n oldest index entries and raises IndexFloor past them.
func (r *IntentRecord) DropIndex(n int) {
	n = min(n, len(r.Index))
	if n == 0 {
		return
	}
	r.IndexFloor = max(r.IndexFloor, r.Index[n-1].Seq)
	r.Index = slices.Clone(r.Index[n:])
}
```

(d) In `encodeRecord`, wrap the size error with the sentinel:

```go
	if len(body) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: %d bytes; the limit is %d", errRecordTooLarge, len(body), MaxRecordBytes)
	}
```

Add `cmp` and `errors` to the imports.

Run: `go test ./internal/gateway/ -run 'RecordIndex|State' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing QueryAudit tests**

Create `internal/gateway/queryaudit_test.go`:

```go
package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func (e *testEnv) query(req *gatewayv1.QueryAuditRequest) (*gatewayv1.QueryAuditResponse, error) {
	if req.Intent == nil {
		req.Intent = ref()
	}
	return e.g.queryAudit(e.ctx, req)
}

// twoLLMCalls makes two CallLLM calls for the env's Intent: four indexed entries.
func twoLLMCalls(t *testing.T, env *testEnv) {
	t.Helper()
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	// The script has one answer, so this call fails, but it still records request and response.
	_, _ = env.callLLM(&gatewayv1.Message{Role: "assistant", Content: "a"}, &gatewayv1.Message{Role: roleUser, Content: "second"})
}

func TestQueryAuditReturnsTheIntentsEntriesOldestFirst(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	resp, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypeLLMRequest}})
	if err != nil || len(resp.GetEntries()) != 2 || resp.GetTruncated() || resp.GetMore() {
		t.Fatalf("QueryAudit = %v, %v", resp, err)
	}
	first, second := resp.GetEntries()[0], resp.GetEntries()[1]
	if first.GetSeq() >= second.GetSeq() || first.GetType() != audit.TypeLLMRequest {
		t.Fatalf("entries = %v", resp.GetEntries())
	}
	var p llmRequestPayload
	if err := json.Unmarshal(second.GetPayload(), &p); err != nil || len(p.Messages) != 2 || p.Messages[1].Content != "second" {
		t.Fatalf("payload = %s, %v", second.GetPayload(), err)
	}
	after, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypeLLMRequest}, AfterSeq: first.GetSeq()})
	if err != nil || len(after.GetEntries()) != 1 || after.GetEntries()[0].GetSeq() != second.GetSeq() {
		t.Fatalf("after_seq = %v, %v", after, err)
	}
	all, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(all.GetEntries()) != 4 {
		t.Fatalf("every type = %v, %v", all, err)
	}
}

func TestQueryAuditNeverReturnsOtherIntentsEntries(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	api := intentWithPlan(t, twoSteps())
	api.Name, api.UID = "api", "api-uid"
	if err := env.crd.Create(env.ctx, api); err != nil {
		t.Fatal(err)
	}
	cm, err := env.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), "llm-script", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["team-x.api"] = `["api answer"]`
	if _, err := env.kube.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	apiRef := &gatewayv1.IntentRef{Namespace: envNS, Name: "api"}
	if _, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: apiRef, Messages: []*gatewayv1.Message{{Role: roleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	twoLLMCalls(t, env)
	owner := map[uint64]string{}
	for _, e := range env.entries() {
		owner[e.Seq] = e.Intent
	}
	for intent, r := range map[string]*gatewayv1.IntentRef{envKey: ref(), envNS + "/api": apiRef} {
		resp, err := env.query(&gatewayv1.QueryAuditRequest{Intent: r})
		if err != nil || len(resp.GetEntries()) == 0 {
			t.Fatalf("%s: QueryAudit = %v, %v", intent, resp, err)
		}
		for _, e := range resp.GetEntries() {
			if owner[e.GetSeq()] != intent {
				t.Fatalf("%s: returned entry %d of %s", intent, e.GetSeq(), owner[e.GetSeq()])
			}
		}
	}
}

func TestQueryAuditAfterSeqBeyondTheHeadIsEmpty(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	resp, err := env.query(&gatewayv1.QueryAuditRequest{AfterSeq: 1 << 40})
	if err != nil || len(resp.GetEntries()) != 0 || resp.GetMore() {
		t.Fatalf("QueryAudit = %v, %v", resp, err)
	}
}

func TestQueryAuditLimitPagesWithMore(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	page, err := env.query(&gatewayv1.QueryAuditRequest{Limit: 1})
	if err != nil || len(page.GetEntries()) != 1 || !page.GetMore() {
		t.Fatalf("first page = %v, %v", page, err)
	}
	next, err := env.query(&gatewayv1.QueryAuditRequest{Limit: 3, AfterSeq: page.GetEntries()[0].GetSeq()})
	if err != nil || len(next.GetEntries()) != 3 || next.GetMore() {
		t.Fatalf("next page = %v, %v", next, err)
	}
}

func TestQueryAuditRejectsBadQueries(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	for name, req := range map[string]*gatewayv1.QueryAuditRequest{
		"checkpoint type":  {Types: []string{audit.TypeCheckpoint}},
		"policy.denied":    {Types: []string{audit.TypePolicyDenied}},
		"limit too large":  {Limit: maxIndexEntries + 1},
		"negative limit":   {Limit: -1},
		"bad intent":       {Intent: &gatewayv1.IntentRef{Namespace: "Not_A_Namespace", Name: "x"}},
	} {
		if _, err := env.query(req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	env.g.writer.Store(nil)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("no writer: %v, want Unavailable", err)
	}
}

func TestQueryAuditTheIndexSurvivesARestart(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	before, err := env.query(&gatewayv1.QueryAuditRequest{}) // flushes the index
	if err != nil {
		t.Fatal(err)
	}
	env.g = env.newGateway()
	after, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(after.GetEntries()) != len(before.GetEntries()) {
		t.Fatalf("after a restart: %v, %v; want %d entries", after, err, len(before.GetEntries()))
	}
	// Entries collected but not flushed before a restart are missing, never an error.
	_, _ = env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "lost"})
	env.g = env.newGateway()
	again, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || len(again.GetEntries()) != len(before.GetEntries()) {
		t.Fatalf("unflushed entries: %v, %v", again, err)
	}
}

func TestQueryAuditReportsTruncation(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); err != nil {
		t.Fatal(err)
	}
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.DropIndex(1)
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	resp, err := env.query(&gatewayv1.QueryAuditRequest{})
	if err != nil || !resp.GetTruncated() || len(resp.GetEntries()) != 3 {
		t.Fatalf("after_seq 0 = %v, %v; want truncated", resp, err)
	}
	resp, err = env.query(&gatewayv1.QueryAuditRequest{AfterSeq: rec.IndexFloor})
	if err != nil || resp.GetTruncated() {
		t.Fatalf("after the floor = %v, %v; want not truncated", resp, err)
	}
}

func TestQueryAuditRefusesAnEntryThatIsNotWhatTheIndexSays(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	twoLLMCalls(t, env)
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); err != nil {
		t.Fatal(err)
	}
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.Index[0].Type = audit.TypeCommandResult
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.query(&gatewayv1.QueryAuditRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("QueryAudit = %v, want Internal", err)
	}
}

func TestFlushIndexDropsOldestEntriesWhenTheRecordIsFull(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.Executed = &ExecutedPlan{Stage: "Create", PlanHash: "sha256:x", Plan: strings.Repeat("p", MaxRecordBytes-2000)}
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= maxIndexEntries; seq++ {
		env.g.index.observe(audit.Entry{Seq: seq, Type: audit.TypeLLMRequest, Intent: envKey})
	}
	if err := env.g.flushIndex(env.ctx, envKey); err != nil {
		t.Fatal(err)
	}
	rec, err = env.g.state.Load(env.ctx, envKey)
	if err != nil || len(rec.Index) == 0 || len(rec.Index) == maxIndexEntries || rec.IndexFloor == 0 || rec.Executed == nil {
		t.Fatalf("record = %d index entries, floor %d, executed %v; %v", len(rec.Index), rec.IndexFloor, rec.Executed != nil, err)
	}
}
```

Run: `go test ./internal/gateway/ -run 'QueryAudit|FlushIndex'`
Expected: FAIL to compile, with `env.g.queryAudit undefined` and `env.g.index undefined`.

- [ ] **Step 6: Implement the index and the RPC**

Create `internal/gateway/index.go`:

```go
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// indexedTypes are the audit entry types QueryAudit serves (M3 spec §3.5).
var indexedTypes = map[string]bool{
	audit.TypeLLMRequest: true, audit.TypeLLMResponse: true,
	audit.TypeCommandStart: true, audit.TypeCommandResult: true,
	audit.TypePhaseBegin: true, audit.TypePhaseEnd: true,
}

const (
	// indexFlushInterval is how often collected index entries are saved.
	indexFlushInterval = 2 * time.Second
	// indexDropStep is how many of the oldest index entries go at a time when a
	// record would outgrow its ConfigMap.
	indexDropStep = 50
)

// auditIndex collects indexed entries as the writer appends them, until a flush
// saves them into each Intent's record. It is best-effort: entries collected but
// not yet saved are lost if the process stops.
type auditIndex struct {
	mu      sync.Mutex
	pending map[string][]IndexEntry
}

func (x *auditIndex) observe(e audit.Entry) {
	if e.Intent == "" || !indexedTypes[e.Type] {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending == nil {
		x.pending = map[string][]IndexEntry{}
	}
	x.pending[e.Intent] = append(x.pending[e.Intent], IndexEntry{Seq: e.Seq, Type: e.Type})
}

func (x *auditIndex) take(intent string) []IndexEntry {
	x.mu.Lock()
	defer x.mu.Unlock()
	es := x.pending[intent]
	delete(x.pending, intent)
	return es
}

func (x *auditIndex) restore(intent string, es []IndexEntry) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending == nil {
		x.pending = map[string][]IndexEntry{}
	}
	x.pending[intent] = append(es, x.pending[intent]...)
}

func (x *auditIndex) intents() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	keys := make([]string, 0, len(x.pending))
	for k := range x.pending {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// observeWrites installs the audit writer's write hook: metrics and the
// QueryAudit index. The hook runs under the writer's mutex, so it only records.
func (g *Gateway) observeWrites(w *audit.Writer) {
	w.SetOnWrite(func(e audit.Entry) {
		g.metrics.ObserveWrite(e)
		g.index.observe(e)
	})
}

// flushIndex saves an Intent's collected index entries into its record. When
// the record would outgrow its ConfigMap, the oldest index entries go first.
func (g *Gateway) flushIndex(ctx context.Context, intent string) error {
	es := g.index.take(intent)
	if len(es) == 0 {
		return nil
	}
	unlock := g.locks.lock(intent)
	defer unlock()
	rec, err := g.state.Load(ctx, intent)
	if err != nil {
		g.index.restore(intent, es)
		return err
	}
	rec.AddIndex(es)
	for {
		err := g.state.Save(ctx, rec)
		if errors.Is(err, errRecordTooLarge) && len(rec.Index) > 0 {
			rec.DropIndex(indexDropStep)
			continue
		}
		if err != nil {
			g.index.restore(intent, es)
		}
		return err
	}
}

// flushIndexes saves every Intent's collected entries; failures are logged and
// retried on the next flush.
func (g *Gateway) flushIndexes(ctx context.Context) {
	for _, intent := range g.index.intents() {
		if err := g.flushIndex(ctx, intent); err != nil {
			slog.Warn("Could not save the audit index", "intent", intent, "err", err)
		}
	}
}
```

Create `internal/gateway/queryaudit.go`:

```go
package gateway

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

const (
	defaultQueryLimit = 50
	// maxQueryBytes bounds a response's payloads below the 4 MiB default gRPC
	// receive size; the caller pages with after_seq when more is set.
	maxQueryBytes = 3 << 20
)

// queryAudit returns one Intent's indexed audit entries, oldest first, each read
// from the sink and verified (M3 spec §3.5). It writes no audit entry.
func (g *Gateway) queryAudit(ctx context.Context, req *gatewayv1.QueryAuditRequest) (*gatewayv1.QueryAuditResponse, error) {
	_, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	_, _, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range req.GetTypes() {
		if !indexedTypes[t] {
			return nil, status.Errorf(codes.InvalidArgument, "type %q is not served by QueryAudit", truncateReason(t))
		}
		want[t] = true
	}
	limit := int(req.GetLimit())
	switch {
	case limit == 0:
		limit = defaultQueryLimit
	case limit < 0 || limit > maxIndexEntries:
		return nil, status.Errorf(codes.InvalidArgument, "limit must be between 1 and %d", maxIndexEntries)
	}
	if err := g.flushIndex(ctx, key); err != nil {
		slog.Warn("Could not save the audit index", "intent", key, "err", err)
	}
	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.metrics.ObserveQuery()
	resp := &gatewayv1.QueryAuditResponse{Truncated: req.GetAfterSeq() < rec.IndexFloor}
	size := 0
	for _, ie := range rec.Index {
		if ie.Seq <= req.GetAfterSeq() || (len(want) > 0 && !want[ie.Type]) {
			continue
		}
		if len(resp.Entries) == limit {
			resp.More = true
			break
		}
		e, err := g.readIndexed(ctx, key, ie)
		if err != nil {
			return nil, err
		}
		if len(resp.Entries) > 0 && size+len(e.Payload) > maxQueryBytes {
			resp.More = true
			break
		}
		size += len(e.Payload)
		resp.Entries = append(resp.Entries, &gatewayv1.AuditEntry{Seq: e.Seq, Type: e.Type, Ts: e.TS, Stage: e.Stage,
			SubjectHash: e.SubjectHash, Payload: e.Payload})
	}
	return resp, nil
}

// readIndexed reads one indexed entry from the sink and verifies it: strict
// decoding, the signature, and that it is the entry the index says it is.
func (g *Gateway) readIndexed(ctx context.Context, intent string, ie IndexEntry) (audit.Entry, error) {
	body, err := g.sink.Get(ctx, audit.EntryKey(g.cfg.ChainID, ie.Seq), "")
	if errors.Is(err, audit.ErrNotFound) {
		return audit.Entry{}, status.Errorf(codes.Internal, "indexed audit entry %d does not exist", ie.Seq)
	}
	if err != nil {
		return audit.Entry{}, status.Errorf(codes.Unavailable, "read audit entry %d: %v", ie.Seq, err)
	}
	e, err := audit.DecodeEntry(body)
	if err != nil {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d failed verification: %v", ie.Seq, err)
	}
	if err := audit.CheckSeal(g.signer.Public(), e); err != nil {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d failed verification: %v", ie.Seq, err)
	}
	if e.Seq != ie.Seq || e.Intent != intent || e.Type != ie.Type {
		return audit.Entry{}, status.Errorf(codes.Internal, "audit entry %d is not the indexed %s of %s", ie.Seq, ie.Type, intent)
	}
	return e, nil
}
```

Wire it up:
- **`internal/gateway/server.go`:** add `index auditIndex` to the `Gateway` struct, after `probing`.
- **`lead`:**
  - replace `w.SetOnWrite(g.metrics.ObserveWrite)` with `g.observeWrites(w)`;
  - add `flush := time.NewTicker(indexFlushInterval)` and `defer flush.Stop()` next to the existing `ticker`;
  - add a case to the loop's `select`:

```go
		case <-flush.C:
			g.flushIndexes(ctx)
```

- **`internal/gateway/rpc.go`:** add

```go
func (s *rpcServer) QueryAudit(ctx context.Context, req *gatewayv1.QueryAuditRequest) (*gatewayv1.QueryAuditResponse, error) {
	return s.g.queryAudit(ctx, req)
}
```

- **`internal/gateway/metrics.go`:** add a counter `queries prometheus.Counter` (`last_audit_queries_total`, "QueryAudit calls.") and register it. Add:

```go
// ObserveQuery counts one QueryAudit call.
func (m *Metrics) ObserveQuery() { m.queries.Inc() }
```

- **`internal/gateway/testenv_test.go` `newGateway`:** after `g.writer.Store(w)`, add `g.observeWrites(w)`.

Run: `go test ./internal/gateway/ -count=1 -v -run 'QueryAudit|FlushIndex|RecordIndex'`
Expected: PASS. Then run `go test ./internal/gateway/ -count=1` and expect PASS.

- [ ] **Step 7: Amend the main spec**

In §4.2's API table, replace the `QueryAudit` row with:

```markdown
| `QueryAudit` | Read-only. Returns one Intent's verified audit entries (`llm.*`, `command.*`, `phase.*`), oldest first, from a bounded per-Intent index the gateway keeps in its record. Used for conversation reload (§7) and, later, rule suggestions |
```

- [ ] **Step 8: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add proto/ internal/gatewayapi/ internal/audit/verify.go internal/audit/verify_test.go internal/gateway/ docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(gateway): serve verified per-Intent audit entries through QueryAudit"
```

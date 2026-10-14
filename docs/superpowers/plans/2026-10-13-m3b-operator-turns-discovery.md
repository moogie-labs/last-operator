# M3b: Operator Turns, Clarifying and Discovery — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator plans through structured turns (`plan`, `questions` or `command`). It asks clarifying questions, runs approved discovery commands as one-step Discovery phases, caps each planning session, and rebuilds a lost conversation from `QueryAudit`.

**Architecture:**
- **`internal/agent`** owns the model-facing side:
  - the wire format of a turn, its JSON Schema, and its parser and encoder;
  - the `Conversation` type and `Planner.Next`, which returns one valid turn per call and handles corrections and caps;
  - the prompts and user turns;
  - reading the audit log (`DiscoveryResult`, `Reload`).
- **`internal/controller`** drives a planning session across reconciles, keeping the user turn the model is owed in the Intent's status:
  - Planning sends the conversation and records the turn it gets back.
  - Clarifying waits for `spec.answers`.
  - AwaitingDiscoveryApproval runs the approved command through the gateway and records how it ended in the `Executed` condition.
  - Conversations live in memory, keyed by Intent and session, and are reloaded from the audit log on a miss.

**Tech Stack:**
- Go 1.26
- kubebuilder and controller-runtime
- gRPC (the `gatewayv1` client)
- Ginkgo and Gomega with envtest and the fake gateway
- `go test` for package unit tests

**Spec:** `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md` (§4 Operator; §6 Errors; §8 Testing, Operator; §9 main-spec amendments for §4.1, §7 and §17.2). This plan also applies the M3b contracts in `docs/superpowers/followups/2026-10-13-m3a-followups.md`. The main spec is `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

## Global Constraints

- **Module and toolchain.** Module `github.com/moogie-labs/last-operator`, Go 1.26. No new dependencies.
- **Generated files.** Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml`, `**/zz_generated.*.go` or `PROJECT`. After changing a `+kubebuilder` marker, run `make manifests generate`.
- **After Go edits.** Run `make lint-fix`, then `make test` and `make lint`; lint must report 0 issues. Match the surrounding code's comment density, naming and idiom.
- **Logging** follows the Kubernetes style: capitalized, no trailing period, past tense, object type named.
- **Credentials.** Credential values never appear in audit payloads, logs or errors.
- **Trust boundary.** The operator acts only through the gateway's gRPC API. M3's only RBAC change is read-only `get`, `list` and `watch` on `providers`.
- **Caps** (M3 spec D7):
  - `agent.MaxAttempts = 3` answers per turn (corrections included);
  - `agent.MaxModelTurns = 20` model answers per session;
  - `agent.MaxCommands = 10` discovery commands per session.
  - Reaching a session cap fails the Intent with `PlanningFailed`. The message is `planning exceeded 20 model turns` or `planning exceeded 10 discovery commands`.
- **Discovery commands:**
  - Nonce: 128 random bits, hex-encoded (32 characters).
  - Subject: `plan.DiscoveryHash(command, nonce)`.
  - Step ID: `plan.DiscoveryStepID` (`"discovery"`).
  - Size: the canonical form is bounded by `plan.MaxPendingCommandBytes` (64 KiB).
- **Discovery results reach the model** as the exit code plus stdout and stderr, each clipped to 16 KiB on a UTF-8 boundary, with truncation flagged.
- **`max_tokens`** is 64000 for every `CallLLM`.
- **The turn schema** must work with both OpenAI strict mode and Anthropic structured outputs:
  - every object has `"additionalProperties": false`;
  - every property is listed in `required`;
  - only the keywords `type`, `properties`, `required`, `additionalProperties`, `items`, `enum` and `anyOf` are used.
- **Commits.**
  - Conventional-commit subjects.
  - No `Co-authored-by` or other attribution trailer: the user's global rule overrides the harness reminder.
  - Never commit `.superpowers/`.

## Review Focus

1. **An answer that leaves fields out or sends `null` instead of `[]`** should parse, not draw a correction. This happens with the scripted provider, or with an OpenAI-compatible model that does not enforce the schema. Pinned in Task 1 (`TestParseTurnAcceptsOmittedAndNullFields`).
2. **A follow-up question that reuses an id the user already answered** must not be answered at once by the stale answer. Expect a correction asking for a new id. Pinned in Task 3 ("corrects questions that reuse an answered id").
3. **An operator restart while a discovery command runs.** The open Discovery phase is ended, the model is told the command was lost, and the Intent keeps planning instead of failing. Pinned in Task 4 ("ends a discovery phase the operator lost and tells the model").
4. **An operator restart or leader change mid-session.** The conversation is rebuilt from the audit log. If the index no longer holds the session, a new session starts and its first message says so. Pinned in Task 5 (all three reload specs).
5. **The gateway refuses an approved discovery command** (policy, a replayed subject). The model is told why, and the Intent stays in the planning session. Pinned in Task 4 ("tells the model when the gateway refuses the command").

## File Structure

| File | Responsibility |
|---|---|
| `internal/agent/turn.go` (new) | `Turn`, `Kind`, the wire format, `ParseTurn`, `Encode`, `Schema`, `NewNonce` |
| `internal/agent/parse.go` | Shared decoding (`decodeOne`), plan checks (`checkPlan`, `checkCommand`). The M2 `Parse` is removed in Task 3. |
| `internal/agent/agent.go` | `Conversation`, `Planner.Next`, caps, `CapError`, `ErrNoTurn`, `ErrNoSeq` |
| `internal/agent/prompt.go` | The system prompt, `FirstMessage`, `Environment`, and the user turns (answers, denial, not run, result) |
| `internal/agent/audit.go` (new) | `Auditor`, paging (`scan`), `DiscoveryResult`, `Reload`, `ErrNoReload` |
| `internal/controller/sessions.go` (new) | In-memory session store, `conversation`, `newConversation`, `pendingTurn`, `environment` |
| `internal/controller/intent_planning.go` | `startPlanning`, `plan`, `record`, stored-subject check, `awaitApproval`, `decision` |
| `internal/controller/intent_clarifying.go` (new) | `clarify`, `answered`, `answeredCheck` |
| `internal/controller/intent_discovery.go` (new) | `awaitDiscovery`, `runDiscovery`, `discovered` |
| `internal/controller/intent_execution.go` | `runStep`/`endPhase` take the subject; `endPhase` retries `Aborted` |
| `internal/controller/intent_controller.go` | Phase dispatch, session store field, RBAC marker |
| `internal/controller/status.go` | New reasons; `fail` drops the session |
| `internal/policy/policy.go` | `Effective.Egress()` |
| `internal/controller/*_test.go` | Fake gateway (audit log, Discovery, `QueryAudit`), helpers, specs |
| `test/e2e/operator_e2e_test.go` | Scripted answer encoded as a plan turn |
| `charts/last-operator/files/operator-clusterrole.yaml`, `test/admission/operator_rbac_test.go` | Read-only RBAC on providers |
| `docs/superpowers/specs/*.md`, `docs/superpowers/followups/2026-10-13-m3a-followups.md` | Amendments |

---
### Task 1: Turn wire format, parser, encoder and schema

The model answers with one turn object. Structured outputs need closed objects with every property required, so the wire format is not the CRD's JSON:
- maps become arrays of pairs;
- the free-form `identity` and `desired` become JSON strings;
- fields a kind does not use are empty.

This task adds the new format next to the M2 `Parse`, which keeps working until Task 3 removes it.

**Files:**
- Create: `internal/agent/turn.go`
- Create: `internal/agent/turn_test.go`
- Modify: `internal/agent/parse.go` (extract `decodeOne` and `checkPlan`; `Parse` keeps its behaviour)

**Interfaces:**
- Consumes:
  - `plan.Validate`, `plan.Canonical`, `plan.Hash`, `plan.FromStatus`;
  - `plan.DiscoveryHash(c lastv1alpha1.Command, nonce string) (string, int, error)`;
  - `plan.DiscoveryStepID`, `plan.MaxPendingCommandBytes`.
- Produces:
  - `type Kind string`; `KindPlan`, `KindQuestions`, `KindCommand`.
  - `type Turn struct { Kind Kind; Plan *lastv1alpha1.Plan; Questions []lastv1alpha1.Question; Command *lastv1alpha1.Command; Reason string }`
  - `func ParseTurn(content string) (Turn, error)`.
  - `func Encode(t Turn) (string, error)`. A plan turn's `Plan.Hash` is ignored; `ParseTurn` recomputes it.
  - `func Schema() []byte`: the JSON Schema of a turn, a fresh copy on every call.
  - `func NewNonce() (string, error)`: 32 hex characters.
  - `const MaxQuestions = 5`, `const NonceBytes = 16`.
  - In `parse.go`: `decodeOne(content string, v any, what string) error` and `checkPlan(summary string, steps []lastv1alpha1.Command, resources []lastv1alpha1.ManagedResourceSpec) (*lastv1alpha1.Plan, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/turn_test.go`:

```go
package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// fullPlan uses every field the wire format carries.
var fullPlan = lastv1alpha1.Plan{Summary: "nginx", Steps: []lastv1alpha1.Command{
	{ID: "apply", Reason: "create it", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "apply", "-f", "/work/a.yaml"}},
		Files:     map[string]string{"/work/a.yaml": "kind: Deployment", "/work/b.yaml": "kind: Service"},
		Providers: []string{"k8s"}, Egress: []string{"10.0.0.1:443"}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
	{ID: "svc", Run: lastv1alpha1.CommandRun{Script: "kubectl get svc web -o json"}},
}, Resources: []lastv1alpha1.ManagedResourceSpec{{
	Description: "web",
	Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","name":"web"}`)},
	Desired:     &runtime.RawExtension{Raw: []byte(`{"replicas":2}`)},
	Probes: []lastv1alpha1.Probe{{ID: "ready",
		Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "result.exitCode == 0"}},
	OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
	Outputs:          map[string]string{"ip": "${steps.svc.json.spec.clusterIP}"},
	Teardown:         []lastv1alpha1.Command{{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "delete", "deploy", "web"}}}},
}}}

const oneOfArgvScript = "exactly one of run.argv and run.script"

func encode(t *testing.T, turn Turn) string {
	t.Helper()
	s, err := Encode(turn)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEncodeAndParseTurnRoundTripAPlan(t *testing.T) {
	got, err := ParseTurn(encode(t, Turn{Kind: KindPlan, Plan: &fullPlan}))
	if err != nil {
		t.Fatalf("ParseTurn = %v", err)
	}
	want, err := plan.Hash(plan.FromStatus(&fullPlan))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindPlan || got.Plan.Hash != want {
		t.Fatalf("turn = %+v, want a plan hashed %s", got, want)
	}
	got.Plan.Hash = ""
	if !reflect.DeepEqual(*got.Plan, fullPlan) {
		t.Errorf("plan = %+v\nwant %+v", *got.Plan, fullPlan)
	}
}

func TestParseTurnReadsQuestionsAndCommands(t *testing.T) {
	qs := []lastv1alpha1.Question{{ID: "q1", Text: "Which namespace?", Options: []string{"default", "web"}}, {ID: "q2", Text: "Public?"}}
	got, err := ParseTurn(encode(t, Turn{Kind: KindQuestions, Questions: qs}))
	if err != nil || got.Kind != KindQuestions || !reflect.DeepEqual(got.Questions, qs) {
		t.Fatalf("questions turn = %+v, %v", got, err)
	}

	cmd := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "ns"}}, Providers: []string{"k8s"}}
	got, err = ParseTurn(encode(t, Turn{Kind: KindCommand, Command: &cmd, Reason: "see what exists"}))
	if err != nil || got.Kind != KindCommand || got.Reason != "see what exists" {
		t.Fatalf("command turn = %+v, %v", got, err)
	}
	want := cmd
	want.ID = plan.DiscoveryStepID
	if !reflect.DeepEqual(*got.Command, want) {
		t.Errorf("command = %+v, want %+v", *got.Command, want)
	}
}

func TestParseTurnAcceptsFencesAndWhitespace(t *testing.T) {
	turn := encode(t, Turn{Kind: KindQuestions, Questions: []lastv1alpha1.Question{{ID: "q1", Text: "Which?"}}})
	for name, content := range map[string]string{
		"fenced":        "```json\n" + turn + "\n```",
		"bare fence":    "```\n" + turn + "\n```",
		"surrounded ws": "\n  " + turn + "  \n",
	} {
		if _, err := ParseTurn(content); err != nil {
			t.Errorf("%s: ParseTurn = %v", name, err)
		}
	}
}

// A model without enforced structured output may leave fields out or send null.
func TestParseTurnAcceptsOmittedAndNullFields(t *testing.T) {
	for name, content := range map[string]string{
		"omitted":   `{"kind":"plan","steps":[{"id":"s1","argv":["true"]}]}`,
		"nulls":     `{"kind":"plan","summary":null,"steps":[{"id":"s1","argv":["true"],"files":null}],"resources":null,"questions":null,"command":null}`,
		"questions": `{"kind":"questions","questions":[{"id":"q1","text":"Which?"}]}`,
		"command":   `{"kind":"command","reason":"look","command":{"argv":["kubectl","get","ns"]}}`,
	} {
		if _, err := ParseTurn(content); err != nil {
			t.Errorf("%s: ParseTurn = %v", name, err)
		}
	}
}

func TestParseTurnRefusesMalformedTurns(t *testing.T) {
	step := `{"id":"s1","argv":["true"]}`
	planTurn := func(rest string) string { return `{"kind":"plan","steps":[` + step + `]` + rest + `}` }
	resource := func(fields string) string { return planTurn(`,"resources":[{` + fields + `}]`) }
	big := strings.Repeat("x", MaxPlanBytes)
	file := strings.Repeat("y", 70<<10)
	sixQuestions := make([]string, 0, 6)
	for i := range 6 {
		sixQuestions = append(sixQuestions, fmt.Sprintf(`{"id":"q%d","text":"?"}`, i))
	}
	cases := map[string]struct{ content, want string }{
		"prose":             {"Here is my turn: " + planTurn(""), "not a turn object"},
		"trailing text":     {planTurn("") + " hope this helps", "text after the turn object"},
		"unknown field":     {planTurn(`,"extra":1`), "unknown field"},
		"unknown kind":      {`{"kind":"answer"}`, `kind "answer" is not one of`},
		"plan and command":  {planTurn(`,"command":{"argv":["true"]}`), "a plan turn leaves"},
		"no steps":          {`{"kind":"plan"}`, "at least one step"},
		"argv and script":   {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"script":"b"}]}`, oneOfArgvScript},
		"neither":           {`{"kind":"plan","steps":[{"id":"s1"}]}`, oneOfArgvScript},
		"duplicate file":    {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"files":[{"path":"/w/a"},{"path":"/w/a"}]}]}`, "listed twice"},
		"bad timeout":       {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"timeout":"soon"}]}`, "is not a duration"},
		"probe without id":  {resource(`"probes":[{"command":{"argv":["true"]},"assert":"true"}]`), "probe 0 of resource 0 has no id"},
		"probe no assert":   {resource(`"probes":[{"id":"p","command":{"argv":["true"]}}]`), `probe "p" has no assert`},
		"probe bad command": {resource(`"probes":[{"id":"p","command":{},"assert":"true"}]`), oneOfArgvScript},
		"identity array":    {resource(`"identity":"[1]"`), "JSON object"},
		"desired garbage":   {resource(`"desired":"{nope"`), "JSON object"},
		"literal output":    {resource(`"outputsSecret":"o","outputs":[{"key":"k","value":"hunter2"}]`), "not a single step reference"},
		"duplicate output":  {resource(`"outputsSecret":"o","outputs":[{"key":"k","value":"${steps.s1.stdout}"},{"key":"k","value":"${steps.s1.stdout}"}]`), "listed twice"},
		"too large":         {`{"kind":"plan","steps":[{"id":"s1","argv":["echo","` + big + `"]}]}`, "the limit is"},
		"no questions":      {`{"kind":"questions","questions":[]}`, "1 to 5 questions"},
		"six questions":     {`{"kind":"questions","questions":[` + strings.Join(sixQuestions, ",") + `]}`, "1 to 5 questions"},
		"question no id":    {`{"kind":"questions","questions":[{"id":" ","text":"?"}]}`, "question 0 has no id"},
		"question repeated": {`{"kind":"questions","questions":[{"id":"q1","text":"?"},{"id":"q1","text":"?"}]}`, `duplicate question id "q1"`},
		"question no text":  {`{"kind":"questions","questions":[{"id":"q1","text":""}]}`, `question "q1" has no text`},
		"empty option":      {`{"kind":"questions","questions":[{"id":"q1","text":"?","options":["a",""]}]}`, `question "q1" has an empty option`},
		"questions + steps": {`{"kind":"questions","questions":[{"id":"q1","text":"?"}],"steps":[` + step + `]}`, "a questions turn leaves"},
		"no command":        {`{"kind":"command","reason":"look"}`, "needs a command"},
		"no reason":         {`{"kind":"command","command":{"argv":["true"]}}`, "needs a reason"},
		"step reference":    {`{"kind":"command","reason":"look","command":{"argv":["echo","${steps.s1.stdout}"]}}`, "${steps"},
		"command both":      {`{"kind":"command","reason":"look","command":{"argv":["a"],"script":"b"}}`, oneOfArgvScript},
		"command too large": {`{"kind":"command","reason":"look","command":{"argv":["cat","/w/f"],"files":[{"path":"/w/f","content":"` + file + `"}]}}`, "the limit is"},
		"command + summary": {`{"kind":"command","summary":"s","reason":"look","command":{"argv":["true"]}}`, "a command turn leaves"},
	}
	for name, tc := range cases {
		_, err := ParseTurn(tc.content)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ParseTurn = %v, want an error containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats an output value", name)
		}
	}
}

// The schema must stay inside what OpenAI strict mode and Anthropic structured
// outputs accept, and must describe exactly the wire types ParseTurn decodes.
func TestSchemaIsStrictAndMatchesTheWireFormat(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "turn", root, reflect.TypeFor[wireTurn]())
	if kinds := root["properties"].(map[string]any)["kind"].(map[string]any)["enum"]; !reflect.DeepEqual(kinds,
		[]any{"plan", "questions", "command"}) {
		t.Errorf("kind enum = %v", kinds)
	}
	s := Schema()
	s[0] = 'x'
	if Schema()[0] == 'x' {
		t.Error("Schema returns shared storage")
	}
}

var allowedKeywords = []string{"type", "properties", "required", "additionalProperties", "items", "enum", "anyOf"}

func checkSchema(t *testing.T, path string, node map[string]any, typ reflect.Type) {
	t.Helper()
	for k := range node {
		if !slices.Contains(allowedKeywords, k) {
			t.Errorf("%s: keyword %q", path, k)
		}
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		if len(anyOf) != 2 || anyOf[1].(map[string]any)["type"] != "null" {
			t.Fatalf("%s: anyOf is not <type> or null", path)
		}
		checkSchema(t, path, anyOf[0].(map[string]any), typ)
		return
	}
	switch node["type"] {
	case "object":
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties is not false", path)
		}
		props := node["properties"].(map[string]any)
		required := node["required"].([]any)
		if len(required) != len(props) || typ.NumField() != len(props) {
			t.Errorf("%s: %d properties, %d required, %d wire fields", path, len(props), len(required), typ.NumField())
		}
		for i := range typ.NumField() {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			p, ok := props[name].(map[string]any)
			if !ok || !slices.Contains(required, any(name)) {
				t.Errorf("%s: property %q is missing or not required", path, name)
				continue
			}
			checkSchema(t, path+"."+name, p, typ.Field(i).Type)
		}
	case "array":
		if typ.Kind() != reflect.Slice {
			t.Errorf("%s: array for %s", path, typ)
			return
		}
		checkSchema(t, path+"[]", node["items"].(map[string]any), typ.Elem())
	case "string":
		if typ.Kind() != reflect.String {
			t.Errorf("%s: string for %s", path, typ)
		}
	default:
		t.Errorf("%s: type %v", path, node["type"])
	}
}

func TestNewNonceIs128RandomBits(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) || a == b {
		t.Fatalf("nonces %q and %q", a, b)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run 'Turn|Schema|Nonce'`
Expected: FAIL to compile (`undefined: Encode`, `undefined: ParseTurn`, `undefined: wireTurn`, …).

- [ ] **Step 3: Extract the shared decoding and plan checks**

Replace `internal/agent/parse.go` with:

```go
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// MaxPlanBytes bounds a plan's canonical JSON. It stays well under the gateway's
// bounds (512 KiB per plan, about 900 KiB per record) and the Intent object size.
const MaxPlanBytes = 256 << 10

// answer is the JSON object the model must answer with (see SystemPrompt).
type answer struct {
	Summary   string                             `json:"summary"`
	Steps     []lastv1alpha1.Command             `json:"steps"`
	Resources []lastv1alpha1.ManagedResourceSpec `json:"resources,omitempty"`
}

var fence = regexp.MustCompile("(?s)^\\s*```(?:json)?[ \\t]*\\n(.*?)\\n?```\\s*$")

// Parse reads a model answer: one JSON object, optionally inside a ```json
// fence. It checks everything the gateway and the CRD schema would refuse, so a
// bad answer becomes a correction turn rather than a rejected status write.
func Parse(content string) (*lastv1alpha1.Plan, error) {
	var a answer
	if err := decodeOne(content, &a, "plan"); err != nil {
		return nil, err
	}
	return checkPlan(a.Summary, a.Steps, a.Resources)
}

// decodeOne reads content as exactly one JSON object, optionally inside a
// ```json fence, refusing unknown fields. what names the object in errors.
func decodeOne(content string, v any, what string) error {
	raw := strings.TrimSpace(content)
	if m := fence.FindStringSubmatch(content); m != nil {
		raw = m[1]
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the answer is not a %s object: %w", what, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the answer has text after the %s object", what)
	}
	return nil
}

// checkPlan checks a plan for everything the gateway and the CRD schema would
// refuse, and hashes it.
func checkPlan(summary string, steps []lastv1alpha1.Command, resources []lastv1alpha1.ManagedResourceSpec) (*lastv1alpha1.Plan, error) {
	for _, s := range steps {
		if err := checkCommand("step "+s.ID, s); err != nil {
			return nil, err
		}
	}
	for i, r := range resources {
		if r.IntentRef != nil {
			return nil, fmt.Errorf("resource %d sets intentRef; the operator sets it", i)
		}
		for j, p := range r.Probes {
			if p.ID == "" {
				return nil, fmt.Errorf("probe %d of resource %d has no id", j, i)
			}
			if p.Assert == "" {
				return nil, fmt.Errorf("probe %q has no assert", p.ID)
			}
			if err := checkCommand("probe "+p.ID, p.Command); err != nil {
				return nil, err
			}
		}
	}
	body := plan.Body{Steps: steps, Resources: resources}
	if err := plan.Validate(body); err != nil {
		return nil, err
	}
	canonical, err := plan.Canonical(body)
	if err != nil {
		return nil, err
	}
	if len(canonical) > MaxPlanBytes {
		return nil, fmt.Errorf("the plan is %d bytes; the limit is %d", len(canonical), MaxPlanBytes)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		return nil, err
	}
	return &lastv1alpha1.Plan{Hash: hash, Summary: summary, Steps: steps, Resources: resources}, nil
}

// checkCommand mirrors the CRD's rule for commands and refuses image builds,
// which arrive in M4.
func checkCommand(where string, c lastv1alpha1.Command) error {
	if (len(c.Run.Argv) > 0) == (c.Run.Script != "") {
		return fmt.Errorf("%s: set exactly one of run.argv and run.script", where)
	}
	if c.Image != nil {
		return fmt.Errorf("%s: image.install is not supported yet", where)
	}
	return nil
}
```

Run: `go test ./internal/agent/ -run 'TestParse$|TestParseAccepts|TestPlanner'`
Expected: PASS. `Parse` behaves as before.

- [ ] **Step 4: Write the turn format**

Create `internal/agent/turn.go`:

```go
package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// Kind is what a turn does (M3 spec §4.1).
type Kind string

// Turn kinds.
const (
	KindPlan      Kind = "plan"
	KindQuestions Kind = "questions"
	KindCommand   Kind = "command"
)

const (
	// MaxQuestions bounds one questions turn.
	MaxQuestions = 5
	// NonceBytes is the size of a discovery nonce before hex encoding: 128 bits.
	NonceBytes = 16
	// maxQuestionsBytes bounds a questions turn's JSON, so status.questions stays small.
	maxQuestionsBytes = 16 << 10
	// maxReasonBytes bounds a discovery command's reason.
	maxReasonBytes = 4 << 10
)

// Turn is one valid model answer.
type Turn struct {
	Kind Kind
	// Plan is set for KindPlan, hash included.
	Plan *lastv1alpha1.Plan
	// Questions is set for KindQuestions.
	Questions []lastv1alpha1.Question
	// Command is set for KindCommand, with id plan.DiscoveryStepID; Reason says
	// why the model wants it run.
	Command *lastv1alpha1.Command
	Reason  string
}

// The wire format is the JSON the model writes. Structured outputs (OpenAI
// strict mode, Anthropic's schema subset) need every object closed and every
// property required, so maps become arrays of pairs, free-form objects become
// JSON strings, and a field a turn does not use is empty: "", [], or null for
// command. Decoding is lenient about missing fields and null, for models that
// do not enforce the schema.
type wireTurn struct {
	Kind      Kind           `json:"kind"`
	Summary   string         `json:"summary"`
	Steps     []wireCommand  `json:"steps"`
	Resources []wireResource `json:"resources"`
	Questions []wireQuestion `json:"questions"`
	Reason    string         `json:"reason"`
	Command   *wireCommand   `json:"command"`
}

type wireCommand struct {
	ID        string     `json:"id"`
	Reason    string     `json:"reason"`
	Argv      []string   `json:"argv"`
	Script    string     `json:"script"`
	Files     []wireFile `json:"files"`
	Providers []string   `json:"providers"`
	Egress    []string   `json:"egress"`
	Timeout   string     `json:"timeout"`
}

type wireFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type wireResource struct {
	Description   string        `json:"description"`
	Identity      string        `json:"identity"`
	Desired       string        `json:"desired"`
	Probes        []wireProbe   `json:"probes"`
	OutputsSecret string        `json:"outputsSecret"`
	Outputs       []wireOutput  `json:"outputs"`
	Teardown      []wireCommand `json:"teardown"`
}

type wireProbe struct {
	ID      string      `json:"id"`
	Command wireCommand `json:"command"`
	Assert  string      `json:"assert"`
}

type wireOutput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type wireQuestion struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

// ParseTurn reads a model answer: one turn object, optionally inside a ```json
// fence. It checks everything the gateway and the CRD schema would refuse, so a
// bad answer becomes a correction turn rather than a rejected status write.
func ParseTurn(content string) (Turn, error) {
	var w wireTurn
	if err := decodeOne(content, &w, "turn"); err != nil {
		return Turn{}, err
	}
	switch w.Kind {
	case KindPlan:
		return parsePlan(w)
	case KindQuestions:
		return parseQuestions(w)
	case KindCommand:
		return parseCommand(w)
	}
	return Turn{}, fmt.Errorf("kind %q is not one of plan, questions and command", w.Kind)
}

func parsePlan(w wireTurn) (Turn, error) {
	if len(w.Questions) > 0 || w.Reason != "" || w.Command != nil {
		return Turn{}, errors.New(`a plan turn leaves "questions", "reason" and "command" empty`)
	}
	var steps []lastv1alpha1.Command
	for _, s := range w.Steps {
		c, err := s.command("step " + s.ID)
		if err != nil {
			return Turn{}, err
		}
		steps = append(steps, c)
	}
	var resources []lastv1alpha1.ManagedResourceSpec
	for i, r := range w.Resources {
		spec, err := r.spec(i)
		if err != nil {
			return Turn{}, err
		}
		resources = append(resources, spec)
	}
	p, err := checkPlan(w.Summary, steps, resources)
	if err != nil {
		return Turn{}, err
	}
	return Turn{Kind: KindPlan, Plan: p}, nil
}

func parseQuestions(w wireTurn) (Turn, error) {
	if w.Summary != "" || len(w.Steps) > 0 || len(w.Resources) > 0 || w.Reason != "" || w.Command != nil {
		return Turn{}, errors.New(`a questions turn leaves everything but "questions" empty`)
	}
	if len(w.Questions) == 0 || len(w.Questions) > MaxQuestions {
		return Turn{}, fmt.Errorf("a questions turn asks 1 to %d questions, not %d", MaxQuestions, len(w.Questions))
	}
	seen := map[string]bool{}
	qs := make([]lastv1alpha1.Question, 0, len(w.Questions))
	for i, q := range w.Questions {
		switch {
		case strings.TrimSpace(q.ID) == "":
			return Turn{}, fmt.Errorf("question %d has no id", i)
		case seen[q.ID]:
			return Turn{}, fmt.Errorf("duplicate question id %q", q.ID)
		case strings.TrimSpace(q.Text) == "":
			return Turn{}, fmt.Errorf("question %q has no text", q.ID)
		case slices.ContainsFunc(q.Options, func(o string) bool { return strings.TrimSpace(o) == "" }):
			return Turn{}, fmt.Errorf("question %q has an empty option", q.ID)
		}
		seen[q.ID] = true
		qs = append(qs, lastv1alpha1.Question{ID: q.ID, Text: q.Text, Options: nilIfEmpty(q.Options)})
	}
	if b, err := json.Marshal(qs); err != nil || len(b) > maxQuestionsBytes {
		return Turn{}, fmt.Errorf("the questions are %d bytes; the limit is %d", len(b), maxQuestionsBytes)
	}
	return Turn{Kind: KindQuestions, Questions: qs}, nil
}

func parseCommand(w wireTurn) (Turn, error) {
	const where = "the discovery command"
	switch {
	case w.Summary != "" || len(w.Steps) > 0 || len(w.Resources) > 0 || len(w.Questions) > 0:
		return Turn{}, errors.New(`a command turn leaves everything but "reason" and "command" empty`)
	case w.Command == nil:
		return Turn{}, errors.New("a command turn needs a command")
	case strings.TrimSpace(w.Reason) == "":
		return Turn{}, errors.New("a command turn needs a reason: the approver reads it")
	case len(w.Reason) > maxReasonBytes:
		return Turn{}, fmt.Errorf("the reason is %d bytes; the limit is %d", len(w.Reason), maxReasonBytes)
	}
	c, err := w.Command.command(where)
	if err != nil {
		return Turn{}, err
	}
	c.ID = plan.DiscoveryStepID
	if err := checkCommand(where, c); err != nil {
		return Turn{}, err
	}
	for _, a := range c.Run.Argv {
		if strings.Contains(a, "${steps.") {
			return Turn{}, fmt.Errorf("%s uses %q; a discovery command runs alone and cannot use ${steps…} references", where, a)
		}
	}
	// Every nonce has the same length, so a placeholder gives the exact size.
	_, size, err := plan.DiscoveryHash(c, strings.Repeat("0", 2*NonceBytes))
	if err != nil {
		return Turn{}, err
	}
	if size > plan.MaxPendingCommandBytes {
		return Turn{}, fmt.Errorf("%s is %d bytes; the limit is %d", where, size, plan.MaxPendingCommandBytes)
	}
	return Turn{Kind: KindCommand, Command: &c, Reason: w.Reason}, nil
}

// command converts a wire command. It leaves the run.argv and run.script rule
// to callers, for the commands the CRD schema checks (checkCommand).
func (c wireCommand) command(where string) (lastv1alpha1.Command, error) {
	cmd := lastv1alpha1.Command{ID: c.ID, Reason: c.Reason,
		Run:       lastv1alpha1.CommandRun{Argv: nilIfEmpty(c.Argv), Script: c.Script},
		Providers: nilIfEmpty(c.Providers), Egress: nilIfEmpty(c.Egress)}
	for _, f := range c.Files {
		if _, dup := cmd.Files[f.Path]; dup {
			return cmd, fmt.Errorf("%s: file %q is listed twice", where, f.Path)
		}
		if cmd.Files == nil {
			cmd.Files = map[string]string{}
		}
		cmd.Files[f.Path] = f.Content
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return cmd, fmt.Errorf("%s: timeout %q is not a duration such as \"5m\"", where, c.Timeout)
		}
		cmd.Timeout = &metav1.Duration{Duration: d}
	}
	return cmd, nil
}

func (r wireResource) spec(i int) (lastv1alpha1.ManagedResourceSpec, error) {
	spec := lastv1alpha1.ManagedResourceSpec{Description: r.Description}
	var err error
	if spec.Identity, err = rawObject(r.Identity); err != nil {
		return spec, fmt.Errorf("resource %d identity: %w", i, err)
	}
	if spec.Desired, err = rawObject(r.Desired); err != nil {
		return spec, fmt.Errorf("resource %d desired: %w", i, err)
	}
	for _, p := range r.Probes {
		c, err := p.Command.command("probe " + p.ID)
		if err != nil {
			return spec, err
		}
		spec.Probes = append(spec.Probes, lastv1alpha1.Probe{ID: p.ID, Command: c, Assert: p.Assert})
	}
	if r.OutputsSecret != "" {
		spec.OutputsSecretRef = &lastv1alpha1.LocalRef{Name: r.OutputsSecret}
	}
	for _, o := range r.Outputs {
		if _, dup := spec.Outputs[o.Key]; dup {
			return spec, fmt.Errorf("resource %d output %q is listed twice", i, o.Key)
		}
		if spec.Outputs == nil {
			spec.Outputs = map[string]string{}
		}
		spec.Outputs[o.Key] = o.Value
	}
	for _, t := range r.Teardown {
		c, err := t.command("teardown command " + t.ID)
		if err != nil {
			return spec, err
		}
		spec.Teardown = append(spec.Teardown, c)
	}
	return spec, nil
}

// rawObject decodes a free-form field: "" or a JSON object written as a string.
func rawObject(s string) (*runtime.RawExtension, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, errors.New("must be a JSON object written as a string, or empty")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return nil, err
	}
	return &runtime.RawExtension{Raw: buf.Bytes()}, nil
}

// Encode writes t in the wire format. Tests and scripted conversations use it;
// ParseTurn(Encode(t)) gives t back, with a plan's hash recomputed.
func Encode(t Turn) (string, error) {
	w := wireTurn{Kind: t.Kind, Steps: []wireCommand{}, Resources: []wireResource{}, Questions: []wireQuestion{}}
	switch t.Kind {
	case KindPlan:
		if t.Plan == nil {
			return "", errors.New("agent: a plan turn needs a plan")
		}
		w.Summary = t.Plan.Summary
		for _, s := range t.Plan.Steps {
			c, err := commandToWire(s)
			if err != nil {
				return "", err
			}
			w.Steps = append(w.Steps, c)
		}
		for _, r := range t.Plan.Resources {
			wr, err := resourceToWire(r)
			if err != nil {
				return "", err
			}
			w.Resources = append(w.Resources, wr)
		}
	case KindQuestions:
		for _, q := range t.Questions {
			w.Questions = append(w.Questions, wireQuestion{ID: q.ID, Text: q.Text, Options: nonNil(q.Options)})
		}
	case KindCommand:
		if t.Command == nil {
			return "", errors.New("agent: a command turn needs a command")
		}
		c, err := commandToWire(*t.Command)
		if err != nil {
			return "", err
		}
		w.Reason, w.Command = t.Reason, &c
	default:
		return "", fmt.Errorf("agent: unknown turn kind %q", t.Kind)
	}
	b, err := json.Marshal(w)
	return string(b), err
}

func commandToWire(c lastv1alpha1.Command) (wireCommand, error) {
	if c.Image != nil {
		return wireCommand{}, errors.New("agent: image commands have no wire form")
	}
	w := wireCommand{ID: c.ID, Reason: c.Reason, Argv: nonNil(c.Run.Argv), Script: c.Run.Script, Files: []wireFile{},
		Providers: nonNil(c.Providers), Egress: nonNil(c.Egress)}
	for _, p := range slices.Sorted(maps.Keys(c.Files)) {
		w.Files = append(w.Files, wireFile{Path: p, Content: c.Files[p]})
	}
	if c.Timeout != nil {
		w.Timeout = c.Timeout.Duration.String()
	}
	return w, nil
}

func resourceToWire(r lastv1alpha1.ManagedResourceSpec) (wireResource, error) {
	if r.IntentRef != nil {
		return wireResource{}, errors.New("agent: intentRef has no wire form")
	}
	w := wireResource{Description: r.Description, Probes: []wireProbe{}, Outputs: []wireOutput{}, Teardown: []wireCommand{}}
	if r.Identity != nil {
		w.Identity = string(r.Identity.Raw)
	}
	if r.Desired != nil {
		w.Desired = string(r.Desired.Raw)
	}
	for _, p := range r.Probes {
		c, err := commandToWire(p.Command)
		if err != nil {
			return wireResource{}, err
		}
		w.Probes = append(w.Probes, wireProbe{ID: p.ID, Command: c, Assert: p.Assert})
	}
	if r.OutputsSecretRef != nil {
		w.OutputsSecret = r.OutputsSecretRef.Name
	}
	for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
		w.Outputs = append(w.Outputs, wireOutput{Key: k, Value: r.Outputs[k]})
	}
	for _, t := range r.Teardown {
		c, err := commandToWire(t)
		if err != nil {
			return wireResource{}, err
		}
		w.Teardown = append(w.Teardown, c)
	}
	return w, nil
}

// Schema returns the JSON Schema of a turn, sent as CallLLM's response_schema.
// It stays inside what OpenAI strict mode and Anthropic structured outputs
// accept: every object closed, every property required, no formats or bounds.
// ParseTurn checks the rest.
func Schema() []byte { return slices.Clone(schemaJSON) }

var schemaJSON = buildSchema()

func buildSchema() []byte {
	str := map[string]any{"type": "string"}
	strs := array(str)
	command := object(map[string]any{
		"id": str, "reason": str, "argv": strs, "script": str,
		"files":     array(object(map[string]any{"path": str, "content": str})),
		"providers": strs, "egress": strs, "timeout": str,
	})
	probe := object(map[string]any{"id": str, "command": command, "assert": str})
	resource := object(map[string]any{
		"description": str, "identity": str, "desired": str, "probes": array(probe), "outputsSecret": str,
		"outputs":  array(object(map[string]any{"key": str, "value": str})),
		"teardown": array(command),
	})
	question := object(map[string]any{"id": str, "text": str, "options": strs})
	turn := object(map[string]any{
		"kind":      map[string]any{"type": "string", "enum": []Kind{KindPlan, KindQuestions, KindCommand}},
		"summary":   str,
		"steps":     array(command),
		"resources": array(resource),
		"questions": array(question),
		"reason":    str,
		"command":   map[string]any{"anyOf": []any{command, map[string]any{"type": "null"}}},
	})
	b, err := json.Marshal(turn)
	if err != nil {
		panic(err)
	}
	return b
}

// object is a closed JSON Schema object whose every property is required.
func object(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props,
		"required": slices.Sorted(maps.Keys(props)), "additionalProperties": false}
}

func array(items any) map[string]any { return map[string]any{"type": "array", "items": items} }

// NewNonce returns a fresh discovery nonce: NonceBytes random bytes, hex-encoded.
func NewNonce() (string, error) {
	b := make([]byte, NonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/`
Expected: PASS (the new tests, the M2 `Parse` tests and the `Planner` tests).

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/agent/turn.go internal/agent/turn_test.go internal/agent/parse.go
git commit -m "feat(agent): add the turn wire format, its schema, parser and encoder"
```

---
### Task 2: Conversations, `Planner.Next` and the turn prompts

`Planner.Next` sends a conversation and returns the model's next valid turn. It corrects malformed answers (at most `MaxAttempts` answers per turn) and enforces the session caps, counting from the conversation itself. That way the counts survive a reload.

The new system prompt teaches the turn protocol. `FirstMessage` gives the model the request together with the Providers and egress it may use.

The M2 `Plan`, `SystemPrompt`, `userPrompt` and `correction` stay until Task 3 removes them.

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `internal/agent/prompt.go`
- Modify: `internal/agent/agent_test.go`

**Interfaces:**
- Consumes (Task 1): `ParseTurn`, `Schema`, `Turn`, `KindCommand`, `KindQuestions`.
- Produces:
  - `const MaxAttempts = 3`, `MaxModelTurns = 20`, `MaxCommands = 10`.
  - `var ErrNoTurn`, `var ErrNoSeq`; `type CapError struct{ Max int; What string }`, whose message is `planning exceeded <Max> <What>`.
  - `type Conversation []*gatewayv1.Message`, with:
    - `func NewConversation(first string) Conversation`;
    - `func (c Conversation) With(content string) Conversation`, which appends a user message to a copy;
    - `ModelTurns() int`, `Commands() int`, `Waiting() bool`.
  - `func (p *Planner) Next(ctx context.Context, ref *gatewayv1.IntentRef, conv Conversation, check func(Turn) error) (Turn, Conversation, uint64, error)`. It returns the turn, the conversation with every answer and correction appended, and the `request_seq` of its first `CallLLM`.
  - `Request` gains `Note string` and `Env Environment`.
  - `type Environment struct { Providers []Provider; Egress [][]string; NoScripts bool }` and `type Provider struct { Name, Description string }`.
  - `func FirstMessage(r Request) string`.
  - `func AnswersTurn(qs []lastv1alpha1.Question, as []lastv1alpha1.Answer) string`. The output is exactly `"Answers:\n- q1: v1\n- q2: v2\n"`.
  - `func DeniedTurn(reason string) string`: `"The command was denied: <reason>"`, or `"The command was denied."` when the reason is blank.
  - `func NotRunTurn(why string) string`: `"The command did not run: <why>"`.
  - The unexported `systemPrompt`, `correctionTurn(err error) string`, and `roleSystem`, `roleUser` and `roleAssistant`.

- [ ] **Step 1: Write the failing tests**

In `internal/agent/agent_test.go`:

1. Add `"bytes"` to the imports.
2. Replace `fakeLLM` and its `CallLLM` with the version below.
3. Append the tests below.

```go
// fakeLLM answers in order and records every request. Its llm.request
// sequence numbers start at 101, or are 0 when noSeq is set.
type fakeLLM struct {
	answers []string
	err     error
	noSeq   bool
	reqs    []*gatewayv1.CallLLMRequest
}

func (f *fakeLLM) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	n := min(len(f.reqs), len(f.answers)) - 1
	seq := uint64(100 + len(f.reqs))
	if f.noSeq {
		seq = 0
	}
	return &gatewayv1.CallLLMResponse{Content: f.answers[n], RequestSeq: seq}, nil
}
```

```go
const (
	questionsAnswer = `{"kind":"questions","questions":[{"id":"q1","text":"Which namespace?"}]}`
	commandAnswer   = `{"kind":"command","reason":"look","command":{"argv":["kubectl","get","ns"]}}`
)

var ref = &gatewayv1.IntentRef{Namespace: "team-x", Name: "web"}

func roles(c Conversation) string {
	out := make([]string, 0, len(c))
	for _, m := range c {
		out = append(out, m.GetRole())
	}
	return strings.Join(out, ",")
}

func TestNextAsksForOneTurnWithTheSchema(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	turn, conv, seq, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), nil)
	if err != nil || turn.Kind != KindQuestions || seq != 101 {
		t.Fatalf("Next = %+v, %d, %v", turn, seq, err)
	}
	if roles(conv) != "system,user,assistant" || conv[2].GetContent() != questionsAnswer {
		t.Fatalf("conversation = %v", conv)
	}
	req := llm.reqs[0]
	if req.GetIntent() != ref || req.GetMaxTokens() != maxTokens || !bytes.Equal(req.GetResponseSchema(), Schema()) {
		t.Fatalf("request = %+v", req)
	}
	if req.GetMessages()[0].GetContent() != systemPrompt || req.GetMessages()[1].GetContent() != "run nginx" {
		t.Errorf("messages = %v", req.GetMessages())
	}
}

func TestNextCorrectsMalformedAnswers(t *testing.T) {
	llm := &fakeLLM{answers: []string{"I think you want nginx.", questionsAnswer}}
	turn, conv, seq, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), nil)
	if err != nil || turn.Kind != KindQuestions || seq != 101 {
		t.Fatalf("Next = %+v, %d, %v; want the first call's sequence number", turn, seq, err)
	}
	if roles(conv) != "system,user,assistant,user,assistant" || !strings.Contains(conv[3].GetContent(), "not a valid turn") {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestNextGivesUpAfterMaxAttempts(t *testing.T) {
	llm := &fakeLLM{answers: []string{"no"}}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), nil)
	if !errors.Is(err, ErrNoTurn) || len(llm.reqs) != MaxAttempts {
		t.Fatalf("Next = %v after %d calls, want ErrNoTurn after %d", err, len(llm.reqs), MaxAttempts)
	}
}

func TestNextTurnsACheckFailureIntoACorrection(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer, commandAnswer}}
	check := func(turn Turn) error {
		if turn.Kind == KindQuestions {
			return errors.New(`question id "q1" was answered earlier`)
		}
		return nil
	}
	turn, conv, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), check)
	if err != nil || turn.Kind != KindCommand || !strings.Contains(conv[3].GetContent(), `"q1" was answered earlier`) {
		t.Fatalf("Next = %+v, %v; conversation %v", turn, err, conv)
	}
}

func TestNextStopsAtMaxModelTurns(t *testing.T) {
	conv := NewConversation("run nginx")
	for range MaxModelTurns {
		conv = append(conv, &gatewayv1.Message{Role: roleAssistant, Content: questionsAnswer})
		conv = conv.With("Answers:\n- q1: default\n")
	}
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, conv, nil)
	var capped *CapError
	if !errors.As(err, &capped) || err.Error() != "planning exceeded 20 model turns" || len(llm.reqs) != 0 {
		t.Fatalf("Next = %v after %d calls", err, len(llm.reqs))
	}
}

func TestNextStopsAtMaxCommands(t *testing.T) {
	conv := NewConversation("run nginx")
	for range MaxCommands - 1 {
		conv = append(conv, &gatewayv1.Message{Role: roleAssistant, Content: commandAnswer})
		conv = conv.With("The command exited 0.")
	}
	p := &Planner{LLM: &fakeLLM{answers: []string{commandAnswer}}}
	turn, conv, _, err := p.Next(context.Background(), ref, conv, nil)
	if err != nil || turn.Kind != KindCommand || conv.Commands() != MaxCommands {
		t.Fatalf("command %d: Next = %v", MaxCommands, err)
	}
	_, _, _, err = p.Next(context.Background(), ref, conv.With("The command exited 0."), nil)
	if err == nil || err.Error() != "planning exceeded 10 discovery commands" {
		t.Fatalf("command %d: Next = %v", MaxCommands+1, err)
	}
}

func TestNextReturnsGatewayErrorsUnchanged(t *testing.T) {
	llm := &fakeLLM{err: status.Error(codes.Unavailable, "audit down")}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), nil)
	if status.Code(err) != codes.Unavailable || len(llm.reqs) != 1 {
		t.Fatalf("Next = %v after %d calls", err, len(llm.reqs))
	}
}

func TestNextRefusesACallWithoutASequenceNumber(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}, noSeq: true}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation("run nginx"), nil)
	if !errors.Is(err, ErrNoSeq) {
		t.Fatalf("Next = %v, want ErrNoSeq", err)
	}
}

func TestNextNeedsSomethingToAnswer(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	conv := append(NewConversation("run nginx"), &gatewayv1.Message{Role: roleAssistant, Content: questionsAnswer})
	if _, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, conv, nil); err == nil || len(llm.reqs) != 0 {
		t.Fatalf("Next = %v after %d calls; a conversation ending with an answer would be sent as a prefill", err, len(llm.reqs))
	}
}

func TestConversationWithCopies(t *testing.T) {
	base := NewConversation("run nginx")
	a, b := base.With("one"), base.With("two")
	if len(base) != 2 || a[2].GetContent() != "one" || b[2].GetContent() != "two" || !a.Waiting() {
		t.Fatalf("base %v, a %v, b %v", base, a, b)
	}
}

func TestFirstMessageCarriesTheRequestAndWhatCommandsMayUse(t *testing.T) {
	got := FirstMessage(Request{Namespace: "team-x", Name: "web", Text: "run nginx",
		Answers:  []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}},
		Feedback: "step s1 exited 1: boom", Note: "planning starts over",
		Env: Environment{Providers: []Provider{{Name: "k8s", Description: "this cluster"}, {Name: "aws"}},
			Egress: [][]string{{"10.0.0.0/8:443"}, {"10.1.0.0/16:443"}}, NoScripts: true}})
	for _, want := range []string{"Note: planning starts over", "team-x/web", "run nginx", "- q1: internal only",
		"- k8s: this cluster\n", "- aws\n", "allowed by every one of these lists", "- 10.0.0.0/8:443\n",
		"- 10.1.0.0/16:443\n", "Scripts are not allowed", "step s1 exited 1: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("first message lacks %q:\n%s", want, got)
		}
	}

	bare := FirstMessage(Request{Namespace: "team-x", Name: "web", Text: "run nginx"})
	for _, want := range []string{"- none: commands run without credentials", "Egress: none beyond what the Providers imply"} {
		if !strings.Contains(bare, want) {
			t.Errorf("bare first message lacks %q:\n%s", want, bare)
		}
	}
	for _, unwanted := range []string{"Note:", "Answers", "previous plan failed", "Scripts are not allowed"} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("bare first message has %q:\n%s", unwanted, bare)
		}
	}
	one := FirstMessage(Request{Text: "x", Env: Environment{Egress: [][]string{{"10.0.0.0/8:443", "10.2.0.1/32:5432"}}}})
	if !strings.Contains(one, "Egress commands may request: 10.0.0.0/8:443, 10.2.0.1/32:5432\n") {
		t.Errorf("one egress layer:\n%s", one)
	}
	closed := FirstMessage(Request{Text: "x", Env: Environment{Egress: [][]string{{"10.0.0.0/8:443"}, {}}}})
	if !strings.Contains(closed, "Egress: none beyond what the Providers imply") {
		t.Errorf("a layer that allows nothing:\n%s", closed)
	}
}

func TestUserTurns(t *testing.T) {
	qs := []lastv1alpha1.Question{{ID: "q1", Text: "Which namespace?"}, {ID: "q2", Text: "Public?"}}
	as := []lastv1alpha1.Answer{{ID: "q2", Value: "no"}, {ID: "q1", Value: "default"}, {ID: "q9", Value: "x"}}
	if got := AnswersTurn(qs, as); got != "Answers:\n- q1: default\n- q2: no\n" {
		t.Errorf("AnswersTurn = %q", got)
	}
	if got := DeniedTurn("too broad"); got != "The command was denied: too broad" {
		t.Errorf("DeniedTurn = %q", got)
	}
	if got := DeniedTurn(" "); got != "The command was denied." {
		t.Errorf("DeniedTurn without a reason = %q", got)
	}
	if got := NotRunTurn("the gateway refused it: policy"); got != "The command did not run: the gateway refused it: policy" {
		t.Errorf("NotRunTurn = %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run 'Next|Conversation|FirstMessage|UserTurns'`
Expected: FAIL to compile (`undefined: NewConversation`, `undefined: systemPrompt`, …).

- [ ] **Step 3: Write the conversation API**

Replace `internal/agent/agent.go` with:

```go
// Package agent runs the planning conversation. It asks the LLM, through the
// gateway's CallLLM, for one turn at a time and checks each answer (spec §4.1,
// §7; M3 spec §4).
package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// MaxTurns is how many answers the model gets to produce a valid plan.
const MaxTurns = 3

// Caps on one turn and one planning session (M3 spec §4.1, D7).
const (
	// MaxAttempts is how many answers the model gets to produce one valid turn:
	// a malformed answer draws a correction, at most MaxAttempts-1 in a row.
	MaxAttempts = 3
	// MaxModelTurns caps the model's answers in one session, corrections included.
	MaxModelTurns = 20
	// MaxCommands caps the discovery commands in one session.
	MaxCommands = 10
)

// maxTokens caps each answer, thinking included. The gateway streams, so a long
// answer does not hit an HTTP timeout (M3 spec §3.2).
const maxTokens = 64000

// Message roles.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// ErrNoPlan means the model gave no valid plan within MaxTurns answers.
var ErrNoPlan = errors.New("agent: the model gave no valid plan")

var (
	// ErrNoTurn means the model gave no valid turn within MaxAttempts answers.
	ErrNoTurn = errors.New("agent: the model gave no valid turn")
	// ErrNoSeq means the gateway did not say where the call's llm.request landed
	// in the audit log, so the session cannot be anchored for reload. A gateway
	// older than M3 does this.
	ErrNoSeq = errors.New("agent: the gateway reported no audit sequence number for the call")
)

// CapError reports a planning session that reached one of its caps (M3 spec D7).
type CapError struct {
	Max  int
	What string // "model turns" or "discovery commands"
}

func (e *CapError) Error() string { return fmt.Sprintf("planning exceeded %d %s", e.Max, e.What) }

// LLM is the part of the gateway client the agent uses.
type LLM interface {
	CallLLM(ctx context.Context, in *gatewayv1.CallLLMRequest, opts ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error)
}

// Request is what a planning session starts from.
type Request struct {
	Namespace, Name string
	Text            string // spec.request
	Answers         []lastv1alpha1.Answer
	Feedback        string // why the previous plan failed, if it did
	Note            string // why the session started over, if it did
	Env             Environment
}

// Planner asks the LLM for a plan.
type Planner struct{ LLM LLM }

// Plan runs one planning conversation and returns a valid plan with its hash
// set. Gateway errors are returned unchanged so callers can classify their
// codes. When the model gives no valid plan within MaxTurns answers, the error
// wraps ErrNoPlan.
func (p *Planner) Plan(ctx context.Context, req Request) (*lastv1alpha1.Plan, error) {
	ref := &gatewayv1.IntentRef{Namespace: req.Namespace, Name: req.Name}
	msgs := []*gatewayv1.Message{{Role: "system", Content: SystemPrompt}, {Role: "user", Content: userPrompt(req)}}
	var lastErr error
	for range MaxTurns {
		resp, err := p.LLM.CallLLM(ctx, &gatewayv1.CallLLMRequest{Intent: ref, Messages: msgs, MaxTokens: maxTokens})
		if err != nil {
			return nil, err
		}
		pl, err := Parse(resp.GetContent())
		if err == nil {
			return pl, nil
		}
		lastErr = err
		msgs = append(msgs, &gatewayv1.Message{Role: "assistant", Content: resp.GetContent()},
			&gatewayv1.Message{Role: "user", Content: correction(err)})
	}
	return nil, fmt.Errorf("%w: %v", ErrNoPlan, lastErr)
}

// Conversation is one planning session's messages, the system prompt first.
// Messages are never changed in place: With and Next return new slices.
type Conversation []*gatewayv1.Message

// NewConversation starts a session whose first user message is first.
func NewConversation(first string) Conversation {
	return Conversation{{Role: roleSystem, Content: systemPrompt}, {Role: roleUser, Content: first}}
}

// With returns a copy of c followed by a user message.
func (c Conversation) With(content string) Conversation {
	return append(slices.Clip(c), &gatewayv1.Message{Role: roleUser, Content: content})
}

// ModelTurns counts the model's answers so far, corrections included.
func (c Conversation) ModelTurns() int {
	n := 0
	for _, m := range c {
		if m.GetRole() == roleAssistant {
			n++
		}
	}
	return n
}

// Commands counts the discovery commands the model has asked for so far.
func (c Conversation) Commands() int {
	n := 0
	for _, m := range c {
		if m.GetRole() != roleAssistant {
			continue
		}
		if t, err := ParseTurn(m.GetContent()); err == nil && t.Kind == KindCommand {
			n++
		}
	}
	return n
}

// Waiting reports whether c ends with a user message: something for the model
// to answer. A conversation ending with the model's own answer would be sent
// as a prefill.
func (c Conversation) Waiting() bool { return len(c) > 0 && c[len(c)-1].GetRole() == roleUser }

// Next asks the model for the session's next turn; conv must be Waiting. It
// returns the turn, the conversation with every answer and correction
// appended, and the audit sequence number of the first llm.request it caused.
// check, when set, refuses turns the caller cannot accept; its error becomes a
// correction like a malformed answer's. Gateway errors are returned unchanged
// so callers can classify their codes. ErrNoTurn, ErrNoSeq and *CapError end
// the session.
func (p *Planner) Next(ctx context.Context, ref *gatewayv1.IntentRef, conv Conversation, check func(Turn) error) (Turn, Conversation, uint64, error) {
	if !conv.Waiting() {
		return Turn{}, nil, 0, errors.New("agent: the conversation has nothing for the model to answer")
	}
	var first uint64
	var lastErr error
	for range MaxAttempts {
		if conv.ModelTurns() >= MaxModelTurns {
			return Turn{}, nil, 0, &CapError{Max: MaxModelTurns, What: "model turns"}
		}
		resp, err := p.LLM.CallLLM(ctx, &gatewayv1.CallLLMRequest{Intent: ref, Messages: conv, MaxTokens: maxTokens,
			ResponseSchema: Schema()})
		if err != nil {
			return Turn{}, nil, 0, err
		}
		if resp.GetRequestSeq() == 0 {
			return Turn{}, nil, 0, ErrNoSeq
		}
		if first == 0 {
			first = resp.GetRequestSeq()
		}
		commands := conv.Commands()
		conv = append(slices.Clip(conv), &gatewayv1.Message{Role: roleAssistant, Content: resp.GetContent()})
		t, err := ParseTurn(resp.GetContent())
		if err == nil && check != nil {
			err = check(t)
		}
		if err == nil {
			if t.Kind == KindCommand && commands >= MaxCommands {
				return Turn{}, nil, 0, &CapError{Max: MaxCommands, What: "discovery commands"}
			}
			return t, conv, first, nil
		}
		lastErr = err
		conv = conv.With(correctionTurn(err))
	}
	return Turn{}, nil, 0, fmt.Errorf("%w: %v", ErrNoTurn, lastErr)
}
```

- [ ] **Step 4: Write the prompts and user turns**

In `internal/agent/prompt.go`:

1. Change the imports to `"fmt"`, `"slices"`, `"strings"` and `lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"`.
2. Keep `SystemPrompt`, `userPrompt` and `correction` as they are.
3. Append:

```go
// systemPrompt teaches the turn protocol (M3 spec §4.1; spec §8.1, §8.2, §10, §15).
const systemPrompt = `You are last-operator's planner. You turn a natural-language request into a plan
that a Kubernetes operator runs, step by step, in sandbox pods. A human approves every
plan and every discovery command before it runs.

Answer every message with exactly one JSON turn object and nothing else. A turn has
every field below. The fields its kind does not use are empty ("" or []), and "command"
is null unless kind is "command":
{
  "kind": "plan" | "questions" | "command",
  "summary": "", "steps": [], "resources": [],
  "questions": [],
  "reason": "", "command": null
}

Choose the kind:
- "questions" when the request is ambiguous in a way that changes the plan and you
  cannot find out yourself. Ask 1 to 5 questions, each
  {"id": "q1", "text": "...", "options": ["...", ...]}; leave "options" empty for a
  free-text answer. Every id is new: never reuse an id from earlier in this conversation.
- "command" when you need to inspect the cluster or another system before you can plan.
  Set "reason" (why you need it; the approver reads it) and "command" (one command, as
  below, with id ""). Once a human approves it, it runs alone in a sandbox and you
  receive its exit code, stdout and stderr. Discovery commands only read; never change
  anything with them. A session allows at most 10 commands and 20 answers in all.
- "plan" when you can plan: "summary" is one line a reviewer reads first, "steps" the
  commands to run in order, "resources" what the plan provisions.

A command is:
{
  "id": "s1",
  "reason": "why this command exists",
  "argv": ["kubectl", "apply", "-f", "/work/app.yaml"],
  "script": "",
  "files": [{"path": "/work/app.yaml", "content": "file content"}],
  "providers": ["provider-name"],
  "egress": ["10.0.0.1:443"],
  "timeout": "5m"
}
Set exactly one of "argv" (run directly) or "script" (run with /bin/sh -c). Step ids are
required and unique among the steps. "timeout" is a duration such as "5m", or "" for the
default. In steps, argv elements may use ${steps.<id>.stdout} or ${steps.<id>.json.<path>}
to use the output of an earlier step; files, scripts and discovery commands never do.
Use only the Providers and egress the first message lists.

A resource describes one thing the plan provisions:
{
  "description": "nginx Deployment team-x/web",
  "identity": "{\"kind\":\"Deployment\",\"namespace\":\"team-x\",\"name\":\"web\"}",
  "desired": "{\"replicas\":2}",
  "probes": [{"id": "web-ready", "command": <command with id "">, "assert": "<CEL>"}],
  "outputsSecret": "web-outputs",
  "outputs": [{"key": "<secret key>", "value": "${steps.<id>.json.<path>}"}],
  "teardown": []
}
"identity" and "desired" are JSON objects written as strings, or "". A probe's assert
is a CEL expression over result.exitCode, result.stdout, result.stderr and result.json
(stdout parsed as JSON). Probe ids are unique across the plan. Probes only read; they
run again on a schedule to detect drift. Sensitive values go in outputs, never in argv,
files, or the summary. Leave "teardown" empty.`

// Environment is what the namespace's effective policy lets commands use (M3
// spec §4.4). The gateway enforces the policy; this only tells the model.
type Environment struct {
	Providers []Provider
	// Egress holds each policy layer's allowed destinations; a destination
	// must be allowed by every layer.
	Egress    [][]string
	NoScripts bool
}

// Provider is a set of credentials a command may name.
type Provider struct{ Name, Description string }

// FirstMessage is a session's first user message: the request, the answers so
// far, what commands may use, and the failure a replan must address.
func FirstMessage(r Request) string {
	var b strings.Builder
	if r.Note != "" {
		fmt.Fprintf(&b, "Note: %s\n\n", r.Note)
	}
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s.\n\nRequest:\n%s\n", r.Namespace, r.Name, r.Namespace, r.Text)
	if len(r.Answers) > 0 {
		b.WriteString("\nAnswers to earlier questions:\n")
		for _, a := range r.Answers {
			fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Value)
		}
	}
	b.WriteString("\n")
	r.Env.describe(&b)
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous plan failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	return b.String()
}

func (e Environment) describe(b *strings.Builder) {
	b.WriteString("Providers commands may use:\n")
	if len(e.Providers) == 0 {
		b.WriteString("- none: commands run without credentials\n")
	}
	for _, p := range e.Providers {
		if p.Description == "" {
			fmt.Fprintf(b, "- %s\n", p.Name)
		} else {
			fmt.Fprintf(b, "- %s: %s\n", p.Name, p.Description)
		}
	}
	switch {
	case len(e.Egress) == 0 || slices.ContainsFunc(e.Egress, func(l []string) bool { return len(l) == 0 }):
		b.WriteString("\nEgress: none beyond what the Providers imply.\n")
	case len(e.Egress) == 1:
		fmt.Fprintf(b, "\nEgress commands may request: %s\n", strings.Join(e.Egress[0], ", "))
	default:
		b.WriteString("\nEgress commands may request: a destination must be allowed by every one of these lists:\n")
		for _, l := range e.Egress {
			fmt.Fprintf(b, "- %s\n", strings.Join(l, ", "))
		}
	}
	if e.NoScripts {
		b.WriteString("\nScripts are not allowed; use argv.\n")
	}
}

// AnswersTurn reports the requester's answers to the model's questions.
func AnswersTurn(qs []lastv1alpha1.Question, as []lastv1alpha1.Answer) string {
	var b strings.Builder
	b.WriteString("Answers:\n")
	for _, q := range qs {
		v := ""
		if i := slices.IndexFunc(as, func(a lastv1alpha1.Answer) bool { return a.ID == q.ID }); i >= 0 {
			v = as[i].Value
		}
		fmt.Fprintf(&b, "- %s: %s\n", q.ID, v)
	}
	return b.String()
}

// DeniedTurn reports a discovery command a human denied.
func DeniedTurn(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "The command was denied."
	}
	return "The command was denied: " + reason
}

// NotRunTurn reports a discovery command that did not run.
func NotRunTurn(why string) string { return "The command did not run: " + why }

func correctionTurn(err error) string {
	return fmt.Sprintf("That answer is not a valid turn: %v\nAnswer again with only the JSON turn object.", err)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/agent/agent.go internal/agent/prompt.go internal/agent/agent_test.go
git commit -m "feat(agent): ask for one turn at a time with corrections and session caps"
```

---
### Task 3: Planning sessions, Clarifying, and recording each turn

The controller now drives a planning session across reconciles. Planning sends the conversation, plus any user turn the Intent's state owes the model (in this task, the answers to `status.questions`), and records the turn it gets back:
- **plan:** the Intent moves to AwaitingApproval;
- **questions:** it moves to Clarifying;
- **command:** it moves to AwaitingDiscoveryApproval, with a nonce-bound `pendingCommand`.

Clarifying waits until `spec.answers` covers every question, then returns to Planning in the same session. Only a changed `spec.request` starts a new session.

Conversations are kept in memory. This task treats a memory miss as a lost session: a new session starts, with a note to the model. Task 5 adds reloading from the audit log.

The M2 single-shot planner is removed.

**Files:**
- Create: `internal/controller/sessions.go`
- Create: `internal/controller/intent_clarifying.go`
- Modify: `internal/controller/intent_planning.go` (whole file below)
- Modify: `internal/controller/intent_controller.go`
- Modify: `internal/controller/status.go`
- Modify: `internal/agent/agent.go`, `internal/agent/parse.go`, `internal/agent/prompt.go`, `internal/agent/agent_test.go`
- Delete: `internal/agent/parse_test.go`
- Test: `internal/controller/fakegateway_test.go`, `internal/controller/helpers_test.go`, `internal/controller/intent_controller_test.go`
- Modify: `test/e2e/operator_e2e_test.go`

**Interfaces:**
- Consumes (Tasks 1–2):
  - `agent.Turn`, `agent.KindPlan`, `KindQuestions`, `KindCommand`;
  - `agent.Encode`, `agent.Schema`, `agent.NewNonce`;
  - `agent.Conversation` (`NewConversation`, `With`);
  - `(*agent.Planner).Next`;
  - `agent.ErrNoTurn`, `agent.ErrNoSeq`, `*agent.CapError`;
  - `agent.FirstMessage`, `agent.Request` (with `Note`), `agent.AnswersTurn`.
- Produces (later tasks extend these):
  - `type sessionStore` with `get(in) (agent.Conversation, bool)`, `put(in, conv)` and `drop(key)`; the field `IntentReconciler.sessions`.
  - `func (r *IntentReconciler) conversation(in *lastv1alpha1.Intent) (agent.Conversation, bool)`, where the bool means fresh.
  - `func newConversation(in *lastv1alpha1.Intent, note string) agent.Conversation`; `const noteRestarted`.
  - `func pendingTurn(in *lastv1alpha1.Intent) string`.
  - `func (r *IntentReconciler) record(ctx, in, turn agent.Turn, conv agent.Conversation, session int64) error`.
  - `func requestHash(request string) string`, which returns `"sha256:<hex>"`.
  - `func replanFeedback(in *lastv1alpha1.Intent) string`.
  - `func storedTurnChanged(stored *lastv1alpha1.Intent, kind agent.Kind, hash string) bool`.
  - `func (r *IntentReconciler) clarify(ctx, in) (ctrl.Result, error)`, `answered(in) bool`, `answeredCheck(in) func(agent.Turn) error`.
  - Ready reasons `ReasonClarifying = "Clarifying"` and `ReasonAwaitingDiscoveryApproval = "AwaitingDiscoveryApproval"`.
  - Fake gateway:
    - `appendAudit(key, typ, stage, subject string, payload any) uint64` (`f.mu` held);
    - `auditOf(key, typ string) []*gatewayv1.AuditEntry`;
    - `CallLLM` writes `llm.request` and `llm.response` entries and returns `RequestSeq`.
  - Test helpers: `questionsTurn(qs ...Question) string`, `commandTurn(reason string, c Command) string`, `answerQuestions(key, answers ...Answer)`, `roles(msgs) []string`, `lastUserMessage(key) string`, `pruningClient(prune func(*Intent) bool) client.Client`.

- [ ] **Step 1: Teach the fake gateway the audit log**

In `internal/controller/fakegateway_test.go`:

1. Add two fields to `fakeGateway`, after `executed`:

```go
	seq      uint64                                         // the last audit sequence number, across Intents
	audit    map[types.NamespacedName][]*gatewayv1.AuditEntry // indexed entries, oldest first
```

2. Initialize `audit: map[types.NamespacedName][]*gatewayv1.AuditEntry{}` in `newFakeGateway`.
3. Replace `CallLLM` with the code below, and add `fakeMessage`, `appendAudit` and `auditOf`:

```go
// fakeMessage is a message as the gateway's llm.request payload holds it.
type fakeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (f *fakeGateway) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	key := keyOf(in.GetIntent())
	msgs := make([]fakeMessage, 0, len(in.GetMessages()))
	for _, m := range in.GetMessages() {
		msgs = append(msgs, fakeMessage{Role: m.GetRole(), Content: m.GetContent()})
	}
	f.mu.Lock()
	f.record(key, callLLM)
	f.llmReqs[key] = append(f.llmReqs[key], in)
	seq := f.appendAudit(key, "llm.request", "", "", map[string]any{"messages": msgs})
	llm := f.scriptFor(key).llm
	f.mu.Unlock()
	content, err := "", status.Error(codes.FailedPrecondition, "llm: no script for this intent")
	if llm != nil {
		content, err = llm(in)
	}
	payload := map[string]any{"requestSeq": seq, "content": content}
	if err != nil {
		payload["error"] = err.Error()
	}
	f.mu.Lock()
	f.appendAudit(key, "llm.response", "", "", payload)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &gatewayv1.CallLLMResponse{Content: content, RequestSeq: seq}, nil
}

// appendAudit adds an entry to the Intent's audit index, as the gateway would,
// and returns its sequence number. f.mu must be held.
func (f *fakeGateway) appendAudit(key types.NamespacedName, typ, stage, subject string, payload any) uint64 {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	f.seq++
	f.audit[key] = append(f.audit[key], &gatewayv1.AuditEntry{Seq: f.seq, Type: typ, Stage: stage, SubjectHash: subject, Payload: b})
	return f.seq
}

// auditOf returns the Intent's audit entries of one type, oldest first.
func (f *fakeGateway) auditOf(key types.NamespacedName, typ string) []*gatewayv1.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*gatewayv1.AuditEntry
	for _, e := range f.audit[key] {
		if e.GetType() == typ {
			out = append(out, e)
		}
	}
	return out
}
```

- [ ] **Step 2: Update the test helpers**

In `internal/controller/helpers_test.go`:

1. Drop the `encoding/json` import and add `"github.com/moogie-labs/last-operator/internal/agent"`.
2. Replace `answer` with:

```go
// answer is a model answer holding a plan.
func answer(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	return encodeTurn(agent.Turn{Kind: agent.KindPlan,
		Plan: &lastv1alpha1.Plan{Summary: "test plan", Steps: steps, Resources: resources}})
}

// questionsTurn is a model answer asking questions.
func questionsTurn(qs ...lastv1alpha1.Question) string {
	return encodeTurn(agent.Turn{Kind: agent.KindQuestions, Questions: qs})
}

// commandTurn is a model answer asking to run a discovery command.
func commandTurn(reason string, c lastv1alpha1.Command) string {
	return encodeTurn(agent.Turn{Kind: agent.KindCommand, Command: &c, Reason: reason})
}

func encodeTurn(t agent.Turn) string {
	s, err := agent.Encode(t)
	Expect(err).NotTo(HaveOccurred())
	return s
}

// answerQuestions sets spec.answers, as the requester does.
func answerQuestions(key types.NamespacedName, answers ...lastv1alpha1.Answer) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		in.Spec.Answers = answers
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

// roles lists a conversation's message roles.
func roles(msgs []*gatewayv1.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.GetRole())
	}
	return out
}

// lastUserMessage returns the last message of the Intent's last CallLLM, which
// is always the user's.
func lastUserMessage(key types.NamespacedName) string {
	msgs := gw.lastRequest(key).GetMessages()
	Expect(msgs).NotTo(BeEmpty())
	last := msgs[len(msgs)-1]
	Expect(last.GetRole()).To(Equal("user"))
	return last.GetContent()
}
```

- [ ] **Step 3: Write the failing specs**

In `internal/controller/intent_controller_test.go`:

1. Add the import `"github.com/moogie-labs/last-operator/internal/plan"`.
2. In "plans and waits for an approval of that exact plan", insert these lines right after `Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))`:

```go
		Expect(in.Status.ObservedRequestHash).To(Equal(requestHash("list namespaces")))
		Expect(in.Status.PlanningSessionSeq).To(Equal(int64(gw.auditOf(key, "llm.request")[0].GetSeq())))
		Expect(gw.lastRequest(key).GetResponseSchema()).To(Equal(agent.Schema()))
```

3. Rename "fails after MaxTurns answers that are not plans, and plans again after Failed when the spec changes" to "fails after MaxAttempts answers that are not turns, and plans again after Failed when the spec changes". Leave its body unchanged.

4. Replace the whole spec "fails when the API server changes the plan it stores" with the two specs below, and add `pruningClient` after `directReconciler`:

```go
// pruningClient writes Intent status as an API server with a stale CRD would:
// it stores the copy prune changed, and hands back only the new
// resourceVersion. The client decodes a response into the object it sent, and
// a decode merges, so the pruned field survives in memory.
func pruningClient(prune func(*lastv1alpha1.Intent) bool) client.Client {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			in, ok := obj.(*lastv1alpha1.Intent)
			if !ok {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			pruned := in.DeepCopy()
			if !prune(pruned) {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			if err := c.SubResource(sub).Update(ctx, pruned, opts...); err != nil {
				return err
			}
			in.SetResourceVersion(pruned.GetResourceVersion())
			return nil
		}})
}
```

```go
	It("fails when the API server changes the plan it stores", func() {
		resource := lastv1alpha1.ManagedResourceSpec{Description: "namespaces", OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
			Outputs: map[string]string{"name": "${steps.s1.stdout}"}}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(answer(steps, resource))})

		// A stale CRD prunes the fields it does not know, such as outputs.
		pruning := pruningClient(func(in *lastv1alpha1.Intent) bool {
			if in.Status.Plan == nil {
				return false
			}
			for i := range in.Status.Plan.Resources {
				in.Status.Plan.Resources[i].Outputs = nil
			}
			return true
		})
		in := getIntent(key)
		in.Status.Phase = lastv1alpha1.PhasePlanning
		_, err := directReconciler(pruning).plan(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("The API server changed the plan when storing it; are the CRDs current?"))
	})

	It("fails when the API server changes the discovery command it stores", func() {
		look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespaces"}}}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(commandTurn("see what exists", look))})

		// A CRD from before M3 has no pendingCommand.nonce.
		pruning := pruningClient(func(in *lastv1alpha1.Intent) bool {
			if in.Status.PendingCommand == nil {
				return false
			}
			in.Status.PendingCommand.Nonce = ""
			return true
		})
		in := getIntent(key)
		in.Status.Phase = lastv1alpha1.PhasePlanning
		_, err := directReconciler(pruning).plan(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyCondition(key).Message).To(Equal("The API server changed the command when storing it; are the CRDs current?"))
	})
```

5. Append these specs to the `Describe("Intent planning", …)` block:

```go
	It("asks questions, waits for every answer, and continues the same session", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: "Which namespace?", Options: []string{"default", "web"}}
		q2 := lastv1alpha1.Question{ID: "q2", Text: "How many replicas?"}
		gw.setScript(key, &script{llm: turns(questionsTurn(q1, q2), answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		in := getIntent(key)
		Expect(in.Status.Questions).To(Equal([]lastv1alpha1.Question{q1, q2}))
		Expect(readyReason(key)).To(Equal(ReasonClarifying))
		session := in.Status.PlanningSessionSeq
		Expect(session).To(Equal(int64(gw.auditOf(key, "llm.request")[0].GetSeq())))

		By("waiting while a question has no answer")
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))

		By("continuing the session once every question has an answer")
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"}, lastv1alpha1.Answer{ID: "q2", Value: "2"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in = getIntent(key)
		Expect(in.Status.PlanningSessionSeq).To(Equal(session))
		Expect(in.Status.Questions).To(BeEmpty())
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{"system", "user", "assistant", "user"}))
		Expect(msgs[3].GetContent()).To(Equal("Answers:\n- q1: web\n- q2: 2\n"))
	})

	It("starts a new session when the request changes while clarifying", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: "Which namespace?"}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(steps), nil
			}
			return questionsTurn(q1), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		first := getIntent(key).Status.PlanningSessionSeq

		editRequest(key, "list namespaces v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.PlanningSessionSeq).NotTo(Equal(first))
		Expect(in.Status.ObservedRequestHash).To(Equal(requestHash("list namespaces v2")))
		Expect(in.Status.Questions).To(BeEmpty())
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{"system", "user"}))
	})

	It("corrects questions that reuse an answered id", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: "Which namespace?"}
		again := lastv1alpha1.Question{ID: "q1", Text: "Which port?"}
		q2 := lastv1alpha1.Question{ID: "q2", Text: "Which port?"}
		gw.setScript(key, &script{llm: turns(questionsTurn(q1), questionsTurn(again), questionsTurn(q2))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))

		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Eventually(func() []lastv1alpha1.Question { return getIntent(key).Status.Questions }).
			Should(Equal([]lastv1alpha1.Question{q2}))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseClarifying))
		Expect(lastUserMessage(key)).To(ContainSubstring(`question id "q1" was answered earlier`))
	})

	It("asks for approval of a discovery command", func() {
		look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespaces"}}}
		gw.setScript(key, &script{llm: turns(commandTurn("see what exists", look))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		in := getIntent(key)
		pc := in.Status.PendingCommand
		Expect(pc).NotTo(BeNil())
		Expect(pc.Command.ID).To(Equal(plan.DiscoveryStepID))
		Expect(pc.Command.Run.Argv).To(Equal(look.Run.Argv))
		Expect(pc.Reason).To(Equal("see what exists"))
		Expect(pc.Nonce).To(MatchRegexp("^[0-9a-f]{32}$"))
		hash, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
		Expect(err).NotTo(HaveOccurred())
		Expect(pc.Hash).To(Equal(hash))
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageDiscovery))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingDiscoveryApproval))
	})
```

- [ ] **Step 4: Run the specs to verify they fail**

Run: `make test`
Expected: FAIL to compile: `undefined: requestHash`, `undefined: ReasonClarifying`, `undefined: ReasonAwaitingDiscoveryApproval`.

- [ ] **Step 5: Remove the M2 planner**

In `internal/agent`:

- `agent.go`:
  - Delete `MaxTurns`, `ErrNoPlan` and `(*Planner).Plan`.
  - Change the `Planner` comment to `// Planner asks the LLM for turns.`
- `parse.go`:
  - Delete the `answer` type and `Parse`.
  - Change `checkPlan`'s doc so it no longer refers to `Parse`.
  - Keep `MaxPlanBytes`, `fence`, `decodeOne`, `checkPlan` and `checkCommand`.
- `prompt.go`: delete `SystemPrompt`, `userPrompt` and `correction`.
- `agent_test.go`: delete `var req`, `TestPlannerRetriesUntilTheAnswerIsAPlan`, `TestPlannerGivesUpAfterMaxTurns`, `TestPlannerReturnsGatewayErrorsUnchanged` and `TestUserPromptCarriesTheRequestAnswersAndFeedback`.
- `parse_test.go`: delete the file. `turn_test.go` covers every case it held.

Run: `go test ./internal/agent/`
Expected: PASS.

- [ ] **Step 6: Write the session store**

Create `internal/controller/sessions.go`:

```go
package controller

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
)

// noteRestarted tells the model why its session started over.
const noteRestarted = "The operator restarted and could not reload the earlier conversation, so planning starts over."

// sessionStore keeps planning conversations in memory (M3 spec §4.3). An entry
// serves only the Intent UID and session it was stored for.
type sessionStore struct {
	mu sync.Mutex
	m  map[types.NamespacedName]session
}

type session struct {
	uid  types.UID
	seq  int64
	conv agent.Conversation
}

func (s *sessionStore) get(in *lastv1alpha1.Intent) (agent.Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[client.ObjectKeyFromObject(in)]
	if !ok || e.uid != in.UID || e.seq != in.Status.PlanningSessionSeq {
		return nil, false
	}
	return e.conv, true
}

// put stores the Intent's conversation for its current session.
func (s *sessionStore) put(in *lastv1alpha1.Intent, conv agent.Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[types.NamespacedName]session{}
	}
	s.m[client.ObjectKeyFromObject(in)] = session{uid: in.UID, seq: in.Status.PlanningSessionSeq, conv: conv}
}

func (s *sessionStore) drop(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// conversation returns the Intent's planning session so far. fresh means a new
// session starts: the Intent has none yet, or this process does not hold it.
func (r *IntentReconciler) conversation(in *lastv1alpha1.Intent) (conv agent.Conversation, fresh bool) {
	if in.Status.PlanningSessionSeq == 0 {
		return newConversation(in, ""), true
	}
	if conv, ok := r.sessions.get(in); ok {
		return conv, false
	}
	return newConversation(in, noteRestarted), true
}

// newConversation starts a session for the Intent's current spec.
func newConversation(in *lastv1alpha1.Intent, note string) agent.Conversation {
	return agent.NewConversation(agent.FirstMessage(agent.Request{Namespace: in.Namespace, Name: in.Name,
		Text: in.Spec.Request, Answers: in.Spec.Answers, Feedback: replanFeedback(in), Note: note}))
}

// pendingTurn returns the user turn the Intent's state owes the model (M3 spec
// §4.3), or "" when there is none: the answers to status.questions.
func pendingTurn(in *lastv1alpha1.Intent) string {
	if len(in.Status.Questions) > 0 {
		return agent.AnswersTurn(in.Status.Questions, in.Spec.Answers)
	}
	return ""
}
```

- [ ] **Step 7: Write the Clarifying phase**

Create `internal/controller/intent_clarifying.go`:

```go
package controller

import (
	"context"
	"fmt"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
)

// clarify waits until spec.answers answers every question, then continues the
// planning session. Answers bump the generation without starting a new
// session; only a changed spec.request does (M3 spec §4.2).
func (r *IntentReconciler) clarify(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.ObservedRequestHash != requestHash(in.Spec.Request) || len(in.Status.Questions) == 0 {
		return r.startPlanning(ctx, in, "")
	}
	if !answered(in) {
		return ctrl.Result{}, nil // the Intent watch wakes us when spec.answers changes
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: continue only on what was checked.
		if in.Status.Phase != lastv1alpha1.PhaseClarifying || in.Status.ObservedRequestHash != requestHash(in.Spec.Request) ||
			!answered(in) {
			return
		}
		in.Status.ObservedGeneration = in.Generation
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Answers received; asking the LLM to continue")
	})
}

// answered reports whether spec.answers holds an answer for every question.
func answered(in *lastv1alpha1.Intent) bool {
	for _, q := range in.Status.Questions {
		if !hasAnswer(in, q.ID) {
			return false
		}
	}
	return true
}

func hasAnswer(in *lastv1alpha1.Intent, id string) bool {
	return slices.ContainsFunc(in.Spec.Answers, func(a lastv1alpha1.Answer) bool { return a.ID == id })
}

// answeredCheck refuses questions whose ids spec.answers already holds: the
// old answers would answer them at once.
func answeredCheck(in *lastv1alpha1.Intent) func(agent.Turn) error {
	return func(t agent.Turn) error {
		for _, q := range t.Questions {
			if hasAnswer(in, q.ID) {
				return fmt.Errorf("question id %q was answered earlier in this conversation; give every question a new id", q.ID)
			}
		}
		return nil
	}
}
```

- [ ] **Step 8: Write the planning session**

Replace `internal/controller/intent_planning.go` with:

```go
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// planTimeout bounds one model turn, corrections included.
const planTimeout = 10 * time.Minute

// startPlanning discards the current plan and planning session and starts a
// new session for the current spec. feedback, when set, is a failure the new
// plan must address.
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageCreate
		in.Status.ObservedGeneration = in.Generation
		in.Status.ObservedRequestHash = requestHash(in.Spec.Request)
		in.Status.PlanningSessionSeq = 0
		in.Status.Questions = nil
		in.Status.PendingCommand = nil
		in.Status.Plan = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		if feedback == "" {
			meta.RemoveStatusCondition(&in.Status.Conditions, CondPlanned)
		} else {
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionFalse, ReasonReplanning, feedback)
		}
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM for a plan")
	})
}

// requestHash identifies a spec.request. Answers to clarifying questions do not
// start a new planning session; a changed request does (M3 spec §4.2).
func requestHash(request string) string {
	sum := sha256.Sum256([]byte(request))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// replanFeedback returns the failure the session's plan must address, if any.
func replanFeedback(in *lastv1alpha1.Intent) string {
	if c := meta.FindStatusCondition(in.Status.Conditions, CondPlanned); c != nil && c.Reason == ReasonReplanning {
		return c.Message
	}
	return ""
}

// plan continues the planning session (M3 spec §4.2): it sends the
// conversation, with the user turn the Intent's state owes the model, and
// records the model's next turn.
func (r *IntentReconciler) plan(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	conv, fresh := r.conversation(in)
	if !fresh {
		next := pendingTurn(in)
		if next == "" {
			// Nothing in the Intent's state continues the session, which ends
			// with the model's own answer: start a new one.
			return r.startPlanning(ctx, in, replanFeedback(in))
		}
		conv = conv.With(next)
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), conv, answeredCheck(in))
	var capped *agent.CapError
	switch {
	case errors.Is(err, agent.ErrNoTurn) || errors.Is(err, agent.ErrNoSeq) || errors.As(err, &capped) || terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	session := in.Status.PlanningSessionSeq
	if fresh {
		session = int64(seq)
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, session)
}

// record writes the model's turn to the Intent's status and moves the Intent
// to the phase that waits on it: AwaitingApproval for a plan, Clarifying for
// questions, AwaitingDiscoveryApproval for a command. The conversation stays
// in memory until a plan ends the session.
func (r *IntentReconciler) record(ctx context.Context, in *lastv1alpha1.Intent, turn agent.Turn, conv agent.Conversation, session int64) error {
	subject := ""
	var pending *lastv1alpha1.PendingCommand
	switch turn.Kind {
	case agent.KindPlan:
		subject = turn.Plan.Hash
	case agent.KindCommand:
		nonce, err := agent.NewNonce()
		if err != nil {
			return err
		}
		if subject, _, err = plan.DiscoveryHash(*turn.Command, nonce); err != nil {
			return err
		}
		pending = &lastv1alpha1.PendingCommand{Hash: subject, Command: *turn.Command, Nonce: nonce, Reason: turn.Reason}
	}
	gen, prev := in.Generation, in.Status.PlanningSessionSeq
	applied := false
	err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		applied = in.Generation == gen && in.Status.Phase == lastv1alpha1.PhasePlanning && in.Status.PlanningSessionSeq == prev
		if !applied {
			return // the spec changed while planning; the next reconcile plans again
		}
		in.Status.PlanningSessionSeq = session
		in.Status.Questions = nil
		in.Status.PendingCommand = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		switch turn.Kind {
		case agent.KindPlan:
			in.Status.Stage = lastv1alpha1.StageCreate
			in.Status.Plan = turn.Plan
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionTrue, ReasonPlanWritten, "Plan "+subject)
			setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
				"Waiting for an IntentApproval of plan "+subject+" for stage Create")
		case agent.KindQuestions:
			in.Status.Questions = turn.Questions
			setPhase(in, lastv1alpha1.PhaseClarifying, ReasonClarifying,
				fmt.Sprintf("Waiting for spec.answers to answer %d questions", len(turn.Questions)))
		case agent.KindCommand:
			in.Status.Stage = lastv1alpha1.StageDiscovery
			in.Status.PendingCommand = pending
			setPhase(in, lastv1alpha1.PhaseAwaitingDiscoveryApproval, ReasonAwaitingDiscoveryApproval,
				"Waiting for an IntentApproval of discovery command "+subject+" for stage Discovery")
		}
	})
	if apierrors.IsInvalid(err) {
		// update left the rejected turn on in; start the Failed write from the
		// stored status, read uncached into a fresh object so nothing stale survives
		var fresh lastv1alpha1.Intent
		if gerr := r.Reader.Get(ctx, client.ObjectKeyFromObject(in), &fresh); gerr != nil {
			return gerr
		}
		*in = fresh
		return r.fail(ctx, in, ReasonPlanningFailed, fmt.Sprintf("The API server rejected the %s: %v", turn.Kind, err))
	}
	if err != nil || !applied {
		return err
	}
	if turn.Kind == agent.KindPlan {
		r.sessions.drop(client.ObjectKeyFromObject(in)) // a plan ends the session
	} else {
		r.sessions.put(in, conv)
	}
	if turn.Kind == agent.KindQuestions {
		return nil
	}
	// Hash what was stored, read uncached into a fresh object: the write's
	// response was decoded into in, and a decode merges, so a field the API
	// server pruned would still be there.
	var stored lastv1alpha1.Intent
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(in), &stored); err != nil {
		return err
	}
	if storedTurnChanged(&stored, turn.Kind, subject) {
		*in = stored
		return r.fail(ctx, in, ReasonPlanningFailed, fmt.Sprintf(msgTurnChanged, turn.Kind))
	}
	return nil
}

// msgTurnChanged explains a plan or command the API server did not store as written.
const msgTurnChanged = "The API server changed the %s when storing it; are the CRDs current?"

// storedTurnChanged reports whether stored, as read back from the API server,
// holds the plan or discovery command hashed as hash but no longer hashes to
// it. An API server with a stale CRD prunes unknown fields silently, and the
// gateway would later refuse the subject with a hash mismatch.
func storedTurnChanged(stored *lastv1alpha1.Intent, kind agent.Kind, hash string) bool {
	switch kind {
	case agent.KindPlan:
		p := stored.Status.Plan
		if p == nil || p.Hash != hash {
			return false // the write was skipped; the spec changed while planning
		}
		got, err := plan.Hash(plan.FromStatus(p))
		return err != nil || got != hash
	case agent.KindCommand:
		pc := stored.Status.PendingCommand
		if pc == nil || pc.Hash != hash {
			return false
		}
		got, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
		return err != nil || got != hash
	}
	return false
}

// awaitApproval waits for an IntentApproval of the current plan. The gateway
// re-checks every approval itself (spec §8.3); this only decides when to try.
// A Deny wins over any Approve, as in the gateway.
func (r *IntentReconciler) awaitApproval(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration || in.Status.Plan == nil {
		return r.startPlanning(ctx, in, "")
	}
	var list lastv1alpha1.IntentApprovalList
	if err := r.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	approved := false
	for _, a := range list.Items {
		s := a.Spec
		if s.IntentRef.Name != in.Name || s.Stage != lastv1alpha1.StageCreate || s.SubjectHash != in.Status.Plan.Hash {
			continue
		}
		if s.Decision == lastv1alpha1.DecisionDeny {
			return ctrl.Result{}, r.fail(ctx, in, ReasonApprovalDenied, fmt.Sprintf("IntentApproval %s denied the plan: %s", a.Name, s.Reason))
		}
		approved = true
	}
	if !approved {
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	gen, subject := in.Generation, in.Status.Plan.Hash
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: run only the plan that was approved.
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval ||
			in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return
		}
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}
```

- [ ] **Step 9: Wire the phase, the reasons and the session store**

In `internal/controller/status.go`:

1. Add two Ready reasons to the Ready reasons block, after `ReasonPlanning`:

```go
	ReasonClarifying                = "Clarifying"
	ReasonAwaitingDiscoveryApproval = "AwaitingDiscoveryApproval"
```

2. In `fail`, drop the session before the write:

```go
// fail moves the Intent to Failed; a spec edit retries it (spec §7). A failure
// ends the planning session.
func (r *IntentReconciler) fail(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	logf.FromContext(ctx).Info("Intent failed", "reason", reason, "message", clip(msg))
	r.sessions.drop(client.ObjectKeyFromObject(in))
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) { setPhase(in, lastv1alpha1.PhaseFailed, reason, msg) })
}
```

In `internal/controller/intent_controller.go`:

1. Add the import `apierrors "k8s.io/apimachinery/pkg/api/errors"`.
2. Add this field at the end of `IntentReconciler`:

```go
	// sessions holds planning conversations in memory (M3 spec §4.3).
	sessions sessionStore
```

3. Replace the `Reader.Get` error branch in `Reconcile` with:

```go
	if err := r.Reader.Get(ctx, req.NamespacedName, &in); err != nil {
		if apierrors.IsNotFound(err) {
			r.sessions.drop(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
```

4. Add a case to the phase switch, after `PhasePlanning`:

```go
	case lastv1alpha1.PhaseClarifying:
		return r.clarify(ctx, &in)
```

- [ ] **Step 10: Encode the e2e's scripted answer as a plan turn**

In `test/e2e/operator_e2e_test.go`:

1. Add the import `"github.com/moogie-labs/last-operator/internal/agent"`.
2. Replace

```go
	answer, err := json.Marshal(map[string]any{"summary": "nginx Deployment and Service web in " + ns, "steps": steps, "resources": resources})
```

with

```go
	answer, err := agent.Encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{
		Summary: "nginx Deployment and Service web in " + ns, Steps: steps, Resources: resources}})
```

3. In the next statement, change `json.Marshal([]string{string(answer)})` to `json.Marshal([]string{answer})`.

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output.

- [ ] **Step 11: Run the tests to verify they pass**

Run: `make test`
Expected: PASS, with 0 failures. The M2 specs pass unchanged: their answers are now plan turns.

- [ ] **Step 12: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/agent internal/controller test/e2e/operator_e2e_test.go
git commit -m "feat(controller): plan in sessions of structured turns, with a Clarifying phase"
```

---
### Task 4: The Discovery phase

AwaitingDiscoveryApproval waits for an `IntentApproval` with stage Discovery for `pendingCommand.hash`. A Deny wins, as for plans.
- **Approved:** the command runs as a one-step Discovery phase: `BeginPhase(Discovery)`, then `RunCommand("discovery")`, then `EndPhase`, using the M2 `PhaseOpen` checkpoint.
- **How it ended** is recorded in the `Executed` condition, and the Intent returns to Planning:

  | Outcome | Reason |
  |---|---|
  | Ran (any exit code) | `DiscoveryRan` |
  | Denied | `DiscoveryDenied` |
  | Refused, could not run, or lost | `DiscoveryFailed` |

- **In Planning,** `pendingTurn` turns the outcome into the user turn:
  - a result is read back from the command's `command.result` audit entry;
  - a denial or failure turn is the condition's message.
- **A phase this process lost** (checkpoint `PhaseOpen`) is ended as failed before anything else, and the model is told.
- **`EndPhase` retries `Aborted`** in-loop, a contract from M3a.

**Files:**
- Create: `internal/agent/audit.go`, `internal/agent/audit_test.go`
- Modify: `internal/agent/prompt.go`
- Create: `internal/controller/intent_discovery.go`, `internal/controller/intent_discovery_test.go`
- Modify: `internal/controller/sessions.go`, `internal/controller/intent_planning.go`, `internal/controller/intent_execution.go`, `internal/controller/intent_controller.go`, `internal/controller/status.go`
- Test: `internal/controller/fakegateway_test.go`, `internal/controller/helpers_test.go`, `internal/controller/intent_execution_test.go`

**Interfaces:**
- Consumes (Task 3): `pendingTurn`, `record`, `sessions`, `(*IntentReconciler).plan`; Task 2's `agent.DeniedTurn` and `agent.NotRunTurn`; M2's `checkpoint`, `setExecuted`, `stepFailure`, `phaseCallTimeout` and `retries`.
- Produces:
  - **Agent:**
    - `type Auditor interface { QueryAudit(...) }`;
    - `scan(ctx, a Auditor, ref, typ string, after uint64, each func(*gatewayv1.AuditEntry)) (truncated bool, err error)`;
    - `type commandResult`;
    - `func DiscoveryResult(ctx, a Auditor, ref *gatewayv1.IntentRef, subject string, after uint64) (string, error)`;
    - `func ResultLostTurn(why string) string`, `resultTurn(commandResult) string`, `const maxResultBytes = 16 << 10`.
  - **Controller:**
    - `(r) pendingTurn(ctx, in) (string, error)` replaces the function form;
    - `(r) awaitDiscovery`, `(r) runDiscovery(ctx, in) error`, `(r) discovered(ctx, in, reason, msg string) error`;
    - `(r) decision(ctx, in, stage lastv1alpha1.Stage, subject string) (approved bool, denial *lastv1alpha1.IntentApproval, err error)`;
    - `runStep(ctx, in, subject, id string, raw []byte)` and `endPhase(ctx, in, subject string, succeeded bool, reason string)`;
    - Executed reasons `ReasonDiscoveryRan`, `ReasonDiscoveryDenied`, `ReasonDiscoveryFailed`.
  - **Fake gateway:**
    - Discovery `BeginPhase` checks the pending command and single use;
    - `RunCommand` writes `command.result` entries;
    - `QueryAudit`, `executedOf`;
    - script fields `beginErr`, `queryErr`, `auditTruncated`;
    - call-log constants `beginDiscovery`, `runDiscoveryStep`, `queryAudit`.
  - **Test helper:** `approveCommand(key, subject string, decision, reason string)`.

- [ ] **Step 1: Write the failing agent tests**

Create `internal/agent/audit_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// fakeAuditor serves QueryAudit from entries, at most pageSize per call when set.
type fakeAuditor struct {
	entries   []*gatewayv1.AuditEntry
	truncated bool
	pageSize  int
	err       error
	calls     int
}

func (f *fakeAuditor) QueryAudit(_ context.Context, in *gatewayv1.QueryAuditRequest, _ ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	resp := &gatewayv1.QueryAuditResponse{Truncated: f.truncated}
	for _, e := range f.entries {
		if e.GetSeq() <= in.GetAfterSeq() || !slices.Contains(in.GetTypes(), e.GetType()) {
			continue
		}
		if f.pageSize > 0 && len(resp.Entries) == f.pageSize {
			resp.More = true
			break
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

func entry(seq uint64, typ, stage, subject string, payload any) *gatewayv1.AuditEntry {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return &gatewayv1.AuditEntry{Seq: seq, Type: typ, Stage: stage, SubjectHash: subject, Payload: b}
}

func TestDiscoveryResultReadsTheCommandsEntry(t *testing.T) {
	a := &fakeAuditor{pageSize: 1, entries: []*gatewayv1.AuditEntry{
		entry(3, audit.TypeCommandResult, "Discovery", "sha256:old", commandResult{Stdout: "old"}),
		entry(4, audit.TypeLLMRequest, "", "", map[string]any{}),
		entry(5, audit.TypeCommandResult, "Create", "sha256:cmd", commandResult{Stdout: "a step"}),
		entry(7, audit.TypeCommandResult, "Discovery", "sha256:cmd",
			commandResult{ExitCode: 2, Stdout: "default\n", Stderr: "boom", StderrTruncated: true}),
	}}
	got, err := DiscoveryResult(context.Background(), a, ref, "sha256:cmd", 1)
	if err != nil || got != "The command exited 2.\nstdout:\ndefault\nstderr (truncated):\nboom\n" {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
	if a.calls != 3 {
		t.Errorf("QueryAudit called %d times, want 3 pages", a.calls)
	}
}

func TestDiscoveryResultReportsALostResult(t *testing.T) {
	got, err := DiscoveryResult(context.Background(), &fakeAuditor{truncated: true}, ref, "sha256:cmd", 0)
	if err != nil || got != "The command ran, but its result could not be read back: the audit log index no longer holds it" {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
	got, err = DiscoveryResult(context.Background(), &fakeAuditor{}, ref, "sha256:cmd", 0)
	if err != nil || !strings.HasSuffix(got, "the audit log has no result entry for it") {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
}

func TestDiscoveryResultReturnsGatewayErrorsUnchanged(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "the index could not be saved")}
	if _, err := DiscoveryResult(context.Background(), a, ref, "sha256:cmd", 0); status.Code(err) != codes.Unavailable {
		t.Fatalf("DiscoveryResult = %v", err)
	}
}

func TestResultTurnClipsEachStream(t *testing.T) {
	big := strings.Repeat("é", maxResultBytes) // two bytes each
	got := resultTurn(commandResult{Stdout: big, Error: "timed out"})
	if !strings.HasPrefix(got, "The command could not finish: timed out\nstdout (truncated):\n") ||
		!strings.HasSuffix(got, "\nstderr: empty\n") || !utf8.ValidString(got) || len(got) > maxResultBytes+100 {
		t.Fatalf("resultTurn = %d bytes: %.120q", len(got), got)
	}
}
```

Run: `go test ./internal/agent/ -run 'DiscoveryResult|ResultTurn'`
Expected: FAIL to compile (`undefined: DiscoveryResult`, `undefined: commandResult`, …).

- [ ] **Step 2: Write the agent's audit reader and result turns**

Create `internal/agent/audit.go`:

```go
package agent

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// Auditor is the part of the gateway client that reads the audit log.
type Auditor interface {
	QueryAudit(ctx context.Context, in *gatewayv1.QueryAuditRequest, opts ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error)
}

// queryLimit is the page size of every QueryAudit call: the gateway's maximum.
const queryLimit = 200

// scan calls each with every indexed entry of type typ after sequence number
// after, oldest first, and reports whether the gateway's index had already
// dropped some of them. QueryAudit is read-your-writes, so every entry written
// before the call is there unless truncated says otherwise.
func scan(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, typ string, after uint64,
	each func(*gatewayv1.AuditEntry)) (bool, error) {
	truncated := false
	for {
		resp, err := a.QueryAudit(ctx, &gatewayv1.QueryAuditRequest{Intent: ref, Types: []string{typ}, AfterSeq: after, Limit: queryLimit})
		if err != nil {
			return false, err
		}
		truncated = truncated || resp.GetTruncated()
		for _, e := range resp.GetEntries() {
			each(e)
			after = e.GetSeq()
		}
		if !resp.GetMore() || len(resp.GetEntries()) == 0 {
			return truncated, nil
		}
	}
}

// commandResult is the payload of a command.result audit entry, as the
// gateway's executor writes it. Output is redacted before it is audited.
type commandResult struct {
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error"`
}

// DiscoveryResult returns the user turn that reports a discovery command's
// result, read from its command.result audit entry: the Discovery entry for
// subject after sequence number after. Gateway errors are returned unchanged.
func DiscoveryResult(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, subject string, after uint64) (string, error) {
	var found *commandResult
	truncated, err := scan(ctx, a, ref, audit.TypeCommandResult, after, func(e *gatewayv1.AuditEntry) {
		var r commandResult
		if e.GetStage() == string(lastv1alpha1.StageDiscovery) && e.GetSubjectHash() == subject &&
			json.Unmarshal(e.GetPayload(), &r) == nil {
			found = &r
		}
	})
	if err != nil {
		return "", err
	}
	if found == nil {
		why := "the audit log has no result entry for it"
		if truncated {
			why = "the audit log index no longer holds it"
		}
		return ResultLostTurn(why), nil
	}
	return resultTurn(*found), nil
}
```

In `internal/agent/prompt.go`, add `"unicode/utf8"` to the imports and append:

```go
// maxResultBytes bounds each of a discovery command's stdout and stderr in the
// conversation (M3 spec §4.2).
const maxResultBytes = 16 << 10

// resultTurn reports a discovery command's result.
func resultTurn(r commandResult) string {
	var b strings.Builder
	if r.Error != "" {
		fmt.Fprintf(&b, "The command could not finish: %s\n", r.Error)
	} else {
		fmt.Fprintf(&b, "The command exited %d.\n", r.ExitCode)
	}
	writeStream(&b, "stdout", r.Stdout, r.StdoutTruncated)
	writeStream(&b, "stderr", r.Stderr, r.StderrTruncated)
	return b.String()
}

// writeStream writes one output stream, clipped to maxResultBytes.
func writeStream(b *strings.Builder, name, s string, truncated bool) {
	s, clipped := head(s, maxResultBytes)
	switch {
	case s == "" && !truncated:
		fmt.Fprintf(b, "%s: empty\n", name)
		return
	case truncated || clipped:
		fmt.Fprintf(b, "%s (truncated):\n", name)
	default:
		fmt.Fprintf(b, "%s:\n", name)
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
	}
}

// head returns the first n bytes of s, cut on a UTF-8 boundary, and whether it cut.
func head(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// ResultLostTurn reports a discovery command that ran but whose result cannot
// be read back.
func ResultLostTurn(why string) string {
	return "The command ran, but its result could not be read back: " + why
}
```

Run: `go test ./internal/agent/`
Expected: PASS.

- [ ] **Step 3: Teach the fake gateway Discovery phases and QueryAudit**

In `internal/controller/fakegateway_test.go`:

1. Add the import `"github.com/moogie-labs/last-operator/internal/plan"`.
2. Add to the call-log constants:

```go
	beginDiscovery   = "BeginPhase:Discovery"
	runDiscoveryStep = "RunCommand:discovery"
	queryAudit       = "QueryAudit"
```

3. Add to `script`:

```go
	beginErr       error // returned by BeginPhase once the call is recorded
	queryErr       error // returned by QueryAudit
	auditTruncated bool  // QueryAudit reports that the index dropped entries
```

4. Add to `fakeGateway`, after `open`, and initialize both in `newFakeGateway`:

```go
	openStage map[types.NamespacedName]string // stage of the open phase
	used      map[string]bool                 // Discovery subjects already opened: they are single-use
```

5. Replace `approved` and `BeginPhase`, and add `pendingSubject`:

```go
// approved reports whether an IntentApproval approves subject for the Intent's
// stage and none denies it.
func (f *fakeGateway) approved(ctx context.Context, key types.NamespacedName, stage, subject string) (bool, error) {
	var list lastv1alpha1.IntentApprovalList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return false, err
	}
	ok := false
	for _, a := range list.Items {
		if a.Spec.IntentRef.Name != key.Name || string(a.Spec.Stage) != stage || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return false, nil
		}
		ok = true
	}
	return ok, nil
}

// pendingSubject checks a Discovery subject as the gateway does: it must be the
// Intent's pending command, hashed with its nonce.
func (f *fakeGateway) pendingSubject(ctx context.Context, key types.NamespacedName, subject string) error {
	var in lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &in); err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	pc := in.Status.PendingCommand
	if pc == nil {
		return status.Error(codes.FailedPrecondition, "intent has no pending discovery command")
	}
	hash, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
	if err != nil || pc.Hash != subject || hash != subject {
		return status.Error(codes.PermissionDenied, "the subject is not the pending discovery command")
	}
	return nil
}

func (f *fakeGateway) BeginPhase(ctx context.Context, in *gatewayv1.BeginPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.BeginPhaseResponse, error) {
	key, stage, subject := keyOf(in.GetIntent()), in.GetStage(), in.GetSubjectHash()
	discovery := stage == string(lastv1alpha1.StageDiscovery)
	approved, err := f.approved(ctx, key, stage, subject)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	var pendingErr error
	if discovery {
		pendingErr = f.pendingSubject(ctx, key, subject)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if discovery {
		f.record(key, beginDiscovery)
	} else {
		f.record(key, beginPhase)
	}
	if err := f.scriptFor(key).beginErr; err != nil {
		return nil, err
	}
	switch {
	case f.open[key] != "":
		return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
	case pendingErr != nil:
		return nil, pendingErr
	case f.used[subject]:
		return nil, status.Error(codes.FailedPrecondition, "this discovery command was already run")
	case !approved:
		return nil, status.Error(codes.FailedPrecondition, "no IntentApproval approves this plan")
	}
	f.open[key], f.openStage[key] = subject, stage
	if discovery {
		f.used[subject] = true
	}
	return &gatewayv1.BeginPhaseResponse{}, nil
}
```

6. In `RunCommand`, insert this just before the final `return &gatewayv1.RunCommandResponse{Result: res}, nil`:

```go
	f.appendAudit(key, "command.result", f.openStage[key], in.GetSubjectHash(),
		map[string]any{"exitCode": res.ExitCode, "stdout": res.Stdout, "stderr": res.Stderr})
```

7. In `EndPhase`, replace

```go
	delete(f.open, key)
	if in.GetSucceeded() {
		f.executed[key] = in.GetSubjectHash()
	}
```

with

```go
	stage := f.openStage[key]
	delete(f.open, key)
	delete(f.openStage, key)
	// A Discovery phase ran one inspection command; it never becomes the executed plan.
	if in.GetSucceeded() && stage != string(lastv1alpha1.StageDiscovery) {
		f.executed[key] = in.GetSubjectHash()
	}
```

8. Add:

```go
func (f *fakeGateway) QueryAudit(_ context.Context, in *gatewayv1.QueryAuditRequest, _ ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, queryAudit)
	s := f.scriptFor(key)
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	limit := int(in.GetLimit())
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return nil, status.Error(codes.InvalidArgument, "limit must be 1..200")
	}
	resp := &gatewayv1.QueryAuditResponse{Truncated: s.auditTruncated}
	for _, e := range f.audit[key] {
		if e.GetSeq() <= in.GetAfterSeq() || (len(in.GetTypes()) > 0 && !slices.Contains(in.GetTypes(), e.GetType())) {
			continue
		}
		if len(resp.Entries) == limit {
			resp.More = true
			break
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

// executedOf returns the hash of the Intent's last executed plan, or "".
func (f *fakeGateway) executedOf(key types.NamespacedName) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executed[key]
}
```

In `internal/controller/helpers_test.go`, add:

```go
// approveCommand files a decision on a discovery command, as kubectl last does.
func approveCommand(key types.NamespacedName, subject string, decision lastv1alpha1.Decision, reason string) {
	a := &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: key.Name}, Stage: lastv1alpha1.StageDiscovery,
			SubjectHash: subject, Decision: decision, ApprovedBy: "alice", Reason: reason}}
	Expect(k8sClient.Create(ctx, a)).To(Succeed())
}
```

- [ ] **Step 4: Write the failing controller specs**

Create `internal/controller/intent_discovery_test.go`:

```go
package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

var _ = Describe("Intent discovery", func() {
	var key types.NamespacedName
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespaces"}}}
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// awaitingDiscovery scripts the gateway and waits until the Intent asks to
	// run a discovery command, whose subject it returns.
	awaitingDiscovery := func(s *script) string {
		gw.setScript(key, s)
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		return getIntent(key).Status.PendingCommand.Hash
	}

	It("runs an approved command and gives the model its result", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn("see what exists", look), answer(steps)),
			stdout: map[string]string{plan.DiscoveryStepID: "default\nkube-system\n"}})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded, queryAudit, callLLM}))
		Expect(gw.argvOf(key, plan.DiscoveryStepID)).To(Equal(look.Run.Argv))
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{"system", "user", "assistant", "user"}))
		Expect(lastUserMessage(key)).To(Equal("The command exited 0.\nstdout:\ndefault\nkube-system\nstderr: empty\n"))
		in := getIntent(key)
		Expect(in.Status.PendingCommand).To(BeNil())
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(meta.FindStatusCondition(in.Status.Conditions, CondExecuted)).To(BeNil())
		Expect(gw.executedOf(key)).To(BeEmpty(), "a Discovery phase never becomes the executed plan")
	})

	It("gives the model a failing command's exit code and output", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn("see what exists", look), answer(steps)),
			exit: map[string]int32{plan.DiscoveryStepID: 2}})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(ContainElement(endFailed))
		Expect(lastUserMessage(key)).To(Equal("The command exited 2.\nstdout: empty\nstderr:\nboom\n"))
	})

	It("tells the model when the command is denied", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn("see what exists", look), answer(steps))})
		approveCommand(key, subject, lastv1alpha1.DecisionDeny, "too broad")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM}))
		Expect(lastUserMessage(key)).To(Equal("The command was denied: too broad"))
	})

	It("tells the model when the gateway refuses the command", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn("see what exists", look), answer(steps)),
			beginErr: status.Error(codes.FailedPrecondition, "egress 10.0.0.1:443 is not allowed")})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, callLLM}))
		Expect(lastUserMessage(key)).To(Equal("The command did not run: the gateway refused it: egress 10.0.0.1:443 is not allowed"))
	})

	It("ends a discovery phase the operator lost and tells the model", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn("see what exists", look), answer(steps))})
		// As if an earlier process opened the phase and stopped while the command ran.
		gw.setOpen(key, subject)
		setStatus(key, func(in *lastv1alpha1.Intent) {
			setCondition(&in.Status.Conditions, in.Generation, CondExecuted, metav1.ConditionUnknown, ReasonPhaseOpen, "set by the test")
		})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, endFailed, callLLM}))
		Expect(gw.openPhase(key)).To(BeEmpty())
		Expect(lastUserMessage(key)).To(ContainSubstring("the operator lost track of it"))
	})

	It("starts a new session when the request changes while a command waits", func() {
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(steps), nil
			}
			return commandTurn("see what exists", look), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))

		editRequest(key, "list namespaces v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.PendingCommand).To(BeNil())
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{"system", "user"}))
	})

	It("fails when the model asks for more than MaxCommands commands", func() {
		gw.setScript(key, &script{llm: func(*gatewayv1.CallLLMRequest) (string, error) {
			return commandTurn("see what exists", look), nil
		}})
		createIntent(key, "list namespaces")
		approved := map[string]bool{}
		for range agent.MaxCommands {
			var subject string
			Eventually(func() bool {
				in := getIntent(key)
				if in.Status.Phase != lastv1alpha1.PhaseAwaitingDiscoveryApproval || in.Status.PendingCommand == nil {
					return false
				}
				subject = in.Status.PendingCommand.Hash
				return !approved[subject]
			}).Should(BeTrue())
			approved[subject] = true
			approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")
		}

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("planning exceeded 10 discovery commands"))
	})
})
```

In `internal/controller/intent_execution_test.go`, append to the "Intent execution" `Describe`:

```go
	It("retries an EndPhase that a running step holds off", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1})), stdout: stdout,
			endErrs: []error{status.Error(codes.Aborted, "a step of this phase is still running; retry")}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:5]).To(Equal([]string{callLLM, beginPhase, runS1, endSucceeded, endSucceeded}))
	})
```

Run: `make test`
Expected: FAIL. The controller has no handler for AwaitingDiscoveryApproval, so the discovery specs time out. The `Aborted` spec fails because `EndPhase` is not retried in-loop.

- [ ] **Step 5: Give runStep and endPhase the subject, and retry Aborted**

In `internal/controller/intent_execution.go`:

1. Replace `runStep` and `endPhase` with:

```go
// runStep calls RunCommand, retrying while another call of the phase holds it (Aborted).
func (r *IntentReconciler) runStep(ctx context.Context, in *lastv1alpha1.Intent, subject, id string, raw []byte) (*gatewayv1.CommandResult, error) {
	req := &gatewayv1.RunCommandRequest{Intent: intentRef(in), SubjectHash: subject, StepId: id, CommandJson: raw}
	for attempt := 1; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, stepTimeout)
		resp, err := r.Gateway.RunCommand(sctx, req)
		cancel()
		if status.Code(err) != codes.Aborted || attempt == retries {
			return resp.GetResult(), err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return nil, err
		}
	}
}
```

```go
// endPhase calls EndPhase with the same outcome until it is recorded, retrying
// while the gateway is unavailable (it treats a repeated EndPhase as a retry of
// its record save) or a step of the phase still holds it (Aborted).
func (r *IntentReconciler) endPhase(ctx context.Context, in *lastv1alpha1.Intent, subject string, succeeded bool, reason string) error {
	req := &gatewayv1.EndPhaseRequest{Intent: intentRef(in), SubjectHash: subject, Succeeded: succeeded, Reason: reason}
	for attempt := 1; ; attempt++ {
		ectx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
		_, err := r.Gateway.EndPhase(ectx, req)
		cancel()
		if code := status.Code(err); (code != codes.Unavailable && code != codes.Aborted) || attempt == retries {
			return err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return err
		}
	}
}
```

2. Update the callers to pass the plan's hash:
   - In `runSteps`: `res, err := r.runStep(ctx, in, in.Status.Plan.Hash, step.ID, raw)`.
   - In `endFailed`: `r.endPhase(ctx, in, in.Status.Plan.Hash, false, reason)`.
   - In `abandonPhase`: `r.endPhase(ctx, in, in.Status.Plan.Hash, false, why)`.
   - In `finishPhase`: `r.endPhase(ctx, in, in.Status.Plan.Hash, true, "")` and `r.endPhase(ctx, in, in.Status.Plan.Hash, false, why)`.

- [ ] **Step 6: Share the approval check**

In `internal/controller/intent_planning.go`, replace `awaitApproval` with the two functions below:

```go
// awaitApproval waits for an IntentApproval of the current plan.
func (r *IntentReconciler) awaitApproval(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration || in.Status.Plan == nil {
		return r.startPlanning(ctx, in, "")
	}
	approved, denial, err := r.decision(ctx, in, lastv1alpha1.StageCreate, in.Status.Plan.Hash)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case denial != nil:
		return ctrl.Result{}, r.fail(ctx, in, ReasonApprovalDenied,
			fmt.Sprintf("IntentApproval %s denied the plan: %s", denial.Name, denial.Spec.Reason))
	case !approved:
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	gen, subject := in.Generation, in.Status.Plan.Hash
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: run only the plan that was approved.
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval ||
			in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return
		}
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}

// decision reads the IntentApprovals of subject for the Intent and stage. The
// gateway re-checks every approval itself (spec §8.3); this only decides when
// to try. A Deny wins over any Approve, as in the gateway.
func (r *IntentReconciler) decision(ctx context.Context, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage,
	subject string) (approved bool, denial *lastv1alpha1.IntentApproval, err error) {
	var list lastv1alpha1.IntentApprovalList
	if err := r.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return false, nil, err
	}
	for i := range list.Items {
		s := list.Items[i].Spec
		if s.IntentRef.Name != in.Name || s.Stage != stage || s.SubjectHash != subject {
			continue
		}
		if s.Decision == lastv1alpha1.DecisionDeny {
			return false, &list.Items[i], nil
		}
		approved = true
	}
	return approved, nil, nil
}
```

- [ ] **Step 7: Write the Discovery phase**

In `internal/controller/status.go`, add after the Executed reasons block:

```go
// Executed reasons for a discovery command that ended. The Intent is back in
// Planning, and pendingTurn tells the model.
const (
	ReasonDiscoveryRan    = "DiscoveryRan"    // the command ran; its command.result audit entry holds the output
	ReasonDiscoveryDenied = "DiscoveryDenied" // an IntentApproval denied it; the message is the turn for the model
	ReasonDiscoveryFailed = "DiscoveryFailed" // it did not run, or its run was lost; the message is the turn for the model
)
```

Create `internal/controller/intent_discovery.go`:

```go
package controller

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// awaitDiscovery waits for an IntentApproval of the pending discovery command
// and runs it as a one-step Discovery phase (M3 spec §3.4, §4.2). However it
// ends, with a result, a denial or a refusal, the model hears about it in
// Planning.
func (r *IntentReconciler) awaitDiscovery(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	pc := in.Status.PendingCommand
	if pc != nil && checkpoint(in) == ReasonPhaseOpen {
		// This process lost the running command, and a phase is never resumed.
		// End it before anything else, a spec edit included, so no phase stays open.
		if err := r.endPhase(ctx, in, pc.Hash, false, "The operator lost track of the running command"); err != nil &&
			status.Code(err) != codes.FailedPrecondition {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.discovered(ctx, in, ReasonDiscoveryFailed,
			agent.NotRunTurn("the operator lost track of it while it ran; ask again if you still need it"))
	}
	if in.Generation != in.Status.ObservedGeneration || pc == nil {
		return r.startPlanning(ctx, in, "")
	}
	approved, denial, err := r.decision(ctx, in, lastv1alpha1.StageDiscovery, pc.Hash)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case denial != nil:
		return ctrl.Result{}, r.discovered(ctx, in, ReasonDiscoveryDenied, agent.DeniedTurn(denial.Spec.Reason))
	case !approved:
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	return ctrl.Result{}, r.runDiscovery(ctx, in)
}

// runDiscovery runs the approved command as a one-step Discovery phase. A
// command that exits non-zero still completes the phase: the model reads its
// result.
func (r *IntentReconciler) runDiscovery(ctx context.Context, in *lastv1alpha1.Intent) error {
	subject := in.Status.PendingCommand.Hash
	raw, err := json.Marshal(in.Status.PendingCommand.Command)
	if err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	_, err = r.Gateway.BeginPhase(bctx, &gatewayv1.BeginPhaseRequest{Intent: intentRef(in),
		Stage: string(lastv1alpha1.StageDiscovery), SubjectHash: subject})
	cancel()
	switch {
	case status.Code(err) == codes.AlreadyExists:
		const why = "a phase from an earlier attempt was still open"
		if eerr := r.endPhase(ctx, in, subject, false, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
			return eerr
		}
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn(why+"; ask again if you still need it"))
	case terminal(err):
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn("the gateway refused it: "+describe(err)))
	case err != nil:
		return err
	}
	logf.FromContext(ctx).Info("Opened discovery phase", "command", subject)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonPhaseOpen, "Running discovery command "+subject)
	}); err != nil {
		return err // the next attempt finds the phase open (AlreadyExists) and ends it
	}
	res, err := r.runStep(ctx, in, subject, plan.DiscoveryStepID, raw)
	if ctx.Err() != nil {
		return ctx.Err() // shutting down; the next process ends the phase as lost
	}
	succeeded := err == nil && res.GetExitCode() == 0 && res.GetError() == ""
	why := ""
	switch {
	case err != nil:
		why = "the command could not run: " + describe(err)
	case !succeeded:
		why = stepFailure(plan.DiscoveryStepID, res)
	}
	if eerr := r.endPhase(ctx, in, subject, succeeded, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
		return eerr // still PhaseOpen: the next attempt reports the command as lost
	}
	if err != nil {
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn(describe(err)))
	}
	logf.FromContext(ctx).Info("Ran discovery command", "command", subject, "exitCode", res.GetExitCode())
	return r.discovered(ctx, in, ReasonDiscoveryRan, "Discovery command "+subject+" ran")
}

// discovered records how the pending discovery command ended in the Executed
// condition and returns the Intent to Planning, where pendingTurn tells the
// model. For ReasonDiscoveryDenied and ReasonDiscoveryFailed, msg is that turn.
func (r *IntentReconciler) discovered(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	subject := in.Status.PendingCommand.Hash
	s := metav1.ConditionFalse
	if reason == ReasonDiscoveryRan {
		s = metav1.ConditionTrue
	}
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: record only the command that ended.
		if in.Status.Phase != lastv1alpha1.PhaseAwaitingDiscoveryApproval || in.Status.PendingCommand == nil ||
			in.Status.PendingCommand.Hash != subject {
			return
		}
		setExecuted(in, s, reason, msg)
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM to continue")
	})
}
```

In `internal/controller/intent_controller.go`, add a case to the phase switch, after `PhaseClarifying`:

```go
	case lastv1alpha1.PhaseAwaitingDiscoveryApproval:
		return r.awaitDiscovery(ctx, &in)
```

- [ ] **Step 8: Derive the pending turn from a discovery outcome**

In `internal/controller/sessions.go`:

1. Add the imports `"context"` and `"k8s.io/apimachinery/pkg/api/meta"`.
2. Replace `pendingTurn` with:

```go
// pendingTurn returns the user turn the Intent's state owes the model (M3 spec
// §4.3), or "" when there is none: the answers to status.questions, or how the
// pending discovery command ended, which the Executed condition records.
func (r *IntentReconciler) pendingTurn(ctx context.Context, in *lastv1alpha1.Intent) (string, error) {
	if len(in.Status.Questions) > 0 {
		return agent.AnswersTurn(in.Status.Questions, in.Spec.Answers), nil
	}
	pc := in.Status.PendingCommand
	c := meta.FindStatusCondition(in.Status.Conditions, CondExecuted)
	if pc == nil || c == nil {
		return "", nil
	}
	switch c.Reason {
	case ReasonDiscoveryRan:
		turn, err := agent.DiscoveryResult(ctx, r.Gateway, intentRef(in), pc.Hash, uint64(in.Status.PlanningSessionSeq))
		if terminal(err) {
			return agent.ResultLostTurn(describe(err)), nil
		}
		return turn, err
	case ReasonDiscoveryDenied, ReasonDiscoveryFailed:
		return c.Message, nil
	}
	return "", nil
}
```

In `internal/controller/intent_planning.go`, replace `plan` with:

```go
// plan continues the planning session (M3 spec §4.2): it sends the
// conversation, with the user turn the Intent's state owes the model, and
// records the model's next turn.
func (r *IntentReconciler) plan(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	conv, fresh := r.conversation(in)
	if !fresh {
		next, err := r.pendingTurn(pctx, in)
		if err != nil {
			return ctrl.Result{}, err // transient: retried with backoff
		}
		if next == "" {
			// Nothing in the Intent's state continues the session, which ends
			// with the model's own answer: start a new one.
			return r.startPlanning(ctx, in, replanFeedback(in))
		}
		conv = conv.With(next)
	}
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), conv, answeredCheck(in))
	var capped *agent.CapError
	switch {
	case errors.Is(err, agent.ErrNoTurn) || errors.Is(err, agent.ErrNoSeq) || errors.As(err, &capped) || terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	session := in.Status.PlanningSessionSeq
	if fresh {
		session = int64(seq)
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, session)
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `make test`
Expected: PASS, with 0 failures.

- [ ] **Step 10: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/agent internal/controller
git commit -m "feat(controller): run approved discovery commands and feed their outcome back to the model"
```

---
### Task 5: Reload a lost conversation from the audit log

A session this process does not hold in memory, after an operator restart or a leader change, is rebuilt from `QueryAudit`. The rebuilt conversation is the latest `llm.request` of the session whose `llm.response` holds a valid turn, followed by that turn. The pending user turn then comes from the Intent's state, exactly as in Task 4.

A new session, with a note to the model, starts only when the session cannot be rebuilt:
- the index is `truncated`;
- the session's first `llm.request` is missing, for example lost in a crash;
- no valid answer is found;
- the gateway refuses the query with a terminal code.

**Files:**
- Modify: `internal/agent/audit.go`, `internal/agent/audit_test.go`
- Modify: `internal/controller/sessions.go`, `internal/controller/intent_planning.go`, `internal/controller/suite_test.go`
- Create: `internal/controller/intent_reload_test.go`

**Interfaces:**
- Consumes (Task 4): `scan`, `fakeAuditor`, `entry`, `Auditor`, `pendingTurn`, the fake's `QueryAudit` and `auditTruncated`.
- Produces:
  - `var ErrNoReload`; `func Reload(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, session uint64) (Conversation, error)`.
  - `(r) conversation(ctx context.Context, in *lastv1alpha1.Intent) (agent.Conversation, bool, error)`.
  - The suite variable `intentReconciler *IntentReconciler`.

- [ ] **Step 1: Write the failing agent tests**

In `internal/agent/audit_test.go`, add `"errors"` to the imports and append:

```go
// sessionEntries is the audit index of an Intent whose current planning
// session started at llm.request 5, after an earlier session at 2.
func sessionEntries() []*gatewayv1.AuditEntry {
	msg := func(role, content string) map[string]string { return map[string]string{"role": role, "content": content} }
	sys, user := msg("system", "sys"), msg("user", "run nginx")
	asked, answers := msg("assistant", questionsAnswer), msg("user", "Answers:\n- q1: default\n")
	asks, result := msg("assistant", commandAnswer), msg("user", "The command exited 0.")
	garbage, correction := msg("assistant", "garbage"), msg("user", "That answer is not a valid turn")
	req := func(seq uint64, msgs ...map[string]string) *gatewayv1.AuditEntry {
		return entry(seq, audit.TypeLLMRequest, "", "", map[string]any{"messages": msgs})
	}
	resp := func(seq, request uint64, content, errText string) *gatewayv1.AuditEntry {
		return entry(seq, audit.TypeLLMResponse, "", "", llmResponse{RequestSeq: request, Content: content, Error: errText})
	}
	return []*gatewayv1.AuditEntry{
		req(2, sys, user), resp(3, 2, commandAnswer, ""),
		req(5, sys, user), resp(6, 5, questionsAnswer, ""),
		req(8, sys, user, asked, answers), resp(9, 8, commandAnswer, ""),
		req(10, sys, user, asked, answers, asks, result), resp(11, 10, "garbage", ""),
		req(12, sys, user, asked, answers, asks, result, garbage, correction), resp(13, 12, "", "llm: unavailable"),
	}
}

func TestReloadRebuildsTheSessionFromItsLatestValidAnswer(t *testing.T) {
	a := &fakeAuditor{pageSize: 2, entries: sessionEntries()}
	conv, err := Reload(context.Background(), a, ref, 5)
	if err != nil {
		t.Fatal(err)
	}
	// llm.request 12 failed and 10's answer is not a turn: 8's answer is the last one the model gave.
	if roles(conv) != "system,user,assistant,user,assistant" || conv[4].GetContent() != commandAnswer ||
		conv.ModelTurns() != 2 || conv.Commands() != 1 {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestReloadRefusesASessionTheIndexNoLongerHolds(t *testing.T) {
	unanswered := []*gatewayv1.AuditEntry{
		entry(5, audit.TypeLLMRequest, "", "", map[string]any{"messages": []any{}}),
		entry(6, audit.TypeLLMResponse, "", "", llmResponse{RequestSeq: 5, Content: "garbage"}),
	}
	for name, a := range map[string]*fakeAuditor{
		"truncated":          {entries: sessionEntries(), truncated: true},
		"first request lost": {entries: sessionEntries()[4:]},
		"no valid answer":    {entries: unanswered},
	} {
		if _, err := Reload(context.Background(), a, ref, 5); !errors.Is(err, ErrNoReload) {
			t.Errorf("%s: Reload = %v, want ErrNoReload", name, err)
		}
	}
}

func TestReloadReturnsGatewayErrorsUnchanged(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "the index could not be saved")}
	if _, err := Reload(context.Background(), a, ref, 5); status.Code(err) != codes.Unavailable || errors.Is(err, ErrNoReload) {
		t.Fatalf("Reload = %v", err)
	}
}
```

Run: `go test ./internal/agent/ -run Reload`
Expected: FAIL to compile (`undefined: Reload`, `undefined: llmResponse`, `undefined: ErrNoReload`).

- [ ] **Step 2: Write Reload**

In `internal/agent/audit.go`, add `"errors"` and `"fmt"` to the imports and append:

```go
// ErrNoReload means the audit log no longer holds the whole planning session,
// so it cannot be rebuilt.
var ErrNoReload = errors.New("agent: the conversation cannot be reloaded")

// llmRequest and llmResponse are the payloads of llm.request and llm.response
// audit entries, as the gateway writes them.
type llmRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type llmResponse struct {
	RequestSeq uint64 `json:"requestSeq"`
	Content    string `json:"content"`
	Error      string `json:"error,omitempty"`
}

// Reload rebuilds a planning session from the audit log (M3 spec §4.3): the
// messages of the session's latest llm.request whose llm.response holds a valid
// turn, followed by that turn. session is the sequence number of the session's
// first llm.request. Gateway errors are returned unchanged; a session the
// audit index no longer wholly holds gives an error wrapping ErrNoReload.
func Reload(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, session uint64) (Conversation, error) {
	if session == 0 {
		return nil, fmt.Errorf("%w: the Intent has no planning session", ErrNoReload)
	}
	var requests []*gatewayv1.AuditEntry
	truncated, err := scan(ctx, a, ref, audit.TypeLLMRequest, session-1, func(e *gatewayv1.AuditEntry) {
		requests = append(requests, e)
	})
	if err != nil {
		return nil, err
	}
	// The index can also lose entries in a gateway crash, which truncated does
	// not report: the session's first request must be there.
	if truncated || len(requests) == 0 || requests[0].GetSeq() != session {
		return nil, fmt.Errorf("%w: the audit index no longer holds llm.request %d", ErrNoReload, session)
	}
	answers := map[uint64]string{}
	truncated, err = scan(ctx, a, ref, audit.TypeLLMResponse, session-1, func(e *gatewayv1.AuditEntry) {
		var p llmResponse
		if json.Unmarshal(e.GetPayload(), &p) == nil && p.Error == "" {
			answers[p.RequestSeq] = p.Content
		}
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("%w: the audit index dropped part of the session", ErrNoReload)
	}
	for i := len(requests) - 1; i >= 0; i-- {
		seq := requests[i].GetSeq()
		content, ok := answers[seq]
		if !ok {
			continue
		}
		if _, err := ParseTurn(content); err != nil {
			continue
		}
		var p llmRequest
		if err := json.Unmarshal(requests[i].GetPayload(), &p); err != nil {
			return nil, fmt.Errorf("%w: llm.request %d: %v", ErrNoReload, seq, err)
		}
		conv := make(Conversation, 0, len(p.Messages)+1)
		for _, m := range p.Messages {
			conv = append(conv, &gatewayv1.Message{Role: m.Role, Content: m.Content})
		}
		if len(conv) == 0 || conv[0].GetRole() != roleSystem {
			return nil, fmt.Errorf("%w: llm.request %d does not start with a system prompt", ErrNoReload, seq)
		}
		return append(conv, &gatewayv1.Message{Role: roleAssistant, Content: content}), nil
	}
	return nil, fmt.Errorf("%w: no valid answer since llm.request %d", ErrNoReload, session)
}
```

Run: `go test ./internal/agent/`
Expected: PASS.

- [ ] **Step 3: Write the failing controller specs**

In `internal/controller/suite_test.go`:

1. Add this variable next to `gw`:

```go
	// intentReconciler is the suite's Intent controller; specs drop its
	// sessions to act out an operator restart.
	intentReconciler *IntentReconciler
```

2. Replace the `Expect((&IntentReconciler{…}).SetupWithManager(mgr)).To(Succeed())` statement with:

```go
	intentReconciler = &IntentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Gateway: gw, Reader: mgr.GetAPIReader(),
		Planner: &agent.Planner{LLM: gw}, Recorder: mgr.GetEventRecorder("last-operator"),
		RetryDelay: 10 * time.Millisecond}
	Expect(intentReconciler.SetupWithManager(mgr)).To(Succeed())
```

Create `internal/controller/intent_reload_test.go`:

```go
package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

var _ = Describe("Intent planning after an operator restart", func() {
	var key types.NamespacedName
	q1 := lastv1alpha1.Question{ID: "q1", Text: "Which namespace?"}
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespaces"}}}
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// restart drops the suite controller's conversation, as an operator restart
	// or a leader change would.
	restart := func() { intentReconciler.sessions.drop(key) }

	It("reloads the conversation from the audit log", func() {
		gw.setScript(key, &script{llm: turns(questionsTurn(q1), answer(steps))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		session := getIntent(key).Status.PlanningSessionSeq
		first := gw.lastRequest(key).GetMessages()

		restart()
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, queryAudit, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{"system", "user", "assistant", "user"}))
		Expect(msgs[0].GetContent()).To(Equal(first[0].GetContent()))
		Expect(msgs[1].GetContent()).To(Equal(first[1].GetContent()))
		Expect(msgs[2].GetContent()).To(Equal(questionsTurn(q1)))
		Expect(msgs[3].GetContent()).To(Equal("Answers:\n- q1: web\n"))
		Expect(getIntent(key).Status.PlanningSessionSeq).To(Equal(session))
	})

	It("reloads the conversation and a discovery command's result", func() {
		gw.setScript(key, &script{llm: turns(commandTurn("see what exists", look), answer(steps)),
			stdout: map[string]string{plan.DiscoveryStepID: "default\n"}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))

		restart()
		approveCommand(key, getIntent(key).Status.PendingCommand.Hash, lastv1alpha1.DecisionApprove, "")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded,
			queryAudit, queryAudit, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{"system", "user", "assistant", "user"}))
		Expect(msgs[3].GetContent()).To(Equal("The command exited 0.\nstdout:\ndefault\nstderr: empty\n"))
	})

	It("starts a new session, saying why, when the audit log no longer holds the conversation", func() {
		gw.setScript(key, &script{auditTruncated: true, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "could not reload") {
				return answer(steps), nil
			}
			return questionsTurn(q1), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		session := getIntent(key).Status.PlanningSessionSeq

		restart()
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{"system", "user"}))
		Expect(msgs[1].GetContent()).To(HavePrefix("Note: " + noteRestarted))
		Expect(msgs[1].GetContent()).To(ContainSubstring("- q1: web"))
		in := getIntent(key)
		Expect(in.Status.PlanningSessionSeq).NotTo(Equal(session))
		Expect(in.Status.PlanningSessionSeq).To(Equal(int64(gw.auditOf(key, "llm.request")[1].GetSeq())))
	})
})
```

Run: `make test`
Expected: FAIL. Without reload, a restarted spec starts a new session and makes no `QueryAudit` calls, so the call logs differ.

- [ ] **Step 4: Reload on a memory miss**

In `internal/controller/sessions.go`, add the imports `"errors"` and `logf "sigs.k8s.io/controller-runtime/pkg/log"`, and replace `conversation` with:

```go
// conversation returns the Intent's planning session so far. fresh means a new
// session starts: the Intent has none yet, or its session can no longer be
// rebuilt. A session this process does not hold, after an operator restart or
// a leader change, is reloaded from the audit log (M3 spec §4.3).
func (r *IntentReconciler) conversation(ctx context.Context, in *lastv1alpha1.Intent) (conv agent.Conversation, fresh bool, err error) {
	if in.Status.PlanningSessionSeq == 0 {
		return newConversation(in, ""), true, nil
	}
	if conv, ok := r.sessions.get(in); ok {
		return conv, false, nil
	}
	conv, err = agent.Reload(ctx, r.Gateway, intentRef(in), uint64(in.Status.PlanningSessionSeq))
	switch {
	case err == nil:
		logf.FromContext(ctx).Info("Reloaded planning conversation", "session", in.Status.PlanningSessionSeq, "messages", len(conv))
		r.sessions.put(in, conv)
		return conv, false, nil
	case errors.Is(err, agent.ErrNoReload) || terminal(err):
		logf.FromContext(ctx).Info("Could not reload planning conversation; starting a new session", "reason", err.Error())
		return newConversation(in, noteRestarted), true, nil
	}
	return nil, false, err
}
```

In `internal/controller/intent_planning.go`, in `plan`, replace

```go
	conv, fresh := r.conversation(in)
```

with

```go
	conv, fresh, err := r.conversation(pctx, in)
	if err != nil {
		return ctrl.Result{}, err // transient: retried with backoff
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `make test`
Expected: PASS, with 0 failures.

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/agent internal/controller
git commit -m "feat(controller): reload a planning session from the audit log after a restart"
```

---
### Task 6: Provider and egress context, read-only Provider RBAC, and the spec amendments

A session's first message tells the model what commands may use:
- the Providers that exist and that the namespace's effective policy allows, each with its description;
- the allowed egress, one list per policy layer;
- whether scripts are allowed.

The reads go through the uncached reader, so a policy edited moments before a session starts is current. An invalid policy fails planning; the gateway would refuse every command anyway.

The operator gains read-only RBAC on `providers`. That is M3's only RBAC change.

The task closes by amending the main spec (§4.1, §7, §17.2) and the M3 spec (§3.2, §4.1–§4.3), and the M3a follow-ups record that the token budget is done.

**Files:**
- Modify: `internal/policy/policy.go`, `internal/policy/policy_test.go`
- Modify: `internal/controller/sessions.go`, `internal/controller/intent_planning.go`, `internal/controller/intent_controller.go`
- Test: `internal/controller/intent_controller_test.go`
- Regenerate: `config/rbac/role.yaml` (`make manifests`)
- Modify: `charts/last-operator/files/operator-clusterrole.yaml`, `test/admission/operator_rbac_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md`, `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`, `docs/superpowers/followups/2026-10-13-m3a-followups.md`

**Interfaces:**
- Consumes:
  - `policy.Resolve`, `Effective.AllowsProvider`, `Effective.AllowScripts`;
  - Task 2's `agent.Environment`, `agent.Provider`, `agent.Request.Env`;
  - Task 5's `conversation(ctx, in)`.
- Produces:
  - `func (e Effective) Egress() [][]string`;
  - `func (r *IntentReconciler) environment(ctx context.Context, ns string) (agent.Environment, error)`, with `var errBadPolicy`;
  - `func (r *IntentReconciler) newConversation(ctx context.Context, in *lastv1alpha1.Intent, note string) (agent.Conversation, error)`, which replaces the function form.

- [ ] **Step 1: Write the failing policy test**

In `internal/policy/policy_test.go`, add `"reflect"` to the imports and append:

```go
func TestEgressListsEachLayersRules(t *testing.T) {
	narrow := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{
		Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.1.0.0/16:443"}}}}
	e, err := policy.Resolve(opConfig(nil, []string{"10.0.0.0/8:443", "db.example.com:5432"}, nil), []lastv1alpha1.IntentPolicy{narrow})
	if err != nil {
		t.Fatal(err)
	}
	// Host patterns need Cilium (spec §11.1) and allow nothing yet.
	if got := e.Egress(); !reflect.DeepEqual(got, [][]string{{"10.0.0.0/8:443"}, {"10.1.0.0/16:443"}}) {
		t.Errorf("Egress = %v", got)
	}
	shipped, err := policy.Resolve(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := shipped.Egress(); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Errorf("shipped defaults: Egress = %v, want one layer that allows nothing", got)
	}
}
```

Run: `go test ./internal/policy/`
Expected: FAIL to compile (`e.Egress undefined`).

- [ ] **Step 2: Write Egress**

In `internal/policy/policy.go`, after `AllowsEgress`, add:

```go
// Egress returns each layer's allowed destinations, as CIDR:port. A
// destination must be allowed by every layer (AllowsEgress).
func (e Effective) Egress() [][]string {
	out := make([][]string, 0, len(e.egressLayers))
	for _, layer := range e.egressLayers {
		rules := make([]string, 0, len(layer))
		for _, r := range layer {
			rules = append(rules, r.String())
		}
		out = append(out, rules)
	}
	return out
}
```

Run: `go test ./internal/policy/`
Expected: PASS.

- [ ] **Step 3: Write the failing controller specs**

Append to `internal/controller/intent_controller_test.go`:

```go
var _ = Describe("Intent planning context", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	createCleanly := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, obj)).To(Succeed()) })
	}
	provider := func(name, description string) *lastv1alpha1.Provider {
		return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: lastv1alpha1.ProviderSpec{
			Type: lastv1alpha1.ProviderEnvSecret, Description: description, SecretRef: &lastv1alpha1.LocalRef{Name: name}}}
	}

	It("tells the model which Providers and egress the namespace may use", func() {
		scripts := false
		createCleanly(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{
				Providers: []string{"ctx-k8s", "ctx-aws"}, AllowScripts: &scripts,
				Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}}}}})
		createCleanly(provider("ctx-k8s", "cluster-admin on this cluster"))
		createCleanly(provider("ctx-db", "the orders database")) // exists, but the policy does not allow it
		Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: key.Namespace},
			Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.1.0.0/16:443"}}}})).To(Succeed())
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		first := requestText(gw.lastRequest(key))
		Expect(first).To(ContainSubstring("Providers commands may use:\n- ctx-k8s: cluster-admin on this cluster\n\n"))
		Expect(first).NotTo(ContainSubstring("ctx-db"))
		Expect(first).NotTo(ContainSubstring("ctx-aws"), "allowed, but no such Provider exists")
		Expect(first).To(ContainSubstring("every one of these lists:\n- 10.0.0.0/8:443\n- 10.1.0.0/16:443\n"))
		Expect(first).To(ContainSubstring("Scripts are not allowed"))
	})

	It("fails planning when the namespace's policy is invalid", func() {
		Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: key.Namespace},
			Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"nope"}}}})).To(Succeed())
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("effective policy is invalid"))
		Expect(gw.calls(key)).To(BeEmpty())
	})
})
```

If `metav1` is not yet imported in that file, add `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`.

Run: `make test`
Expected: FAIL. The first message has no Providers, egress or scripts lines from the policy, and the invalid policy plans anyway.

- [ ] **Step 4: Build the environment into the first message**

In `internal/controller/sessions.go`:

1. Add the imports `"fmt"`, `"slices"`, `"strings"`, `apierrors "k8s.io/apimachinery/pkg/api/errors"` and `"github.com/moogie-labs/last-operator/internal/policy"`.
2. Replace `newConversation` with the code below, and add `errBadPolicy` and `environment`:

```go
// newConversation starts a session for the Intent's current spec, telling the
// model what the namespace's policy lets commands use.
func (r *IntentReconciler) newConversation(ctx context.Context, in *lastv1alpha1.Intent, note string) (agent.Conversation, error) {
	env, err := r.environment(ctx, in.Namespace)
	if err != nil {
		return nil, err
	}
	return agent.NewConversation(agent.FirstMessage(agent.Request{Namespace: in.Namespace, Name: in.Name,
		Text: in.Spec.Request, Answers: in.Spec.Answers, Feedback: replanFeedback(in), Note: note, Env: env})), nil
}

// errBadPolicy marks a namespace whose effective policy cannot be resolved:
// the gateway would refuse every command, so planning fails.
var errBadPolicy = errors.New("the namespace's effective policy is invalid")

// environment returns what the namespace's effective policy lets commands use
// (M3 spec §4.4): the Providers that exist and the policy allows, the allowed
// egress, and whether scripts may run. The gateway enforces the policy; this
// only tells the model. It reads uncached, so a policy edited just before a
// session starts is current.
func (r *IntentReconciler) environment(ctx context.Context, ns string) (agent.Environment, error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: "default"}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return agent.Environment{}, err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := r.Reader.List(ctx, &policies, client.InNamespace(ns)); err != nil {
		return agent.Environment{}, err
	}
	eff, err := policy.Resolve(op, policies.Items)
	if err != nil {
		return agent.Environment{}, fmt.Errorf("%w: %v", errBadPolicy, err)
	}
	var providers lastv1alpha1.ProviderList
	if err := r.Reader.List(ctx, &providers); err != nil {
		return agent.Environment{}, err
	}
	slices.SortFunc(providers.Items, func(a, b lastv1alpha1.Provider) int { return strings.Compare(a.Name, b.Name) })
	env := agent.Environment{Egress: eff.Egress(), NoScripts: !eff.AllowScripts}
	for _, p := range providers.Items {
		if eff.AllowsProvider(p.Name) {
			env.Providers = append(env.Providers, agent.Provider{Name: p.Name, Description: p.Spec.Description})
		}
	}
	return env, nil
}
```

3. In `conversation`, replace the two `return newConversation(…), true, nil` lines with the code below. It assigns `conv` and `err`, the named results.

```go
		conv, err = r.newConversation(ctx, in, "")
		return conv, true, err
```

and

```go
		conv, err = r.newConversation(ctx, in, noteRestarted)
		return conv, true, err
```

In `internal/controller/intent_planning.go`, in `plan`, replace

```go
	conv, fresh, err := r.conversation(pctx, in)
	if err != nil {
		return ctrl.Result{}, err // transient: retried with backoff
	}
```

with

```go
	conv, fresh, err := r.conversation(pctx, in)
	switch {
	case errors.Is(err, errBadPolicy):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, err.Error())
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
```

- [ ] **Step 5: Grant read-only Provider access**

In `internal/controller/intent_controller.go`, add this marker after the `intentapprovals` marker:

```go
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=providers;intentpolicies;operatorconfigs,verbs=get;list;watch
```

Run: `make manifests`
Expected: `config/rbac/role.yaml` now lists `providers` with `get`, `list` and `watch`. Check with `git diff config/rbac/role.yaml`.

In `charts/last-operator/files/operator-clusterrole.yaml`, change

```yaml
    resources: [intentapprovals, intentpolicies, operatorconfigs]
```

to

```yaml
    resources: [intentapprovals, intentpolicies, operatorconfigs, providers]
```

and in the header comment, change `it reads approvals, policy and configuration` to `it reads approvals, policy, configuration and Providers`.

In `test/admission/operator_rbac_test.go`, add these entries to the `allowed` list, after `attr(last, "", "operatorconfigs", "", "get")`:

```go
		attr(last, "", "providers", "", "list"),
		attr(last, "", "providers", "", "watch"),
```

The `denied` list already checks that the operator cannot create, update, patch or delete `providers`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `make test`
Expected: PASS, with 0 failures, including `test/admission`.

Run: `make helm-lint`
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 7: Amend the main spec**

In `docs/superpowers/specs/2026-09-26-last-operator-design.md`:

**§4.1:** replace

```
- get/list/watch on `IntentApproval`, `IntentPolicy`, and `OperatorConfig`.
```

with

```
- get/list/watch on `IntentApproval`, `IntentPolicy`, `OperatorConfig`,
  and `Provider`. The first message of a planning session lists the
  Providers the namespace may use (M3).
```

**§7:** replace the Clarifying and Planning bullets:

```
- **Clarifying.** The agent may ask questions instead of acting. They
  appear in `status.questions`; answers go in `spec.answers`.
- **Planning.** An agentic conversation. The LLM may run **discovery
  commands** to inspect the world. Each one is treated as a
  single-command approval subject (stage `Discovery`). When it needs
  approval, the Intent shows `phase: AwaitingDiscoveryApproval` with
  `status.pendingCommand`. The planning conversation is kept in memory
  and the audit log, so it resumes after approval.
```

with

```
- **Clarifying.** The agent may ask 1–5 questions instead of acting.
  They appear in `status.questions`; answers go in `spec.answers`. Once
  every question has an answer, planning continues in the same session.
  Answers bump `generation` but never start a new session; only a
  changed `spec.request` does (`status.observedRequestHash`).
- **Planning.** An agentic conversation. A **planning session** starts
  with a new Intent, a `spec.request` edit, or a replan after a failed
  step, and ends when a plan is written.
  - **Turns.** Every model answer is one JSON turn: a plan, questions,
    or a discovery command. It is checked against a JSON Schema sent
    with `CallLLM`, and again by the operator. A malformed answer gets a
    correction, at most 3 in a row.
  - **Discovery commands** inspect the world. Each one is a
    single-command approval subject: stage `Discovery`, subject
    `sha256(canonical {command, nonce})`, so each approval is
    single-use. The Intent shows `phase: AwaitingDiscoveryApproval`
    with `status.pendingCommand`. Once approved, the command runs as a
    one-step Discovery phase. Its result, a denial, or the gateway's
    refusal goes back to the model.
  - **Caps.** A session allows at most 20 model answers and 10
    discovery commands. Reaching either fails the Intent with
    `PlanningFailed` (M3 design D7, §4).
```

**§7:** replace the restart paragraph:

```
**Operator restarts during Planning:** the operator reloads the
conversation from `QueryAudit` (`llm.*` and `command.*` entries for the
current phase) and continues. Until `QueryAudit` arrives (M3), a restart during
Planning starts the conversation over.
```

with

```
**Operator restarts during a planning session:** the conversation lives
in the operator's memory.
- **Reload.** After a restart or a leader change, the operator rebuilds
  the conversation from `QueryAudit`: the session's latest
  `llm.request` (from `status.planningSessionSeq` on) whose
  `llm.response` holds a valid turn, followed by that turn.
- **The owed user turn** is re-derived from the Intent: the answers, or
  the discovery outcome its `Executed` condition records. A discovery
  result is read from the command's `command.result` entry.
- **Lost sessions.** If the audit index no longer holds the session, a
  new session starts and its first message says so.
- **Lost discovery phases.** A discovery phase that was open when the
  operator stopped is ended as failed, and the model is told the
  command was lost.
```

**§17.2:** replace

```
- **No hard caps on agent turns or cost.** Runaway agents are visible
  in metrics but not stopped automatically.
```

with

```
- **Caps on planning, not on cost.** A planning session stops after 20
  model answers or 10 discovery commands (M3 design D7). Nothing caps
  cost across sessions or replans: runaway spend shows in the token
  metrics but is not stopped automatically.
```

- [ ] **Step 8: Amend the M3 spec and the M3a follow-ups**

In `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`:

**§3.2 Streaming:** replace `(the operator sends 16000).` with

```
(the operator sends 64000: thinking, always on, shares the budget, so 16000 risked truncated answers at effort `high`).
```

**§4.1:** replace the bullet that starts `- **The schema.**` with:

```
- **The schema.** The operator builds the JSON Schema for this union once and sends it as `response_schema`.
  - **One closed object.** OpenAI strict mode needs an object root with every property required, and both providers need closed objects. So the schema is one closed object: `kind` plus every field of every kind. The fields a kind does not use are left empty, and `command` is null.
  - **A wire format of its own.** The JSON the model writes differs from the CRD's:
    - `run` is flattened to `argv` and `script`;
    - the maps `files` and `outputs` are arrays of `{path, content}` and `{key, value}`;
    - the free-form `identity` and `desired` are JSON objects written as strings;
    - `outputsSecretRef` is the string `outputsSecret`.
  - **Lenient parsing.** The parser accepts omitted fields and `null`, for models that do not enforce the schema.
```

**§4.2:** after the line that starts `5. **Caps.**`, insert:

```

**Between turns.**
- **Owed turns.** Returning to Planning keeps `status.questions` or `status.pendingCommand` until the model's next turn replaces them. How a discovery command ended is recorded in the `Executed` condition: `DiscoveryRan`, `DiscoveryDenied` or `DiscoveryFailed`. A restart can then re-derive the user turn the model is owed (§4.3).
- **Reused question ids.** A question whose `id` is already in `spec.answers` is refused with a correction, because its old answer would answer it at once.
- **Lost discovery phases.** A Discovery phase found open after a restart is ended as failed and reported to the model as lost.
- **Refusals.** A gateway refusal of an approved command is reported as "The command did not run: …".
```

**§4.3:** replace

```
  - the latest `llm.request` gives the message list;
  - its `llm.response` gives the assistant turn;
  - the discovery `command.result` entries since then give the user turns that followed.
```

with

```
  - the session's latest `llm.request` whose `llm.response` holds a valid turn gives the message list;
  - that `llm.response` gives the assistant turn;
  - the pending user turn follows (below). A discovery result is read from the command's `command.result` entry.
```

**§4.3:** in the bullet that starts `- **The pending user turn.**`, replace `a denial from the matching \`IntentApproval\`` with:

```
a denial or failure from the `Executed` condition's message
```

**§4.3:** at the end of the bullet that starts `- **Unrecoverable reload.**`, append:

```
 The session's first `llm.request` must still be in the index, which also catches index entries lost in a gateway crash. Known gap: an answer the model gave just before a restart, which the operator never recorded, stays in the reloaded conversation, and the model then sees the pending user turn a second time.
```

In `docs/superpowers/followups/2026-10-13-m3a-followups.md`, replace the `- **Token budget.**` item with:

```
- **Token budget.** Done in M3b. The operator sends `max_tokens` 64000, and M3 spec §3.2 is amended. Still open: OpenAI-compatible models with a smaller output limit reject 64000 with a 400 (`InvalidArgument`), which fails planning. A per-provider limit (`LLMProvider.spec.maxTokens`, or clamping in the adapter) belongs with the first OpenAI-compatible user.
```

- [ ] **Step 9: Verify and commit**

Run: `make lint-fix && make lint && make test`
Expected: `0 issues.` and PASS, with 0 failures.

```bash
git add internal/policy internal/controller config/rbac/role.yaml charts/last-operator/files/operator-clusterrole.yaml \
  test/admission/operator_rbac_test.go docs/superpowers/specs docs/superpowers/followups/2026-10-13-m3a-followups.md
git commit -m "feat(controller): tell the model which Providers and egress it may use; amend the specs for M3b"
```

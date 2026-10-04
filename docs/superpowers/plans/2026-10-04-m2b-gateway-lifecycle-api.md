# M2b: Gateway lifecycle API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the operator everything it needs from the gateway to take an Intent through Create. That means:
- open an approved phase;
- run the plan's steps exactly as approved, one at a time;
- write output Secrets;
- close the phase;
- run the executed plan's probes;
- call a (scripted) LLM.

All of it is enforced and audited by the gateway.

**Architecture:** The gateway trusts nothing the operator says:
- **Plan:** it reads the plan from `Intent.status.plan`, recomputes its hash, and requires a matching `IntentApproval` before it writes `phase.begin`.
- **Steps:** it resolves `${steps…}` references itself and runs a step only if it is exactly the next approved step.
- **Probes:** it runs only the probes of the last plan it executed.

Phase progress and the last executed plan for each Intent live in a gateway-owned ConfigMap in `last-gateway`; the operator has no access to it. That makes a gateway restart safe: an interrupted phase is closed as `interrupted`, and the same subject is never re-run without a new plan. Kubernetes Provider tokens are bound to a per-run anchor Secret, so cleanup revokes them.

**Tech Stack:** Go 1.26, gRPC + buf, client-go fake clientset, controller-runtime client (fake and envtest), ValidatingAdmissionPolicy, Helm, kind.

**Spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§4.2, §5.1–5.3, §6, §7, §8, §10, §12, §14.2, §15). M2a follow-ups: `docs/superpowers/followups/2026-10-03-m2a-followups.md`.

**Scope split.** Spec milestone M2 is delivered in three plans:
- **M2a** (done): sandbox execution and gRPC authentication.
- **M2b** (this plan): the gateway lifecycle API.
- **M2c** (next): the operator side, which is the `Intent` and `ManagedResource` controllers, the operator Deployment and its RBAC, and the scripted-LLM web-server e2e.

## Global Constraints

- Module `github.com/moogie-labs/last-operator`; API group `last.moogie-labs.com/v1alpha1`.
- **Fail closed.** Every side effect is preceded by its audit entry. If the writer is missing or broken, or an append fails, the RPC returns `codes.Unavailable` and does nothing.
- **Actors.**
  - Entries caused by an operator RPC carry `Actor: gateway.CallerFrom(ctx)`.
  - Entries the gateway writes on its own carry `sandbox.GatewayActor` (`"gateway"`): an interrupted `phase.end`, the reaper, and `egress.revoked`.
- **Intent key.** The `Intent` field of every entry is `"<namespace>/<name>"`.
- **Phases.** M2b opens phases for stage `Create` only. Discovery arrives in M3; Update, Remediate and Delete in M5. Other stages → `FailedPrecondition`.
- **Approvals.** Auto modes are not honored until M5: every phase needs a matching `IntentApproval` with `decision: Approve`. Allow rules arrive in M6.
- **No image builds.** Commands with `image.install` are refused (`PermissionDenied`); builds arrive in M4.
- **Credentials.**
  - Credential values never appear in audit payloads, logs or errors.
  - Output Secret values are audited as SHA-256 hex digests only.
- **gRPC status codes, used consistently:**

  | Condition | Code |
  |---|---|
  | Audit unavailable, or not leading | `Unavailable` |
  | No open phase, wrong step order, no approval, interrupted phase, no executed plan | `FailedPrecondition` |
  | Policy violation, command mismatch, approval denied, ownership violation | `PermissionDenied` |
  | Malformed request | `InvalidArgument` |
  | Intent or ManagedResource missing | `NotFound` |
  | A phase is already open | `AlreadyExists` |
  | Concurrent step, or a probe run already in flight | `Aborted` |
  | Sandbox infrastructure failure | `Internal` |
- **Logging.** Follow the Kubernetes message style: a capital letter, no trailing period, past tense ("Could not …"), balanced key/value pairs.
- **Commits.**
  - After each task, run `make lint-fix`, `make test` and `make lint`.
  - After editing API types or markers, also run `make manifests generate helm-crds`.
  - Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml`, `zz_generated.*` or `PROJECT`.
  - No `Co-authored-by` or other attribution trailers.
  - Never commit `.superpowers/`.
- **e2e.** Run on the kind cluster `KIND_CLUSTER=last-e2e-m2a` (reuse it if it exists).

## Review Focus

1. **A RunCommand payload that differs from the approved step only cosmetically** (map order, `null` vs `[]`, a trailing newline in a file) must be compared on canonical JSON. Map order and `null`/`[]` must be accepted. Any real difference, such as a trailing newline in file content, must be refused with `policy.denied`. Owned by Task 9.
2. **A gateway restart in the middle of a phase.** The next `BeginPhase` for that Intent must close the old phase as `interrupted` and refuse to re-run the same subject (no double execution). It must still accept a new subject. Owned by Task 8.
3. **A forged approval.** An `IntentApproval` whose `approvedBy` is not its creator, or that the operator ServiceAccount creates, must be rejected at admission. An approval naming the operator identity must also be refused by the gateway. Owned by Tasks 13 and 8.
4. **An output Secret name that collides with an existing, user-owned Secret** must be refused, never overwritten. Owned by Task 11.
5. **A plan whose `desired` snapshot contains floats or very large integers** must hash, open and execute normally. Such numbers would break the audit writer's number rules if stored as a JSON object, so `phase.begin` stores the plan as its canonical JSON string. Owned by Tasks 1 and 8.

## File map

| File | Responsibility | Task |
|---|---|---|
| `internal/plan/plan.go`, `plan_test.go` | Plan body, canonical hash, validation, `${steps…}` resolution | 1 |
| `internal/policy/policy.go`, `policy_test.go` | Layered providers/egress/scripts policy and command checks | 2 |
| `internal/sandbox/credentials.go`, `executor.go`, `reaper.go` (+ tests) | Per-run token anchor Secret; cleanup/reaper cover the provider namespace | 3 |
| `api/v1alpha1/llmprovider_types.go`, `internal/audit/entry.go`, spec | `scripted` LLM type; `output.written` entry type; spec text | 4 |
| `proto/last/gateway/v1/gateway.proto`, generated code | Lifecycle RPCs and messages | 5 |
| `internal/gateway/config.go`, `server.go`, `runtime.go`, `cmd/gateway/main.go` (+ tests) | Sandbox config, CRD client, executor factory, in-flight tracking and drain, reap on lead | 6 |
| `internal/gateway/state.go`, `state_test.go` | Gateway-owned per-Intent record (open phase, executed plan) | 7 |
| `internal/gateway/phases.go`, `phases_test.go`, `testenv_test.go` | `BeginPhase` / `EndPhase` | 8 |
| `internal/gateway/commands.go`, `commands_test.go` | `RunCommand` | 9 |
| `internal/gateway/probes.go`, `probes_test.go` | `RunProbes` | 10 |
| `internal/gateway/outputs.go`, `outputs_test.go` | `WriteOutputSecret` | 11 |
| `internal/gateway/llm/{llm.go,scripted.go,scripted_test.go}`, `internal/gateway/llmrpc.go`, `llmrpc_test.go` | `CallLLM` with the scripted adapter | 12 |
| `charts/last-operator/files/intentapproval-policy.yaml`, `templates/admission.yaml`, `test/admission/admission_test.go` | IntentApproval admission policy | 13 |
| `charts/last-operator/templates/gateway.yaml`, `values.yaml`, `Makefile` | Gateway RBAC, env, grace period, render checks | 14 |
| `test/e2e/lifecycle_e2e_test.go`, `hack/e2e.sh` | Lifecycle over gRPC on kind, including token revocation | 15 |

---

### Task 1: Plan hashing, validation and step references

**Files:**
- Create: `internal/plan/plan.go`, `internal/plan/plan_test.go`

**Interfaces:**
- Consumes: `audit.Canonical(v any) ([]byte, error)` (`internal/audit/canonical.go`); `runnerapi.Result`; `lastv1alpha1.Command`, `ManagedResourceSpec`, `Plan`.
- Produces (package `plan`):
  - `type Body struct{ Steps []lastv1alpha1.Command \`json:"steps"\`; Resources []lastv1alpha1.ManagedResourceSpec \`json:"resources,omitempty"\` }`.
  - `func FromStatus(p *lastv1alpha1.Plan) Body`, which treats nil steps as empty.
  - `func Canonical(b Body) ([]byte, error)` and `func Hash(b Body) (string, error)`. `Hash` returns `"sha256:<64 hex>"`.
  - `func Validate(b Body) error`:
    - at least one step;
    - every step ID non-empty and unique;
    - every probe ID unique across all resources.
  - `type Results map[string]runnerapi.Result`.
  - `func (r Results) Value(expr string) (string, error)` for a single `${steps.<id>.stdout}` or `${steps.<id>.json.<path>}` expression.
  - `func Resolve(c lastv1alpha1.Command, r Results) (lastv1alpha1.Command, error)`, which replaces references inside `Run.Argv` only.
  - `var ErrReference`.

- [ ] **Step 1: Write the failing tests**

`internal/plan/plan_test.go`:
```go
package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func step(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func TestHashIsSHA256OfCanonicalBody(t *testing.T) {
	b := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "get", "ns")}}
	c, err := audit.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(c)
	got, err := plan.Hash(b)
	if err != nil || got != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("Hash = %q, %v", got, err)
	}
}

func TestHashIgnoresMapOrderAndNilVersusEmpty(t *testing.T) {
	a := step("s1", "true")
	a.Files = map[string]string{"/work/a": "1", "/work/b": "2"}
	b := step("s1", "true")
	b.Files = map[string]string{"/work/b": "2", "/work/a": "1"}
	ha, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{a}})
	hb, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{b}})
	if ha != hb {
		t.Fatalf("map order changed the hash: %s vs %s", ha, hb)
	}
	h1, _ := plan.Hash(plan.FromStatus(&lastv1alpha1.Plan{}))
	h2, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{}})
	if h1 != h2 {
		t.Fatalf("nil and empty steps hash differently")
	}
}

func TestHashChangesWithAnyStepField(t *testing.T) {
	base, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "apply")}})
	changed, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "delete")}})
	if base == changed {
		t.Fatal("hash did not change")
	}
}

func TestHashAcceptsFloatsAndLargeNumbersInDesired(t *testing.T) {
	b := plan.Body{
		Steps: []lastv1alpha1.Command{step("s1", "true")},
		Resources: []lastv1alpha1.ManagedResourceSpec{{
			Desired: &runtime.RawExtension{Raw: []byte(`{"ratio":0.75,"big":12345678901234567890}`)},
		}},
	}
	if _, err := plan.Hash(b); err != nil {
		t.Fatalf("Hash: %v", err)
	}
}

func TestValidate(t *testing.T) {
	probe := func(id string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Command: step("", "true"), Assert: "true"}
	}
	cases := []struct {
		name string
		body plan.Body
		want string
	}{
		{"no steps", plan.Body{}, "at least one step"},
		{"empty id", plan.Body{Steps: []lastv1alpha1.Command{step("", "true")}}, "has no id"},
		{"duplicate id", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a"), step("s1", "b")}}, `duplicate step id "s1"`},
		{"duplicate probe", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Probes: []lastv1alpha1.Probe{probe("p")}}, {Probes: []lastv1alpha1.Probe{probe("p")}}}}, `duplicate probe id "p"`},
	}
	for _, tc := range cases {
		err := plan.Validate(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.want)
		}
	}
	ok := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a"), step("s2", "b")}}
	if err := plan.Validate(ok); err != nil {
		t.Fatalf("Validate(ok) = %v", err)
	}
}

func results() plan.Results {
	return plan.Results{
		"get":   {ID: "get", Stdout: `{"spec":{"replicas":2,"ports":[{"port":80}]},"ready":true,"name":"web","obj":{}}` + "\n"},
		"echo":  {ID: "echo", Stdout: "10.0.0.5\n"},
		"bad":   {ID: "bad", ExitCode: 1, Stdout: "x"},
		"trunc": {ID: "trunc", Stdout: `{"a":`, StdoutTruncated: true},
		"evil":  {ID: "evil", Stdout: "${steps.get.stdout}"},
	}
}

func TestValue(t *testing.T) {
	r := results()
	ok := map[string]string{
		"${steps.echo.stdout}":                 "10.0.0.5",
		"${steps.get.json.spec.replicas}":      "2",
		"${steps.get.json.spec.ports.0.port}":  "80",
		"${steps.get.json.ready}":              "true",
		"${steps.get.json.name}":               "web",
	}
	for expr, want := range ok {
		if got, err := r.Value(expr); err != nil || got != want {
			t.Errorf("Value(%s) = %q, %v; want %q", expr, got, err, want)
		}
	}
	bad := []string{
		"${steps.missing.stdout}", "${steps.bad.stdout}", "${steps.get.json.spec.nope}",
		"${steps.get.json.obj}", "${steps.get.json.spec.ports.9.port}", "${steps.trunc.json.a}",
		"${steps.echo.json.x}", "steps.echo.stdout", "${steps.echo.stdout} ",
	}
	for _, expr := range bad {
		if _, err := r.Value(expr); !errors.Is(err, plan.ErrReference) {
			t.Errorf("Value(%q) = %v, want ErrReference", expr, err)
		}
	}
}

func TestResolveReplacesArgvReferencesOnly(t *testing.T) {
	c := step("s2", "kubectl", "scale", "--replicas=${steps.get.json.spec.replicas}", "${steps.echo.stdout}")
	c.Files = map[string]string{"/work/x": "${steps.echo.stdout}"}
	got, err := plan.Resolve(c, results())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "scale", "--replicas=2", "10.0.0.5"}
	if strings.Join(got.Run.Argv, "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %q", got.Run.Argv)
	}
	if got.Files["/work/x"] != "${steps.echo.stdout}" {
		t.Fatalf("files were resolved: %q", got.Files)
	}
	if c.Run.Argv[2] != "--replicas=${steps.get.json.spec.replicas}" {
		t.Fatal("Resolve mutated its input")
	}
}

func TestResolveDoesNotReexpandValues(t *testing.T) {
	got, err := plan.Resolve(step("s", "echo", "${steps.evil.stdout}"), results())
	if err != nil || got.Run.Argv[1] != "${steps.get.stdout}" {
		t.Fatalf("argv = %q, %v", got.Run.Argv, err)
	}
}

func TestResolveRejectsMalformedAndFailedReferences(t *testing.T) {
	for _, arg := range []string{"${steps.echo}", "${steps.echo.stderr}", "x${steps.bad.stdout}"} {
		if _, err := plan.Resolve(step("s", "echo", arg), results()); !errors.Is(err, plan.ErrReference) {
			t.Errorf("Resolve(%q) = %v, want ErrReference", arg, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/plan/ -count=1`
Expected: FAIL. The build fails with `no non-test Go files` or `undefined: plan.Body`.

- [ ] **Step 3: Implement**

`internal/plan/plan.go`:
```go
// Package plan hashes agent plans and resolves ${steps…} references (spec §8.1–§8.3).
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Body is the hashed part of a plan: its steps and resources (spec §8.2).
type Body struct {
	Steps     []lastv1alpha1.Command             `json:"steps"`
	Resources []lastv1alpha1.ManagedResourceSpec `json:"resources,omitempty"`
}

// FromStatus returns the hashed body of an Intent's status plan.
func FromStatus(p *lastv1alpha1.Plan) Body {
	b := Body{Steps: p.Steps, Resources: p.Resources}
	if b.Steps == nil {
		b.Steps = []lastv1alpha1.Command{}
	}
	return b
}

// Canonical returns the RFC 8785 canonical JSON of the body.
func Canonical(b Body) ([]byte, error) {
	if b.Steps == nil {
		b.Steps = []lastv1alpha1.Command{}
	}
	return audit.Canonical(b)
}

// Hash returns "sha256:<hex>" of the canonical body; it is the plan's approval subject.
func Hash(b Body) (string, error) {
	c, err := Canonical(b)
	if err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Validate checks what the gateway relies on: step IDs name the steps that
// ${steps…} references and RunCommand use, and probe IDs name probe outcomes.
func Validate(b Body) error {
	if len(b.Steps) == 0 {
		return errors.New("plan: a plan needs at least one step")
	}
	seen := map[string]bool{}
	for i, s := range b.Steps {
		if s.ID == "" {
			return fmt.Errorf("plan: step %d has no id", i)
		}
		if seen[s.ID] {
			return fmt.Errorf("plan: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
	}
	probes := map[string]bool{}
	for _, r := range b.Resources {
		for _, p := range r.Probes {
			if probes[p.ID] {
				return fmt.Errorf("plan: duplicate probe id %q", p.ID)
			}
			probes[p.ID] = true
		}
	}
	return nil
}

// ErrReference reports a ${steps…} reference that cannot be resolved.
var ErrReference = errors.New("plan: bad step reference")

var refPattern = regexp.MustCompile(`\$\{steps\.([A-Za-z0-9_-]+)\.(stdout|json((?:\.[A-Za-z0-9_-]+)+))\}`)

// Results holds the results of earlier steps of a phase, by step ID.
type Results map[string]runnerapi.Result

// Value returns the value of a single reference expression, either
// "${steps.<id>.stdout}" (trailing newlines removed) or "${steps.<id>.json.<path>}"
// (path segments are object keys or array indexes; the value must be a string,
// number, or boolean).
func (r Results) Value(expr string) (string, error) {
	m := refPattern.FindStringSubmatch(expr)
	if m == nil || m[0] != expr {
		return "", fmt.Errorf("%w: %q is not a single step reference", ErrReference, expr)
	}
	res, ok := r[m[1]]
	if !ok {
		return "", fmt.Errorf("%w: step %q has no result", ErrReference, m[1])
	}
	if res.ExitCode != 0 || res.Error != "" {
		return "", fmt.Errorf("%w: step %q did not succeed", ErrReference, m[1])
	}
	if m[2] == "stdout" {
		return strings.TrimRight(res.Stdout, "\n"), nil
	}
	if res.StdoutTruncated {
		return "", fmt.Errorf("%w: step %q output was truncated", ErrReference, m[1])
	}
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("%w: step %q output is not JSON", ErrReference, m[1])
	}
	for _, key := range strings.Split(strings.TrimPrefix(m[3], "."), ".") {
		switch node := v.(type) {
		case map[string]any:
			child, ok := node[key]
			if !ok {
				return "", fmt.Errorf("%w: %s: no key %q", ErrReference, expr, key)
			}
			v = child
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return "", fmt.Errorf("%w: %s: no index %q", ErrReference, expr, key)
			}
			v = node[i]
		default:
			return "", fmt.Errorf("%w: %s: %q is not inside an object or array", ErrReference, expr, key)
		}
	}
	switch val := v.(type) {
	case string:
		return val, nil
	case json.Number:
		return val.String(), nil
	case bool:
		return strconv.FormatBool(val), nil
	default:
		return "", fmt.Errorf("%w: %s is not a string, number, or boolean", ErrReference, expr)
	}
}

// Resolve returns a copy of c with every ${steps…} reference in its argv
// replaced. Files, scripts, and other fields are never resolved (spec §8.1).
func Resolve(c lastv1alpha1.Command, r Results) (lastv1alpha1.Command, error) {
	if len(c.Run.Argv) == 0 {
		return c, nil
	}
	argv := make([]string, len(c.Run.Argv))
	for i, arg := range c.Run.Argv {
		if strings.Contains(refPattern.ReplaceAllString(arg, ""), "${steps.") {
			return lastv1alpha1.Command{}, fmt.Errorf("%w: malformed reference in %q", ErrReference, arg)
		}
		var firstErr error
		argv[i] = refPattern.ReplaceAllStringFunc(arg, func(expr string) string {
			v, err := r.Value(expr)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return v
		})
		if firstErr != nil {
			return lastv1alpha1.Command{}, firstErr
		}
	}
	out := c
	out.Run.Argv = argv
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/plan/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/plan
git commit -m "feat(plan): hash plans and resolve step references"
```

---

### Task 2: Layered sandbox policy

**Files:**
- Create: `internal/policy/policy.go`, `internal/policy/policy_test.go`

**Interfaces:**
- Consumes:
  - `sandbox.ParseEgress(s string) (sandbox.EgressRule, error)` and `sandbox.ErrHostEgressUnsupported`;
  - `sandbox.EgressRule{CIDR string; Port int32}`, where CIDR is canonical and IPv4-mapped forms are normalized to IPv4;
  - `lastv1alpha1.OperatorConfig`, `IntentPolicy`, `Command`.
- Produces (package `policy`):
  - `type Effective struct{ AllowScripts bool; /* unexported layers */ }`;
  - `func Resolve(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) (Effective, error)`;
  - `func (e Effective) AllowsProvider(name string) bool`;
  - `func (e Effective) AllowsEgress(r sandbox.EgressRule) bool`;
  - `func (e Effective) CheckCommand(c lastv1alpha1.Command) error`, which returns an error wrapping `ErrDenied`;
  - `var ErrDenied`.
- Semantics (spec §6, plus the M2a ruling that egress bounds compare *parsed* CIDRs):
  - The cluster layer (`OperatorConfig.spec.defaults`) always applies. An unset provider or egress list there allows **nothing**, which is the shipped default.
  - Each IntentPolicy in the namespace adds a layer only for the lists it sets. An unset list inherits, and an empty list allows nothing.
  - A provider is allowed only if every layer lists it.
  - An egress rule is allowed only if, in every layer, some allowed rule covers it: same port, an allowed prefix length no longer than the requested one, and the allowed prefix contains the requested network.
  - `host:port` patterns allow nothing in M2b, because they need Cilium.
  - `allowScripts` is false if any layer sets it false.
  - `image.install` is always denied until M4.

- [ ] **Step 1: Write the failing tests**

`internal/policy/policy_test.go`:
```go
package policy_test

import (
	"errors"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func opConfig(providers []string, egress []string, scripts *bool) *lastv1alpha1.OperatorConfig {
	d := &lastv1alpha1.IntentPolicySpec{Providers: providers, AllowScripts: scripts}
	if egress != nil {
		d.Egress = &lastv1alpha1.EgressPolicy{Allowed: egress}
	}
	return &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Defaults: d}}
}

func rule(t *testing.T, s string) sandbox.EgressRule {
	t.Helper()
	r, err := sandbox.ParseEgress(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestShippedDefaultsAllowNothing(t *testing.T) {
	e, err := policy.Resolve(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.AllowsProvider("any") || e.AllowsEgress(rule(t, "10.0.0.1:443")) || !e.AllowScripts {
		t.Fatalf("nil config: providers/egress must be denied and scripts allowed: %+v", e)
	}
}

func TestProvidersIntersectAcrossLayers(t *testing.T) {
	ip := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Providers: []string{"b", "c"}}}
	e, _ := policy.Resolve(opConfig([]string{"a", "b"}, nil, nil), []lastv1alpha1.IntentPolicy{ip})
	if e.AllowsProvider("a") || !e.AllowsProvider("b") || e.AllowsProvider("c") {
		t.Fatal("providers must be the intersection")
	}
	inherit := lastv1alpha1.IntentPolicy{}
	e, _ = policy.Resolve(opConfig([]string{"a"}, nil, nil), []lastv1alpha1.IntentPolicy{inherit})
	if !e.AllowsProvider("a") {
		t.Fatal("an IntentPolicy without providers must inherit")
	}
	none := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Providers: []string{}}}
	e, _ = policy.Resolve(opConfig([]string{"a"}, nil, nil), []lastv1alpha1.IntentPolicy{none})
	if e.AllowsProvider("a") {
		t.Fatal("an empty provider list must allow nothing")
	}
}

func TestEgressCoverageComparesParsedCIDRs(t *testing.T) {
	ip := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.1.0.0/16:443"}}}}
	e, err := policy.Resolve(opConfig(nil, []string{"10.0.0.0/8:443", "api.example.com:443"}, nil), []lastv1alpha1.IntentPolicy{ip})
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"10.1.2.3:443", "10.1.0.0/24:443", "[::ffff:10.1.2.3]:443"}
	denied := []string{"10.2.0.1:443", "10.1.2.3:80", "10.0.0.0/8:443", "[::ffff:0:0/96]:443", "0.0.0.0/0:443"}
	for _, s := range allowed {
		if !e.AllowsEgress(rule(t, s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
	for _, s := range denied {
		if e.AllowsEgress(rule(t, s)) {
			t.Errorf("%s must be denied", s)
		}
	}
}

func TestMalformedPolicyPatternIsAnError(t *testing.T) {
	if _, err := policy.Resolve(opConfig(nil, []string{"10.0.0.0/8"}, nil), nil); err == nil {
		t.Fatal("a pattern without a port must be rejected")
	}
}

func TestCheckCommand(t *testing.T) {
	no := false
	e, _ := policy.Resolve(opConfig([]string{"p"}, []string{"10.0.0.0/8:443"}, &no), nil)
	ok := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}, Providers: []string{"p"}, Egress: []string{"10.1.1.1:443"}}
	if err := e.CheckCommand(ok); err != nil {
		t.Fatalf("CheckCommand(ok) = %v", err)
	}
	bad := map[string]lastv1alpha1.Command{
		"script":   {Run: lastv1alpha1.CommandRun{Script: "true"}},
		"provider": {Run: ok.Run, Providers: []string{"q"}},
		"egress":   {Run: ok.Run, Egress: []string{"192.168.0.1:443"}},
		"badrule":  {Run: ok.Run, Egress: []string{"nope"}},
		"install":  {Run: ok.Run, Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "x"}}}}},
	}
	for name, c := range bad {
		err := e.CheckCommand(c)
		if !errors.Is(err, policy.ErrDenied) {
			t.Errorf("%s: CheckCommand = %v, want ErrDenied", name, err)
		}
	}
	if err := e.CheckCommand(bad["install"]); !strings.Contains(err.Error(), "image.install") {
		t.Errorf("install denial should name image.install: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/policy/ -count=1`
Expected: FAIL (the package does not exist yet).

- [ ] **Step 3: Implement**

`internal/policy/policy.go`:
```go
// Package policy resolves the layered sandbox policy the gateway enforces (spec §6).
package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// ErrDenied reports a command the effective policy does not allow.
var ErrDenied = errors.New("policy: denied")

// Effective is the resolved policy for one namespace. Each list layer narrows the
// layers above it; a command must satisfy every layer.
type Effective struct {
	AllowScripts   bool
	providerLayers [][]string
	egressLayers   [][]sandbox.EgressRule
}

// Resolve layers OperatorConfig.spec.defaults and every IntentPolicy in the
// namespace. The cluster layer always applies, and an unset list there allows
// nothing (the shipped default); an IntentPolicy only adds the lists it sets.
func Resolve(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) (Effective, error) {
	e := Effective{AllowScripts: true}
	var defaults lastv1alpha1.IntentPolicySpec
	if op != nil && op.Spec.Defaults != nil {
		defaults = *op.Spec.Defaults
	}
	if err := e.addLayer(defaults, true); err != nil {
		return Effective{}, err
	}
	for _, p := range policies {
		if err := e.addLayer(p.Spec, false); err != nil {
			return Effective{}, fmt.Errorf("intentpolicy %s: %w", p.Name, err)
		}
	}
	return e, nil
}

func (e *Effective) addLayer(s lastv1alpha1.IntentPolicySpec, always bool) error {
	if s.Providers != nil || always {
		e.providerLayers = append(e.providerLayers, s.Providers)
	}
	if s.Egress != nil || always {
		var patterns []string
		if s.Egress != nil {
			patterns = s.Egress.Allowed
		}
		rules, err := parseAllowed(patterns)
		if err != nil {
			return err
		}
		e.egressLayers = append(e.egressLayers, rules)
	}
	if s.AllowScripts != nil && !*s.AllowScripts {
		e.AllowScripts = false
	}
	return nil
}

func parseAllowed(patterns []string) ([]sandbox.EgressRule, error) {
	rules := make([]sandbox.EgressRule, 0, len(patterns))
	for _, p := range patterns {
		r, err := sandbox.ParseEgress(p)
		if errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			continue // host patterns need Cilium (spec §11.1); in M2b they allow nothing
		}
		if err != nil {
			return nil, fmt.Errorf("policy: egress pattern %q: %w", p, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// AllowsProvider reports whether every layer lists the provider.
func (e Effective) AllowsProvider(name string) bool {
	for _, layer := range e.providerLayers {
		if !slices.Contains(layer, name) {
			return false
		}
	}
	return true
}

// AllowsEgress reports whether, in every layer, some allowed rule covers r. The
// comparison uses parsed prefixes, never strings, so spellings such as
// ::ffff:0:0/96 (which normalizes to 0.0.0.0/0) cannot slip through.
func (e Effective) AllowsEgress(r sandbox.EgressRule) bool {
	for _, layer := range e.egressLayers {
		if !slices.ContainsFunc(layer, func(a sandbox.EgressRule) bool { return covers(a, r) }) {
			return false
		}
	}
	return true
}

func covers(allowed, r sandbox.EgressRule) bool {
	pa, errA := netip.ParsePrefix(allowed.CIDR)
	pr, errR := netip.ParsePrefix(r.CIDR)
	return errA == nil && errR == nil && allowed.Port == r.Port &&
		pa.Bits() <= pr.Bits() && pa.Contains(pr.Addr())
}

// CheckCommand returns an ErrDenied error when c uses anything the policy forbids.
func (e Effective) CheckCommand(c lastv1alpha1.Command) error {
	if c.Image != nil {
		return fmt.Errorf("%w: image.install commands are not supported until image builds (M4)", ErrDenied)
	}
	if c.Run.Script != "" && !e.AllowScripts {
		return fmt.Errorf("%w: scripts are not allowed by policy", ErrDenied)
	}
	for _, p := range c.Providers {
		if !e.AllowsProvider(p) {
			return fmt.Errorf("%w: provider %q is not allowed", ErrDenied, p)
		}
	}
	for _, s := range c.Egress {
		r, err := sandbox.ParseEgress(s)
		if err != nil {
			return fmt.Errorf("%w: egress %q: %v", ErrDenied, s, err)
		}
		if !e.AllowsEgress(r) {
			return fmt.Errorf("%w: egress %s is not allowed", ErrDenied, r)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/policy/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/policy
git commit -m "feat(policy): resolve layered provider, egress and script policy"
```

---

### Task 3: Bind Kubernetes Provider tokens to a per-run anchor

The user chose this option. Today a minted token stays valid until it expires, which can be hours after cleanup. Binding the token to an anchor Secret in the provider namespace (`last-gateway`) changes that. The API server rejects a bound token as soon as its object is gone, so deleting the anchor during cleanup revokes the token immediately. `TokenRequest.boundObjectRef` must name an object in the ServiceAccount's own namespace, which is why the anchor lives in `last-gateway` and not in `last-sandbox`.

**Files:**
- Modify: `internal/sandbox/credentials.go`, `internal/sandbox/executor.go`, `internal/sandbox/reaper.go`
- Test: `internal/sandbox/credentials_test.go`, `internal/sandbox/executor_test.go`, `internal/sandbox/reaper_test.go`

**Interfaces:**
- Consumes: the existing `Materialize`, `CredentialConfig`, `launch`, `deleteRun`, `ReapOrphans`, `RunLabels`, `stampExpiry`, plus the `expires` value that `launch` already computes (M2a F2).
- Produces:
  - A new field `CredentialConfig.ExpiresAt time.Time`. Zero means no expiry label.
  - A new constant `AnchorSuffix = "-anchor"`. The anchor Secret is named `last-run-<runID>-anchor`, lives in `cfg.ProviderNamespace`, carries `RunLabels(runID)` plus the expiry label, and is created by `Materialize` once per run before the first kubernetes token is minted.
  - Each `TokenRequest` carries `BoundObjectRef{Kind: "Secret", APIVersion: "v1", Name: <anchor>, UID: <anchor UID>}`. `Materialize` returns an error if the created anchor has no UID; a real API server always sets one.
  - `deleteRun` also deletes Secrets that match the selector in `e.cfg.ProviderNamespace`, when it differs from the sandbox namespace.
  - `ReapOrphans` also records and deletes Secrets in the provider namespace.

- [ ] **Step 1: Write the failing tests**

Add to `internal/sandbox/credentials_test.go`. The existing helpers are `k8sProvider`, `providers`, `gatewayNS`, `credCfg`, `runID`, `tokenSubresource` and `clusterName`; read the file first, and if a helper is named differently, use the existing name.
```go
// anchorClient is a fake clientset that assigns UIDs to created Secrets (a real
// API server does) and records every TokenRequest.
func anchorClient(t *testing.T) (*fake.Clientset, *[]*authv1.TokenRequest) {
	t.Helper()
	kube := fake.NewClientset()
	var reqs []*authv1.TokenRequest
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		s.UID = types.UID("uid-" + s.Name)
		return false, nil, nil
	})
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		reqs = append(reqs, tr.DeepCopy())
		tr.Status.Token = "tok-" + strings.Repeat("x", 16)
		return true, tr, nil
	})
	return kube, &reqs
}

func TestMaterializeBindsTheTokenToARunAnchor(t *testing.T) {
	ctx := context.Background()
	kube, reqs := anchorClient(t)
	plans, err := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	if err != nil {
		t.Fatal(err)
	}
	cfg := credCfg
	cfg.ExpiresAt = time.Unix(2000000000, 0)
	if _, err := sandbox.Materialize(ctx, kube, cfg, runID, plans); err != nil {
		t.Fatal(err)
	}
	anchor, err := kube.CoreV1().Secrets(gatewayNS).Get(ctx, "last-run-"+runID+sandbox.AnchorSuffix, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("anchor secret: %v", err)
	}
	if anchor.Labels[sandbox.LabelRun] != runID || anchor.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue ||
		anchor.Labels[sandbox.LabelExpiresAt] != "2000000000" {
		t.Fatalf("anchor labels = %v", anchor.Labels)
	}
	if len(*reqs) != 1 {
		t.Fatalf("token requests = %d", len(*reqs))
	}
	ref := (*reqs)[0].Spec.BoundObjectRef
	if ref == nil || ref.Kind != "Secret" || ref.APIVersion != "v1" || ref.Name != anchor.Name || ref.UID != anchor.UID {
		t.Fatalf("bound object ref = %+v, want the anchor %s/%s", ref, anchor.Name, anchor.UID)
	}
}

func TestMaterializeCreatesNoAnchorWithoutAKubernetesProvider(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: demoCreds, Namespace: gatewayNS},
		Data: map[string][]byte{tokenKey: []byte(secretValue), configKey: []byte("cfg-" + secretValue)}})
	plans, _ := sandbox.PlanCredentials(ctx, providers(envProvider()), gatewayNS, []string{demoProvider})
	if _, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Secrets(gatewayNS).Get(ctx, "last-run-"+runID+sandbox.AnchorSuffix, metav1.GetOptions{}); err == nil {
		t.Fatal("an env-secret run must not create an anchor")
	}
}

func TestMaterializeRefusesAnAnchorWithoutUID(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset() // the plain fake does not assign UIDs
	plans, _ := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	if _, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans); err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("Materialize = %v, want an anchor uid error", err)
	}
}
```
Add these imports if they are missing: `k8s.io/apimachinery/pkg/types`, `time`, `strings`.

Add to `internal/sandbox/executor_test.go`. It uses the file's own `fakeCluster`, `apiServerObjects`, `newExec`, `newAudit`, `envelopeLogs`, `cmd`, `clusterName`, `testActor` and `tokenSubresource` helpers. Every existing kubernetes-provider test in this file must call `withSecretUIDs(kube)` after `fakeCluster`, because `Materialize` now refuses an anchor that has no UID:
```go
// withSecretUIDs makes the fake clientset assign UIDs to created Secrets, as a real
// API server does; token anchors need one for boundObjectRef.
func withSecretUIDs(kube *fake.Clientset) {
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		s.UID = types.UID("uid-" + s.Name)
		return false, nil, nil
	})
}

func TestRunDeletesTheTokenAnchorOnCleanup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if tr.Spec.BoundObjectRef == nil || tr.Spec.BoundObjectRef.Name != "last-run-"+runID+sandbox.AnchorSuffix {
			t.Errorf("token request is not bound to the run anchor: %+v", tr.Spec.BoundObjectRef)
		}
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, _ := newAudit(t)
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{clusterName}, nil)}); err != nil {
		t.Fatal(err)
	}
	left, err := kube.CoreV1().Secrets("last-gateway").List(context.Background(),
		metav1.ListOptions{LabelSelector: sandbox.LabelRun + "=" + runID})
	if err != nil || len(left.Items) != 0 {
		t.Fatalf("anchors left in last-gateway: %v (err %v)", left, err)
	}
	deleted := false
	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "secrets" && a.GetNamespace() == "last-gateway" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("cleanup never deleted the anchor in last-gateway")
	}
}
```
Add the `k8s.io/apimachinery/pkg/types` import if it is missing.

Add to `internal/sandbox/reaper_test.go`:
```go
func TestReapOrphansDeletesAnOrphanedAnchor(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	meta := runMeta("last-run-o-anchor", "o", old)
	meta.Namespace = gatewayNS
	kube, _ := fakeCluster(corev1.PodSucceeded, &corev1.Secret{ObjectMeta: meta})
	aud, _ := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Minute, now)
	if err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v, want 1", n, err)
	}
	if _, err := kube.CoreV1().Secrets(gatewayNS).Get(context.Background(), "last-run-o-anchor", metav1.GetOptions{}); err == nil {
		t.Fatal("orphaned anchor survived")
	}
}
```
`runMeta` sets `Namespace: sandboxNS`, so the test overrides it to `gatewayNS`. `newExec` must configure `ProviderNamespace: gatewayNS`; check it does.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run 'Anchor'`
Expected: FAIL. The build fails with `undefined: sandbox.AnchorSuffix` and `cfg.ExpiresAt undefined`.

- [ ] **Step 3: Implement**

In `internal/sandbox/credentials.go`:
1. Add `ExpiresAt time.Time` to `CredentialConfig`, with this doc comment: `ExpiresAt stamps the run's expiry on the token anchor; zero means no expiry label.`
2. Add the constant:
```go
// AnchorSuffix names the per-run Secret in the provider namespace that kubernetes
// tokens are bound to; deleting it revokes them (TokenRequest boundObjectRef).
const AnchorSuffix = "-anchor"
```
3. In `Materialize`, declare `var anchor *corev1.Secret` before the provider loop. Then, in the `ProviderKubernetes` case, right before `CreateToken`, add:
```go
			if anchor == nil {
				meta := metav1.ObjectMeta{Name: "last-run-" + runID + AnchorSuffix, Namespace: cfg.ProviderNamespace, Labels: RunLabels(runID)}
				if !cfg.ExpiresAt.IsZero() {
					stampExpiry(&meta, cfg.ExpiresAt)
				}
				created, err := kube.CoreV1().Secrets(cfg.ProviderNamespace).Create(ctx,
					&corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque}, metav1.CreateOptions{})
				if err != nil {
					return Materialized{}, fmt.Errorf("sandbox: create token anchor: %w", err)
				}
				if created.UID == "" {
					return Materialized{}, fmt.Errorf("sandbox: token anchor %s has no uid", created.Name)
				}
				anchor = created
			}
```
Then change the TokenRequest spec to:
```go
&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &secs,
	BoundObjectRef: &authv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: anchor.Name, UID: anchor.UID}}}
```
Update the `Materialize` doc comment. It now creates exactly one object, the anchor in the provider namespace, and it is called only after `credential.issued` is durable. The anchor carries the run labels, so cleanup and the reaper remove it even when `Materialize` fails later.

4. In `internal/sandbox/executor.go`, `launch` computes `expires` before calling `Materialize` (M2a F2). Set `p.credCfg.ExpiresAt = expires` before the `Materialize` call. If `expires` is computed after `Materialize` today, move its computation above that call. It depends only on `time.Now()`, the deadline and `StartupGrace`.

5. In `deleteRun`, after the sandbox-namespace Secret loop, add:
```go
	if pns := e.cfg.ProviderNamespace; pns != "" && pns != ns {
		if anchors, err := e.kube.CoreV1().Secrets(pns).List(ctx, opts); err != nil {
			errs = append(errs, err)
		} else {
			for _, s := range anchors.Items {
				errs = append(errs, ignore(e.kube.CoreV1().Secrets(pns).Delete(ctx, s.Name, metav1.DeleteOptions{})))
			}
		}
	}
```
Also update the `deleteRun` doc comment: it deletes run objects in the sandbox namespace plus token anchors in the provider namespace.

6. In `ReapOrphans`, after recording the sandbox Secrets, record the provider-namespace Secrets the same way, under the same condition (`pns != "" && pns != ns`). A list error returns `fmt.Errorf("sandbox: list anchors: %w", err)`. No other reaper change is needed, because reaping a run calls `cleanup`, which goes through `deleteRun`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/sandbox/... -count=1`
Expected: PASS, including every existing test. Existing kubernetes-provider tests that use a plain fake clientset now fail with the anchor-UID error. Give them the UID reactor from `anchorClient`, or an equivalent `create secrets` reactor. Do not weaken the UID check.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/sandbox
git commit -m "feat(sandbox): bind kubernetes provider tokens to a per-run anchor secret"
```

---

### Task 4: API and audit additions: scripted LLM provider and `output.written`

**Files:**
- Modify:
  - `api/v1alpha1/llmprovider_types.go`;
  - `api/v1alpha1/config_validation_test.go`;
  - `internal/audit/entry.go`;
  - `config/samples/last_v1alpha1_llmprovider.yaml` (only if it no longer validates);
  - `docs/superpowers/specs/2026-09-26-last-operator-design.md`.
- Regenerate: `make manifests generate helm-crds`.

**Interfaces:**
- Produces:
  - **New provider type.** `LLMScripted LLMProviderType = "scripted"` is added to the enum.
  - **`SecretRef` becomes optional.** It changes to `*LocalRef`, and a CEL rule keeps it required unless `type == 'scripted'`.
  - **New `Scripted` field.** `LLMProviderSpec.Scripted *ScriptedLLM`, where `type ScriptedLLM struct{ ConfigMapRef LocalRef \`json:"configMapRef"\` }`. A CEL rule requires it when `type == 'scripted'`.
  - **New audit type.** `audit.TypeOutputWritten = "output.written"`.

- [ ] **Step 1: Write the failing tests**

Append to `api/v1alpha1/config_validation_test.go`, and change the existing `SecretRef: lastv1alpha1.LocalRef{Name: "key"}` literal to `SecretRef: &lastv1alpha1.LocalRef{Name: "key"}`:
```go
func TestLLMProviderScriptedNeedsConfigMapNotSecret(t *testing.T) {
	ok := &lastv1alpha1.LLMProvider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "fixture",
		Scripted: &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: "llm-script"}},
	}}
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatalf("create scripted: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })

	missing := &lastv1alpha1.LLMProvider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "fixture"}}
	expectInvalid(t, k8sClient.Create(ctx, missing), "scripted is required when type is scripted")

	noSecret := &lastv1alpha1.LLMProvider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMAnthropic, Model: "claude"}}
	expectInvalid(t, k8sClient.Create(ctx, noSecret), "secretRef is required unless type is scripted")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `make test 2>&1 | grep -E 'FAIL|undefined' | head`
Expected: the build fails with `undefined: lastv1alpha1.LLMScripted`.

- [ ] **Step 3: Implement**

In `api/v1alpha1/llmprovider_types.go`:
```go
// LLMProviderType selects a gateway LLM adapter.
// +kubebuilder:validation:Enum=anthropic;openai;openai-compatible;scripted
type LLMProviderType string

const (
	LLMAnthropic        LLMProviderType = "anthropic"
	LLMOpenAI           LLMProviderType = "openai"
	LLMOpenAICompatible LLMProviderType = "openai-compatible"
	// LLMScripted replays fixed responses from a ConfigMap; for tests and demos (spec §18).
	LLMScripted LLMProviderType = "scripted"
)

// ScriptedLLM points at a ConfigMap in last-gateway. Each key is an Intent
// ("<namespace>.<name>") and each value a JSON array of response strings; the
// n-th response answers a conversation that already holds n assistant messages.
type ScriptedLLM struct {
	ConfigMapRef LocalRef `json:"configMapRef"`
}

// LLMProviderSpec configures one model endpoint.
// +kubebuilder:validation:XValidation:rule="self.type != 'openai-compatible' || has(self.endpoint)",message="endpoint is required for openai-compatible"
// +kubebuilder:validation:XValidation:rule="self.type == 'scripted' || has(self.secretRef)",message="secretRef is required unless type is scripted"
// +kubebuilder:validation:XValidation:rule="self.type != 'scripted' || has(self.scripted)",message="scripted is required when type is scripted"
type LLMProviderSpec struct {
	Type LLMProviderType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef names a Secret in last-gateway holding the API key.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// +optional
	Scripted *ScriptedLLM `json:"scripted,omitempty"`
}
```
In `internal/audit/entry.go`, add the entry type `TypeOutputWritten = "output.written"` to the const block, after `TypePolicyDenied`. Give it this comment: `// output Secret written; values recorded as SHA-256 (spec §15)`.

Then run `make manifests generate helm-crds`. If `config/samples/last_v1alpha1_llmprovider.yaml` sets `secretRef`, it stays valid as it is.

Spec edits in `docs/superpowers/specs/2026-09-26-last-operator-design.md`:
- **§5.6 `LLMProvider` example.** Change the type line to `type: anthropic | openai | openai-compatible | scripted`. Under it, add:

  ```yaml
  scripted: { configMapRef: { name: llm-script } }   # type scripted only: fixed responses for tests and demos
  ```

- **§5.6 `OperatorConfig`.** After the `baseImage` line's comment, add: "When `sandbox.baseImage` is unset, the gateway falls back to the Helm value `sandbox.image` (development and e2e installs only; production pins the image by digest here)."
- **§14.2 entry types.** Add `output.written` after `policy.denied`, and add this sentence: "`output.written` records a WriteOutputSecret with each value as its SHA-256."
- **§8.3.** At the end of the paragraph "**The gateway takes the snapshot from its own records.**", add: "In v1 the gateway keeps that record (the open phase and the last executed plan per Intent) in a gateway-owned ConfigMap in `last-gateway`, written only after the matching `phase.begin`/`phase.end`. The operator has no access to it, and a gateway restart closes an open phase as `interrupted` rather than resuming it."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `make test`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add api config/crd charts/last-operator/crds internal/audit/entry.go docs/superpowers/specs config/samples
git commit -m "feat(api): add the scripted LLM provider type and the output.written entry"
```

---

### Task 5: Lifecycle RPCs in the gRPC API

**Files:**
- Modify: `proto/last/gateway/v1/gateway.proto`
- Regenerate: `internal/gatewayapi/gatewayv1/*` via `make proto`

**Interfaces:**
- Produces the messages and RPCs used by Tasks 8–12 and 15. Commands travel as JSON (`command_json`), the same JSON as `lastv1alpha1.Command`, so the proto does not duplicate the CRD schema. Values for output Secrets are reference expressions (`${steps.<id>.json.<path>}`), never literal values.

- [ ] **Step 1: Write the proto**

Replace the service block and append the messages in `proto/last/gateway/v1/gateway.proto`. Keep `PingRequest` and `PingResponse` as they are.
```proto
// GatewayService is the trusted gateway's API. Only the operator's ServiceAccount
// may call it (spec §4.2). Every side effect is audited before it happens.
service GatewayService {
  // Ping reports the authenticated caller and whether the audit log is writable.
  rpc Ping(PingRequest) returns (PingResponse);
  // BeginPhase opens an approved phase for the Intent's current plan (spec §8.3).
  rpc BeginPhase(BeginPhaseRequest) returns (BeginPhaseResponse);
  // EndPhase closes the open phase and records its outcome.
  rpc EndPhase(EndPhaseRequest) returns (EndPhaseResponse);
  // RunCommand runs the next approved step of the open phase in a sandbox.
  rpc RunCommand(RunCommandRequest) returns (RunCommandResponse);
  // RunProbes runs the probes of the Intent's last executed plan (spec §10).
  rpc RunProbes(RunProbesRequest) returns (RunProbesResponse);
  // WriteOutputSecret writes step outputs to a ManagedResource's output Secret (spec §15).
  rpc WriteOutputSecret(WriteOutputSecretRequest) returns (WriteOutputSecretResponse);
  // CallLLM sends a conversation to the configured LLM provider (spec §12).
  rpc CallLLM(CallLLMRequest) returns (CallLLMResponse);
}

// IntentRef names an Intent.
message IntentRef {
  string namespace = 1;
  string name = 2;
}

// BeginPhaseRequest names the approval subject to open a phase for.
message BeginPhaseRequest {
  IntentRef intent = 1;
  // Stage, e.g. "Create".
  string stage = 2;
  // The plan hash ("sha256:<hex>").
  string subject_hash = 3;
}

// BeginPhaseResponse identifies the opened phase.
message BeginPhaseResponse {
  // Sequence number of the phase.begin audit entry.
  uint64 begin_seq = 1;
  // Number of plan steps the phase must run.
  int32 steps = 2;
}

// EndPhaseRequest closes the open phase.
message EndPhaseRequest {
  IntentRef intent = 1;
  string subject_hash = 2;
  // True only when every step ran and exited 0.
  bool succeeded = 3;
  // Why the phase failed, shown in the audit log.
  string reason = 4;
}

// EndPhaseResponse reports the phase.end entry.
message EndPhaseResponse {
  uint64 end_seq = 1;
}

// RunCommandRequest is the operator's copy of the next step, with ${steps…} resolved.
message RunCommandRequest {
  IntentRef intent = 1;
  string subject_hash = 2;
  string step_id = 3;
  // JSON of the resolved lastv1alpha1.Command; it must equal the gateway's own resolution.
  bytes command_json = 4;
}

// CommandResult is one sandboxed command's outcome.
message CommandResult {
  string run_id = 1;
  int32 exit_code = 2;
  string stdout = 3;
  string stderr = 4;
  bool stdout_truncated = 5;
  bool stderr_truncated = 6;
  // Set when the command could not run or did not finish.
  string error = 7;
}

// RunCommandResponse carries the step's result.
message RunCommandResponse {
  CommandResult result = 1;
}

// RunProbesRequest names the Intent whose probes to run.
message RunProbesRequest {
  IntentRef intent = 1;
}

// ProbeOutcome is one probe's result and assertion outcome.
message ProbeOutcome {
  string id = 1;
  bool passed = 2;
  CommandResult result = 3;
  string error = 4;
}

// RunProbesResponse lists outcomes in plan order.
message RunProbesResponse {
  // Hash of the executed plan the probes came from.
  string plan_hash = 1;
  repeated ProbeOutcome outcomes = 2;
}

// WriteOutputSecretRequest writes step outputs into a ManagedResource's output Secret.
message WriteOutputSecretRequest {
  IntentRef intent = 1;
  string subject_hash = 2;
  // Name of the ManagedResource (same namespace as the Intent).
  string managed_resource = 3;
  // Secret key -> step reference such as "${steps.s1.json.password}".
  map<string, string> values = 4;
}

// WriteOutputSecretResponse names the Secret written.
message WriteOutputSecretResponse {
  string secret_name = 1;
}

// Message is one turn of an LLM conversation.
message Message {
  // "system", "user", or "assistant".
  string role = 1;
  string content = 2;
}

// CallLLMRequest is a provider-neutral LLM request.
message CallLLMRequest {
  IntentRef intent = 1;
  repeated Message messages = 2;
  int32 max_tokens = 3;
}

// CallLLMResponse is the model's reply.
message CallLLMResponse {
  string content = 1;
  int32 input_tokens = 2;
  int32 output_tokens = 3;
}
```

- [ ] **Step 2: Regenerate and build**

Run: `make proto && go build ./...`
Expected: `buf lint` passes, the code generates, and the build succeeds. `rpcServer` embeds `UnimplementedGatewayServiceServer`, so new methods return `Unimplemented` until Tasks 8–12.

- [ ] **Step 3: Check the generated code is reproducible**

Run: `make proto && git status --porcelain internal/gatewayapi`
Expected: the second run produces no further diff.

- [ ] **Step 4: Commit**

```bash
make lint
git add proto internal/gatewayapi
git commit -m "feat(gateway): define the lifecycle RPCs"
```

---

### Task 6: Gateway runtime — CRD client, executor, canaries, drain, reap on lead

**Files:**
- Modify: `internal/gateway/config.go`, `internal/gateway/config_test.go`, `internal/gateway/server.go`, `internal/gateway/server_test.go`, `cmd/gateway/main.go`
- Create: `internal/gateway/runtime.go`, `internal/gateway/runtime_test.go`

**Interfaces:**
- Consumes:
  - `sandbox.NewExecutor`, `ExecutorConfig`, `PodConfig`, `NewScheduler`, `KubeLogReader`, `LogReader`, `ProviderGetter`;
  - `(*Executor).ReapOrphans(ctx, margin, now)`;
  - controller-runtime `client.Reader` / `client.Client`;
  - `lastv1alpha1.AddToScheme`.
- Produces:
  - **Config fields:**
    - `SandboxNamespace` (`LAST_SANDBOX_NAMESPACE`, default `last-sandbox`);
    - `SandboxImage` (`LAST_SANDBOX_IMAGE`, the fallback image);
    - `EgressCanaries []string` (`LAST_EGRESS_CANARIES`, required). Each entry is a comma-separated spec in one of three forms: `IP:port`, `service/<namespace>/<name>:<port>`, or `node:<port>`.
    - `DrainTimeout` (`LAST_DRAIN_TIMEOUT`, default `5m`).
  - **`New` signature:** `New(cfg Config, kube kubernetes.Interface, crd client.Reader, sink audit.Sink, signer *audit.Signer) *Gateway`.
  - **Unexported helpers used by Tasks 8–12:**
    - `(*Gateway).beginRPC() (*audit.Writer, func(), error)`;
    - `(*Gateway).drain(timeout) bool`;
    - `(*Gateway).executor(w *audit.Writer, image string) *sandbox.Executor`;
    - `(*Gateway).sandboxImage(ctx) (string, error)`;
    - `(*Gateway).operatorConfig(ctx) (*lastv1alpha1.OperatorConfig, error)`;
    - `(*Gateway).resolveCanaries(ctx) ([]string, error)`;
    - `keyedMutex` (`lock(key) func()`).
  - **Gateway fields:** `crd`, `epoch`, `sched atomic.Pointer[sandbox.Scheduler]`, `logs`, `execCfg`, `locks`, and the drain state.
  - **`lead` changes.** At lead start, `lead` does the following before it publishes the writer:
    1. resolves the canaries;
    2. sizes the scheduler from `OperatorConfig.spec.sandbox.maxConcurrent` (default 20);
    3. reaps every orphaned run (`ReapOrphans(ctx, 0, now)`).

    Any failure ends leadership (fail closed). When `lead` ends, it drains in-flight RPCs, up to `DrainTimeout`, before it clears the writer.

- [ ] **Step 1: Write the failing tests**

In `internal/gateway/config_test.go`, add `"LAST_EGRESS_CANARIES": "10.96.0.10:8080"` to the base env map used by every existing test. Find the map literal the tests share and add the key there. Then add:
```go
func TestConfigFromEnvSandboxSettings(t *testing.T) {
	cfg, err := ConfigFromEnv(envWith(map[string]string{
		"LAST_EGRESS_CANARIES": " 10.96.0.10:8080 , service/last-gateway/last-gateway:8080,node:10250 ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxNamespace != "last-sandbox" || cfg.DrainTimeout != 5*time.Minute {
		t.Fatalf("defaults = %q %v", cfg.SandboxNamespace, cfg.DrainTimeout)
	}
	want := []string{"10.96.0.10:8080", "service/last-gateway/last-gateway:8080", "node:10250"}
	if strings.Join(cfg.EgressCanaries, ",") != strings.Join(want, ",") {
		t.Fatalf("canaries = %q", cfg.EgressCanaries)
	}
	for _, bad := range []string{"", "  ", "example.com:80", "service/x:80", "node:0", "10.0.0.1:99999", "pod/a/b:1"} {
		if _, err := ConfigFromEnv(envWith(map[string]string{"LAST_EGRESS_CANARIES": bad})); err == nil {
			t.Errorf("LAST_EGRESS_CANARIES=%q was accepted", bad)
		}
	}
}
```
If `config_test.go` has no `envWith(overrides) func(string) string` helper yet, add one. It must merge `overrides` onto the shared base env and return a getenv function. Model it on how the existing tests build their getenv.

`internal/gateway/runtime_test.go`:
```go
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := lastv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func openWriter(t *testing.T, sink audit.Sink) *audit.Writer {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	w, err := audit.Open(context.Background(), sink, audit.NewSigner(key),
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: "gateway"}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestBeginRPCNeedsAWritableAuditLog(t *testing.T) {
	g := testGateway(t, audittest.NewMemSink())
	if _, _, err := g.beginRPC(); status.Code(err) != codes.Unavailable {
		t.Fatalf("without a writer: %v, want Unavailable", err)
	}
	g.writer.Store(openWriter(t, audittest.NewMemSink()))
	w, release, err := g.beginRPC()
	if err != nil || w == nil {
		t.Fatalf("with a writer: %v", err)
	}
	release()
}

func TestDrainWaitsForInflightRPCs(t *testing.T) {
	g := testGateway(t, audittest.NewMemSink())
	g.writer.Store(openWriter(t, audittest.NewMemSink()))
	_, release, err := g.beginRPC()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan bool)
	go func() { drained <- g.drain(5 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drain returned while an RPC was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if _, _, err := g.beginRPC(); status.Code(err) != codes.Unavailable {
		t.Fatalf("a new RPC was admitted while draining: %v", err)
	}
	release()
	if ok := <-drained; !ok {
		t.Fatal("drain reported a timeout")
	}
	g2 := testGateway(t, audittest.NewMemSink())
	g2.writer.Store(openWriter(t, audittest.NewMemSink()))
	if _, _, err := g2.beginRPC(); err != nil {
		t.Fatal(err)
	}
	if g2.drain(20 * time.Millisecond) {
		t.Fatal("drain must report a timeout when an RPC never finishes")
	}
}

func TestSandboxImagePrefersOperatorConfig(t *testing.T) {
	pinned := "ghcr.io/moogie-labs/last-sandbox-base@sha256:" + strings.Repeat("a", 64)
	g := testGatewayWith(t, &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: lastv1alpha1.OperatorConfigSpec{Sandbox: &lastv1alpha1.SandboxConfig{BaseImage: pinned}}})
	if img, err := g.sandboxImage(context.Background()); err != nil || img != pinned {
		t.Fatalf("image = %q, %v", img, err)
	}
	g = testGatewayWith(t)
	g.cfg.SandboxImage = "sandbox:dev"
	if img, err := g.sandboxImage(context.Background()); err != nil || img != "sandbox:dev" {
		t.Fatalf("fallback image = %q, %v", img, err)
	}
	g.cfg.SandboxImage = ""
	if _, err := g.sandboxImage(context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no image: %v, want FailedPrecondition", err)
	}
}

func TestResolveCanaries(t *testing.T) {
	g := testGatewayWith(t)
	g.kube = fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "last-gateway", Namespace: "last-gateway"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.7.7"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "n1"}, {Type: corev1.NodeInternalIP, Address: "172.18.0.2"}}}},
	)
	g.cfg.EgressCanaries = []string{"10.1.1.1:80", "service/last-gateway/last-gateway:8080", "node:10250"}
	got, err := g.resolveCanaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "10.1.1.1:80,10.96.7.7:8080,172.18.0.2:10250" {
		t.Fatalf("canaries = %q", got)
	}
	g.cfg.EgressCanaries = []string{"service/last-gateway/missing:8080"}
	if _, err := g.resolveCanaries(context.Background()); err == nil {
		t.Fatal("a missing service must be an error")
	}
}

func TestLeadReapsOrphanedRunsBeforeServing(t *testing.T) {
	sink := audittest.NewMemSink()
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 365 * 24 * time.Hour})
	g := testGateway(t, sink)
	orphan := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "last-run-o", Namespace: "last-sandbox",
		Labels: sandbox.RunLabels("o"), CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}}
	g.kube = fake.NewClientset(orphan)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for g.writer.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g.writer.Load() == nil {
		cancel()
		t.Fatalf("lead never published a writer: %v", <-done)
	}
	if _, err := g.kube.CoreV1().Pods("last-sandbox").Get(ctx, "last-run-o", metav1.GetOptions{}); err == nil {
		t.Fatal("orphaned sandbox pod survived lead start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead = %v", err)
	}
}
```

In `internal/gateway/server_test.go`, update `testGateway`. Add `testGatewayWith`, and add the constant `testOperator` if the file does not already define it:
```go
const testOperator = "system:serviceaccount:last-system:last-operator"

func testGateway(t *testing.T, sink audit.Sink, objs ...client.Object) *Gateway {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Namespace: ns, PodName: "gw-0", ChainID: testChainID, Retention: 365 * 24 * time.Hour, HTTPAddr: ":0",
		LeaseName: "l", SandboxNamespace: "last-sandbox", EgressCanaries: []string{"10.96.0.10:8080"},
		OperatorIdentity: testOperator, DrainTimeout: time.Second}
	crd := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return New(cfg, fake.NewClientset(), crd, sink, audit.NewSigner(key))
}

func testGatewayWith(t *testing.T, objs ...client.Object) *Gateway {
	t.Helper()
	return testGateway(t, audittest.NewMemSink(), objs...)
}
```
Keep the existing `ns` constant. `testGateway`'s existing callers, which pass `(t, sink)`, still compile because `objs` is variadic.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1`
Expected: FAIL to compile. The errors are `undefined: g.beginRPC`, `too many arguments in call to New`, and `cfg.SandboxNamespace undefined`.

- [ ] **Step 3: Implement the config**

In `internal/gateway/config.go`, add these fields to `Config` after `OperatorIdentity`:
```go
	// SandboxNamespace (LAST_SANDBOX_NAMESPACE, default last-sandbox) holds sandbox pods.
	SandboxNamespace string
	// SandboxImage (LAST_SANDBOX_IMAGE) is used only when OperatorConfig
	// spec.sandbox.baseImage is unset; production pins the image by digest there.
	SandboxImage string
	// EgressCanaries (LAST_EGRESS_CANARIES, required) are comma-separated canary
	// specs: "IP:port", "service/<namespace>/<name>:<port>" (the Service's
	// ClusterIP), or "node:<port>" (a node InternalIP). Every sandbox waits until
	// all of them are observed denied before running anything (spec §11.1).
	EgressCanaries []string
	// DrainTimeout (LAST_DRAIN_TIMEOUT, default 5m) bounds the wait for in-flight
	// RPCs when this replica stops leading.
	DrainTimeout time.Duration
```
In `ConfigFromEnv`, set `SandboxNamespace: or("LAST_SANDBOX_NAMESPACE", "last-sandbox")` and `SandboxImage: strings.TrimSpace(getenv("LAST_SANDBOX_IMAGE"))` in the literal. After the retention parse, add:
```go
	cfg.DrainTimeout, err = time.ParseDuration(or("LAST_DRAIN_TIMEOUT", "5m"))
	if err != nil || cfg.DrainTimeout <= 0 {
		return Config{}, fmt.Errorf("gateway: LAST_DRAIN_TIMEOUT must be a positive duration: %v", err)
	}
	for _, spec := range strings.Split(getenv("LAST_EGRESS_CANARIES"), ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		if err := validateCanarySpec(spec); err != nil {
			return Config{}, fmt.Errorf("gateway: LAST_EGRESS_CANARIES: %w", err)
		}
		cfg.EgressCanaries = append(cfg.EgressCanaries, spec)
	}
	if len(cfg.EgressCanaries) == 0 {
		return Config{}, errors.New("gateway: LAST_EGRESS_CANARIES is required (the sandbox egress gate needs at least one canary)")
	}
```
and add the validator:
```go
// validateCanarySpec accepts "IP:port", "service/<ns>/<name>:<port>", or "node:<port>".
func validateCanarySpec(spec string) error {
	head, portStr, ok := strings.Cut(spec, ":")
	if strings.Count(spec, ":") > 1 || strings.HasPrefix(spec, "[") {
		head, portStr, ok = "", "", false
		if ap, err := netip.ParseAddrPort(spec); err == nil && ap.Port() != 0 {
			return nil
		}
	}
	if !ok {
		return fmt.Errorf("canary %q: want IP:port, service/<ns>/<name>:<port>, or node:<port>", spec)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("canary %q: bad port", spec)
	}
	switch {
	case head == "node":
		return nil
	case strings.HasPrefix(head, "service/"):
		if parts := strings.Split(head, "/"); len(parts) == 3 && parts[1] != "" && parts[2] != "" {
			return nil
		}
		return fmt.Errorf("canary %q: want service/<namespace>/<name>:<port>", spec)
	default:
		if _, err := netip.ParseAddr(head); err != nil {
			return fmt.Errorf("canary %q: %q is not an IP address", spec, head)
		}
		return nil
	}
}
```
Add the `errors` and `net/netip` imports to `config.go`.

- [ ] **Step 4: Implement the runtime**

`internal/gateway/runtime.go`:
```go
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// defaultMaxConcurrent is the sandbox cap when OperatorConfig does not set one (spec §6).
const defaultMaxConcurrent = 20

func newEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// beginRPC admits a side-effecting RPC: it returns the audit writer and a release
// func, or Unavailable when this replica cannot write the audit log or is draining.
func (g *Gateway) beginRPC() (*audit.Writer, func(), error) {
	g.rpcMu.Lock()
	defer g.rpcMu.Unlock()
	if g.draining {
		return nil, nil, status.Error(codes.Unavailable, "gateway is stepping down")
	}
	w := g.writer.Load()
	if w == nil || w.Broken() {
		return nil, nil, status.Error(codes.Unavailable, "audit log is not writable on this replica")
	}
	g.inflight.Add(1)
	return w, g.inflight.Done, nil
}

// drain refuses new side-effecting RPCs and waits up to timeout for in-flight ones,
// so their outcome entries are written before the writer is released.
func (g *Gateway) drain(timeout time.Duration) bool {
	g.rpcMu.Lock()
	g.draining = true
	g.rpcMu.Unlock()
	done := make(chan struct{})
	go func() { g.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		slog.Warn("Could not drain in-flight RPCs before stepping down", "timeout", timeout)
		return false
	}
}

func (g *Gateway) providerGetter(ctx context.Context, name string) (*lastv1alpha1.Provider, error) {
	var p lastv1alpha1.Provider
	if err := g.crd.Get(ctx, client.ObjectKey{Name: name}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// executor builds a sandbox executor for one RPC; every executor shares the scheduler.
func (g *Gateway) executor(w *audit.Writer, image string) *sandbox.Executor {
	cfg := g.execCfg
	cfg.Pod.Image = image
	return sandbox.NewExecutor(g.kube, g.providerGetter, w, g.sched.Load(), g.logs, cfg)
}

// operatorConfig returns the cluster OperatorConfig, or nil when none exists.
func (g *Gateway) operatorConfig(ctx context.Context) (*lastv1alpha1.OperatorConfig, error) {
	var op lastv1alpha1.OperatorConfig
	err := g.crd.Get(ctx, client.ObjectKey{Name: "default"}, &op)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read OperatorConfig: %v", err)
	}
	return &op, nil
}

// sandboxImage returns OperatorConfig's digest-pinned base image, or the
// configured fallback (development installs only).
func (g *Gateway) sandboxImage(ctx context.Context) (string, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return "", err
	}
	if op != nil && op.Spec.Sandbox != nil && op.Spec.Sandbox.BaseImage != "" {
		return op.Spec.Sandbox.BaseImage, nil
	}
	if g.cfg.SandboxImage != "" {
		return g.cfg.SandboxImage, nil
	}
	return "", status.Error(codes.FailedPrecondition, "no sandbox image: set OperatorConfig spec.sandbox.baseImage")
}

// resolveCanaries turns canary specs into IP:port literals. Services resolve to
// their ClusterIP, "node" to the first node InternalIP.
func (g *Gateway) resolveCanaries(ctx context.Context) ([]string, error) {
	out := make([]string, 0, len(g.cfg.EgressCanaries))
	for _, spec := range g.cfg.EgressCanaries {
		head, port, _ := strings.Cut(spec, ":")
		switch {
		case head == "node":
			nodes, err := g.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("gateway: canary %s: list nodes: %w", spec, err)
			}
			ip := firstInternalIP(nodes.Items)
			if ip == "" {
				return nil, fmt.Errorf("gateway: canary %s: no node has an InternalIP", spec)
			}
			out = append(out, net.JoinHostPort(ip, port))
		case strings.HasPrefix(head, "service/"):
			parts := strings.Split(head, "/")
			svc, err := g.kube.CoreV1().Services(parts[1]).Get(ctx, parts[2], metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("gateway: canary %s: %w", spec, err)
			}
			if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
				return nil, fmt.Errorf("gateway: canary %s: service has no ClusterIP", spec)
			}
			out = append(out, net.JoinHostPort(svc.Spec.ClusterIP, port))
		default:
			out = append(out, spec)
		}
	}
	return out, nil
}

func firstInternalIP(nodes []corev1.Node) string {
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return a.Address
			}
		}
	}
	return ""
}

// prepareLeading runs before this replica publishes its writer: canaries are
// resolved, the scheduler is sized, and every orphaned run is reaped. Any error
// ends leadership, so nothing runs without an egress gate or over stale grants.
func (g *Gateway) prepareLeading(ctx context.Context, w *audit.Writer) error {
	canaries, err := g.resolveCanaries(ctx)
	if err != nil {
		return err
	}
	g.execCfg.EgressCanaries = canaries
	maxConcurrent := defaultMaxConcurrent
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return err
	}
	if op != nil && op.Spec.Sandbox != nil && op.Spec.Sandbox.MaxConcurrent > 0 {
		maxConcurrent = int(op.Spec.Sandbox.MaxConcurrent)
	}
	g.sched.Store(sandbox.NewScheduler(maxConcurrent))
	reaped, err := g.executor(w, "").ReapOrphans(ctx, 0, time.Now())
	if err != nil {
		return fmt.Errorf("gateway: reap orphaned sandbox runs: %w", err)
	}
	slog.Info("Reaped orphaned sandbox runs", "count", reaped, "maxConcurrent", maxConcurrent, "canaries", len(canaries))
	g.rpcMu.Lock()
	g.draining = false
	g.rpcMu.Unlock()
	return nil
}

// keyedMutex serializes work per key (per Intent).
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}
```
In `internal/gateway/server.go`, extend `Gateway` and `New`:
```go
type Gateway struct {
	cfg     Config
	kube    kubernetes.Interface
	crd     client.Reader
	sink    audit.Sink
	signer  *audit.Signer
	metrics *Metrics
	writer  atomic.Pointer[audit.Writer]

	epoch   string // per process; an open phase from another epoch was interrupted
	sched   atomic.Pointer[sandbox.Scheduler]
	logs    sandbox.LogReader
	execCfg sandbox.ExecutorConfig
	locks   keyedMutex

	rpcMu    sync.Mutex
	draining bool
	inflight sync.WaitGroup
}

func New(cfg Config, kube kubernetes.Interface, crd client.Reader, sink audit.Sink, signer *audit.Signer) *Gateway {
	g := &Gateway{cfg: cfg, kube: kube, crd: crd, sink: sink, signer: signer, metrics: NewMetrics(),
		epoch: newEpoch(), logs: sandbox.KubeLogReader(kube)}
	g.execCfg = sandbox.ExecutorConfig{
		Pod:               sandbox.PodConfig{Namespace: cfg.SandboxNamespace, ImagePullPolicy: corev1.PullIfNotPresent},
		ProviderNamespace: cfg.Namespace,
	}
	return g
}
```
In `lead`, after `w.SetOnFailure(...)` and before `g.writer.Store(w)`, add:
```go
	if err := g.prepareLeading(ctx, w); err != nil {
		return err
	}
```
Replace `g.writer.Store(w); defer g.writer.Store(nil)` with:
```go
	g.writer.Store(w)
	defer g.writer.Store(nil)
	// Deferred later, so it runs first: in-flight RPCs finish and record their
	// outcomes before the writer is withdrawn.
	defer g.drain(g.cfg.DrainTimeout)
```
`TestLeadReapsOrphanedRunsBeforeServing` uses a fake clientset whose objects have no anchors and no Nodes. It uses the literal canary `10.96.0.10:8080`, so the `node` and `service` specs are not exercised there.

In `cmd/gateway/main.go`, build the CRD client and pass it to `New`:
```go
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := lastv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	crd, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	g := gateway.New(cfg, kube, crd, audit.NewS3Sink(client3, cfg.Audit.Bucket), audit.NewSigner(key))
```
The existing S3 variable is named `client`. Rename it to `client3`, or alias the controller-runtime import as `crclient`; pick one and keep it consistent. The imports are `k8s.io/apimachinery/pkg/runtime`, `clientgoscheme "k8s.io/client-go/kubernetes/scheme"`, `sigs.k8s.io/controller-runtime/pkg/client`, and `lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1 && go build ./...`
Expected: PASS. Every existing gateway test still passes, and the existing `lead` tests now also exercise `prepareLeading`.

- [ ] **Step 6: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway cmd/gateway
git commit -m "feat(gateway): wire the sandbox executor, egress canaries and drain into leadership"
```

---

### Task 7: Gateway-owned Intent records

**Files:**
- Create: `internal/gateway/state.go`, `internal/gateway/state_test.go`

**Interfaces:**
- Produces:
  - **Record types:**
    - `type IntentRecord struct{ Intent string; Open *OpenPhase; Executed *ExecutedPlan; Used []string }`. `Used` holds the approval subjects that have already opened a phase (at most `maxUsedSubjects`, oldest dropped).
    - `type OpenPhase struct{ Stage, SubjectHash string; BeginSeq uint64; Epoch string }`.
    - `type ExecutedPlan struct{ Stage, PlanHash, Plan string; EndSeq uint64 }`. `Plan` holds the plan's canonical JSON.
  - **Store:**
    - `type StateStore struct`, built by `func NewStateStore(kube kubernetes.Interface, namespace string) *StateStore`.
    - `type LoadedRecord struct{ IntentRecord; /* unexported ConfigMap */ }`.
    - `func (s *StateStore) Load(ctx, intent string) (*LoadedRecord, error)`.
    - `func (s *StateStore) Save(ctx, r *LoadedRecord) error`, which creates the ConfigMap on first save and updates it after that.
  - **Recording a used subject:** `func (r *IntentRecord) MarkUsed(subject string)`.
  - **Storage layout:** each record is a ConfigMap named `last-intent-<first 20 hex of sha256(intent)>` in the gateway namespace, labelled `app.kubernetes.io/managed-by=last-gateway` and `app.kubernetes.io/component=intent-state`, with the record under key `record.json`.

- [ ] **Step 1: Write the failing tests**

`internal/gateway/state_test.go`:
```go
package gateway

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	s := NewStateStore(kube, "last-gateway")
	r, err := s.Load(ctx, "team-x/web")
	if err != nil || r.Intent != "team-x/web" || r.Open != nil || r.Executed != nil {
		t.Fatalf("missing record = %+v, %v", r, err)
	}
	r.Open = &OpenPhase{Stage: "Create", SubjectHash: "sha256:a", BeginSeq: 7, Epoch: "e1"}
	r.MarkUsed("sha256:a")
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Open = nil
	r.Executed = &ExecutedPlan{Stage: "Create", PlanHash: "sha256:a", Plan: `{"steps":[]}`, EndSeq: 9}
	if err := s.Save(ctx, r); err != nil {
		t.Fatalf("second save (update): %v", err)
	}
	got, err := s.Load(ctx, "team-x/web")
	if err != nil || got.Open != nil || got.Executed == nil || got.Executed.EndSeq != 9 || len(got.Used) != 1 {
		t.Fatalf("reloaded = %+v, %v", got.IntentRecord, err)
	}
	cms, _ := kube.CoreV1().ConfigMaps("last-gateway").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=intent-state"})
	if len(cms.Items) != 1 || !strings.HasPrefix(cms.Items[0].Name, "last-intent-") || len(cms.Items[0].Name) != len("last-intent-")+20 ||
		cms.Items[0].Labels["app.kubernetes.io/managed-by"] != "last-gateway" {
		t.Fatalf("configmaps = %+v", cms.Items)
	}
}

func TestStateStoreRejectsForeignAndCorruptRecords(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	s := NewStateStore(kube, "last-gateway")
	_, _ = kube.CoreV1().ConfigMaps("last-gateway").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recordName("a/b"), Namespace: "last-gateway"},
		Data:       map[string]string{"record.json": `{"intent":"other/x"}`},
	}, metav1.CreateOptions{})
	if _, err := s.Load(ctx, "a/b"); err == nil {
		t.Fatal("a record for another intent must be refused")
	}
	_, _ = kube.CoreV1().ConfigMaps("last-gateway").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recordName("c/d"), Namespace: "last-gateway"},
		Data:       map[string]string{"record.json": `{not json`},
	}, metav1.CreateOptions{})
	if _, err := s.Load(ctx, "c/d"); err == nil {
		t.Fatal("a corrupt record must be an error")
	}
}

func TestMarkUsedIsBounded(t *testing.T) {
	var r IntentRecord
	for i := range maxUsedSubjects + 5 {
		r.MarkUsed("sha256:" + strings.Repeat("0", i+1))
	}
	if len(r.Used) != maxUsedSubjects || r.Used[0] != "sha256:"+strings.Repeat("0", 6) {
		t.Fatalf("used = %d entries, first %q", len(r.Used), r.Used[0])
	}
	r.MarkUsed(r.Used[3])
	if len(r.Used) != maxUsedSubjects {
		t.Fatal("re-marking a subject must not grow the list")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1 -run 'StateStore|MarkUsed'`
Expected: FAIL with `undefined: NewStateStore`.

- [ ] **Step 3: Implement**

`internal/gateway/state.go`:
```go
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	stateKey        = "record.json"
	labelComponent  = "app.kubernetes.io/component"
	stateComponent  = "intent-state"
	maxUsedSubjects = 50
)

// IntentRecord is the gateway's own record of an Intent (spec §8.3): the open
// phase, the last successfully executed plan, and the approval subjects that
// already opened a phase. Only the leading gateway writes it, and only after the
// matching phase.begin / phase.end entry is durable; the operator cannot read it.
type IntentRecord struct {
	Intent   string        `json:"intent"`
	Open     *OpenPhase    `json:"open,omitempty"`
	Executed *ExecutedPlan `json:"executed,omitempty"`
	Used     []string      `json:"used,omitempty"`
}

// OpenPhase is a phase.begin without a phase.end yet.
type OpenPhase struct {
	Stage       string `json:"stage"`
	SubjectHash string `json:"subjectHash"`
	BeginSeq    uint64 `json:"beginSeq"`
	Epoch       string `json:"epoch"`
}

// ExecutedPlan is the last plan whose phase ended successfully.
type ExecutedPlan struct {
	Stage    string `json:"stage"`
	PlanHash string `json:"planHash"`
	Plan     string `json:"plan"` // canonical JSON of plan.Body
	EndSeq   uint64 `json:"endSeq"`
}

// MarkUsed records that subject opened a phase; a subject opens at most one.
func (r *IntentRecord) MarkUsed(subject string) {
	if slices.Contains(r.Used, subject) {
		return
	}
	r.Used = append(r.Used, subject)
	if len(r.Used) > maxUsedSubjects {
		r.Used = r.Used[len(r.Used)-maxUsedSubjects:]
	}
}

// StateStore keeps IntentRecords in ConfigMaps in the gateway namespace.
type StateStore struct {
	kube kubernetes.Interface
	ns   string
}

func NewStateStore(kube kubernetes.Interface, namespace string) *StateStore {
	return &StateStore{kube: kube, ns: namespace}
}

// LoadedRecord is a record plus the ConfigMap it came from (nil when new).
type LoadedRecord struct {
	IntentRecord
	cm *corev1.ConfigMap
}

func recordName(intent string) string {
	sum := sha256.Sum256([]byte(intent))
	return "last-intent-" + hex.EncodeToString(sum[:])[:20]
}

// Load returns the intent's record, or an empty one when none exists.
func (s *StateStore) Load(ctx context.Context, intent string) (*LoadedRecord, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, recordName(intent), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &LoadedRecord{IntentRecord: IntentRecord{Intent: intent}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: load intent record: %w", err)
	}
	var rec IntentRecord
	if err := json.Unmarshal([]byte(cm.Data[stateKey]), &rec); err != nil {
		return nil, fmt.Errorf("gateway: intent record %s is corrupt: %w", cm.Name, err)
	}
	if rec.Intent != intent {
		return nil, fmt.Errorf("gateway: intent record %s belongs to %q, not %q", cm.Name, rec.Intent, intent)
	}
	return &LoadedRecord{IntentRecord: rec, cm: cm}, nil
}

// Save writes the record, creating its ConfigMap on first use. An update carries
// the loaded resourceVersion, so a concurrent writer fails instead of overwriting.
func (s *StateStore) Save(ctx context.Context, r *LoadedRecord) error {
	body, err := json.Marshal(r.IntentRecord)
	if err != nil {
		return fmt.Errorf("gateway: encode intent record: %w", err)
	}
	cms := s.kube.CoreV1().ConfigMaps(s.ns)
	if r.cm == nil {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: recordName(r.Intent), Namespace: s.ns,
				Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelComponent: stateComponent}},
			Data: map[string]string{stateKey: string(body)},
		}
		created, err := cms.Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("gateway: create intent record: %w", err)
		}
		r.cm = created
		return nil
	}
	cm := r.cm.DeepCopy()
	cm.Data = map[string]string{stateKey: string(body)}
	updated, err := cms.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("gateway: update intent record: %w", err)
	}
	r.cm = updated
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1 -run 'StateStore|MarkUsed'`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway/state.go internal/gateway/state_test.go
git commit -m "feat(gateway): keep a gateway-owned record of each intent's phases"
```

---

### Task 8: BeginPhase and EndPhase

**Files:**
- Create: `internal/gateway/phases.go`, `internal/gateway/phases_test.go`, `internal/gateway/testenv_test.go`
- Modify: `internal/gateway/server.go` (fields `state`, `phases`), `internal/gateway/rpc.go` (delegate the two RPCs)

**Interfaces:**
- Consumes:
  - from Task 1: `plan.FromStatus`, `Canonical`, `Hash`, `Validate`, `Results`;
  - from Task 2: `policy.Resolve`, `(Effective).CheckCommand`;
  - from Task 6: `beginRPC`, `operatorConfig`, `locks`;
  - from Task 7: `StateStore`, `LoadedRecord`, `OpenPhase`, `ExecutedPlan`, `MarkUsed`;
  - from Task 5: `gatewayv1` request and response types.
- Produces, for use by Tasks 9–12:
  - **Phase state.** `type phase` (fields `mu`, `stage`, `subject`, `body`, `auth`, `beginSeq`, `next`, `results`, `failed`), `phaseTable` (`get`, `put`, `remove`), and the Gateway fields `state *StateStore` and `phases phaseTable`.
  - **Request helpers.** `intentKey(*gatewayv1.IntentRef) (ns, name, key string, err error)` and `notFoundOr(err, what) error`.
  - **Denials.** `type denial` with `deny(code, format, args...) error`, which writes `policy.denied` before returning the gRPC error.
  - **Policy helpers.** `(g *Gateway) effectivePolicy(ctx, ns) (policy.Effective, error)` and `checkPlanPolicy(policy.Effective, plan.Body) error`.
  - **Payload types.** `phaseBegin`, `phaseEnd`, `approvalObserved`, `policyDenied`.
  - **Outcomes.** `outcomeSucceeded`, `outcomeFailed`, `outcomeInterrupted`, `outcomeAborted`.
  - **Test environment** in `testenv_test.go`:
    - `newTestEnv(t, objs...) *testEnv`, with `env.newGateway()` to simulate a restart: a new epoch on the same cluster, audit sink and signing key;
    - `env.begin`, `env.end`, `env.entries`, `env.last`;
    - `intentWithPlan`, `approvalFor`, `allowAll`, `argvStep`, `ref`.

- [ ] **Step 1: Write the shared test environment**

`internal/gateway/testenv_test.go`:
```go
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	envNS     = "team-x"
	envIntent = "web"
	envKey    = envNS + "/" + envIntent
	approver  = "alice@example.com"
)

// testEnv is a leading gateway on a fake cluster whose sandbox pods succeed at
// once; result decides what each sandboxed command printed and returned.
type testEnv struct {
	t      *testing.T
	g      *Gateway
	kube   *fake.Clientset
	crd    client.Client
	sink   *audittest.MemSink
	signer *audit.Signer
	ctx    context.Context
	result func(c runnerapi.Command) runnerapi.Result
}

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	kube := fake.NewClientset()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); s.UID == "" {
			s.UID = types.UID("uid-" + s.Name)
		}
		return false, nil, nil
	})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	env := &testEnv{t: t, kube: kube, sink: audittest.NewMemSink(), signer: audit.NewSigner(key),
		crd: crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()}
	env.g = env.newGateway()
	env.ctx = context.WithValue(context.Background(), callerKey{}, testOperator)
	return env
}

// newGateway starts a gateway process (a new epoch) on the env's cluster, audit
// chain, and signing key, as a restart would.
func (e *testEnv) newGateway() *Gateway {
	e.t.Helper()
	cfg := Config{Namespace: "last-gateway", PodName: "gw-0", ChainID: testChainID, Retention: time.Hour,
		SandboxNamespace: "last-sandbox", SandboxImage: "sandbox:test", EgressCanaries: []string{"10.96.0.10:8080"},
		OperatorIdentity: testOperator, DrainTimeout: time.Second}
	g := New(cfg, e.kube, e.crd, e.sink, e.signer)
	g.execCfg.EgressCanaries = cfg.EgressCanaries
	g.execCfg.PollInterval = time.Millisecond
	g.execCfg.DeadlineOverhead = time.Millisecond
	g.execCfg.StartupGrace = 200 * time.Millisecond
	g.execCfg.CleanupTimeout = time.Second
	g.logs = e.logs
	g.sched.Store(sandbox.NewScheduler(4))
	w, err := audit.Open(context.Background(), e.sink, e.signer,
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: "gateway"}, func(audit.StartInfo) any { return nil })
	if err != nil {
		e.t.Fatal(err)
	}
	g.writer.Store(w)
	return g
}

// logs plays the runner: it reads the run's spec Secret and answers every command.
func (e *testEnv) logs(ctx context.Context, namespace, pod string) ([]byte, error) {
	sec, err := e.kube.CoreV1().Secrets(namespace).Get(ctx, pod+"-spec", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var spec runnerapi.Spec
	if err := json.Unmarshal(sec.Data["spec.json"], &spec); err != nil {
		return nil, err
	}
	envl := runnerapi.Envelope{Version: runnerapi.Version}
	for _, c := range spec.Commands {
		r := runnerapi.Result{}
		if e.result != nil {
			r = e.result(c)
		}
		r.ID = c.ID
		envl.Results = append(envl.Results, r)
	}
	b, _ := json.Marshal(envl)
	return append([]byte(runnerapi.ResultPrefix), b...), nil
}

func (e *testEnv) entries() []audit.Entry {
	e.t.Helper()
	objs, err := e.sink.List(context.Background(), audit.EntryPrefix(testChainID), "")
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]audit.Entry, 0, len(objs))
	for _, o := range objs {
		body, err := e.sink.Get(context.Background(), o.Key, "")
		if err != nil {
			e.t.Fatal(err)
		}
		var en audit.Entry
		if err := json.Unmarshal(body, &en); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, en)
	}
	return out
}

func (e *testEnv) last() audit.Entry {
	e.t.Helper()
	es := e.entries()
	return es[len(es)-1]
}

func (e *testEnv) begin(subject string) (*gatewayv1.BeginPhaseResponse, error) {
	return e.g.beginPhase(e.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: "Create", SubjectHash: subject})
}

func (e *testEnv) end(subject string, succeeded bool) (*gatewayv1.EndPhaseResponse, error) {
	return e.g.endPhase(e.ctx, &gatewayv1.EndPhaseRequest{Intent: ref(), SubjectHash: subject, Succeeded: succeeded})
}

func ref() *gatewayv1.IntentRef { return &gatewayv1.IntentRef{Namespace: envNS, Name: envIntent} }

func argvStep(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func intentWithPlan(t *testing.T, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) *lastv1alpha1.Intent {
	t.Helper()
	h, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	return &lastv1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: envIntent, Namespace: envNS, UID: "intent-uid"},
		Spec:       lastv1alpha1.IntentSpec{Request: "run a web server"},
		Status: lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingApproval, Stage: lastv1alpha1.StageCreate,
			Plan: &lastv1alpha1.Plan{Hash: h, Steps: steps, Resources: resources}},
	}
}

func approvalFor(name, subject string, decision lastv1alpha1.Decision, by string) *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNS, UID: types.UID(name + "-uid")},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: envIntent}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: subject, Decision: decision, ApprovedBy: by},
	}
}

// allowAll is an OperatorConfig whose defaults allow provider "cluster" and egress to 10.0.0.0/8:443.
func allowAll() *lastv1alpha1.OperatorConfig {
	return &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{
			Providers: []string{"cluster"}, Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}}}}}
}
```

- [ ] **Step 2: Write the failing phase tests**

`internal/gateway/phases_test.go`:
```go
package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
)

func twoSteps() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{
		argvStep("s1", "kubectl", "get", "ns", "team-x", "-o", "json"),
		argvStep("s2", "echo", "${steps.s1.json.kind}"),
	}
}

func TestBeginPhaseOpensAnApprovedPlan(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	resp, err := env.begin(h)
	if err != nil || resp.GetSteps() != 2 {
		t.Fatalf("BeginPhase = %v, %v", resp, err)
	}
	es := env.entries()
	observed, begin := es[len(es)-2], es[len(es)-1]
	if observed.Type != audit.TypeApprovalObserved || begin.Type != audit.TypePhaseBegin {
		t.Fatalf("last entries = %s, %s", observed.Type, begin.Type)
	}
	if begin.Actor != testOperator || begin.Intent != envKey || begin.Stage != "Create" || begin.SubjectHash != h ||
		begin.AuthorizedBy == nil || begin.AuthorizedBy.Kind != "approval" || begin.AuthorizedBy.Name != envNS+"/ok" {
		t.Fatalf("phase.begin = %+v", begin)
	}
	var p phaseBegin
	if err := json.Unmarshal(begin.Payload, &p); err != nil {
		t.Fatal(err)
	}
	canonical, _ := plan.Canonical(plan.FromStatus(in.Status.Plan))
	if p.Plan != string(canonical) || p.Steps != 2 {
		t.Fatalf("phase.begin payload = %+v", p)
	}
	rec, err := env.g.state.Load(context.Background(), envKey)
	if err != nil || rec.Open == nil || rec.Open.BeginSeq != begin.Seq || rec.Open.Epoch != env.g.epoch {
		t.Fatalf("record = %+v, %v", rec.IntentRecord, err)
	}
	if _, err := env.begin(h); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("second BeginPhase = %v, want AlreadyExists", err)
	}
}

func TestBeginPhaseNeedsAWritableAuditLog(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	env.g.writer.Store(nil)
	if _, err := env.begin(in.Status.Plan.Hash); status.Code(err) != codes.Unavailable {
		t.Fatalf("BeginPhase = %v, want Unavailable", err)
	}
	if len(env.entries()) != before {
		t.Fatal("an entry was written without a writer")
	}
}

func TestBeginPhaseDenials(t *testing.T) {
	steps := twoSteps()
	cases := []struct {
		name    string
		mutate  func(in *lastv1alpha1.Intent) []runtime.Object
		code    codes.Code
		message string
	}{
		{"plan edited after hashing", func(in *lastv1alpha1.Intent) []runtime.Object {
			in.Status.Plan.Steps[0].Run.Argv = []string{"kubectl", "delete", "ns", "team-x"}
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)}
		}, codes.PermissionDenied, "does not match"},
		{"no approval", func(in *lastv1alpha1.Intent) []runtime.Object { return nil }, codes.FailedPrecondition, "no IntentApproval"},
		{"approval for another plan", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("other", "sha256:"+strings.Repeat("0", 64), lastv1alpha1.DecisionApprove, approver)}
		}, codes.FailedPrecondition, "no IntentApproval"},
		{"denied", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver),
				approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")}
		}, codes.PermissionDenied, "denies"},
		{"approved by the operator", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, testOperator)}
		}, codes.PermissionDenied, "names the operator"},
		{"provider not allowed", func(in *lastv1alpha1.Intent) []runtime.Object {
			in.Status.Plan.Steps[0].Providers = []string{"prod"}
			h, _ := plan.Hash(plan.FromStatus(in.Status.Plan))
			in.Status.Plan.Hash = h
			return []runtime.Object{approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver)}
		}, codes.PermissionDenied, `provider "prod"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := intentWithPlan(t, append([]lastv1alpha1.Command(nil), steps...))
			in.Status.Plan.Steps = append([]lastv1alpha1.Command(nil), in.Status.Plan.Steps...)
			extra := tc.mutate(in)
			objs := []runtime.Object{in, allowAll()}
			objs = append(objs, extra...)
			env := newTestEnv(t, toClientObjects(objs)...)
			_, err := env.begin(in.Status.Plan.Hash)
			if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("BeginPhase = %v, want %v containing %q", err, tc.code, tc.message)
			}
			last := env.last()
			if last.Type != audit.TypePolicyDenied || last.Intent != envKey || last.Actor != testOperator {
				t.Fatalf("last entry = %s %+v, want policy.denied", last.Type, last)
			}
			for _, e := range env.entries() {
				if e.Type == audit.TypePhaseBegin {
					t.Fatal("a denied BeginPhase wrote phase.begin")
				}
			}
		})
	}
}

func TestBeginPhaseOpensCreateOnly(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	_, err := env.g.beginPhase(env.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: "Update", SubjectHash: in.Status.Plan.Hash})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Update phase = %v, want FailedPrecondition", err)
	}
}

func TestRestartClosesTheInterruptedPhaseAndNeverRerunsIt(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	env.g = env.newGateway() // the gateway restarted mid-phase

	if _, err := env.begin(h); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-running the interrupted plan = %v, want FailedPrecondition", err)
	}
	var interrupted bool
	for _, e := range env.entries() {
		var p phaseEnd
		if e.Type == audit.TypePhaseEnd && json.Unmarshal(e.Payload, &p) == nil && p.Outcome == outcomeInterrupted {
			interrupted = e.Actor == "gateway" && e.SubjectHash == h
		}
	}
	if !interrupted {
		t.Fatal("no phase.end interrupted entry by the gateway")
	}

	replanned := intentWithPlan(t, []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "ns", "team-x")})
	if err := env.crd.Update(context.Background(), withVersion(t, env, replanned)); err != nil {
		t.Fatal(err)
	}
	if err := env.crd.Create(context.Background(), approvalFor("ok2", replanned.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.begin(replanned.Status.Plan.Hash); err != nil {
		t.Fatalf("a new plan after the restart = %v", err)
	}
}

func TestEndPhase(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.end(h, false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase without a phase = %v", err)
	}
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase(succeeded) before any step = %v, want FailedPrecondition", err)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
	var p phaseEnd
	last := env.last()
	if last.Type != audit.TypePhaseEnd || json.Unmarshal(last.Payload, &p) != nil || p.Outcome != outcomeFailed || last.Actor != testOperator {
		t.Fatalf("last = %s %s", last.Type, last.Payload)
	}
	rec, _ := env.g.state.Load(context.Background(), envKey)
	if rec.Open != nil || rec.Executed != nil {
		t.Fatalf("record after a failed phase = %+v", rec.IntentRecord)
	}
	if _, err := env.begin(h); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-opening a plan that already ran = %v, want FailedPrecondition", err)
	}
}

func TestBeginPhaseStoresPlansWithFloatsAsCanonicalText(t *testing.T) {
	res := lastv1alpha1.ManagedResourceSpec{Desired: &runtime.RawExtension{Raw: []byte(`{"ratio":0.75,"big":12345678901234567890}`)}}
	in := intentWithPlan(t, twoSteps(), res)
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(in.Status.Plan.Hash); err != nil {
		t.Fatalf("BeginPhase with floats in desired = %v", err)
	}
	if !strings.Contains(string(env.last().Payload), "0.75") {
		t.Fatalf("phase.begin = %s", env.last().Payload)
	}
}

func toClientObjects(objs []runtime.Object) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(client.Object))
	}
	return out
}

// withVersion copies the stored Intent's resourceVersion onto in so Update succeeds.
func withVersion(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) *lastv1alpha1.Intent {
	t.Helper()
	var cur lastv1alpha1.Intent
	if err := env.crd.Get(context.Background(), client.ObjectKeyFromObject(in), &cur); err != nil {
		t.Fatal(err)
	}
	in.ResourceVersion = cur.ResourceVersion
	return in
}
```
This file also needs `sigs.k8s.io/controller-runtime/pkg/client` and the `gatewayv1` import.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1 -run 'Phase|Restart'`
Expected: FAIL to compile, with `undefined: (*Gateway).beginPhase` and `undefined: phaseBegin`.

- [ ] **Step 4: Implement**

In `internal/gateway/server.go`, add these fields to `Gateway`:
```go
	state  *StateStore
	phases phaseTable
```
In `New`, set `g.state = NewStateStore(kube, cfg.Namespace)`.

In `internal/gateway/rpc.go`, add:
```go
func (s *rpcServer) BeginPhase(ctx context.Context, req *gatewayv1.BeginPhaseRequest) (*gatewayv1.BeginPhaseResponse, error) {
	return s.g.beginPhase(ctx, req)
}

func (s *rpcServer) EndPhase(ctx context.Context, req *gatewayv1.EndPhaseRequest) (*gatewayv1.EndPhaseResponse, error) {
	return s.g.endPhase(ctx, req)
}
```

`internal/gateway/phases.go`:
```go
package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// MaxPlanBytes bounds a plan's canonical JSON; the executed plan is kept in a ConfigMap.
const MaxPlanBytes = 512 << 10

const (
	outcomeSucceeded   = "succeeded"
	outcomeFailed      = "failed"
	outcomeInterrupted = "interrupted"
	outcomeAborted     = "aborted"
)

// phase is an open phase of this gateway process.
type phase struct {
	mu       sync.Mutex // held while a step or output write runs
	stage    lastv1alpha1.Stage
	subject  string
	body     plan.Body
	auth     *audit.Authorization
	beginSeq uint64
	next     int          // index of the next step to run; also the count of steps that succeeded
	results  plan.Results // results of steps that ran, by step ID
	failed   bool         // a step failed or could not run; only EndPhase(failed) remains
}

type phaseTable struct {
	mu   sync.Mutex
	open map[string]*phase
}

func (t *phaseTable) get(key string) *phase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open[key]
}

func (t *phaseTable) put(key string, p *phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = map[string]*phase{}
	}
	t.open[key] = p
}

func (t *phaseTable) remove(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, key)
}

type phaseBegin struct {
	Plan  string `json:"plan"` // canonical JSON, kept as text so any number survives the audit writer
	Steps int    `json:"steps"`
}

type phaseEnd struct {
	BeginSeq uint64 `json:"beginSeq"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	StepsRun int    `json:"stepsRun"`
}

type approvalObserved struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	ApprovedBy string `json:"approvedBy"`
	Decision   string `json:"decision"`
	Created    string `json:"created"`
}

type policyDenied struct {
	RPC    string `json:"rpc"`
	Reason string `json:"reason"`
}

var errApprovalDenied = errors.New("approval denied")

func intentKey(ref *gatewayv1.IntentRef) (ns, name, key string, err error) {
	if ref.GetNamespace() == "" || ref.GetName() == "" {
		return "", "", "", status.Error(codes.InvalidArgument, "intent namespace and name are required")
	}
	return ref.GetNamespace(), ref.GetName(), ref.GetNamespace() + "/" + ref.GetName(), nil
}

func notFoundOr(err error, what string) error {
	if apierrors.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "%s not found", what)
	}
	return status.Errorf(codes.Unavailable, "read %s: %v", what, err)
}

// denial writes policy.denied before refusing an RPC (spec §8.3).
type denial struct {
	ctx                                 context.Context
	w                                   *audit.Writer
	actor, intent, stage, subject, rpc string
}

func (d denial) deny(code codes.Code, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	if _, err := d.w.Append(d.ctx, audit.Draft{Type: audit.TypePolicyDenied, Actor: d.actor, Intent: d.intent,
		Stage: d.stage, SubjectHash: d.subject, Payload: policyDenied{RPC: d.rpc, Reason: reason}}); err != nil {
		return status.Errorf(codes.Unavailable, "record policy.denied: %v", err)
	}
	return status.Error(code, reason)
}

func (g *Gateway) effectivePolicy(ctx context.Context, ns string) (policy.Effective, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return policy.Effective{}, err
	}
	var list lastv1alpha1.IntentPolicyList
	if err := g.crd.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return policy.Effective{}, status.Errorf(codes.Unavailable, "list IntentPolicies: %v", err)
	}
	eff, err := policy.Resolve(op, list.Items)
	if err != nil {
		return policy.Effective{}, status.Errorf(codes.FailedPrecondition, "invalid policy: %v", err)
	}
	return eff, nil
}

// checkPlanPolicy checks every command the plan can ever run: steps, probes, teardown.
func checkPlanPolicy(eff policy.Effective, b plan.Body) error {
	for _, s := range b.Steps {
		if err := eff.CheckCommand(s); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
	}
	for _, r := range b.Resources {
		for _, p := range r.Probes {
			if err := eff.CheckCommand(p.Command); err != nil {
				return fmt.Errorf("probe %s: %w", p.ID, err)
			}
		}
		for i, c := range r.Teardown {
			if err := eff.CheckCommand(c); err != nil {
				return fmt.Errorf("teardown command %d: %w", i, err)
			}
		}
	}
	return nil
}

// findApproval returns the approving IntentApproval for the subject. Any matching
// Deny wins over every Approve (errApprovalDenied, with the denying object).
func (g *Gateway) findApproval(ctx context.Context, ns, name string, stage lastv1alpha1.Stage, subject string) (*lastv1alpha1.IntentApproval, error) {
	var list lastv1alpha1.IntentApprovalList
	if err := g.crd.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list IntentApprovals: %v", err)
	}
	var found *lastv1alpha1.IntentApproval
	for i := range list.Items {
		a := &list.Items[i]
		if a.Spec.IntentRef.Name != name || a.Spec.Stage != stage || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return a, errApprovalDenied
		}
		if found == nil || a.Name < found.Name {
			found = a
		}
	}
	return found, nil
}

func (g *Gateway) beginPhase(ctx context.Context, req *gatewayv1.BeginPhaseRequest) (*gatewayv1.BeginPhaseResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	stage, subject := lastv1alpha1.Stage(req.GetStage()), req.GetSubjectHash()
	if stage != lastv1alpha1.StageCreate {
		return nil, status.Errorf(codes.FailedPrecondition, "stage %q is not supported yet; this gateway opens Create phases only", stage)
	}
	unlock := g.locks.lock(key)
	defer unlock()
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, stage: string(stage), subject: subject, rpc: "BeginPhase"}

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	if intent.Status.Plan == nil {
		return nil, d.deny(codes.FailedPrecondition, "intent has no plan")
	}
	body := plan.FromStatus(intent.Status.Plan)
	canonical, err := plan.Canonical(body)
	if err != nil {
		return nil, d.deny(codes.InvalidArgument, "plan cannot be encoded: %v", err)
	}
	if len(canonical) > MaxPlanBytes {
		return nil, d.deny(codes.InvalidArgument, "plan is %d bytes; the limit is %d", len(canonical), MaxPlanBytes)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		return nil, d.deny(codes.InvalidArgument, "%v", err)
	}
	if hash != subject || intent.Status.Plan.Hash != subject {
		return nil, d.deny(codes.PermissionDenied, "plan hash %s does not match the subject", hash)
	}
	if err := plan.Validate(body); err != nil {
		return nil, d.deny(codes.InvalidArgument, "%v", err)
	}

	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if rec.Open != nil {
		if rec.Open.Epoch == g.epoch && g.phases.get(key) != nil {
			return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
		}
		if err := g.closeInterrupted(ctx, w, key, rec); err != nil {
			return nil, err
		}
	}
	if slices.Contains(rec.Used, subject) {
		return nil, d.deny(codes.FailedPrecondition, "this plan already opened a phase; replan to run it again")
	}
	eff, err := g.effectivePolicy(ctx, ns)
	if err != nil {
		return nil, err
	}
	if err := checkPlanPolicy(eff, body); err != nil {
		return nil, d.deny(codes.PermissionDenied, "%v", err)
	}
	approval, err := g.findApproval(ctx, ns, name, stage, subject)
	switch {
	case errors.Is(err, errApprovalDenied):
		return nil, d.deny(codes.PermissionDenied, "approval %s denies this plan", approval.Name)
	case err != nil:
		return nil, err
	case approval == nil:
		return nil, d.deny(codes.FailedPrecondition, "no IntentApproval approves this plan")
	case approval.Spec.ApprovedBy == g.cfg.OperatorIdentity:
		return nil, d.deny(codes.PermissionDenied, "approval %s names the operator as approver", approval.Name)
	}

	auth := &audit.Authorization{Kind: "approval", Name: ns + "/" + approval.Name}
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypeApprovalObserved, Actor: actor, Intent: key, Stage: string(stage), SubjectHash: subject,
		Payload: approvalObserved{Name: approval.Name, UID: string(approval.UID), ApprovedBy: approval.Spec.ApprovedBy,
			Decision: string(approval.Spec.Decision), Created: approval.CreationTimestamp.UTC().Format(time.RFC3339)}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "record approval.observed: %v", err)
	}
	begin, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseBegin, Actor: actor, Intent: key, Stage: string(stage),
		SubjectHash: subject, AuthorizedBy: auth, Payload: phaseBegin{Plan: string(canonical), Steps: len(body.Steps)}})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "record phase.begin: %v", err)
	}
	rec.Open = &OpenPhase{Stage: string(stage), SubjectHash: subject, BeginSeq: begin.Seq, Epoch: g.epoch}
	rec.MarkUsed(subject)
	if err := g.state.Save(ctx, rec); err != nil {
		_, _ = w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: sandbox.GatewayActor, Intent: key, Stage: string(stage),
			SubjectHash: subject, Payload: phaseEnd{BeginSeq: begin.Seq, Outcome: outcomeAborted, Reason: "could not record the open phase"}})
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.phases.put(key, &phase{stage: stage, subject: subject, body: body, auth: auth, beginSeq: begin.Seq, results: plan.Results{}})
	return &gatewayv1.BeginPhaseResponse{BeginSeq: begin.Seq, Steps: int32(len(body.Steps))}, nil //nolint:gosec // bounded by MaxPlanBytes
}

// closeInterrupted ends a phase that an earlier gateway process opened. Its
// subject stays in Used, so the same plan never runs twice without replanning.
func (g *Gateway) closeInterrupted(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord) error {
	open := *rec.Open
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: sandbox.GatewayActor, Intent: key, Stage: open.Stage,
		SubjectHash: open.SubjectHash, Payload: phaseEnd{BeginSeq: open.BeginSeq, Outcome: outcomeInterrupted,
			Reason: "the gateway restarted or lost leadership during the phase"}}); err != nil {
		return status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	g.phases.remove(key)
	rec.Open = nil
	if err := g.state.Save(ctx, rec); err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	return nil
}

func (g *Gateway) endPhase(ctx context.Context, req *gatewayv1.EndPhaseRequest) (*gatewayv1.EndPhaseResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	_, _, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	unlock := g.locks.lock(key)
	defer unlock()
	p := g.phases.get(key)
	if p == nil || p.subject != req.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.GetSucceeded() && (p.failed || p.next < len(p.body.Steps)) {
		return nil, status.Errorf(codes.FailedPrecondition, "the phase cannot succeed: %d of %d steps succeeded", p.next, len(p.body.Steps))
	}
	outcome := outcomeFailed
	if req.GetSucceeded() {
		outcome = outcomeSucceeded
	}
	end, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: CallerFrom(ctx), Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth,
		Payload: phaseEnd{BeginSeq: p.beginSeq, Outcome: outcome, Reason: req.GetReason(), StepsRun: p.next}})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	g.phases.remove(key)
	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	rec.Open = nil
	if req.GetSucceeded() {
		canonical, err := plan.Canonical(p.body)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode executed plan: %v", err)
		}
		rec.Executed = &ExecutedPlan{Stage: string(p.stage), PlanHash: p.subject, Plan: string(canonical), EndSeq: end.Seq}
	}
	if err := g.state.Save(ctx, rec); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	return &gatewayv1.EndPhaseResponse{EndSeq: end.Seq}, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway
git commit -m "feat(gateway): open and close approved phases with interrupted-phase recovery"
```

---

### Task 9: RunCommand — the next approved step, exactly

**Files:**
- Create: `internal/gateway/commands.go`, `internal/gateway/commands_test.go`
- Modify: `internal/gateway/rpc.go`

**Interfaces:**
- Consumes:
  - From Task 8: the `phase` struct, `phaseTable`, `denial`, `intentKey`, and `testEnv`.
  - From Task 6: `executor`, `sandboxImage`, `beginRPC`.
  - From Task 1: `plan.Resolve`.
  - From M2a: `sandbox.RunRequest{Actor, Intent, Stage, SubjectHash, AuthorizedBy, Command, Priority}` and `RunResult{RunID, Result}`.
- Produces:
  - **Helpers:** `priorityFor(lastv1alpha1.Stage) sandbox.Priority`, which maps Discovery to `PriorityDiscovery` and every other stage to `PriorityExecution`; `toProtoResult(runID string, r runnerapi.Result) *gatewayv1.CommandResult`; `sameCommand`; `decodeCommand`.
  - **Test helpers** for Tasks 10 and 11: `(e *testEnv) run(subject string, c lastv1alpha1.Command)`, `runAll(t, env, in) plan.Results`, `executeAll(t, env, in)`.
- Semantics:
  - Output is returned only when the executor reports success. If `Run` returns an error, the outcome may not be recorded, so the RPC withholds output and the phase is marked failed.
  - A non-zero exit returns its result and marks the phase failed.
  - A mismatch writes `policy.denied` and leaves the phase usable.

- [ ] **Step 1: Write the failing tests**

`internal/gateway/commands_test.go`:
```go
package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func (e *testEnv) run(subject string, c lastv1alpha1.Command) (*gatewayv1.RunCommandResponse, error) {
	e.t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.runRaw(subject, c.ID, raw)
}

func (e *testEnv) runRaw(subject, stepID string, raw []byte) (*gatewayv1.RunCommandResponse, error) {
	return e.g.runCommand(e.ctx, &gatewayv1.RunCommandRequest{Intent: ref(), SubjectHash: subject, StepId: stepID, CommandJson: raw})
}

// runAll opens the intent's plan and runs every step as the operator would,
// resolving references from earlier results. The phase stays open.
func runAll(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) plan.Results {
	t.Helper()
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	results := plan.Results{}
	for _, s := range in.Status.Plan.Steps {
		c, err := plan.Resolve(s, results)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := env.run(h, c)
		if err != nil {
			t.Fatalf("step %s: %v", s.ID, err)
		}
		results[s.ID] = runnerapi.Result{ID: s.ID, ExitCode: int(resp.GetResult().GetExitCode()), Stdout: resp.GetResult().GetStdout()}
	}
	return results
}

// executeAll runs the whole plan and ends the phase successfully.
func executeAll(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) {
	t.Helper()
	runAll(t, env, in)
	if _, err := env.end(in.Status.Plan.Hash, true); err != nil {
		t.Fatalf("EndPhase: %v", err)
	}
}

// kubectlJSON makes every kubectl command print a Namespace object and every
// other command print "ok".
func kubectlJSON(c runnerapi.Command) runnerapi.Result {
	if len(c.Argv) > 0 && c.Argv[0] == "kubectl" {
		return runnerapi.Result{Stdout: `{"kind":"Namespace","metadata":{"name":"team-x"},"password":"s3cr3t-value-123"}` + "\n"}
	}
	return runnerapi.Result{Stdout: "ok\n"}
}

// newRunEnv is an approved intent plus provider "cluster": an env-secret Provider
// whose Secret lives in last-gateway (probes in Task 10 use it).
func newRunEnv(t *testing.T, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) (*testEnv, *lastv1alpha1.Intent) {
	t.Helper()
	in := intentWithPlan(t, steps, resources...)
	provider := &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "cluster-creds"}, Env: map[string]string{"CLUSTER_TOKEN": "token"}}}
	env := newTestEnv(t, in, allowAll(), provider, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.kube.CoreV1().Secrets("last-gateway").Create(env.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: "last-gateway"},
		Data:       map[string][]byte{"token": []byte("probe-token-value")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env.result = kubectlJSON
	return env, in
}

func TestRunCommandRunsTheResolvedPlanAndEndsThePhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	r1, err := env.run(h, in.Status.Plan.Steps[0])
	if err != nil || r1.GetResult().GetExitCode() != 0 || !strings.Contains(r1.GetResult().GetStdout(), "Namespace") {
		t.Fatalf("s1 = %v, %v", r1, err)
	}
	r2, err := env.run(h, argvStep("s2", "echo", "Namespace"))
	if err != nil || r2.GetResult().GetStdout() != "ok\n" || r2.GetResult().GetRunId() == "" {
		t.Fatalf("s2 = %v, %v", r2, err)
	}
	var s2Start audit.Entry
	for _, e := range env.entries() {
		if e.Type == audit.TypeCommandStart && strings.Contains(string(e.Payload), `"echo"`) {
			s2Start = e
		}
	}
	if s2Start.Actor != testOperator || s2Start.SubjectHash != h || s2Start.AuthorizedBy == nil ||
		s2Start.AuthorizedBy.Kind != "approval" || !strings.Contains(string(s2Start.Payload), `"Namespace"`) {
		t.Fatalf("s2 command.start = %+v %s", s2Start, s2Start.Payload)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatal(err)
	}
	rec, _ := env.g.state.Load(env.ctx, envKey)
	canonical, _ := plan.Canonical(plan.FromStatus(in.Status.Plan))
	if rec.Executed == nil || rec.Executed.PlanHash != h || rec.Executed.Plan != string(canonical) {
		t.Fatalf("executed record = %+v", rec.Executed)
	}
}

func TestRunCommandRefusesAnythingButTheNextApprovedStep(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, argvStep("s2", "echo", "x")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("out of order = %v, want FailedPrecondition", err)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatal("out-of-order step did not write policy.denied")
	}
	tampered := in.Status.Plan.Steps[0]
	tampered.Run.Argv = append(append([]string(nil), tampered.Run.Argv...), "--all-namespaces")
	if _, err := env.run(h, tampered); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("tampered step = %v, want PermissionDenied", err)
	}
	if _, err := env.run(h, in.Status.Plan.Steps[0]); err != nil {
		t.Fatalf("a denied mismatch must leave the phase usable: %v", err)
	}
	if _, err := env.run(h, in.Status.Plan.Steps[1]); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unresolved reference = %v, want PermissionDenied", err)
	}
	if _, err := env.runRaw(h, "s2", []byte(`{"id":"s2","run":{"argv":["echo","Namespace"]},"surprise":1}`)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown field = %v, want InvalidArgument", err)
	}
}

func TestRunCommandComparesCanonicalJSON(t *testing.T) {
	s1 := argvStep("s1", "true")
	s1.Files = map[string]string{"/work/a": "1", "/work/b": "2"}
	env, in := newRunEnv(t, []lastv1alpha1.Command{s1, argvStep("s2", "true")})
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	reordered := []byte(`{"files":{"/work/b":"2","/work/a":"1"},"egress":[],"providers":null,"run":{"argv":["true"]},"id":"s1"}`)
	if _, err := env.runRaw(h, "s1", reordered); err != nil {
		t.Fatalf("map order and nil-versus-empty must not matter: %v", err)
	}
	newline := []byte(`{"id":"s2","run":{"argv":["true"]},"files":{"/work/x":"\n"}}`)
	if _, err := env.runRaw(h, "s2", newline); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an added file = %v, want PermissionDenied", err)
	}
}

func TestRunCommandFailedStepEndsOnlyAsFailed(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	env.result = func(runnerapi.Command) runnerapi.Result { return runnerapi.Result{ExitCode: 3, Stderr: "boom"} }
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	r, err := env.run(h, in.Status.Plan.Steps[0])
	if err != nil || r.GetResult().GetExitCode() != 3 {
		t.Fatalf("failing step = %v, %v", r, err)
	}
	if _, err := env.run(h, argvStep("s2", "echo", "x")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("after a failure = %v, want FailedPrecondition", err)
	}
	if _, err := env.end(h, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase(succeeded) after a failure = %v", err)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommandIsSerializedAndFailsClosed(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	p.mu.Lock()
	if _, err := env.run(h, in.Status.Plan.Steps[0]); status.Code(err) != codes.Aborted {
		t.Fatalf("concurrent step = %v, want Aborted", err)
	}
	p.mu.Unlock()
	env.sink.SetFailPuts(true)
	resp, err := env.run(h, in.Status.Plan.Steps[0])
	if status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("with the audit log down = %v, %v; want Unavailable and no output", resp, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1 -run RunCommand`
Expected: FAIL to compile, with `undefined: (*Gateway).runCommand`.

- [ ] **Step 3: Implement**

`internal/gateway/commands.go`:
```go
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// priorityFor derives the sandbox queue priority from the phase (spec §10.1); the
// operator never chooses it.
func priorityFor(stage lastv1alpha1.Stage) sandbox.Priority {
	if stage == lastv1alpha1.StageDiscovery {
		return sandbox.PriorityDiscovery
	}
	return sandbox.PriorityExecution
}

func toProtoResult(runID string, r runnerapi.Result) *gatewayv1.CommandResult {
	return &gatewayv1.CommandResult{RunId: runID, ExitCode: int32(r.ExitCode), Stdout: r.Stdout, Stderr: r.Stderr, //nolint:gosec // exit codes fit
		StdoutTruncated: r.StdoutTruncated, StderrTruncated: r.StderrTruncated, Error: r.Error}
}

// sameCommand compares by canonical JSON: map order and nil-versus-empty do not
// matter; every other difference does.
func sameCommand(a, b lastv1alpha1.Command) bool {
	ca, errA := audit.Canonical(a)
	cb, errB := audit.Canonical(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

func decodeCommand(raw []byte) (lastv1alpha1.Command, error) {
	var c lastv1alpha1.Command
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return lastv1alpha1.Command{}, err
	}
	if dec.More() {
		return lastv1alpha1.Command{}, errors.New("trailing data after the command")
	}
	return c, nil
}

func (g *Gateway) runCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	_, _, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	p := g.phases.get(key)
	if p == nil || p.subject != req.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	if !p.mu.TryLock() {
		return nil, status.Error(codes.Aborted, "a step of this phase is already running")
	}
	defer p.mu.Unlock()
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, stage: string(p.stage), subject: p.subject, rpc: "RunCommand"}
	if p.failed {
		return nil, status.Error(codes.FailedPrecondition, "an earlier step failed or could not run; end the phase")
	}
	if p.next >= len(p.body.Steps) {
		return nil, status.Error(codes.FailedPrecondition, "every step of this phase has run")
	}
	planned := p.body.Steps[p.next]
	if req.GetStepId() != planned.ID {
		return nil, d.deny(codes.FailedPrecondition, "step %q is next, not %q", planned.ID, req.GetStepId())
	}
	given, err := decodeCommand(req.GetCommandJson())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "command_json: %v", err)
	}
	expected, err := plan.Resolve(planned, p.results)
	if err != nil {
		p.failed = true
		return nil, d.deny(codes.FailedPrecondition, "step %s: %v", planned.ID, err)
	}
	if !sameCommand(given, expected) {
		return nil, d.deny(codes.PermissionDenied, "command does not match approved step %s", planned.ID)
	}
	image, err := g.sandboxImage(ctx)
	if err != nil {
		return nil, err
	}
	res, err := g.executor(w, image).Run(ctx, sandbox.RunRequest{Actor: actor, Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth, Command: expected, Priority: priorityFor(p.stage)})
	if err != nil {
		// The outcome may not be recorded: withhold any output, and stop the phase.
		p.failed = true
		if errors.Is(err, audit.ErrAuditUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "step %s: %v", planned.ID, err)
		}
		return nil, status.Errorf(codes.Internal, "step %s: %v", planned.ID, err)
	}
	p.results[planned.ID] = res.Result
	if res.Result.ExitCode == 0 && res.Result.Error == "" {
		p.next++
	} else {
		p.failed = true
	}
	return &gatewayv1.RunCommandResponse{Result: toProtoResult(res.RunID, res.Result)}, nil
}
```
In `rpc.go`:
```go
func (s *rpcServer) RunCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	return s.g.runCommand(ctx, req)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway
git commit -m "feat(gateway): run only the next approved step of an open phase"
```

---

### Task 10: RunProbes — probes of the executed plan only

**Files:**
- Create: `internal/gateway/probes.go`, `internal/gateway/probes_test.go`
- Modify: `internal/gateway/server.go` (the `probing sync.Map` field), `internal/gateway/rpc.go`

**Interfaces:**
- Consumes:
  - From Task 7: `IntentRecord.Executed`.
  - From Task 8: `effectivePolicy`, `denial`.
  - From Task 9: `toProtoResult`, `executeAll`.
  - From M2a: `sandbox.ProbeRunRequest{Actor, Intent, Probes}` and `[]sandbox.ProbeOutcome{ID, Passed, Result, Error}`.
- Semantics (spec §8.3, §10):
  - The probes always come from the gateway's own record of the last executed plan, never from `Intent.status` or a `ManagedResource`.
  - The current policy is applied again before any probe runs.
  - There is at most one probe run in flight per Intent. A second run is refused with `Aborted`; the operator counts the skip.
  - Outcomes are withheld when the executor reports an error.

- [ ] **Step 1: Write the failing tests**

`internal/gateway/probes_test.go`:
```go
package gateway

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func probedResource() lastv1alpha1.ManagedResourceSpec {
	probe := func(id, assert string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Assert: assert, Command: lastv1alpha1.Command{
			Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "ns", "team-x", "-o", "json"}}, Providers: []string{"cluster"}}}
	}
	return lastv1alpha1.ManagedResourceSpec{Description: "namespace", Probes: []lastv1alpha1.Probe{
		probe("exists", "result.exitCode == 0"),
		probe("kind", `result.json.kind == "Namespace"`),
	}}
}

func (e *testEnv) probe() (*gatewayv1.RunProbesResponse, error) {
	return e.g.runProbes(e.ctx, &gatewayv1.RunProbesRequest{Intent: ref()})
}

func TestRunProbesNeedsAnExecutedPlan(t *testing.T) {
	env, _ := newRunEnv(t, twoSteps(), probedResource())
	if _, err := env.probe(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("RunProbes before execution = %v, want FailedPrecondition", err)
	}
}

func TestRunProbesRunsTheExecutedPlansProbes(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)

	// The operator rewrites the probes in status after execution; the gateway must ignore that.
	var cur lastv1alpha1.Intent
	if err := env.crd.Get(env.ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Plan.Resources[0].Probes[0].Command.Run.Argv = []string{"rm", "-rf", "/"}
	if err := env.crd.Update(env.ctx, &cur); err != nil {
		t.Fatal(err)
	}

	resp, err := env.probe()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPlanHash() != in.Status.Plan.Hash || len(resp.GetOutcomes()) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	for _, o := range resp.GetOutcomes() {
		if !o.GetPassed() {
			t.Errorf("probe %s failed: %s", o.GetId(), o.GetError())
		}
	}
	var start string
	for _, e := range env.entries() {
		if e.Type == audit.TypeProbeStart {
			start = string(e.Payload)
			if e.Actor != testOperator || e.Intent != envKey {
				t.Fatalf("probe.start = %+v", e)
			}
		}
	}
	if strings.Contains(start, `"rm"`) || !strings.Contains(start, `"kubectl"`) {
		t.Fatalf("probe.start ran the operator's edited probe: %s", start)
	}
}

func TestRunProbesAppliesTheCurrentPolicy(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	op := allowAll()
	op.Spec.Defaults.Providers = []string{"other"}
	var cur lastv1alpha1.OperatorConfig
	if err := env.crd.Get(env.ctx, client.ObjectKey{Name: "default"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec = op.Spec
	if err := env.crd.Update(env.ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := env.probe(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RunProbes under a tightened policy = %v, want PermissionDenied", err)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatal("no policy.denied entry")
	}
}

func TestRunProbesAllowsOneRunInFlight(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.g.probing.Store(envKey, struct{}{})
	if _, err := env.probe(); status.Code(err) != codes.Aborted {
		t.Fatalf("overlapping probe run = %v, want Aborted", err)
	}
	env.g.probing.Delete(envKey)
	if _, err := env.probe(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1 -run RunProbes`
Expected: FAIL to compile, with `undefined: (*Gateway).runProbes`.

- [ ] **Step 3: Implement**

Add `probing sync.Map // intent key -> in-flight probe run` to `Gateway`.

`internal/gateway/probes.go`:
```go
package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func (g *Gateway) runProbes(ctx context.Context, req *gatewayv1.RunProbesRequest) (*gatewayv1.RunProbesResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	if _, busy := g.probing.LoadOrStore(key, struct{}{}); busy {
		return nil, status.Error(codes.Aborted, "a probe run for this intent is already in flight")
	}
	defer g.probing.Delete(key)

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if rec.Executed == nil {
		return nil, status.Error(codes.FailedPrecondition, "intent has no executed plan to probe")
	}
	var body plan.Body
	if err := json.Unmarshal([]byte(rec.Executed.Plan), &body); err != nil {
		return nil, status.Errorf(codes.Internal, "executed plan record is corrupt: %v", err)
	}
	var probes []lastv1alpha1.Probe
	for _, r := range body.Resources {
		probes = append(probes, r.Probes...)
	}
	resp := &gatewayv1.RunProbesResponse{PlanHash: rec.Executed.PlanHash}
	if len(probes) == 0 {
		return resp, nil
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, subject: rec.Executed.PlanHash, rpc: "RunProbes"}
	eff, err := g.effectivePolicy(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, p := range probes {
		if err := eff.CheckCommand(p.Command); err != nil {
			return nil, d.deny(codes.PermissionDenied, "probe %s: %v", p.ID, err)
		}
	}
	image, err := g.sandboxImage(ctx)
	if err != nil {
		return nil, err
	}
	outs, err := g.executor(w, image).RunProbes(ctx, sandbox.ProbeRunRequest{Actor: actor, Intent: key, Probes: probes})
	if err != nil {
		if errors.Is(err, audit.ErrAuditUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "probe run: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "probe run: %v", err)
	}
	for _, o := range outs {
		resp.Outcomes = append(resp.Outcomes, &gatewayv1.ProbeOutcome{Id: o.ID, Passed: o.Passed, Result: toProtoResult("", o.Result), Error: o.Error})
	}
	return resp, nil
}
```
In `rpc.go`:
```go
func (s *rpcServer) RunProbes(ctx context.Context, req *gatewayv1.RunProbesRequest) (*gatewayv1.RunProbesResponse, error) {
	return s.g.runProbes(ctx, req)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway
git commit -m "feat(gateway): run the executed plan's probes, one run per intent"
```

---

### Task 11: WriteOutputSecret — step outputs into an owned Secret

**Files:**
- Create: `internal/gateway/outputs.go`, `internal/gateway/outputs_test.go`
- Modify: `internal/gateway/rpc.go`

**Interfaces:**
- Consumes:
  - From Task 8: `phase`, `denial`, `intentKey`, `notFoundOr`.
  - From Task 1: `plan.Results.Value`.
  - From Task 4: `audit.TypeOutputWritten`.
  - From Task 9: `runAll` and `newRunEnv`. The `kubectlJSON` stdout includes `"password":"s3cr3t-value-123"`.
- Semantics (spec §15, §14.2):
  - **Where it runs.** The call needs an open phase, and the ManagedResource must belong to that Intent: `spec.intentRef.name` must match, and it must carry a controller ownerReference to the Intent's UID.
  - **Which Secret.** The Secret name always comes from the ManagedResource's `spec.outputsSecretRef`, never from the request.
  - **Where values come from.** Each value is a step reference resolved from this phase's results. Literal values are impossible: `Value` accepts only `${steps…}` expressions.
  - **Overwrite protection.** An existing Secret is overwritten only if it is this ManagedResource's output Secret, which means it is labelled `managed-by=last-gateway` and owned by the ManagedResource's UID. Any other existing Secret is refused, never overwritten.
  - **Audit entry.** `output.written` is written before the Secret, with each value as `sha256:<hex>`.

- [ ] **Step 1: Write the failing tests**

`internal/gateway/outputs_test.go`:
```go
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

const outputValue = "s3cr3t-value-123"

func managedResource(ownerUID types.UID) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: "web-db", Namespace: envNS, UID: "mr-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "Intent",
				Name: envIntent, UID: ownerUID, Controller: ptr.To(true)}}},
		Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: envIntent},
			OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-db-outputs"}},
	}
}

func (e *testEnv) writeOutputs(subject, mr string, values map[string]string) (*gatewayv1.WriteOutputSecretResponse, error) {
	return e.g.writeOutputSecret(e.ctx, &gatewayv1.WriteOutputSecretRequest{Intent: ref(), SubjectHash: subject, ManagedResource: mr, Values: values})
}

func newOutputEnv(t *testing.T, ownerUID types.UID) (*testEnv, string) {
	t.Helper()
	env, in := newRunEnv(t, twoSteps())
	if err := env.crd.Create(context.Background(), managedResource(ownerUID)); err != nil {
		t.Fatal(err)
	}
	runAll(t, env, in)
	return env, in.Status.Plan.Hash
}

func TestWriteOutputSecretWritesHashedAuditAndOwnedSecret(t *testing.T) {
	env, h := newOutputEnv(t, "intent-uid")
	resp, err := env.writeOutputs(h, "web-db", map[string]string{"password": "${steps.s1.json.password}"})
	if err != nil || resp.GetSecretName() != "web-db-outputs" {
		t.Fatalf("WriteOutputSecret = %v, %v", resp, err)
	}
	s, err := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), "web-db-outputs", metav1.GetOptions{})
	if err != nil || string(s.Data["password"]) != outputValue || s.Labels["app.kubernetes.io/managed-by"] != "last-gateway" ||
		len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "mr-uid" || s.OwnerReferences[0].Kind != "ManagedResource" {
		t.Fatalf("secret = %+v, %v", s, err)
	}
	last := env.last()
	if last.Type != audit.TypeOutputWritten || last.Actor != testOperator || last.AuthorizedBy == nil {
		t.Fatalf("last entry = %+v", last)
	}
	var p outputWritten
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(outputValue))
	if p.Values["password"] != "sha256:"+hex.EncodeToString(sum[:]) || p.Secret != "web-db-outputs" || p.ManagedResource != "web-db" {
		t.Fatalf("output.written payload = %+v", p)
	}
	for _, e := range env.entries() {
		if e.Type != audit.TypeCommandResult && strings.Contains(string(e.Payload), outputValue) {
			t.Fatalf("entry %s carries the output value", e.Type)
		}
	}
}

func TestWriteOutputSecretRefusals(t *testing.T) {
	t.Run("managed resource of another intent", func(t *testing.T) {
		env, h := newOutputEnv(t, "someone-else")
		if _, err := env.writeOutputs(h, "web-db", map[string]string{"password": "${steps.s1.json.password}"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("err = %v, want PermissionDenied", err)
		}
		if env.last().Type != audit.TypePolicyDenied {
			t.Fatal("no policy.denied")
		}
	})
	t.Run("existing user secret is never overwritten", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		mine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-db-outputs", Namespace: envNS}, Data: map[string][]byte{"k": []byte("user")}}
		if _, err := env.kube.CoreV1().Secrets(envNS).Create(context.Background(), mine, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.writeOutputs(h, "web-db", map[string]string{"password": "${steps.s1.json.password}"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("err = %v, want PermissionDenied", err)
		}
		s, _ := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), "web-db-outputs", metav1.GetOptions{})
		if string(s.Data["k"]) != "user" || s.Data["password"] != nil {
			t.Fatalf("the user's secret was modified: %+v", s.Data)
		}
	})
	t.Run("own secret is updated", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		values := map[string]string{"password": "${steps.s1.json.password}"}
		if _, err := env.writeOutputs(h, "web-db", values); err != nil {
			t.Fatal(err)
		}
		values["name"] = "${steps.s1.json.metadata.name}"
		if _, err := env.writeOutputs(h, "web-db", values); err != nil {
			t.Fatalf("second write = %v", err)
		}
		s, _ := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), "web-db-outputs", metav1.GetOptions{})
		if string(s.Data["name"]) != "team-x" {
			t.Fatalf("data = %v", s.Data)
		}
	})
	t.Run("bad reference, bad key, literal value, no phase", func(t *testing.T) {
		env, h := newOutputEnv(t, "intent-uid")
		cases := map[string]struct {
			values map[string]string
			code   codes.Code
		}{
			"missing key": {map[string]string{"p": "${steps.s1.json.nope}"}, codes.FailedPrecondition},
			"literal":     {map[string]string{"p": "hunter2"}, codes.FailedPrecondition},
			"bad key":     {map[string]string{"pass word": "${steps.s1.json.password}"}, codes.InvalidArgument},
			"no values":   {map[string]string{}, codes.InvalidArgument},
		}
		for name, tc := range cases {
			if _, err := env.writeOutputs(h, "web-db", tc.values); status.Code(err) != tc.code {
				t.Errorf("%s: %v, want %v", name, err, tc.code)
			}
		}
		if _, err := env.writeOutputs("sha256:"+strings.Repeat("0", 64), "web-db", map[string]string{"p": "${steps.s1.stdout}"}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("no phase: %v", err)
		}
	})
}
```
If `k8s.io/utils/ptr` is not already a dependency, use a local `t := true; Controller: &t` instead.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -count=1 -run WriteOutputSecret`
Expected: FAIL to compile, with `undefined: (*Gateway).writeOutputSecret`.

- [ ] **Step 3: Implement**

`internal/gateway/outputs.go`:
```go
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

var secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// outputWritten is the output.written payload; values are recorded only as hashes (spec §14.2).
type outputWritten struct {
	ManagedResource string            `json:"managedResource"`
	Secret          string            `json:"secret"`
	Values          map[string]string `json:"values"`
}

func ownedByIntent(mr *lastv1alpha1.ManagedResource, intent *lastv1alpha1.Intent) bool {
	if mr.Spec.IntentRef == nil || mr.Spec.IntentRef.Name != intent.Name {
		return false
	}
	for _, o := range mr.OwnerReferences {
		if o.UID == intent.UID && o.Kind == "Intent" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

func isOutputSecretOf(s *corev1.Secret, mr *lastv1alpha1.ManagedResource) bool {
	if s.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue {
		return false
	}
	for _, o := range s.OwnerReferences {
		if o.UID == mr.UID && o.Kind == "ManagedResource" {
			return true
		}
	}
	return false
}

func (g *Gateway) writeOutputSecret(ctx context.Context, req *gatewayv1.WriteOutputSecretRequest) (*gatewayv1.WriteOutputSecretResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	p := g.phases.get(key)
	if p == nil || p.subject != req.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	if !p.mu.TryLock() {
		return nil, status.Error(codes.Aborted, "a step of this phase is already running")
	}
	defer p.mu.Unlock()
	if req.GetManagedResource() == "" || len(req.GetValues()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "managed_resource and values are required")
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, stage: string(p.stage), subject: p.subject, rpc: "WriteOutputSecret"}

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	var mr lastv1alpha1.ManagedResource
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.GetManagedResource()}, &mr); err != nil {
		return nil, notFoundOr(err, "managed resource")
	}
	if !ownedByIntent(&mr, &intent) {
		return nil, d.deny(codes.PermissionDenied, "managed resource %s is not owned by intent %s", mr.Name, key)
	}
	if mr.Spec.OutputsSecretRef == nil {
		return nil, status.Errorf(codes.InvalidArgument, "managed resource %s has no outputsSecretRef", mr.Name)
	}
	secretName := mr.Spec.OutputsSecretRef.Name

	data := make(map[string][]byte, len(req.GetValues()))
	hashes := make(map[string]string, len(req.GetValues()))
	for k, expr := range req.GetValues() {
		if !secretKeyPattern.MatchString(k) {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not a valid Secret key", k)
		}
		v, err := p.results.Value(expr)
		if err != nil {
			return nil, d.deny(codes.FailedPrecondition, "value %q: %v", k, err)
		}
		sum := sha256.Sum256([]byte(v))
		data[k], hashes[k] = []byte(v), "sha256:"+hex.EncodeToString(sum[:])
	}

	secrets := g.kube.CoreV1().Secrets(ns)
	existing, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "read secret: %v", err)
	case !isOutputSecretOf(existing, &mr):
		return nil, d.deny(codes.PermissionDenied, "secret %s/%s exists and is not %s's output secret", ns, secretName, mr.Name)
	}
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypeOutputWritten, Actor: actor, Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth,
		Payload: outputWritten{ManagedResource: mr.Name, Secret: secretName, Values: hashes}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "record output.written: %v", err)
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns,
			Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "ManagedResource",
				Name: mr.Name, UID: mr.UID}}},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if existing == nil {
		_, err = secrets.Create(ctx, desired, metav1.CreateOptions{})
	} else {
		desired.ResourceVersion = existing.ResourceVersion
		_, err = secrets.Update(ctx, desired, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write output secret: %v", err)
	}
	return &gatewayv1.WriteOutputSecretResponse{SecretName: secretName}, nil
}
```
In `rpc.go`:
```go
func (s *rpcServer) WriteOutputSecret(ctx context.Context, req *gatewayv1.WriteOutputSecretRequest) (*gatewayv1.WriteOutputSecretResponse, error) {
	return s.g.writeOutputSecret(ctx, req)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/ -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway
git commit -m "feat(gateway): write step outputs to a managed resource's own output secret"
```

---

### Task 12: CallLLM with the scripted provider

**Files:**
- Create: `internal/gateway/llm/llm.go`, `internal/gateway/llm/scripted.go`, `internal/gateway/llm/scripted_test.go`, `internal/gateway/llmrpc.go`, `internal/gateway/llmrpc_test.go`
- Modify: `internal/gateway/rpc.go`

**Interfaces:**
- Consumes: from Task 4, `lastv1alpha1.LLMScripted` and `LLMProviderSpec.Scripted`; from Task 6, `operatorConfig` and `beginRPC`; `audit.Writer.Do` and `audit.TypeLLMRequest/TypeLLMResponse`.
- Produces (package `llm`):
  - `type Message struct{ Role, Content string }`, with JSON fields `role` and `content`;
  - `type Request struct{ Intent string; Messages []Message; MaxTokens int }`;
  - `type Response struct{ Content string; InputTokens, OutputTokens int }`;
  - `type Provider interface{ Complete(ctx, Request) (Response, error) }`;
  - `NewScripted(kube kubernetes.Interface, namespace, configMap string) *Scripted`;
  - `ScriptKey(intent string) string`, which turns `"ns/name"` into `"ns.name"`;
  - `ErrNoScript` and `ErrScriptExhausted`.
- Semantics:
  - `llm.request` is written before the call and `llm.response` after it, through `Do`. The response entry carries the content and any error.
  - Content is returned only when `Do` succeeds.
  - Provider types other than `scripted` return `Unimplemented` until M3.

- [ ] **Step 1: Write the failing tests**

`internal/gateway/llm/scripted_test.go`:
```go
package llm_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/gateway/llm"
)

func script(data map[string]string) *fake.Clientset {
	return fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "llm-script", Namespace: "last-gateway"}, Data: data})
}

func TestScriptedAnswersByAssistantTurnCount(t *testing.T) {
	s := llm.NewScripted(script(map[string]string{"team-x.web": `["plan A","plan B"]`}), "last-gateway", "llm-script")
	ctx := context.Background()
	r, err := s.Complete(ctx, llm.Request{Intent: "team-x/web", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil || r.Content != "plan A" {
		t.Fatalf("first = %q, %v", r.Content, err)
	}
	r, err = s.Complete(ctx, llm.Request{Intent: "team-x/web", Messages: []llm.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "plan A"}, {Role: "user", Content: "again"}}})
	if err != nil || r.Content != "plan B" {
		t.Fatalf("second = %q, %v", r.Content, err)
	}
	_, err = s.Complete(ctx, llm.Request{Intent: "team-x/web", Messages: []llm.Message{
		{Role: "assistant"}, {Role: "assistant"}}})
	if !errors.Is(err, llm.ErrScriptExhausted) {
		t.Fatalf("exhausted = %v", err)
	}
}

func TestScriptedErrors(t *testing.T) {
	ctx := context.Background()
	req := llm.Request{Intent: "team-x/web", Messages: []llm.Message{{Role: "user"}}}
	if _, err := llm.NewScripted(script(map[string]string{}), "last-gateway", "llm-script").Complete(ctx, req); !errors.Is(err, llm.ErrNoScript) {
		t.Fatalf("missing key = %v", err)
	}
	if _, err := llm.NewScripted(script(map[string]string{"team-x.web": `{}`}), "last-gateway", "llm-script").Complete(ctx, req); err == nil {
		t.Fatal("a non-array script must be an error")
	}
	if _, err := llm.NewScripted(fake.NewClientset(), "last-gateway", "llm-script").Complete(ctx, req); err == nil {
		t.Fatal("a missing ConfigMap must be an error")
	}
	if llm.ScriptKey("team-x/web") != "team-x.web" {
		t.Fatal("ScriptKey")
	}
}
```
`internal/gateway/llmrpc_test.go`:
```go
package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func llmConfig(providerType lastv1alpha1.LLMProviderType) (*lastv1alpha1.OperatorConfig, *lastv1alpha1.LLMProvider) {
	op := allowAll()
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: "fixture"}
	lp := &lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, Spec: lastv1alpha1.LLMProviderSpec{Type: providerType, Model: "m"}}
	if providerType == lastv1alpha1.LLMScripted {
		lp.Spec.Scripted = &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: "llm-script"}}
	} else {
		lp.Spec.SecretRef = &lastv1alpha1.LocalRef{Name: "key"}
	}
	return op, lp
}

func newLLMEnv(t *testing.T, providerType lastv1alpha1.LLMProviderType) *testEnv {
	t.Helper()
	op, lp := llmConfig(providerType)
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if _, err := env.kube.CoreV1().ConfigMaps("last-gateway").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-script", Namespace: "last-gateway"},
		Data:       map[string]string{"team-x.web": `["the plan"]`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *testEnv) callLLM(msgs ...*gatewayv1.Message) (*gatewayv1.CallLLMResponse, error) {
	return e.g.callLLM(e.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), Messages: msgs, MaxTokens: 1024})
}

func TestCallLLMAuditsRequestAndResponse(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	resp, err := env.callLLM(&gatewayv1.Message{Role: "user", Content: "run nginx"})
	if err != nil || resp.GetContent() != "the plan" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
	es := env.entries()
	req, res := es[len(es)-2], es[len(es)-1]
	if req.Type != audit.TypeLLMRequest || res.Type != audit.TypeLLMResponse || req.Actor != testOperator || req.Intent != envKey {
		t.Fatalf("entries = %s, %s", req.Type, res.Type)
	}
	var rp llmResponsePayload
	if err := json.Unmarshal(res.Payload, &rp); err != nil || rp.RequestSeq != req.Seq || rp.Content != "the plan" {
		t.Fatalf("llm.response = %s", res.Payload)
	}
	if _, err := env.callLLM(&gatewayv1.Message{Role: "assistant"}, &gatewayv1.Message{Role: "user"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("exhausted script = %v, want FailedPrecondition", err)
	}
	var last llmResponsePayload
	_ = json.Unmarshal(env.last().Payload, &last)
	if env.last().Type != audit.TypeLLMResponse || last.Error == "" {
		t.Fatal("a failed call must still record llm.response with the error")
	}
}

func TestCallLLMRefusals(t *testing.T) {
	user := &gatewayv1.Message{Role: "user", Content: "x"}
	if _, err := newLLMEnv(t, lastv1alpha1.LLMAnthropic).callLLM(user); status.Code(err) != codes.Unimplemented {
		t.Fatalf("anthropic = %v, want Unimplemented", err)
	}
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	if _, err := env.callLLM(&gatewayv1.Message{Role: "tool", Content: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad role = %v", err)
	}
	if _, err := env.callLLM(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no messages = %v", err)
	}
	env.g.writer.Store(nil)
	if _, err := env.callLLM(user); status.Code(err) != codes.Unavailable {
		t.Fatalf("no writer = %v", err)
	}
	noLLM := newTestEnv(t, intentWithPlan(t, twoSteps()), allowAll())
	if _, err := noLLM.callLLM(user); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no provider configured = %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/... -count=1 -run 'Scripted|CallLLM'`
Expected: FAIL. The `llm` package does not exist yet, and `(*Gateway).callLLM` is undefined.

- [ ] **Step 3: Implement**

`internal/gateway/llm/llm.go`:
```go
// Package llm is the gateway's provider-neutral LLM interface and its adapters (spec §12).
package llm

import "context"

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
}

// Response is the model's reply.
type Response struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

// Provider completes conversations; adapters translate for each vendor.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
```
`internal/gateway/llm/scripted.go`:
```go
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// ErrNoScript means the script ConfigMap has no entry for the intent.
	ErrNoScript = errors.New("llm: no script for this intent")
	// ErrScriptExhausted means the conversation already used every scripted response.
	ErrScriptExhausted = errors.New("llm: scripted conversation has no more responses")
)

// Scripted replays fixed responses for tests and demos (spec §18). The ConfigMap
// maps ScriptKey(intent) to a JSON array of strings; a conversation that already
// holds n assistant messages gets response n.
type Scripted struct {
	kube      kubernetes.Interface
	namespace string
	configMap string
}

func NewScripted(kube kubernetes.Interface, namespace, configMap string) *Scripted {
	return &Scripted{kube: kube, namespace: namespace, configMap: configMap}
}

// ScriptKey turns "<namespace>/<name>" into a valid ConfigMap key.
func ScriptKey(intent string) string { return strings.Replace(intent, "/", ".", 1) }

func (s *Scripted) Complete(ctx context.Context, req Request) (Response, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.configMap, metav1.GetOptions{})
	if err != nil {
		return Response{}, fmt.Errorf("llm: read script %s/%s: %w", s.namespace, s.configMap, err)
	}
	key := ScriptKey(req.Intent)
	raw, ok := cm.Data[key]
	if !ok {
		return Response{}, fmt.Errorf("%w: %s", ErrNoScript, key)
	}
	var responses []string
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return Response{}, fmt.Errorf("llm: script %s is not a JSON array of strings: %w", key, err)
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	if n >= len(responses) {
		return Response{}, fmt.Errorf("%w (%d of %d used)", ErrScriptExhausted, n, len(responses))
	}
	return Response{Content: responses[n]}, nil
}
```
`internal/gateway/llmrpc.go`:
```go
package gateway

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

type llmRequestPayload struct {
	Provider  string        `json:"provider"`
	Model     string        `json:"model"`
	Messages  []llm.Message `json:"messages"`
	MaxTokens int           `json:"maxTokens,omitempty"`
}

type llmResponsePayload struct {
	RequestSeq   uint64 `json:"requestSeq"`
	Content      string `json:"content"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	Error        string `json:"error,omitempty"`
}

var llmRoles = map[string]bool{"system": true, "user": true, "assistant": true}

func (g *Gateway) llmProvider(lp *lastv1alpha1.LLMProvider) (llm.Provider, error) {
	switch lp.Spec.Type {
	case lastv1alpha1.LLMScripted:
		if lp.Spec.Scripted == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no scripted config", lp.Name)
		}
		return llm.NewScripted(g.kube, g.cfg.Namespace, lp.Spec.Scripted.ConfigMapRef.Name), nil
	default:
		return nil, status.Errorf(codes.Unimplemented, "LLM provider type %q arrives with the real adapters (M3)", lp.Spec.Type)
	}
}

func (g *Gateway) callLLM(ctx context.Context, req *gatewayv1.CallLLMRequest) (*gatewayv1.CallLLMResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	if len(req.GetMessages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "messages are required")
	}
	msgs := make([]llm.Message, 0, len(req.GetMessages()))
	for _, m := range req.GetMessages() {
		if !llmRoles[m.GetRole()] {
			return nil, status.Errorf(codes.InvalidArgument, "role %q is not system, user, or assistant", m.GetRole())
		}
		msgs = append(msgs, llm.Message{Role: m.GetRole(), Content: m.GetContent()})
	}
	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return nil, err
	}
	if op == nil || op.Spec.LLMProviderRef == nil {
		return nil, status.Error(codes.FailedPrecondition, "no LLM provider configured in OperatorConfig")
	}
	var lp lastv1alpha1.LLMProvider
	if err := g.crd.Get(ctx, client.ObjectKey{Name: op.Spec.LLMProviderRef.Name}, &lp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s not found", op.Spec.LLMProviderRef.Name)
		}
		return nil, status.Errorf(codes.Unavailable, "read LLMProvider: %v", err)
	}
	provider, err := g.llmProvider(&lp)
	if err != nil {
		return nil, err
	}
	actor := CallerFrom(ctx)
	lreq := llm.Request{Intent: key, Messages: msgs, MaxTokens: int(req.GetMaxTokens())}
	var resp llm.Response
	err = w.Do(ctx,
		audit.Draft{Type: audit.TypeLLMRequest, Actor: actor, Intent: key,
			Payload: llmRequestPayload{Provider: lp.Name, Model: lp.Spec.Model, Messages: msgs, MaxTokens: lreq.MaxTokens}},
		func(ctx context.Context) (any, error) {
			r, err := provider.Complete(ctx, lreq)
			resp = r
			return r, err
		},
		func(before audit.Entry, _ any, actionErr error) audit.Draft {
			p := llmResponsePayload{RequestSeq: before.Seq, Content: resp.Content, InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			return audit.Draft{Type: audit.TypeLLMResponse, Actor: actor, Intent: key, Payload: p}
		})
	switch {
	case err == nil:
		return &gatewayv1.CallLLMResponse{Content: resp.Content, InputTokens: int32(resp.InputTokens), OutputTokens: int32(resp.OutputTokens)}, nil //nolint:gosec // token counts fit
	case errors.Is(err, audit.ErrAuditUnavailable):
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	case errors.Is(err, llm.ErrNoScript), errors.Is(err, llm.ErrScriptExhausted):
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return nil, status.Errorf(codes.Unavailable, "LLM call failed: %v", err)
	}
}
```
In `rpc.go`:
```go
func (s *rpcServer) CallLLM(ctx context.Context, req *gatewayv1.CallLLMRequest) (*gatewayv1.CallLLMResponse, error) {
	return s.g.callLLM(ctx, req)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/gateway/... -count=1`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint-fix && make lint
git add internal/gateway
git commit -m "feat(gateway): call the configured LLM with request and response audited"
```

---

### Task 13: IntentApproval admission policy

**Files:**
- Create: `charts/last-operator/files/intentapproval-policy.yaml`, `charts/last-operator/templates/admission.yaml`, `test/admission/admission_test.go`

**Interfaces:**
- Produces a cluster-scoped `ValidatingAdmissionPolicy` and binding, both named `last-intentapproval` (spec §5.2):
  - On CREATE, `spec.approvedBy` must equal `request.userInfo.username`.
  - Every UPDATE is refused, because approvals are immutable. The CRD's CEL rule also covers `spec`, and this rule covers labels and annotations as well.
  - Any ServiceAccount named `last-operator` is refused, in any namespace. That keeps the policy free of templating, so the test loads the exact file the chart ships.

- [ ] **Step 1: Write the policy file**

`charts/last-operator/files/intentapproval-policy.yaml`:
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: last-intentapproval
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["last.moogie-labs.com"]
        apiVersions: ["v1alpha1"]
        operations: ["CREATE", "UPDATE"]
        resources: ["intentapprovals"]
  validations:
    - expression: "request.operation == 'CREATE'"
      message: "IntentApprovals are immutable"
    - expression: "object.spec.approvedBy == request.userInfo.username"
      message: "spec.approvedBy must be the requesting user"
    - expression: "!(request.userInfo.username.startsWith('system:serviceaccount:') && request.userInfo.username.endsWith(':last-operator'))"
      message: "the operator may not create IntentApprovals"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: last-intentapproval
spec:
  policyName: last-intentapproval
  validationActions: [Deny]
```
`charts/last-operator/templates/admission.yaml`:
```yaml
{{ .Files.Get "files/intentapproval-policy.yaml" }}
```

- [ ] **Step 2: Write the failing test**

`test/admission/admission_test.go`:
```go
// Package admission_test checks the chart's admission policies against a real API server.
package admission_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	operatorUser = "system:serviceaccount:last-system:last-operator"
	approvalsNS  = "approvals"
)

var (
	adminCfg *rest.Config
	scheme   = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = lastv1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\nset KUBEBUILDER_ASSETS: export KUBEBUILDER_ASSETS=\"$(make -s envtest-assets)\"\n", err)
		os.Exit(1)
	}
	adminCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func clientAs(t *testing.T, user string, groups ...string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(adminCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: append(groups, "system:authenticated")}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func applyChartPolicy(t *testing.T, admin client.Client) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "last-operator", "files", "intentapproval-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		if err := admin.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func grantApprovers(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "approver"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"last.moogie-labs.com"}, Resources: []string{"intentapprovals"},
		Verbs: []string{"create", "get", "update", "patch"}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "approver"},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "approver"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"},
			{Kind: rbacv1.ServiceAccountKind, Name: "last-operator", Namespace: "last-system"},
		}}
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: approvalsNS}}} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}

func approval(name, by string) *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: approvalsNS},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: "sha256:" + strings.Repeat("a", 64), Decision: lastv1alpha1.DecisionApprove, ApprovedBy: by}}
}

func TestIntentApprovalAdmission(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	grantApprovers(t, admin)
	applyChartPolicy(t, admin)
	alice := clientAs(t, "alice")
	operator := clientAs(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:last-system")

	// Admission policies take effect asynchronously; wait for enforcement.
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		err := alice.Create(ctx, approval(fmt.Sprintf("warmup-%d", i), "bob"))
		if err != nil && strings.Contains(err.Error(), "must be the requesting user") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy never took effect: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := alice.Create(ctx, approval("ok", "alice")); err != nil {
		t.Fatalf("alice approving as herself: %v", err)
	}
	if err := alice.Create(ctx, approval("spoof", "bob")); err == nil || !strings.Contains(err.Error(), "must be the requesting user") {
		t.Fatalf("spoofed approvedBy = %v", err)
	}
	if err := operator.Create(ctx, approval("self", operatorUser)); err == nil || !strings.Contains(err.Error(), "operator may not") {
		t.Fatalf("operator approval = %v", err)
	}
	var got lastv1alpha1.IntentApproval
	if err := alice.Get(ctx, client.ObjectKey{Namespace: approvalsNS, Name: "ok"}, &got); err != nil {
		t.Fatal(err)
	}
	got.Labels = map[string]string{"tampered": "yes"}
	if err := alice.Update(ctx, &got); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update = %v, want immutable", err)
	}
}
```

- [ ] **Step 3: Run the test**

Run: `make test 2>&1 | grep -E 'admission|FAIL|ok ' | head`
Expected: the test passes. Before this task created the policy file, it failed with a file-not-found error. The `Makefile` `test` target already runs every package except `/e2e`, with `KUBEBUILDER_ASSETS` set.

- [ ] **Step 4: Render check and commit**

Run: `make helm-lint`
Expected: PASS. The policy renders through `.Files.Get`.
```bash
make lint-fix && make lint
git add charts/last-operator/files charts/last-operator/templates/admission.yaml test/admission
git commit -m "feat(chart): enforce approver identity and immutability on IntentApprovals"
```

---

### Task 14: Chart — gateway RBAC, sandbox settings, drain

**Files:**
- Modify: `charts/last-operator/templates/gateway.yaml`, `charts/last-operator/values.yaml`, `Makefile` (`helm-lint`), `hack/e2e.sh`

**Interfaces:**
- Consumes:
  - from Task 6, `LAST_SANDBOX_NAMESPACE`, `LAST_SANDBOX_IMAGE`, `LAST_EGRESS_CANARIES` and `LAST_DRAIN_TIMEOUT`;
  - the RPCs' RBAC needs, which are spec §3's gateway column, scoped down to what M2b uses.
- Produces:
  - **Gateway ServiceAccount permissions** sufficient for every M2b RPC.
  - **Values** `sandbox.image` (fallback image, default `""`) and `sandbox.egressCanaries` (default `""`). An empty `sandbox.egressCanaries` renders as `service/<gateway ns>/last-gateway:8080,node:10250`.
  - **Drain timing.** `terminationGracePeriodSeconds: 330`, so the 5-minute drain can complete.

- [ ] **Step 1: Add failing render checks**

Append to the `helm-lint` recipe in the `Makefile`:
```make
	helm template last charts/last-operator -n last-system | grep -q '^kind: ValidatingAdmissionPolicy$$'
	helm template last charts/last-operator -n last-system | grep -q 'name: LAST_EGRESS_CANARIES, value: "service/last-gateway/last-gateway:8080,node:10250"'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \[pods/log\]'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \[serviceaccounts/token\]'
	helm template last charts/last-operator -n last-system | grep -q 'terminationGracePeriodSeconds: 330'
	helm template last charts/last-operator -n last-system --set sandbox.image=img:dev | grep -q 'name: LAST_SANDBOX_IMAGE, value: "img:dev"'
	! helm template last charts/last-operator -n last-system | grep -q 'LAST_SANDBOX_IMAGE'
```
Run: `make helm-lint`
Expected: FAIL at the `LAST_EGRESS_CANARIES` check. The `ValidatingAdmissionPolicy` check passes already, because Task 13 added it.

- [ ] **Step 2: Implement the chart changes**

In `charts/last-operator/values.yaml`, add a top-level block:
```yaml
sandbox:
  # Fallback sandbox image, used only while OperatorConfig spec.sandbox.baseImage is
  # unset (development and e2e). Production pins the image by digest in OperatorConfig.
  image: ""
  # Comma-separated egress canaries (spec §11.1): IP:port, service/<ns>/<name>:<port>,
  # or node:<port>. Empty means the gateway Service's http port and a node's kubelet.
  egressCanaries: ""
```
In `templates/gateway.yaml`, replace the `last-gateway` Role's rules with:
```yaml
rules:
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, list, create, delete]   # signing key, provider credentials, per-run token anchors
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get, list, create, update]   # public key, LLM scripts, gateway-owned intent records
  - apiGroups: [""]
    resources: [serviceaccounts/token]
    verbs: [create]                      # kubernetes Provider tokens, bound to the run anchor
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [create]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    resourceNames: [last-gateway-audit-writer]
    verbs: [get, update]
```
After the existing RoleBinding, add:
```yaml
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: last-gateway-sandbox
  namespace: {{ .Values.namespaces.sandbox }}
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [create, get, list, delete]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [create, list, delete]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: [create, list, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: last-gateway-sandbox
  namespace: {{ .Values.namespaces.sandbox }}
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: last-gateway-sandbox}
subjects:
  - {kind: ServiceAccount, name: last-gateway, namespace: {{ $gw }}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: last-gateway-apiserver
  namespace: default
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: [services]
    resourceNames: [kubernetes]
    verbs: [get]
  - apiGroups: [discovery.k8s.io]
    resources: [endpointslices]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: last-gateway-apiserver
  namespace: default
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: last-gateway-apiserver}
subjects:
  - {kind: ServiceAccount, name: last-gateway, namespace: {{ $gw }}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: last-gateway-lifecycle
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: [last.moogie-labs.com]
    resources: [intents, intentapprovals, managedresources, intentpolicies, operatorconfigs, providers, llmproviders]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create, update]         # output Secrets in Intent namespaces (spec §15)
  - apiGroups: [""]
    resources: [services]
    verbs: [get]                         # service egress canaries
  - apiGroups: [""]
    resources: [nodes]
    verbs: [list]                        # the node egress canary
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: last-gateway-lifecycle
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: last-gateway-lifecycle}
subjects:
  - {kind: ServiceAccount, name: last-gateway, namespace: {{ $gw }}}
```
In the Deployment pod spec, add `terminationGracePeriodSeconds: 330 # covers LAST_DRAIN_TIMEOUT (5m)` next to `serviceAccountName`. Then append these to the container `env:` list, after `LAST_OPERATOR_IDENTITY`:
```yaml
            - {name: LAST_SANDBOX_NAMESPACE, value: {{ .Values.namespaces.sandbox | quote }}}
            - {name: LAST_EGRESS_CANARIES, value: {{ .Values.sandbox.egressCanaries | default (printf "service/%s/last-gateway:8080,node:10250" $gw) | quote }}}
            - {name: LAST_DRAIN_TIMEOUT, value: "5m"}
            {{- with .Values.sandbox.image }}
            - {name: LAST_SANDBOX_IMAGE, value: {{ . | quote }}}
            {{- end }}
```
In `hack/e2e.sh`, add `--set sandbox.image="$REGISTRY/last-sandbox-base:$TAG"` to the `helm upgrade --install` command.

- [ ] **Step 3: Run the render checks**

Run: `make helm-lint`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add charts/last-operator Makefile hack/e2e.sh
git commit -m "feat(chart): grant the gateway its lifecycle and sandbox permissions"
```

---

### Task 15: End-to-end — lifecycle over gRPC and token revocation on kind

**Files:**
- Create: `test/e2e/lifecycle_e2e_test.go`
- Modify: `test/e2e/sandbox_e2e_test.go` (one subtest)

**Interfaces:**
- Consumes:
  - **From Tasks 1–14:** the deployed chart, `gatewayv1`, `plan.Hash`, `plan.Resolve`, `plan.Results`.
  - **e2e helpers:** `kubectl`, `secretValue`, `portForward`, plus `setupSandbox`, `sandboxEnv.run` and the `e2e-cluster` Provider.
  - **Audit reads:** `audit.NewS3Client`, `NewS3Sink`, `EntryPrefix`.
- What the e2e proves:
  - **Lifecycle.** The real gateway, deployed by the chart, takes an approved plan through `BeginPhase`, `RunCommand` (with a `${steps…}` reference), `WriteOutputSecret`, `EndPhase` and `RunProbes`. It then refuses to re-run the same plan.
  - **Audit.** Every expected entry type for the Intent appears in the real WORM chain, in order.
  - **Token revocation.** A kubernetes Provider token stops authenticating once its run ends, because the anchor is deleted.

- [ ] **Step 1: Write the token-revocation subtest**

In `test/e2e/sandbox_e2e_test.go`, inside `TestSandboxExecution`, after the existing kubernetes-provider subtests, add:
```go
	t.Run("kubernetes tokens are revoked when the run ends", func(t *testing.T) {
		ctx := context.Background()
		tokens := make(chan string, 1)
		stop := make(chan struct{})
		go func() {
			defer close(tokens)
			for {
				select {
				case <-stop:
					return
				default:
				}
				list, err := s.kube.CoreV1().Secrets("last-sandbox").List(ctx,
					metav1.ListOptions{LabelSelector: sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue})
				if err == nil {
					for _, sec := range list.Items {
						if tok := sec.Data["e2e-cluster.token"]; len(tok) > 0 {
							tokens <- string(tok)
							return
						}
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "sleep 8"}, Providers: []string{"e2e-cluster"}})
		close(stop)
		if r.Result.ExitCode != 0 || r.Result.Error != "" {
			t.Fatalf("result = %+v", r.Result)
		}
		tok, ok := <-tokens
		if !ok || tok == "" {
			t.Fatal("never observed the run's token")
		}
		review, err := s.kube.AuthenticationV1().TokenReviews().Create(ctx,
			&authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: tok}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Authenticated {
			t.Fatal("the run's token still authenticates after cleanup")
		}
	})
```
Add the import `authv1 "k8s.io/api/authentication/v1"`.

- [ ] **Step 2: Write the lifecycle e2e**

`test/e2e/lifecycle_e2e_test.go`:
```go
//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	lifecycleNS = "e2e-lifecycle"
	lifecycleSA = "e2e-lifecycle-reader"
)

func TestGatewayLifecycle(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	ctx := context.Background()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = lastv1alpha1.AddToScheme(sch)
	crc, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	// A namespace, a Provider that can only read that namespace, and a policy allowing the Provider.
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lifecycleNS}})
	t.Cleanup(func() { _ = crc.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lifecycleNS}}) })
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA, Namespace: "last-gateway"}})
	create(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{lifecycleNS}, Verbs: []string{"get"}}}})
	create(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: lifecycleSA},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: lifecycleSA, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: lifecycleSA}}}})
	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		op = lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		create(&op)
		if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if op.Spec.Defaults == nil {
		op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
	}
	op.Spec.Defaults.Providers = append(op.Spec.Defaults.Providers, lifecycleSA)
	if err := crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}

	// The Intent and its plan. The plan is written as an admin would; the operator arrives in M2c.
	getNS := lastv1alpha1.Command{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespace", lifecycleNS, "-o", "json"}},
		Providers: []string{lifecycleSA}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}}
	echo := lastv1alpha1.Command{ID: "s2", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", "${steps.s1.json.metadata.name}"}},
		Timeout: &metav1.Duration{Duration: time.Minute}}
	resource := lastv1alpha1.ManagedResourceSpec{Description: "the e2e namespace",
		Probes: []lastv1alpha1.Probe{{ID: "ns-exists", Command: lastv1alpha1.Command{Run: getNS.Run, Providers: getNS.Providers},
			Assert: `result.exitCode == 0 && result.json.metadata.name == "` + lifecycleNS + `"`}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"}}
	steps := []lastv1alpha1.Command{getNS, echo}
	hash, err := plan.Hash(plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{resource}})
	if err != nil {
		t.Fatal(err)
	}
	intent := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: lifecycleNS},
		Spec: lastv1alpha1.IntentSpec{Request: "e2e lifecycle"}}
	create(intent)
	intent.Status = lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingApproval, Stage: lastv1alpha1.StageCreate,
		Plan: &lastv1alpha1.Plan{Hash: hash, Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{resource}}}
	if err := crc.Status().Update(ctx, intent); err != nil {
		t.Fatal(err)
	}

	// Approve as ourselves: the admission policy requires approvedBy to be the creator.
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	create(&lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: "web-create", Namespace: lifecycleNS},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: hash, Decision: lastv1alpha1.DecisionApprove, ApprovedBy: review.Status.UserInfo.Username}})

	// Call the deployed gateway as the operator.
	conn, err := grpc.NewClient(portForward(t, "last-gateway", "svc/last-gateway", "9090"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	gw := gatewayv1.NewGatewayServiceClient(conn)
	token := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--audience", "last-gateway", "--duration", "30m")
	call := func() (context.Context, context.CancelFunc) {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		return metadata.AppendToOutgoingContext(c, "authorization", "Bearer "+token), cancel
	}
	ref := &gatewayv1.IntentRef{Namespace: lifecycleNS, Name: "web"}

	c, cancel := call()
	if _, err := gw.BeginPhase(c, &gatewayv1.BeginPhaseRequest{Intent: ref, Stage: "Create", SubjectHash: hash}); err != nil {
		t.Fatalf("BeginPhase: %v", err)
	}
	cancel()
	results := plan.Results{}
	for _, s := range steps {
		cmd, err := plan.Resolve(s, results)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(cmd)
		c, cancel := call()
		resp, err := gw.RunCommand(c, &gatewayv1.RunCommandRequest{Intent: ref, SubjectHash: hash, StepId: s.ID, CommandJson: raw})
		cancel()
		if err != nil || resp.GetResult().GetExitCode() != 0 {
			t.Fatalf("step %s: %v %+v", s.ID, err, resp.GetResult())
		}
		results[s.ID] = runnerapi.Result{ID: s.ID, Stdout: resp.GetResult().GetStdout()}
	}
	if got := strings.TrimSpace(results["s2"].Stdout); got != lifecycleNS {
		t.Fatalf("s2 printed %q, want the resolved namespace name", got)
	}

	// The operator (M2c) creates ManagedResources; here the test does, owned by the Intent.
	var current lastv1alpha1.Intent
	if err := crc.Get(ctx, client.ObjectKeyFromObject(intent), &current); err != nil {
		t.Fatal(err)
	}
	mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "web-ns", Namespace: lifecycleNS,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: "web", UID: current.UID, Controller: ptr.To(true)}}},
		Spec: resource}
	mr.Spec.IntentRef = &lastv1alpha1.LocalRef{Name: "web"}
	create(mr)
	c, cancel = call()
	if _, err := gw.WriteOutputSecret(c, &gatewayv1.WriteOutputSecretRequest{Intent: ref, SubjectHash: hash, ManagedResource: "web-ns",
		Values: map[string]string{"namespace": "${steps.s1.json.metadata.name}"}}); err != nil {
		t.Fatalf("WriteOutputSecret: %v", err)
	}
	cancel()
	if v := secretValue(t, lifecycleNS, "web-outputs", "namespace"); v != lifecycleNS {
		t.Fatalf("output secret = %q", v)
	}
	c, cancel = call()
	if _, err := gw.EndPhase(c, &gatewayv1.EndPhaseRequest{Intent: ref, SubjectHash: hash, Succeeded: true}); err != nil {
		t.Fatalf("EndPhase: %v", err)
	}
	cancel()
	c, cancel = call()
	probes, err := gw.RunProbes(c, &gatewayv1.RunProbesRequest{Intent: ref})
	cancel()
	if err != nil || len(probes.GetOutcomes()) != 1 || !probes.GetOutcomes()[0].GetPassed() {
		t.Fatalf("RunProbes = %+v, %v", probes, err)
	}
	c, cancel = call()
	_, err = gw.BeginPhase(c, &gatewayv1.BeginPhaseRequest{Intent: ref, Stage: "Create", SubjectHash: hash})
	cancel()
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-running the executed plan = %v, want FailedPrecondition", err)
	}

	// Every step is in the real WORM chain, in order.
	types := intentEntryTypes(t, lifecycleNS+"/web")
	want := []string{audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeOutputWritten, audit.TypePhaseEnd,
		audit.TypeProbeStart, audit.TypeProbeResult, audit.TypePolicyDenied}
	if !containsInOrder(types, want) {
		t.Fatalf("chain entries for the intent = %v, want %v in order", types, want)
	}
}

// intentEntryTypes reads the bundled audit store and returns the types of entries for intent.
func intentEntryTypes(t *testing.T, intent string) []string {
	t.Helper()
	ctx := context.Background()
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	s3c, err := audit.NewS3Client(ctx, audit.S3Config{
		Endpoint: "http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000"), Region: "us-east-1", Bucket: "last-audit",
		AccessKey: secretValue(t, "last-audit", "last-audit-reader", "accessKey"),
		SecretKey: secretValue(t, "last-audit", "last-audit-reader", "secretKey"), PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewS3Sink(s3c, "last-audit")
	objs, err := sink.List(ctx, audit.EntryPrefix(chain), "")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, o := range objs {
		body, err := sink.Get(ctx, o.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		var e audit.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		if e.Intent == intent {
			types = append(types, e.Type)
		}
	}
	return types
}

func containsInOrder(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}
```
The expected order reflects the M2a executor's audit sequence. Step `s1` uses a kubernetes provider, so its `command.start` is followed by `credential.issued` and `egress.granted`/`egress.revoked`. `containsInOrder` ignores those extra entries.

- [ ] **Step 3: Run the e2e**

Run: `KIND_CLUSTER=last-e2e-m2a make test-e2e 2>&1 | tail -60`
Expected: every e2e test passes: `TestAuditChain`, `TestGatewayGRPCAuth`, `TestGatewayLifecycle` and `TestSandboxExecution`, including the new revocation subtest.

If `TestGatewayLifecycle` fails because the gateway lacks a permission, the gateway logs name the verb and resource:
```bash
kubectl logs -n last-gateway deploy/last-gateway | tail -50
```
Add that verb to the role from Task 14 and re-run. Do not grant broader permissions than the error names.

- [ ] **Step 4: Commit**

```bash
make lint
git add test/e2e
git commit -m "test(e2e): drive an approved plan through the deployed gateway and prove token revocation"
```

---

## Spec coverage

| Spec requirement | Task(s) |
|---|---|
| §4.2 RPCs `BeginPhase`/`EndPhase`/`RunCommand`/`WriteOutputSecret`/`CallLLM` (+ RunProbes for §10) | 5, 8–12 |
| §4.2 the audit entry records the authenticated caller | 8–12 (Actor = `CallerFrom`) |
| §5.2 IntentApproval admission: approvedBy = creator, immutable, operator denied | 13 (+ gateway defense in 8) |
| §6 layering for providers, egress, scripts (modes in M5) | 2, 8, 10 |
| §8.1 `${steps…}` resolution by the gateway | 1, 9 |
| §8.2 plan hash = SHA-256 of canonical steps + resources | 1, 8 |
| §8.3 phase open, authorization, next-step, exact match, policy, audit, `policy.denied` | 8, 9 |
| §8.3 the gateway's own snapshot of the executed plan; probes only from it | 7, 8, 10 |
| §10 probes with compact audit, no overlap | 10 (scheduling cadence is M2c and M5) |
| §11.1 egress gate canaries configured; fail closed without them | 6, 14 |
| §12 `CallLLM` audited; the scripted provider for M2 | 4, 12 |
| §13 Kubernetes tokens: short-lived, revoked on cleanup | 3, 15 |
| §14.1 fail-closed writes; drain before releasing the writer | 6, 8–12 |
| §14.2 entry types incl. `approval.observed`, `phase.*`, `output.written`, `policy.denied` | 4, 8, 11 |
| §15 output Secrets owned by the ManagedResource, values hashed in audit | 11 |
| M2a follow-ups: priority from phase, Actor from caller, refuse when not leading, withhold unrecorded output, canaries, token binding | 3, 6, 9, 10 |

Deferred to M2c, the operator side:
- the Intent and ManagedResource controllers and the state machine;
- the operator Deployment, its namespaced RBAC (no access to sandbox pods, logs or Secrets) and its gRPC client;
- the 9090 `podSelector`;
- fixed-interval probe scheduling and the skip metric;
- the scripted-LLM web-server e2e.

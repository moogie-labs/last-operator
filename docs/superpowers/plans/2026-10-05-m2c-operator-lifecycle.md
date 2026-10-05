# M2c: Operator lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the operator side of M2, so a natural-language `Intent` goes from Pending to Ready with every action taken through the gateway. The Intent controller:
- plans through a (scripted) LLM;
- waits for an `IntentApproval`;
- runs the plan step by step;
- creates the `ManagedResource`s and writes output Secrets;
- verifies with probes.

The ManagedResource controller then re-runs the probes on a fixed interval.

**Architecture:**
- **The operator is untrusted (spec §3).** It has no Secrets and no Pods. Its only way to act is the gateway's gRPC API, authenticated with a projected ServiceAccount token (audience `last-gateway`).
- **Intent controller.** A kubebuilder state machine over `status.phase`, driving the M2b RPCs:
  - Planning → `CallLLM`;
  - Executing → `BeginPhase`, `RunCommand` per step, `WriteOutputSecret`, `EndPhase`;
  - Verifying → `RunProbes`.

  Step results live only in memory. An `Executed` status condition checkpoints each phase, so a phase that a crashed or restarted process left behind is abandoned or confirmed with the gateway, never resumed blindly.
- **ManagedResource controller.** It is keyed by Intent, because one `RunProbes` call covers all of an Intent's ManagedResources. It schedules probe runs on a fixed grid, skips (and counts) runs that would overlap, and records `InSync` on each ManagedResource.

**Tech Stack:** Go 1.26, controller-runtime v0.25 (kubebuilder v4.16 scaffolding), Ginkgo/Gomega + envtest for controllers, gRPC (bufconn in tests), Helm, kind.

**Spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§3, §4.1, §4.2, §5.1, §5.3, §6, §7, §8.3, §10, §10.1, §15, §16, §17.1, §18, §20). M2b follow-ups: `docs/superpowers/followups/2026-10-05-m2b-followups.md` (section "M2c (operator side) must respect or build").

**Scope.** This is the last of the three M2 plans (M2a sandbox, M2b gateway API, M2c operator). M2's done-criterion is: *"e2e: scripted-LLM web server Intent goes Pending → Ready via `kubectl` in a sandbox, with a verified audit chain."*

Deliberately not in M2c:
- Clarifying and Discovery phases, and real LLM adapters: M3.
- Update, Remediate and Delete stages, finalizers, Auto modes, and adaptive probe intervals: M5.
- Allow rules: M6. Rule suggestions: M7.

**Rulings this plan makes** (record them in the follow-ups doc in Task 10):

1. **Phases.** An Intent with no phase is Pending; the first reconcile moves it straight to Planning.
2. **Failure and retry.**
   - **Plan failure.** A step that exits non-zero (or whose references cannot resolve) sends the Intent back to Planning, with the failure as context (spec §8.3).
   - **Other execution failures.** These include a refused RPC, a lost gateway, an interrupted phase and a failed verification. Each one moves the Intent to Failed, which is terminal until the spec is edited.
   - Automatic retry with backoff (spec §7) and a replan cap arrive with Auto modes in M5. Every replan needs a fresh human approval until then, so the loop cannot run unattended.
3. **Restarts.**
   - **Planning.** An operator restart during Planning restarts the conversation. Reloading it from `QueryAudit` arrives in M3.
   - **Executing.** A phase the operator lost track of is ended as failed, and the Intent becomes Failed with reason `ExecutionInterrupted`. Step results are never persisted outside the gateway, so a phase is never resumed.
4. **Plan-declared outputs.**
   - ManagedResource specs gain `outputs` (Secret key → `${steps…}` reference), which is part of the approved plan.
   - The gateway now writes exactly the declared outputs and refuses any other values. Without this, the operator would have nothing approved to send, and the gateway would trust the operator's choice of values.
5. **Naming.** ManagedResources are named `<intent>-<index in plan.resources>`. The operator gets no `delete` on ManagedResources (follow-ups: deletion garbage-collects output Secrets unaudited). A ManagedResource left by a failed phase stays until the M5 Delete stage.
6. **Probe intervals.** The fixed M2 interval is the shortest `probes.interval` set by `OperatorConfig.spec.defaults` or any `IntentPolicy` in the namespace. It defaults to 5m and is never below `OperatorConfig.spec.probes.minInterval` (default 1m).
7. **Intent identity.** A deleted and recreated Intent with the same name still shares the gateway record and approvals, which are keyed by namespace and name. That is deferred to the "approval replay" item (before M5), which ties approvals to the Intent UID. Until then, a recreated Intent whose plan is identical to an executed one is refused with "already opened a phase".

## Global Constraints

- Module `github.com/moogie-labs/last-operator`; API group `last.moogie-labs.com/v1alpha1`.
- **The operator is untrusted.**
  - It acts only through gateway RPCs.
  - It never reads or writes Secrets, Pods, NetworkPolicies, ConfigMaps or anything in `last-gateway`, `last-sandbox` or `last-audit`.
  - It never impersonates, never mints tokens, and never writes IntentApprovals, IntentPolicies, OperatorConfig, Providers, LLMProviders, RBAC or admission policies.
  - It never deletes ManagedResources.
- **gRPC error handling** (follow-ups, binding):

  | Code | Where | Handling |
  |---|---|---|
  | `Aborted` | `RunCommand`, `WriteOutputSecret` | Retry |
  | `Aborted` | Probes | Skip the run and count the skip |
  | `AlreadyExists` | `BeginPhase` | End the existing phase first (as failed) |
  | `Unavailable` | `EndPhase` | Back off and retry with the same outcome |
  | `Unavailable` | Elsewhere | Reconcile error (controller backoff) |
  | `FailedPrecondition` "no open phase" | — | The phase is gone; never reopen the same subject |
  | `PermissionDenied` | Mid-phase | End the phase as failed |
- **Plans.**
  - Hash with `internal/plan.Hash`.
  - Validate with `internal/plan.Validate`: at least one step, unique step IDs, unique probe IDs, and well-formed outputs.
  - Canonical JSON is at most `agent.MaxPlanBytes` (256 KiB), well under the gateway's 512 KiB plan / 900 KiB record bounds.
- **ManagedResources.** Each has a controller ownerReference to the Intent (its UID) **and** `spec.intentRef`. Output values sent to `WriteOutputSecret` are exactly the plan's declared `outputs`.
- **At most one `RunCommand` per Intent at a time.** Steps run sequentially inside one reconcile; controller-runtime never reconciles one key concurrently; leader election allows one operator.
- **Logging.** Follow the Kubernetes message style: a capital letter, no trailing period, past tense ("Could not …"), balanced key/value pairs, `logf.FromContext(ctx)`.
- **Credential values.** They never appear in logs, Events or status. Probe output stays in the audit log; ManagedResource status records only pass/fail, exit code and error.
- **Tests.**
  - `internal/controller` uses Ginkgo/Gomega with envtest (the kubebuilder scaffold) and a fake gateway (spec §18).
  - Every other package uses plain `testing`.
  - e2e tests carry `//go:build e2e`.
- **Kubebuilder.**
  - Scaffold controllers with `kubebuilder create api … --controller=true --resource=false`.
  - Never edit `zz_generated.*`, `config/crd/bases/*`, `config/rbac/role.yaml` or `PROJECT` by hand.
  - Keep every `// +kubebuilder:scaffold:*` marker.
- **After each task:** run `make lint-fix`, `make test` and `make lint`. After editing API types or RBAC markers, also run `make manifests generate helm-crds` and commit the regenerated files.
- **Commits.** No `Co-authored-by` or other attribution trailers. Never commit `.superpowers/`.

## Review Focus

1. **Operator restart at any point of a phase.** This covers a crash after `BeginPhase` but before the checkpoint, between steps, and after `EndPhase` but before the status write. The expected behavior: no step runs twice, the phase is closed at the gateway, and success is reported only when the gateway confirms it. *Tests: Task 6 "abandons…", "ends a phase that was left open…", "confirms with the gateway…", "fails when the gateway does not know…".*
2. **Spec edits in every phase.**
   - Before approval: replan.
   - Failed: replan.
   - While Executing or Ready: ignored until M5. The Executing case is covered by the Executed checkpoint; the Ready case is tested.

   *Tests: Task 5 "plans again when the spec changes…", "plans again after Failed…"; Task 7 "ignores a spec edit at Ready".*
3. **Approvals that must not count.** These are approvals for another subject or another Intent, and a Deny filed next to an Approve. In every such case nothing runs. *Tests: Task 5 "waits for an approval of that exact plan", "fails when the plan is denied…".*
4. **Malformed model output.**
   - Prose around the JSON, or a fenced block.
   - Unknown fields.
   - Both `argv` and `script`.
   - `image.install`.
   - Missing probe IDs or asserts.
   - Oversized plans.
   - Literal output values.

   Each is a correction turn, never a status write the API server rejects. *Tests: Task 4 `TestParse`.*
5. **Transient gateway trouble.** `EndPhase` briefly `Unavailable` is retried with the same outcome; a probe run already in flight is skipped and counted. *Tests: Task 6 "retries EndPhase…"; Task 8 "skips a run when the gateway already has one in flight".*

---

### Task 1: Plan-declared outputs

**Files:**
- Modify: `api/v1alpha1/managedresource_types.go`
- Modify: `internal/plan/plan.go` (`Validate`)
- Modify: `internal/plan/plan_test.go` (`TestValidate`)
- Modify: `internal/gateway/outputs.go` (`plannedOutputSecret` → `plannedOutputs`)
- Modify: `internal/gateway/outputs_test.go`
- Modify: `test/e2e/lifecycle_e2e_test.go` (declare the output it writes)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§5.3, §15)
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*`, `charts/last-operator/crds/*`

**Interfaces:**
- Produces: `lastv1alpha1.ManagedResourceSpec.Outputs map[string]string` (JSON `outputs`).
- Produces: `plan.Validate` additionally rejects:
  - outputs without `outputsSecretRef`;
  - an output value that is not a single `${steps.<id>…}` reference;
  - a reference to an unknown step;
  - two resources with the same `outputsSecretRef` name.

  Its error messages never repeat an output value.
- Produces: gateway `WriteOutputSecret` refuses (`PermissionDenied`) values that differ from the approved plan's `outputs` for that Secret.

- [ ] **Step 1: Add the API field**

In `api/v1alpha1/managedresource_types.go`, add after `OutputsSecretRef`:

```go
	// Outputs maps output Secret keys to step references such as
	// "${steps.s1.json.password}". The gateway writes exactly these values to
	// outputsSecretRef (spec §15).
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
```

Run: `make manifests generate helm-crds`
Expected: deepcopy, CRDs and chart CRDs regenerate with `outputs`.

- [ ] **Step 2: Write the failing plan tests**

In `internal/plan/plan_test.go`, add these cases to the `cases` slice in `TestValidate`:

```go
		{"outputs without secret", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Outputs: map[string]string{"k": "${steps.s1.stdout}"}}}}, "has outputs but no outputsSecretRef"},
		{"literal output", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "out"}, Outputs: map[string]string{"k": "hunter2"}}}}, `output "k" is not a single step reference`},
		{"unknown step output", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "out"}, Outputs: map[string]string{"k": "${steps.nope.stdout}"}}}}, `names unknown step "nope"`},
		{"duplicate output secret", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "out"}}, {OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "out"}}}}, `duplicate output secret "out"`},
```

In the loop body of `TestValidate`, after the existing check, add:

```go
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats an output value: %v", tc.name, err)
		}
```

At the end of `TestValidate`, add:

```go
	withOutputs := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
		{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "out"}, Outputs: map[string]string{"k": "${steps.s1.json.a.b}", "s": "${steps.s1.stdout}"}}}}
	if err := plan.Validate(withOutputs); err != nil {
		t.Fatalf("Validate(withOutputs) = %v", err)
	}
```

- [ ] **Step 3: Run the plan tests to see them fail**

Run: `go test ./internal/plan/ -run TestValidate -v`
Expected: FAIL. The four new cases report `Validate = <nil>`.

- [ ] **Step 4: Extend `Validate`**

In `internal/plan/plan.go`, replace the resource loop at the end of `Validate` (from `probes := map[string]bool{}` to the final `return nil`) with:

```go
	probes := map[string]bool{}
	secrets := map[string]bool{}
	for i, r := range b.Resources {
		for _, p := range r.Probes {
			if probes[p.ID] {
				return fmt.Errorf("plan: duplicate probe id %q", p.ID)
			}
			probes[p.ID] = true
		}
		if r.OutputsSecretRef != nil {
			if secrets[r.OutputsSecretRef.Name] {
				return fmt.Errorf("plan: duplicate output secret %q", r.OutputsSecretRef.Name)
			}
			secrets[r.OutputsSecretRef.Name] = true
		}
		if len(r.Outputs) > 0 && r.OutputsSecretRef == nil {
			return fmt.Errorf("plan: resource %d has outputs but no outputsSecretRef", i)
		}
		for k, expr := range r.Outputs {
			// Errors never repeat expr: a mistaken literal could be a credential.
			m := refPattern.FindStringSubmatch(expr)
			if m == nil || m[0] != expr {
				return fmt.Errorf("plan: resource %d output %q is not a single step reference", i, k)
			}
			if !seen[m[1]] {
				return fmt.Errorf("plan: resource %d output %q names unknown step %q", i, k, m[1])
			}
		}
	}
	return nil
```

Update the `Validate` doc comment to read:

```go
// Validate checks what the gateway relies on: step IDs name the steps that
// ${steps…} references and RunCommand use, probe IDs name probe outcomes, and
// each declared output is a single reference to a step of the plan.
```

Run: `go test ./internal/plan/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing gateway tests**

In `internal/gateway/outputs_test.go`:

(a) Replace `newOutputEnv` with a version that declares the plan's outputs, plus a variant that takes them:

```go
func newOutputEnv(t *testing.T, ownerUID types.UID) (*testEnv, string) {
	t.Helper()
	return newOutputEnvWith(t, ownerUID, map[string]string{passwordKey: passwordRef})
}

// newOutputEnvWith runs a two-step plan whose one resource declares outputs.
func newOutputEnvWith(t *testing.T, ownerUID types.UID, outputs map[string]string) (*testEnv, string) {
	t.Helper()
	env, in := newRunEnv(t, twoSteps(), lastv1alpha1.ManagedResourceSpec{
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outputSecret}, Outputs: outputs})
	if err := env.crd.Create(context.Background(), managedResource(ownerUID)); err != nil {
		t.Fatal(err)
	}
	if _, ok := runAll(t, env, in)["s1"]; !ok {
		t.Fatal("runAll returned no result for s1")
	}
	return env, in.Status.Plan.Hash
}
```

(b) In `TestWriteOutputSecretRefusals`, replace the `"own secret is updated"` subtest body so that both writes send the declared values:

```go
	t.Run("own secret is updated", func(t *testing.T) {
		env, h := newOutputEnv(t, envIntentUID)
		values := map[string]string{passwordKey: passwordRef}
		if _, err := env.writeOutputs(h, outputMR, values); err != nil {
			t.Fatal(err)
		}
		if _, err := env.writeOutputs(h, outputMR, values); err != nil {
			t.Fatalf("second write = %v", err)
		}
		if got := env.secret(outputSecret); string(got.Data[passwordKey]) != outputValue {
			t.Fatalf("data = %v", got.Data)
		}
	})
```

(c) In the same test, replace the `"bad reference, bad key, literal value, no phase"` subtest with these two subtests:

```go
	t.Run("values must be the approved plan's outputs", func(t *testing.T) {
		env, h := newOutputEnv(t, envIntentUID)
		cases := map[string]struct {
			values map[string]string
			code   codes.Code
		}{
			"literal":     {map[string]string{passwordKey: "hunter2"}, codes.PermissionDenied},
			"other ref":   {map[string]string{passwordKey: "${steps.s1.stdout}"}, codes.PermissionDenied},
			"extra key":   {map[string]string{passwordKey: passwordRef, "name": "${steps.s1.json.metadata.name}"}, codes.PermissionDenied},
			"missing key": {map[string]string{"other": passwordRef}, codes.PermissionDenied},
			"no values":   {map[string]string{}, codes.InvalidArgument},
		}
		for name, tc := range cases {
			_, err := env.writeOutputs(h, outputMR, tc.values)
			if status.Code(err) != tc.code || env.last().Type != audit.TypePolicyDenied {
				t.Errorf("%s: %v (last %s), want %v with policy.denied", name, err, env.last().Type, tc.code)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(string(env.last().Payload), "hunter2") {
				t.Errorf("%s: the literal leaked into the error or audit entry", name)
			}
		}
		_, err := env.writeOutputs("sha256:"+strings.Repeat("0", 64), outputMR, map[string]string{passwordKey: passwordRef})
		env.wantDenied(err, codes.FailedPrecondition)
		if _, gerr := env.kube.CoreV1().Secrets(envNS).Get(context.Background(), outputSecret, metav1.GetOptions{}); gerr == nil {
			t.Fatal("a refused write created the secret")
		}
	})
	t.Run("an unresolvable or invalid planned output is refused", func(t *testing.T) {
		cases := map[string]struct {
			outputs map[string]string
			code    codes.Code
		}{
			"missing json key": {map[string]string{"p": "${steps.s1.json.nope}"}, codes.FailedPrecondition},
			"bad key":          {map[string]string{"pass word": passwordRef}, codes.InvalidArgument},
		}
		for name, tc := range cases {
			env, h := newOutputEnvWith(t, envIntentUID, tc.outputs)
			_, err := env.writeOutputs(h, outputMR, tc.outputs)
			if status.Code(err) != tc.code || env.last().Type != audit.TypePolicyDenied {
				t.Errorf("%s: %v (last %s), want %v with policy.denied", name, err, env.last().Type, tc.code)
			}
		}
	})
```

(d) In `TestWriteOutputSecretUpdateKeepsAnnotationsAndReplacesData`, change it so that the stale key is planted by hand rather than by a second, undeclared write. The first write sends `map[string]string{passwordKey: passwordRef}`. Then, alongside the annotation and label edits, add `s.Data["extra"] = []byte("stale")` before the `Update`. The final assertion stays `len(got.Data) != 1`.

- [ ] **Step 6: Run the gateway tests to see them fail**

Run: `go test ./internal/gateway/ -run WriteOutputSecret -v`
Expected: FAIL. The "values must be the approved plan's outputs" cases `other ref`, `extra key`, `missing key` and `literal` do not return `PermissionDenied`.

- [ ] **Step 7: Enforce the declared outputs in the gateway**

In `internal/gateway/outputs.go`:

(a) Replace `plannedOutputSecret` with:

```go
// plannedOutputs returns the outputs the approved plan declares for the output
// Secret name, and whether the plan declares that Secret at all.
func plannedOutputs(b plan.Body, name string) (map[string]string, bool) {
	for _, r := range b.Resources {
		if r.OutputsSecretRef != nil && r.OutputsSecretRef.Name == name {
			return r.Outputs, true
		}
	}
	return nil, false
}
```

(b) In `writeOutputSecret`, replace the `if !plannedOutputSecret(p.body, secretName) { … }` block with:

```go
	planned, ok := plannedOutputs(p.body, secretName)
	if !ok {
		return nil, d.deny(codes.PermissionDenied, "secret %s is not an output secret of the approved plan", secretName)
	}
	// The operator sends the plan's outputs; any other value was not approved.
	if !maps.Equal(planned, req.GetValues()) {
		return nil, d.deny(codes.PermissionDenied, "values for secret %s differ from the approved plan's outputs", secretName)
	}
```

Add `"maps"` to the imports. Keep the existing comment above this block ("The operator can edit a ManagedResource, so its Secret name is trusted only when the approved plan declares it.").

Run: `go test ./internal/gateway/ -v -count=1`
Expected: PASS (all gateway tests).

- [ ] **Step 8: Declare the output in the lifecycle e2e**

In `test/e2e/lifecycle_e2e_test.go`, add `Outputs` to `resource`:

```go
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"namespace": "${steps.s1.json.metadata.name}"}}
```

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output.

- [ ] **Step 9: Amend the spec**

In §5.3, in the YAML example, replace the `outputsSecretRef` line with:

```yaml
  outputsSecretRef: { name: team-x-web-outputs }                         # optional
  outputs: { password: "${steps.s3.json.password}" }                     # Secret key → step reference
```

In §15, replace the second bullet with:

```markdown
- Sensitive outputs (passwords, connection strings) are written by the
  gateway (`WriteOutputSecret`) to a Secret in the Intent's namespace.
  The Secret is owned by the ManagedResource and referenced by
  `outputsSecretRef`. The plan declares each value in the resource's
  `outputs` (Secret key → `${steps.<id>.json.<path>}`), so the LLM never
  needs to repeat it, and approvers see what will be written. The gateway
  writes exactly the declared outputs and refuses any other values.
```

- [ ] **Step 10: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add api/ config/crd/ charts/last-operator/crds/ internal/plan/ internal/gateway/outputs.go internal/gateway/outputs_test.go test/e2e/lifecycle_e2e_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(plan): declare output Secret values in the approved plan"
```

---

### Task 2: Fixed probe interval

**Files:**
- Create: `internal/policy/probes.go`
- Test: `internal/policy/probes_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§10.1)

**Interfaces:**
- Produces: `policy.DefaultProbeInterval = 5 * time.Minute` and `policy.DefaultMinProbeInterval = time.Minute`.
- Produces: `func policy.ProbeInterval(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) time.Duration`. `op` may be nil.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/probes_test.go`:

```go
package policy_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
)

func probes(d time.Duration) *lastv1alpha1.ProbePolicy {
	return &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: d}}
}

func opWith(defaults *lastv1alpha1.ProbePolicy, minInterval time.Duration) *lastv1alpha1.OperatorConfig {
	op := &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{Probes: defaults}}}
	if minInterval > 0 {
		op.Spec.Probes = &lastv1alpha1.ProbeLimits{MinInterval: &metav1.Duration{Duration: minInterval}}
	}
	return op
}

func nsPolicy(p *lastv1alpha1.ProbePolicy) lastv1alpha1.IntentPolicy {
	return lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Probes: p}}
}

func TestProbeInterval(t *testing.T) {
	cases := []struct {
		name     string
		op       *lastv1alpha1.OperatorConfig
		policies []lastv1alpha1.IntentPolicy
		want     time.Duration
	}{
		{"no config", nil, nil, 5 * time.Minute},
		{"cluster default", opWith(probes(3*time.Minute), 0), nil, 3 * time.Minute},
		{"policy shortens", opWith(probes(3*time.Minute), 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(2 * time.Minute))}, 2 * time.Minute},
		{"shortest policy wins", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(4 * time.Minute)), nsPolicy(probes(90 * time.Second))}, 90 * time.Second},
		{"policy cannot go below the default floor", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(10 * time.Second))}, time.Minute},
		{"cluster floor applies", opWith(probes(2*time.Minute), 3*time.Minute), nil, 3 * time.Minute},
		{"floor above the default", opWith(nil, 10*time.Minute), nil, 10 * time.Minute},
		{"unset and zero intervals are ignored", opWith(&lastv1alpha1.ProbePolicy{}, 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(0)), nsPolicy(nil)}, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := policy.ProbeInterval(tc.op, tc.policies); got != tc.want {
			t.Errorf("%s: ProbeInterval = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/policy/ -run TestProbeInterval`
Expected: FAIL to compile. `undefined: policy.ProbeInterval`.

- [ ] **Step 3: Implement**

Create `internal/policy/probes.go`:

```go
package policy

import (
	"time"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// DefaultProbeInterval is the shipped steady-state probe interval (spec §6).
	DefaultProbeInterval = 5 * time.Minute
	// DefaultMinProbeInterval is the shipped floor for every probe interval (spec §10.1).
	DefaultMinProbeInterval = time.Minute
)

// ProbeInterval returns the fixed probe interval for a namespace: the shortest
// probes.interval set by OperatorConfig.spec.defaults or any IntentPolicy
// (DefaultProbeInterval when none sets one), never below the cluster floor
// OperatorConfig.spec.probes.minInterval. Adaptive intervals arrive in M5.
func ProbeInterval(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) time.Duration {
	floor := DefaultMinProbeInterval
	if op != nil && op.Spec.Probes != nil && op.Spec.Probes.MinInterval != nil && op.Spec.Probes.MinInterval.Duration > 0 {
		floor = op.Spec.Probes.MinInterval.Duration
	}
	var interval time.Duration
	consider := func(p *lastv1alpha1.ProbePolicy) {
		if p == nil || p.Interval == nil || p.Interval.Duration <= 0 {
			return
		}
		if interval == 0 || p.Interval.Duration < interval {
			interval = p.Interval.Duration
		}
	}
	if op != nil && op.Spec.Defaults != nil {
		consider(op.Spec.Defaults.Probes)
	}
	for i := range policies {
		consider(policies[i].Spec.Probes)
	}
	if interval == 0 {
		interval = DefaultProbeInterval
	}
	return max(interval, floor)
}
```

Run: `go test ./internal/policy/ -v`
Expected: PASS.

- [ ] **Step 4: Amend the spec**

In §10.1, after the "**Floor.**" bullet, add:

```markdown
- **Fixed interval (M2).** Until adaptive intervals arrive (M5), each
  Intent is probed at a fixed interval: the shortest `probes.interval` set
  by `OperatorConfig.spec.defaults` or any `IntentPolicy` in its namespace
  (default 5m), never below `probes.minInterval`. Runs are scheduled on a
  grid from the previous run's start; grid points that pass while a run is
  in flight are skipped and counted.
```

- [ ] **Step 5: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add internal/policy/probes.go internal/policy/probes_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(policy): resolve the fixed probe interval"
```

---

### Task 3: Gateway client

**Files:**
- Create: `internal/gatewayclient/client.go`
- Test: `internal/gatewayclient/client_test.go`

**Interfaces:**
- Produces: `type gatewayclient.TokenFile struct{ Path string }`, which implements `credentials.PerRPCCredentials`.
- Produces: `func gatewayclient.Dial(addr, tokenPath string) (*grpc.ClientConn, error)`.
- Produces (package-private, for tests): `func dialOptions(tokenPath string) []grpc.DialOption`.

- [ ] **Step 1: Write the failing test**

Create `internal/gatewayclient/client_test.go`:

```go
package gatewayclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// recorder is a gateway that remembers the authorization header of each call.
type recorder struct {
	gatewayv1.UnimplementedGatewayServiceServer
	mu     sync.Mutex
	tokens []string
}

func (r *recorder) Ping(ctx context.Context, _ *gatewayv1.PingRequest) (*gatewayv1.PingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, strings.Join(md.Get("authorization"), ","))
	return &gatewayv1.PingResponse{}, nil
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

func dialBuf(t *testing.T, tokenPath string) (gatewayv1.GatewayServiceClient, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	gatewayv1.RegisterGatewayServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", append(dialOptions(tokenPath),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gatewayv1.NewGatewayServiceClient(conn), rec
}

func ping(c gatewayv1.GatewayServiceClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Ping(ctx, &gatewayv1.PingRequest{})
	return err
}

func TestTokenIsReadOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	c, rec := dialBuf(t, path)
	for _, tok := range []string{"tok-1\n", "tok-2"} {
		if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ping(c); err != nil {
			t.Fatal(err)
		}
	}
	got := rec.seen()
	if len(got) != 2 || got[0] != "Bearer tok-1" || got[1] != "Bearer tok-2" {
		t.Fatalf("authorization headers = %q, want the rotated token on each call", got)
	}
}

func TestMissingOrEmptyTokenFailsTheCall(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "nope"), "empty": empty} {
		c, rec := dialBuf(t, path)
		if err := ping(c); err == nil {
			t.Errorf("%s token: Ping succeeded", name)
		}
		if n := len(rec.seen()); n != 0 {
			t.Errorf("%s token: the gateway saw %d calls", name, n)
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/gatewayclient/`
Expected: FAIL to compile. `undefined: dialOptions`.

- [ ] **Step 3: Implement**

Create `internal/gatewayclient/client.go`:

```go
// Package gatewayclient connects the operator to the gateway (spec §4.2).
package gatewayclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// TokenFile authenticates every call with the projected ServiceAccount token at
// Path (audience last-gateway). The file is read on every call because the
// kubelet rotates it.
type TokenFile struct{ Path string }

func (t TokenFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: read token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return nil, errors.New("gatewayclient: token file is empty")
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity is false: v1 has no TLS on the gRPC hop, and the
// token check is the authentication boundary (spec §4.2).
func (TokenFile) RequireTransportSecurity() bool { return false }

func dialOptions(tokenPath string) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(TokenFile{Path: tokenPath}),
		// The gateway's keepalive policy rejects pings more often than every 30s.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: time.Minute, Timeout: 20 * time.Second}),
	}
}

// Dial returns a connection to the gateway at addr ("host:port"). It does not
// block; calls fail with Unavailable while the gateway is unreachable.
func Dial(addr, tokenPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, dialOptions(tokenPath)...)
}
```

Run: `go test ./internal/gatewayclient/ -v`
Expected: PASS.

- [ ] **Step 4: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add internal/gatewayclient/
git commit -m "feat(operator): add the gateway client with rotating token credentials"
```

---

### Task 4: Planning agent

**Files:**
- Create: `internal/agent/agent.go` (Planner)
- Create: `internal/agent/parse.go` (Parse)
- Create: `internal/agent/prompt.go` (prompts)
- Test: `internal/agent/agent_test.go`, `internal/agent/parse_test.go`

**Interfaces:**
- Consumes: `plan.Validate`, `plan.Canonical`, `plan.Hash`, `gatewayv1.CallLLMRequest` and `gatewayv1.Message`.
- Produces: the LLM seam and the planner:
  - `type agent.LLM interface { CallLLM(ctx context.Context, in *gatewayv1.CallLLMRequest, opts ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) }`. `gatewayv1.GatewayServiceClient` satisfies it.
  - `type agent.Planner struct{ LLM LLM }`.
  - `func (p *Planner) Plan(ctx context.Context, req Request) (*lastv1alpha1.Plan, error)` returns a plan with `Hash` set. Gateway errors come back unchanged, so callers can use `status.Code`. When no valid plan arrives within `MaxTurns` answers, it returns an error wrapping `ErrNoPlan`.
- Produces: the request and parser:
  - `type agent.Request struct { Namespace, Name, Text string; Answers []lastv1alpha1.Answer; Feedback string }`.
  - `func agent.Parse(content string) (*lastv1alpha1.Plan, error)`.
- Produces: constants and an error: `agent.MaxPlanBytes = 256 << 10`, `agent.MaxTurns = 3`, `agent.ErrNoPlan`, `agent.SystemPrompt`.

- [ ] **Step 1: Write the failing parse tests**

Create `internal/agent/parse_test.go`:

```go
package agent

import (
	"encoding/json"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const validAnswer = `{"summary":"list namespaces","steps":[{"id":"s1","run":{"argv":["kubectl","get","ns"]}}],
"resources":[{"description":"ns","probes":[{"id":"p1","command":{"run":{"argv":["true"]}},"assert":"result.exitCode == 0"}],
"outputsSecretRef":{"name":"out"},"outputs":{"names":"${steps.s1.stdout}"}}]}`

func TestParseAcceptsAPlanAndHashesIt(t *testing.T) {
	for name, content := range map[string]string{
		"plain":        validAnswer,
		"fenced":       "```json\n" + validAnswer + "\n```",
		"bare fence":   "```\n" + validAnswer + "\n```",
		"surrounded ws": "\n  " + validAnswer + "  \n",
	} {
		p, err := Parse(content)
		if err != nil {
			t.Fatalf("%s: Parse = %v", name, err)
		}
		want, err := plan.Hash(plan.Body{Steps: p.Steps, Resources: p.Resources})
		if err != nil || p.Hash != want || p.Summary != "list namespaces" || len(p.Steps) != 1 || p.Resources[0].Outputs["names"] != "${steps.s1.stdout}" {
			t.Fatalf("%s: plan = %+v (hash want %s, %v)", name, p, want, err)
		}
	}
}

func TestParse(t *testing.T) {
	step := `{"id":"s1","run":{"argv":["true"]}}`
	big := strings.Repeat("x", MaxPlanBytes)
	bigStep, _ := json.Marshal(lastv1alpha1.Command{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", big}}})
	cases := map[string]struct{ content, want string }{
		"prose":           {"Here is the plan: " + validAnswer, "not a plan object"},
		"trailing text":   {validAnswer + " hope this helps", "text after the plan object"},
		"unknown field":   {`{"steps":[` + step + `],"extra":1}`, "unknown field"},
		"no steps":        {`{"steps":[]}`, "at least one step"},
		"argv and script": {`{"steps":[{"id":"s1","run":{"argv":["a"],"script":"b"}}]}`, "exactly one of run.argv and run.script"},
		"neither":         {`{"steps":[{"id":"s1","run":{}}]}`, "exactly one of run.argv and run.script"},
		"image install":   {`{"steps":[{"id":"s1","run":{"argv":["a"]},"image":{"install":[{"argv":["pip","install","x"]}]}}]}`, "image.install"},
		"probe without id": {`{"steps":[` + step + `],"resources":[{"probes":[{"command":{"run":{"argv":["true"]}},"assert":"true"}]}]}`, "probe 0 of resource 0 has no id"},
		"probe without assert": {`{"steps":[` + step + `],"resources":[{"probes":[{"id":"p","command":{"run":{"argv":["true"]}}}]}]}`, `probe "p" has no assert`},
		"probe bad command": {`{"steps":[` + step + `],"resources":[{"probes":[{"id":"p","command":{"run":{}},"assert":"true"}]}]}`, "exactly one of run.argv and run.script"},
		"intentRef":       {`{"steps":[` + step + `],"resources":[{"intentRef":{"name":"other"}}]}`, "sets intentRef"},
		"literal output":  {`{"steps":[` + step + `],"resources":[{"outputsSecretRef":{"name":"o"},"outputs":{"k":"hunter2"}}]}`, "not a single step reference"},
		"too large":       {`{"steps":[` + string(bigStep) + `]}`, "the limit is"},
	}
	for name, tc := range cases {
		_, err := Parse(tc.content)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Parse = %v, want an error containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats an output value", name)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/agent/`
Expected: FAIL to compile. `undefined: Parse`.

- [ ] **Step 3: Implement the parser**

Create `internal/agent/parse.go`:

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
	raw := strings.TrimSpace(content)
	if m := fence.FindStringSubmatch(content); m != nil {
		raw = m[1]
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var a answer
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("the answer is not a plan object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the answer has text after the plan object")
	}
	for _, s := range a.Steps {
		if err := checkCommand("step "+s.ID, s); err != nil {
			return nil, err
		}
	}
	for i, r := range a.Resources {
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
	body := plan.Body{Steps: a.Steps, Resources: a.Resources}
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
	return &lastv1alpha1.Plan{Hash: hash, Summary: a.Summary, Steps: a.Steps, Resources: a.Resources}, nil
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

Run: `go test ./internal/agent/ -run Parse -v`
Expected: PASS.

- [ ] **Step 4: Write the failing planner tests**

Create `internal/agent/agent_test.go`:

```go
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// fakeLLM answers in order and records every request.
type fakeLLM struct {
	answers []string
	err     error
	reqs    []*gatewayv1.CallLLMRequest
}

func (f *fakeLLM) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	n := len(f.reqs) - 1
	if n >= len(f.answers) {
		n = len(f.answers) - 1
	}
	return &gatewayv1.CallLLMResponse{Content: f.answers[n]}, nil
}

var req = Request{Namespace: "team-x", Name: "web", Text: "run nginx",
	Answers: []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}}}

func TestPlannerRetriesUntilTheAnswerIsAPlan(t *testing.T) {
	llm := &fakeLLM{answers: []string{"I think you want nginx.", validAnswer}}
	p, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if err != nil || p.Hash == "" {
		t.Fatalf("Plan = %+v, %v", p, err)
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("CallLLM called %d times, want 2", len(llm.reqs))
	}
	first, second := llm.reqs[0], llm.reqs[1]
	if first.GetIntent().GetNamespace() != "team-x" || first.GetIntent().GetName() != "web" || first.GetMaxTokens() <= 0 {
		t.Fatalf("request = %+v", first)
	}
	roles := []string{}
	for _, m := range second.GetMessages() {
		roles = append(roles, m.GetRole())
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" {
		t.Fatalf("second conversation roles = %v", roles)
	}
	if second.GetMessages()[2].GetContent() != "I think you want nginx." ||
		!strings.Contains(second.GetMessages()[3].GetContent(), "not a valid plan") {
		t.Fatalf("correction turn = %+v", second.GetMessages()[2:])
	}
}

func TestPlannerGivesUpAfterMaxTurns(t *testing.T) {
	llm := &fakeLLM{answers: []string{"no"}}
	_, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if !errors.Is(err, ErrNoPlan) || len(llm.reqs) != MaxTurns {
		t.Fatalf("Plan = %v after %d calls, want ErrNoPlan after %d", err, len(llm.reqs), MaxTurns)
	}
}

func TestPlannerReturnsGatewayErrorsUnchanged(t *testing.T) {
	llm := &fakeLLM{err: status.Error(codes.Unavailable, "audit down")}
	_, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if status.Code(err) != codes.Unavailable || len(llm.reqs) != 1 {
		t.Fatalf("Plan = %v after %d calls", err, len(llm.reqs))
	}
}

func TestUserPromptCarriesTheRequestAnswersAndFeedback(t *testing.T) {
	r := req
	r.Feedback = "step s1 exited 1: boom"
	got := userPrompt(r)
	for _, want := range []string{"team-x/web", "run nginx", "q1: internal only", "step s1 exited 1: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(userPrompt(req), "previous plan failed") {
		t.Error("the prompt mentions a failure that did not happen")
	}
}
```

- [ ] **Step 5: Run them to see them fail**

Run: `go test ./internal/agent/`
Expected: FAIL to compile. `undefined: Request`, `Planner`, `userPrompt`.

- [ ] **Step 6: Implement the prompts and the planner**

Create `internal/agent/prompt.go`:

```go
package agent

import (
	"fmt"
	"strings"
)

// SystemPrompt tells the model what a plan is and how to answer (spec §8.1, §8.2, §10, §15).
const SystemPrompt = `You are last-operator's planner. You turn a natural-language request into a plan
that a Kubernetes operator runs, step by step, in sandbox pods. A human approves the
plan before anything runs.

Answer with exactly one JSON object and nothing else:

{
  "summary": "one line a reviewer reads first",
  "steps": [ <command>, ... ],
  "resources": [ <resource>, ... ]
}

A command is:
{
  "id": "s1",
  "reason": "why this step exists",
  "run": {"argv": ["kubectl", "apply", "-f", "/work/app.yaml"]},
  "files": {"/work/app.yaml": "file content"},
  "providers": ["provider-name"],
  "egress": ["10.0.0.1:443"],
  "timeout": "5m"
}
"id" is required on steps and unique among them. "run" holds exactly one of "argv" (run
directly) or "script" (run with /bin/sh -c). "files", "providers", "egress" and "timeout"
are optional. argv elements may use ${steps.<id>.stdout} or ${steps.<id>.json.<path>}
to use the output of an earlier step; files and scripts never do. Do not use "image".

A resource describes one thing the plan provisions:
{
  "description": "nginx Deployment team-x/web",
  "identity": {"kind": "Deployment", "namespace": "team-x", "name": "web"},
  "desired": {"replicas": 2},
  "probes": [{"id": "web-ready", "command": <command without id>, "assert": "<CEL>"}],
  "outputsSecretRef": {"name": "web-outputs"},
  "outputs": {"<secret key>": "${steps.<id>.json.<path>}"}
}
A probe's assert is a CEL expression over result.exitCode, result.stdout, result.stderr
and result.json (stdout parsed as JSON). Probe ids are unique across the plan. Probes
only read; they run again on a schedule to detect drift. Sensitive values go in
outputs, never in argv, files, or the summary.`

func userPrompt(r Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s.\n\nRequest:\n%s\n", r.Namespace, r.Name, r.Namespace, r.Text)
	if len(r.Answers) > 0 {
		b.WriteString("\nAnswers to your earlier questions:\n")
		for _, a := range r.Answers {
			fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Value)
		}
	}
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous plan failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	return b.String()
}

func correction(err error) string {
	return fmt.Sprintf("That answer is not a valid plan: %v\nAnswer again with only the JSON object.", err)
}
```

Create `internal/agent/agent.go`:

```go
// Package agent runs the planning conversation. It asks the LLM, through the
// gateway's CallLLM, for a plan and checks the answer (spec §4.1, §7).
package agent

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// MaxTurns is how many answers the model gets to produce a valid plan.
const MaxTurns = 3

// maxTokens caps each answer.
const maxTokens = 8192

// ErrNoPlan means the model gave no valid plan within MaxTurns answers.
var ErrNoPlan = errors.New("agent: the model gave no valid plan")

// LLM is the part of the gateway client the agent uses.
type LLM interface {
	CallLLM(ctx context.Context, in *gatewayv1.CallLLMRequest, opts ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error)
}

// Request is what the agent plans for.
type Request struct {
	Namespace, Name string
	Text            string // spec.request
	Answers         []lastv1alpha1.Answer
	Feedback        string // why the previous plan failed, if it did
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
```

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 7: Lint, test, commit**

```bash
make lint-fix && make test && make lint
git add internal/agent/
git commit -m "feat(agent): plan through CallLLM and check the model's answer"
```

---
### Task 5: Controller scaffold, fake gateway, Planning and AwaitingApproval

**Files:**
- Scaffold (kubebuilder CLI): `PROJECT`, `cmd/operator/main.go`, `go.mod`, `go.sum`, and in `internal/controller/`: `suite_test.go`, `intent_controller.go`, `intent_controller_test.go`, `managedresource_controller.go`, `managedresource_controller_test.go`
- Create: `internal/controller/status.go` (conditions, status writes)
- Create: `internal/controller/gateway.go` (gRPC error helpers)
- Create: `internal/controller/intent_planning.go`
- Create: `internal/controller/fakegateway_test.go`
- Create: `internal/controller/helpers_test.go`
- Replace: `internal/controller/intent_controller.go`, `internal/controller/intent_controller_test.go`, `internal/controller/suite_test.go`
- Modify: `internal/controller/managedresource_controller.go` (RBAC markers only)
- Delete: `internal/controller/managedresource_controller_test.go` (the scaffold's; Task 8 writes its own)
- Regenerate: `config/rbac/role.yaml`

**Interfaces:**
- Consumes: `agent.Planner`, `agent.Request`, `agent.ErrNoPlan` (Task 4).
- Produces the reconciler:

  ```go
  type IntentReconciler struct {
  	client.Client
  	Scheme     *runtime.Scheme
  	Gateway    gatewayv1.GatewayServiceClient
  	Planner    *agent.Planner
  	Recorder   events.EventRecorder // k8s.io/client-go/tools/events
  	RetryDelay time.Duration
  }
  ```

  `SetupWithManager` watches Intents, owned ManagedResources, and IntentApprovals (mapped to `spec.intentRef.name`).
- Produces the condition constants in `status.go`:
  - **Condition types:** `CondReady`, `CondPlanned`, `CondExecuted`, `CondInSync`.
  - **Ready reasons:** `ReasonPlanning`, `ReasonAwaitingApproval`, `ReasonExecuting`, `ReasonVerifying`, `ReasonReady`, `ReasonDrifted`, `ReasonPlanningFailed`, `ReasonApprovalDenied`, `ReasonExecutionFailed`, `ReasonInterrupted`, `ReasonVerificationFailed`.
  - **Planned reasons:** `ReasonPlanWritten`, `ReasonReplanning`.
  - **Executed reasons:** `ReasonPhaseOpen`, `ReasonStepsSucceeded`, `ReasonPhaseEnded`.
  - **InSync reasons:** `ReasonProbesPassed`, `ReasonProbesFailed`.
- Produces the status helpers:
  - `setPhase(in, phase, reason, msg)`;
  - `setCondition(conds *[]metav1.Condition, gen int64, typ string, s metav1.ConditionStatus, reason, msg string)`;
  - `clip(s string) string`;
  - `(r *IntentReconciler) update(ctx, in, change func(*lastv1alpha1.Intent)) error`;
  - `(r *IntentReconciler) fail(ctx, in, reason, msg string) error`;
  - `(r *IntentReconciler) startPlanning(ctx, in, feedback string) (ctrl.Result, error)`.
- Produces the gRPC helpers `terminal(err) bool` and `describe(err) string` (in `gateway.go`).
- Produces the test seam: `fakeGateway` (`newFakeGateway`, `setScript`, `calls`), `script` and `turns`. Later tasks add accessors as their tests need them, so the `unused` linter never sees an unused helper.

- [ ] **Step 1: Scaffold the controllers with the kubebuilder CLI**

The CLI expects `cmd/main.go`, but this project keeps the manager in `cmd/operator/main.go`. Move it for the scaffold, then move it back:

```bash
git mv cmd/operator/main.go cmd/main.go
kubebuilder create api --group last --version v1alpha1 --kind Intent --controller=true --resource=false --make=false
kubebuilder create api --group last --version v1alpha1 --kind ManagedResource --controller=true --resource=false --make=false
git mv cmd/main.go cmd/operator/main.go
go mod tidy
```

Expected:
- `PROJECT` gains `controller: true` for both kinds.
- `cmd/operator/main.go` imports `internal/controller` and sets up both reconcilers with `Client` and `Scheme` above `// +kubebuilder:scaffold:builder`.
- `go.mod` gains Ginkgo and Gomega.

The scaffolded `main.go` wiring has no gateway yet. That is fine until Task 9: the binary is not deployed before then.

Delete the scaffold's ManagedResource test (Task 8 writes the real one):

```bash
git rm -f internal/controller/managedresource_controller_test.go
```

In `internal/controller/managedresource_controller.go`, replace the three scaffolded `// +kubebuilder:rbac` lines with the controller's final, narrower grants:

```go
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intentpolicies;operatorconfigs,verbs=get;list;watch
```

- [ ] **Step 2: Write the fake gateway**

Create `internal/controller/fakegateway_test.go`:

```go
package controller

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// llmFunc answers one CallLLM request.
type llmFunc func(*gatewayv1.CallLLMRequest) (string, error)

// turns answers with the nth answer when the conversation holds n assistant
// messages, like the gateway's scripted provider.
func turns(answers ...string) llmFunc {
	return func(req *gatewayv1.CallLLMRequest) (string, error) {
		n := 0
		for _, m := range req.GetMessages() {
			if m.GetRole() == "assistant" {
				n++
			}
		}
		if n >= len(answers) {
			return "", status.Error(codes.FailedPrecondition, "llm: scripted conversation has no more responses")
		}
		return answers[n], nil
	}
}

// script is how the fake gateway behaves for one Intent.
type script struct {
	llm      llmFunc
	stdout   map[string]string // step ID → stdout
	exit     map[string]int32  // step ID → exit code; a failing step's stderr is "boom"
	runErr   map[string]error  // step ID → RunCommand error
	endErrs  []error           // errors returned by the next EndPhase calls, in order
	failing  map[string]bool   // probe ID → fails; other probes pass
	probeErr error             // RunProbes error
	onProbe  func()            // called during a successful RunProbes
}

// fakeGateway stands in for the gateway in envtest (spec §18). It enforces the
// phase rules the operator depends on (approval before BeginPhase, one open
// phase, calls only within it, probes only of an executed plan) and records
// every call per Intent.
type fakeGateway struct {
	gatewayv1.GatewayServiceClient // unimplemented RPCs panic

	c client.Client

	mu       sync.Mutex
	scripts  map[types.NamespacedName]*script
	log      map[types.NamespacedName][]string // "CallLLM", "BeginPhase", "RunCommand:s1", "EndPhase:failed", …
	llmReqs  map[types.NamespacedName][]*gatewayv1.CallLLMRequest
	argv     map[types.NamespacedName]map[string][]string          // step ID → argv sent
	outputs  map[types.NamespacedName]map[string]map[string]string // ManagedResource → values sent
	open     map[types.NamespacedName]string                       // subject of the open phase
	executed map[types.NamespacedName]string                       // hash of the last executed plan
}

func newFakeGateway(c client.Client) *fakeGateway {
	return &fakeGateway{c: c, scripts: map[types.NamespacedName]*script{}, log: map[types.NamespacedName][]string{},
		llmReqs: map[types.NamespacedName][]*gatewayv1.CallLLMRequest{}, argv: map[types.NamespacedName]map[string][]string{},
		outputs: map[types.NamespacedName]map[string]map[string]string{}, open: map[types.NamespacedName]string{},
		executed: map[types.NamespacedName]string{}}
}

func keyOf(ref *gatewayv1.IntentRef) types.NamespacedName {
	return types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
}

func (f *fakeGateway) setScript(key types.NamespacedName, s *script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[key] = s
}

// calls returns the RPCs made for an Intent, in order.
func (f *fakeGateway) calls(key types.NamespacedName) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log[key])
}

// scriptFor and record are called with f.mu held.
func (f *fakeGateway) scriptFor(key types.NamespacedName) *script {
	if s := f.scripts[key]; s != nil {
		return s
	}
	return &script{}
}

func (f *fakeGateway) record(key types.NamespacedName, call string) {
	f.log[key] = append(f.log[key], call)
}

func (f *fakeGateway) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	f.record(key, "CallLLM")
	f.llmReqs[key] = append(f.llmReqs[key], in)
	llm := f.scriptFor(key).llm
	f.mu.Unlock()
	if llm == nil {
		return nil, status.Error(codes.FailedPrecondition, "llm: no script for this intent")
	}
	content, err := llm(in)
	if err != nil {
		return nil, err
	}
	return &gatewayv1.CallLLMResponse{Content: content}, nil
}

// approved reports whether an IntentApproval approves subject for the Intent's
// Create stage and none denies it.
func (f *fakeGateway) approved(ctx context.Context, key types.NamespacedName, subject string) (bool, error) {
	var list lastv1alpha1.IntentApprovalList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return false, err
	}
	ok := false
	for _, a := range list.Items {
		if a.Spec.IntentRef.Name != key.Name || a.Spec.Stage != lastv1alpha1.StageCreate || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return false, nil
		}
		ok = true
	}
	return ok, nil
}

func (f *fakeGateway) BeginPhase(ctx context.Context, in *gatewayv1.BeginPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.BeginPhaseResponse, error) {
	key := keyOf(in.GetIntent())
	approved, err := f.approved(ctx, key, in.GetSubjectHash())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "BeginPhase")
	switch {
	case f.open[key] != "":
		return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
	case !approved:
		return nil, status.Error(codes.FailedPrecondition, "no IntentApproval approves this plan")
	}
	f.open[key] = in.GetSubjectHash()
	return &gatewayv1.BeginPhaseResponse{}, nil
}

func (f *fakeGateway) RunCommand(_ context.Context, in *gatewayv1.RunCommandRequest, _ ...grpc.CallOption) (*gatewayv1.RunCommandResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "RunCommand:"+in.GetStepId())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	var c lastv1alpha1.Command
	if err := json.Unmarshal(in.GetCommandJson(), &c); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if f.argv[key] == nil {
		f.argv[key] = map[string][]string{}
	}
	f.argv[key][in.GetStepId()] = c.Run.Argv
	s := f.scriptFor(key)
	if err := s.runErr[in.GetStepId()]; err != nil {
		return nil, err
	}
	res := &gatewayv1.CommandResult{RunId: "run-" + in.GetStepId(), ExitCode: s.exit[in.GetStepId()], Stdout: s.stdout[in.GetStepId()]}
	if res.ExitCode != 0 {
		res.Stderr = "boom"
	}
	return &gatewayv1.RunCommandResponse{Result: res}, nil
}

func (f *fakeGateway) WriteOutputSecret(_ context.Context, in *gatewayv1.WriteOutputSecretRequest, _ ...grpc.CallOption) (*gatewayv1.WriteOutputSecretResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "WriteOutputSecret:"+in.GetManagedResource())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	if f.outputs[key] == nil {
		f.outputs[key] = map[string]map[string]string{}
	}
	f.outputs[key][in.GetManagedResource()] = in.GetValues()
	return &gatewayv1.WriteOutputSecretResponse{SecretName: in.GetManagedResource() + "-outputs"}, nil
}

func (f *fakeGateway) EndPhase(_ context.Context, in *gatewayv1.EndPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.EndPhaseResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	outcome := "failed"
	if in.GetSucceeded() {
		outcome = "succeeded"
	}
	f.record(key, "EndPhase:"+outcome)
	if s := f.scriptFor(key); len(s.endErrs) > 0 {
		err := s.endErrs[0]
		s.endErrs = s.endErrs[1:]
		return nil, err
	}
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	delete(f.open, key)
	if in.GetSucceeded() {
		f.executed[key] = in.GetSubjectHash()
	}
	return &gatewayv1.EndPhaseResponse{}, nil
}

func (f *fakeGateway) RunProbes(ctx context.Context, in *gatewayv1.RunProbesRequest, _ ...grpc.CallOption) (*gatewayv1.RunProbesResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	f.record(key, "RunProbes")
	executed := f.executed[key]
	s := f.scriptFor(key)
	f.mu.Unlock()
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	if executed == "" {
		return nil, status.Error(codes.FailedPrecondition, "intent has no executed plan to probe")
	}
	var intent lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &intent); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	resp := &gatewayv1.RunProbesResponse{PlanHash: executed}
	if intent.Status.Plan != nil {
		for _, r := range intent.Status.Plan.Resources {
			for _, p := range r.Probes {
				o := &gatewayv1.ProbeOutcome{Id: p.ID, Passed: !s.failing[p.ID], Result: &gatewayv1.CommandResult{}}
				if s.failing[p.ID] {
					o.Result.ExitCode, o.Error = 1, "assertion failed"
				}
				resp.Outcomes = append(resp.Outcomes, o)
			}
		}
	}
	if s.onProbe != nil {
		s.onProbe()
	}
	return resp, nil
}
```

- [ ] **Step 3: Replace the suite setup**

Replace `internal/controller/suite_test.go` with the following. It keeps the scaffold's structure and markers, and starts a manager running the Intent controller against the fake gateway:

```go
package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	gw        *fakeGateway
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	SetDefaultEventuallyTimeout(20 * time.Second)
	SetDefaultEventuallyPollingInterval(50 * time.Millisecond)
	SetDefaultConsistentlyDuration(time.Second)

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = lastv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("starting the Intent controller against a fake gateway")
	gw = newFakeGateway(k8sClient)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	Expect(err).NotTo(HaveOccurred())
	Expect((&IntentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Gateway: gw,
		Planner: &agent.Planner{LLM: gw}, Recorder: mgr.GetEventRecorder("last-operator"),
		RetryDelay: 10 * time.Millisecond}).SetupWithManager(mgr)).To(Succeed())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
```

- [ ] **Step 4: Write the test helpers and the failing planning tests**

Create `internal/controller/helpers_test.go`:

```go
package controller

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const intentName = "web"

func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func createIntent(key types.NamespacedName, request string) {
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentSpec{Request: request}}
	Expect(k8sClient.Create(ctx, in)).To(Succeed())
}

func getIntent(key types.NamespacedName) *lastv1alpha1.Intent {
	var in lastv1alpha1.Intent
	Expect(k8sClient.Get(ctx, key, &in)).To(Succeed())
	return &in
}

func phase(key types.NamespacedName) func() lastv1alpha1.Phase {
	return func() lastv1alpha1.Phase { return getIntent(key).Status.Phase }
}

func planHash(key types.NamespacedName) func() string {
	return func() string {
		if p := getIntent(key).Status.Plan; p != nil {
			return p.Hash
		}
		return ""
	}
}

func readyCondition(key types.NamespacedName) metav1.Condition {
	c := meta.FindStatusCondition(getIntent(key).Status.Conditions, CondReady)
	Expect(c).NotTo(BeNil())
	return *c
}

func readyReason(key types.NamespacedName) string { return readyCondition(key).Reason }

func editRequest(key types.NamespacedName, request string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		in.Spec.Request = request
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

func createApproval(key types.NamespacedName, stage lastv1alpha1.Stage, subject string, decision lastv1alpha1.Decision) {
	a := &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: key.Name}, Stage: stage,
			SubjectHash: subject, Decision: decision, ApprovedBy: "alice"}}
	Expect(k8sClient.Create(ctx, a)).To(Succeed())
}

// approve files a decision on the Intent's current plan.
func approve(key types.NamespacedName, decision lastv1alpha1.Decision) {
	p := getIntent(key).Status.Plan
	Expect(p).NotTo(BeNil())
	createApproval(key, lastv1alpha1.StageCreate, p.Hash, decision)
}

// answer is a model answer holding a plan.
func answer(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	b, err := json.Marshal(map[string]any{"summary": "test plan", "steps": steps, "resources": resources})
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

func argvStep(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func hashOf(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	h, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	Expect(err).NotTo(HaveOccurred())
	return h
}

// requestText returns the first user message of a conversation: the request prompt.
func requestText(req *gatewayv1.CallLLMRequest) string {
	for _, m := range req.GetMessages() {
		if m.GetRole() == "user" {
			return m.GetContent()
		}
	}
	return ""
}

func zeroHash() string { return "sha256:" + strings.Repeat("0", 64) }
```

Replace `internal/controller/intent_controller_test.go` with:

```go
package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

var _ = Describe("Intent planning", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	It("plans and waits for an approval of that exact plan", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.Plan.Hash).To(Equal(hashOf(steps)))
		Expect(in.Status.Plan.Summary).To(Equal("test plan"))
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))

		By("ignoring approvals of another plan, stage, or Intent")
		createApproval(key, lastv1alpha1.StageCreate, zeroHash(), lastv1alpha1.DecisionApprove)
		createApproval(key, lastv1alpha1.StageDelete, in.Status.Plan.Hash, lastv1alpha1.DecisionApprove)
		createApproval(types.NamespacedName{Namespace: key.Namespace, Name: "someone-else"}, lastv1alpha1.StageCreate,
			in.Status.Plan.Hash, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM"}))
	})

	It("fails when the plan is denied, even next to an approval", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))

		approve(key, lastv1alpha1.DecisionDeny)
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonApprovalDenied))
		Consistently(func() []string { return gw.calls(key) }).ShouldNot(ContainElement("BeginPhase"))
	})

	It("plans again when the spec changes before approval", func() {
		v1 := []lastv1alpha1.Command{argvStep("s1", "echo", "v1")}
		v2 := []lastv1alpha1.Command{argvStep("s1", "echo", "v2")}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(v2), nil
			}
			return answer(v1), nil
		}})
		createIntent(key, "say v1")
		Eventually(planHash(key)).Should(Equal(hashOf(v1)))

		editRequest(key, "say v2")
		Eventually(planHash(key)).Should(Equal(hashOf(v2)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
	})

	It("fails after MaxTurns answers that are not plans, and plans again after Failed when the spec changes", func() {
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "fixed") {
				return answer(steps), nil
			}
			return "not a plan", nil
		}})
		createIntent(key, "broken")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "CallLLM", "CallLLM"}))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))

		editRequest(key, "fixed")
		Eventually(planHash(key)).Should(Equal(hashOf(steps)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
	})

	It("fails when the gateway has no LLM answer for the Intent", func() {
		createIntent(key, "no script")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("no script for this intent"))
	})
})
```

- [ ] **Step 5: Run the tests to see them fail**

Run: `make test`
Expected: FAIL to compile. `IntentReconciler` has no field `Gateway` (and `Planner`, `Recorder`, `RetryDelay`); `ReasonAwaitingApproval` and the other reasons are undefined.

- [ ] **Step 6: Implement the status helpers**

Create `internal/controller/status.go`:

```go
package controller

import (
	"context"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// CondReady summarizes the lifecycle: True only in phase Ready. Its reason
	// says why an Intent is not Ready, including why it failed (spec §7).
	CondReady = "Ready"
	// CondPlanned holds the failure a new plan must address while replanning.
	CondPlanned = "Planned"
	// CondExecuted checkpoints the current plan's phase across operator restarts.
	CondExecuted = "Executed"
	// CondInSync on a ManagedResource reports whether its probes last passed (spec §5.3).
	CondInSync = "InSync"
)

// Ready reasons.
const (
	ReasonPlanning           = "Planning"
	ReasonAwaitingApproval   = "AwaitingApproval"
	ReasonExecuting          = "Executing"
	ReasonVerifying          = "Verifying"
	ReasonReady              = "Ready"
	ReasonDrifted            = "Drifted"
	ReasonPlanningFailed     = "PlanningFailed"
	ReasonApprovalDenied     = "ApprovalDenied"
	ReasonExecutionFailed    = "ExecutionFailed"
	ReasonInterrupted        = "ExecutionInterrupted"
	ReasonVerificationFailed = "VerificationFailed"
)

// Planned reasons.
const (
	ReasonPlanWritten = "PlanWritten"
	ReasonReplanning  = "Replanning"
)

// Executed reasons: the checkpoints of a phase, in order.
const (
	ReasonPhaseOpen      = "PhaseOpen"      // BeginPhase succeeded; steps are running
	ReasonStepsSucceeded = "StepsSucceeded" // every step succeeded and outputs are written
	ReasonPhaseEnded     = "PhaseEnded"     // the gateway recorded the phase as succeeded
)

// InSync reasons.
const (
	ReasonProbesPassed = "ProbesPassed"
	ReasonProbesFailed = "ProbesFailed"
)

// maxMessage bounds condition messages; command output stays in the audit log.
const maxMessage = 1024

// clip cuts s to maxMessage bytes on a UTF-8 boundary.
func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func setCondition(conds *[]metav1.Condition, gen int64, typ string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: clip(msg), ObservedGeneration: gen})
}

// setPhase moves an Intent to phase p and records why in its Ready condition.
func setPhase(in *lastv1alpha1.Intent, p lastv1alpha1.Phase, reason, msg string) {
	in.Status.Phase = p
	s := metav1.ConditionFalse
	if p == lastv1alpha1.PhaseReady {
		s = metav1.ConditionTrue
	}
	setCondition(&in.Status.Conditions, in.Generation, CondReady, s, reason, msg)
}

// update applies change to in and writes its status. On a conflict it
// re-reads the Intent and applies change again, so change must be a function
// of the object it is given. in holds the written object afterwards, so
// consecutive updates in one reconcile never wait for the informer cache. A
// phase change emits an Event (spec §16).
func (r *IntentReconciler) update(ctx context.Context, in *lastv1alpha1.Intent, change func(*lastv1alpha1.Intent)) error {
	from := in.Status.Phase
	key := client.ObjectKeyFromObject(in)
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		change(in)
		err := r.Status().Update(ctx, in)
		if apierrors.IsConflict(err) {
			if gerr := r.Get(ctx, key, in); gerr != nil {
				return gerr
			}
		}
		return err
	})
	if err != nil || in.Status.Phase == from {
		return err
	}
	kind := corev1.EventTypeNormal
	if in.Status.Phase == lastv1alpha1.PhaseFailed {
		kind = corev1.EventTypeWarning
	}
	note := "Moved to " + string(in.Status.Phase)
	if c := meta.FindStatusCondition(in.Status.Conditions, CondReady); c != nil && c.Message != "" {
		note = c.Message
	}
	r.Recorder.Eventf(in, nil, kind, string(in.Status.Phase), "Transition", "%s", note)
	return nil
}

// fail moves the Intent to Failed; a spec edit retries it (spec §7).
func (r *IntentReconciler) fail(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	logf.FromContext(ctx).Info("Intent failed", "reason", reason, "message", clip(msg))
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) { setPhase(in, lastv1alpha1.PhaseFailed, reason, msg) })
}
```

Create `internal/controller/gateway.go`:

```go
package controller

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// terminal reports gateway errors that retrying cannot fix. Everything else
// (Unavailable, DeadlineExceeded, Aborted, Unauthenticated, …) is retried.
func terminal(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.PermissionDenied, codes.InvalidArgument,
		codes.NotFound, codes.AlreadyExists, codes.Unimplemented:
		return true
	}
	return false
}

// describe returns an error's message without the gRPC prefix.
func describe(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}
```

- [ ] **Step 7: Implement Planning and AwaitingApproval**

Replace `internal/controller/intent_controller.go` with:

```go
package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// IntentReconciler drives an Intent through its lifecycle (spec §7). It acts
// only by calling the gateway, the one component that can cause side effects
// (spec §3).
type IntentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Gateway  gatewayv1.GatewayServiceClient
	Planner  *agent.Planner
	Recorder events.EventRecorder
	// RetryDelay is the pause between retries of a gateway call within one reconcile.
	RetryDelay time.Duration
}

// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents/finalizers,verbs=update
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intentapprovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile advances the Intent by one phase. Every status write triggers the
// next reconcile, so each phase handler does its work and records the result.
func (r *IntentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var in lastv1alpha1.Intent
	if err := r.Get(ctx, req.NamespacedName, &in); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !in.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // teardown arrives with the Delete stage
	}
	switch in.Status.Phase {
	case "", lastv1alpha1.PhasePending:
		return r.startPlanning(ctx, &in, "")
	case lastv1alpha1.PhasePlanning:
		return r.plan(ctx, &in)
	case lastv1alpha1.PhaseAwaitingApproval:
		return r.awaitApproval(ctx, &in)
	case lastv1alpha1.PhaseFailed:
		if in.Generation != in.Status.ObservedGeneration {
			return r.startPlanning(ctx, &in, "") // a spec edit retries a failed Intent
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IntentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lastv1alpha1.Intent{}).
		Owns(&lastv1alpha1.ManagedResource{}).
		Watches(&lastv1alpha1.IntentApproval{}, handler.EnqueueRequestsFromMapFunc(approvedIntent)).
		Named("intent").
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

// approvedIntent maps an IntentApproval to the Intent it names.
func approvedIntent(_ context.Context, o client.Object) []reconcile.Request {
	a, ok := o.(*lastv1alpha1.IntentApproval)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.IntentRef.Name}}}
}
```

Create `internal/controller/intent_planning.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
)

// planTimeout bounds one planning conversation.
const planTimeout = 10 * time.Minute

// startPlanning discards the current plan and asks for a new one for the
// current spec. feedback, when set, is a failure the new plan must address.
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageCreate
		in.Status.ObservedGeneration = in.Generation
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

// plan runs the planning conversation (spec §7) and records the plan for approval.
func (r *IntentReconciler) plan(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	feedback := ""
	if c := meta.FindStatusCondition(in.Status.Conditions, CondPlanned); c != nil && c.Reason == ReasonReplanning {
		feedback = c.Message
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	p, err := r.Planner.Plan(pctx, agent.Request{Namespace: in.Namespace, Name: in.Name, Text: in.Spec.Request,
		Answers: in.Spec.Answers, Feedback: feedback})
	if err != nil {
		if errors.Is(err, agent.ErrNoPlan) || terminal(err) {
			return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
		}
		return ctrl.Result{}, err // transient: retried with backoff
	}
	gen := in.Generation
	err = r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhasePlanning {
			return // the spec changed while planning; the next reconcile plans again
		}
		in.Status.Plan = p
		setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionTrue, ReasonPlanWritten, "Plan "+p.Hash)
		setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
			"Waiting for an IntentApproval of plan "+p.Hash+" for stage Create")
	})
	if apierrors.IsInvalid(err) {
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, "The API server rejected the plan: "+err.Error())
	}
	return ctrl.Result{}, err
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
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}
```

- [ ] **Step 8: Run the tests to see them pass**

Run: `make manifests generate && make test`
Expected:
- PASS, including `Intent planning` (5 specs).
- `config/rbac/role.yaml` regenerates with exactly the verbs from the markers: no `delete` anywhere and no `create` on intents.

- [ ] **Step 9: Lint, commit**

```bash
make lint-fix && make test && make lint
git add PROJECT go.mod go.sum cmd/operator/main.go config/rbac/role.yaml internal/controller/
git commit -m "feat(operator): plan Intents through the gateway and wait for approval"
```

---

### Task 6: Executing, with restart checkpoints

**Files:**
- Create: `internal/controller/intent_execution.go`
- Create: `internal/controller/managedresources.go`
- Modify: `internal/controller/intent_controller.go` (dispatch `Executing`)
- Modify: `internal/controller/gateway.go` (`intentRef`, `sleep`)
- Modify: `internal/controller/fakegateway_test.go` (accessors)
- Modify: `internal/controller/helpers_test.go` (helpers)
- Test: `internal/controller/intent_execution_test.go`

**Interfaces:**
- Consumes: `plan.Resolve`, `plan.Results`, and `runnerapi.Result` (`internal/sandbox/runnerapi`); `ManagedResourceSpec.Outputs` (Task 1).
- Produces:
  - `(r *IntentReconciler) execute(ctx, in) (ctrl.Result, error)`;
  - `managedResourceName(intent string, i int) string`, which returns `"<intent>-<i>"`;
  - `checkpoint(in) string` (the Executed reason, or `""`);
  - `intentRef(in) *gatewayv1.IntentRef`;
  - `sleep(ctx, d) error`.
- Leaves an Intent in `Verifying` with `Executed=True/PhaseEnded`. Task 7 handles Verifying.

- [ ] **Step 1: Add the test accessors and helpers**

Append to `internal/controller/fakegateway_test.go`:

```go
// setOpen makes the gateway hold an open phase for subject, as if an earlier
// operator process had opened it.
func (f *fakeGateway) setOpen(key types.NamespacedName, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[key] = subject
}

// setExecuted makes the gateway's last executed plan for the Intent subject.
func (f *fakeGateway) setExecuted(key types.NamespacedName, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed[key] = subject
}

func (f *fakeGateway) argvOf(key types.NamespacedName, step string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.argv[key][step])
}

func (f *fakeGateway) outputsOf(key types.NamespacedName) map[string]map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outputs[key]
}

// lastRequest returns the last CallLLM request for an Intent.
func (f *fakeGateway) lastRequest(key types.NamespacedName) *gatewayv1.CallLLMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	reqs := f.llmReqs[key]
	if len(reqs) == 0 {
		return nil
	}
	return reqs[len(reqs)-1]
}
```

Append to `internal/controller/helpers_test.go` (adding `k8s.io/client-go/util/retry` and `metav1` to its imports if they are not already there):

```go
// setStatus rewrites an Intent's status, as an earlier operator process might
// have left it.
func setStatus(key types.NamespacedName, change func(*lastv1alpha1.Intent)) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		change(&in)
		return k8sClient.Status().Update(ctx, &in)
	})).To(Succeed())
}

// executedReason returns the Intent's execution checkpoint.
func executedReason(key types.NamespacedName) func() string {
	return func() string {
		if c := meta.FindStatusCondition(getIntent(key).Status.Conditions, CondExecuted); c != nil {
			return c.Reason
		}
		return ""
	}
}

// executingAt sets an Intent at phase Executing with the given checkpoint.
func executingAt(key types.NamespacedName, reason string) {
	setStatus(key, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseExecuting
		setCondition(&in.Status.Conditions, in.Generation, CondExecuted, metav1.ConditionUnknown, reason, "set by the test")
	})
}

func getMR(ns, name string) *lastv1alpha1.ManagedResource {
	var mr lastv1alpha1.ManagedResource
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &mr)).To(Succeed())
	return &mr
}
```

- [ ] **Step 2: Write the failing execution tests**

Create `internal/controller/intent_execution_test.go`:

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
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

var _ = Describe("Intent execution", func() {
	var key types.NamespacedName
	s1 := argvStep("s1", "kubectl", "get", "namespace", "x", "-o", "json")
	s2 := argvStep("s2", "echo", "${steps.s1.json.metadata.name}")
	resource := lastv1alpha1.ManagedResourceSpec{Description: "the namespace",
		Probes: []lastv1alpha1.Probe{{ID: "p1", Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "true"}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"name": "${steps.s1.json.metadata.name}"}}
	stdout := map[string]string{"s1": `{"metadata":{"name":"x"}}`, "s2": "x\n"}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// awaitingApproval scripts the gateway and waits until the Intent asks for approval.
	awaitingApproval := func(s *script) string {
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		return getIntent(key).Status.Plan.Hash
	}

	It("runs the approved plan through the gateway and creates its ManagedResources", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1, s2}, resource)), stdout: stdout})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:6]).To(Equal([]string{"CallLLM", "BeginPhase", "RunCommand:s1", "RunCommand:s2",
			"WriteOutputSecret:web-0", "EndPhase:succeeded"}))
		Expect(gw.argvOf(key, "s2")).To(Equal([]string{"echo", "x"}))
		Expect(gw.outputsOf(key)).To(Equal(map[string]map[string]string{"web-0": resource.Outputs}))

		mr := getMR(key.Namespace, "web-0")
		Expect(metav1.IsControlledBy(mr, getIntent(key))).To(BeTrue())
		Expect(mr.Spec.IntentRef).To(Equal(&lastv1alpha1.LocalRef{Name: intentName}))
		Expect(mr.Spec.Probes).To(Equal(resource.Probes))
		Expect(mr.Spec.Outputs).To(Equal(resource.Outputs))
	})

	It("plans again with the failure when a step fails", func() {
		failing := []lastv1alpha1.Command{argvStep("s1", "false")}
		fixed := []lastv1alpha1.Command{argvStep("s2", "true")}
		awaitingApproval(&script{exit: map[string]int32{"s1": 1}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "boom") {
				return answer(fixed), nil
			}
			return answer(failing), nil
		}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(planHash(key)).Should(Equal(hashOf(fixed)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "BeginPhase", "RunCommand:s1", "EndPhase:failed", "CallLLM"}))
		Expect(requestText(gw.lastRequest(key))).To(ContainSubstring("step s1 exited 1: boom"))
		Expect(meta.FindStatusCondition(getIntent(key).Status.Conditions, CondExecuted)).To(BeNil())
	})

	It("ends the phase and fails when the gateway refuses a step", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1, s2})), stdout: stdout,
			runErr: map[string]error{"s2": status.Error(codes.PermissionDenied, "approval withdrawn")}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonExecutionFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("approval withdrawn"))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "BeginPhase", "RunCommand:s1", "RunCommand:s2", "EndPhase:failed"}))
	})

	It("retries EndPhase when the gateway is briefly unavailable", func() {
		unavailable := status.Error(codes.Unavailable, "audit store unreachable")
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1})), stdout: stdout,
			endErrs: []error{unavailable, unavailable}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:6]).To(Equal([]string{"CallLLM", "BeginPhase", "RunCommand:s1",
			"EndPhase:succeeded", "EndPhase:succeeded", "EndPhase:succeeded"}))
	})

	It("abandons a phase that an earlier process opened and did not finish", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setOpen(key, hash)
		executingAt(key, ReasonPhaseOpen)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "EndPhase:failed"}))
	})

	It("ends a phase that was left open before its checkpoint was written", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setOpen(key, hash)
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "BeginPhase", "EndPhase:failed"}))
	})

	It("confirms with the gateway a phase whose end it did not record", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setExecuted(key, hash)
		executingAt(key, ReasonStepsSucceeded)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:3]).To(Equal([]string{"CallLLM", "EndPhase:succeeded", "RunProbes"}))
	})

	It("fails when the gateway does not know the phase it was finishing", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		executingAt(key, ReasonStepsSucceeded)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "EndPhase:succeeded", "RunProbes"}))
	})
})
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `make test`
Expected: FAIL. The new specs time out in `AwaitingApproval` → `Executing`, because Executing is not dispatched yet.

- [ ] **Step 4: Implement the execution helpers**

Append to `internal/controller/gateway.go` (and add the `context`, `time`, `lastv1alpha1` and `gatewayv1` imports):

```go
func intentRef(in *lastv1alpha1.Intent) *gatewayv1.IntentRef {
	return &gatewayv1.IntentRef{Namespace: in.Namespace, Name: in.Name}
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

Create `internal/controller/managedresources.go`:

```go
package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// managedResourceName names the ManagedResource for resource i of an Intent's plan.
func managedResourceName(intent string, i int) string { return fmt.Sprintf("%s-%d", intent, i) }

// ensureManagedResources creates or updates one ManagedResource per plan
// resource. Each is controlled by the Intent and names it in spec.intentRef:
// the gateway requires both before it writes outputs (spec §15).
func (r *IntentReconciler) ensureManagedResources(ctx context.Context, in *lastv1alpha1.Intent) error {
	for i, res := range in.Status.Plan.Resources {
		mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: managedResourceName(in.Name, i), Namespace: in.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, mr, func() error {
			if owner := metav1.GetControllerOf(mr); owner != nil && owner.UID != in.UID {
				return fmt.Errorf("managed resource %s is controlled by another object", mr.Name)
			}
			mr.Spec = *res.DeepCopy()
			mr.Spec.IntentRef = &lastv1alpha1.LocalRef{Name: in.Name}
			return controllerutil.SetControllerReference(in, mr, r.Scheme)
		}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: Implement Executing**

Create `internal/controller/intent_execution.go`:

```go
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	// stepTimeout bounds one RunCommand call, including the wait for a sandbox slot.
	stepTimeout = 30 * time.Minute
	// retries bounds the attempts of one gateway call within a reconcile.
	retries = 5
)

// checkpoint returns the Executed condition's reason: how far the current
// plan's phase got.
func checkpoint(in *lastv1alpha1.Intent) string {
	if c := meta.FindStatusCondition(in.Status.Conditions, CondExecuted); c != nil {
		return c.Reason
	}
	return ""
}

func setExecuted(in *lastv1alpha1.Intent, s metav1.ConditionStatus, reason, msg string) {
	setCondition(&in.Status.Conditions, in.Generation, CondExecuted, s, reason, msg)
}

// execute runs the approved plan (spec §8.3). Step results live only in this
// process, so the Executed checkpoint decides what a reconcile may do: open
// the phase, abandon one this process lost, or confirm one whose steps all
// succeeded. A phase is never resumed.
func (r *IntentReconciler) execute(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, "The Intent has no plan to run")
	}
	switch checkpoint(in) {
	case ReasonStepsSucceeded:
		return r.finishPhase(ctx, in)
	case ReasonPhaseOpen:
		return r.abandonPhase(ctx, in, "The operator lost track of the running phase")
	}
	_, err := r.Gateway.BeginPhase(ctx, &gatewayv1.BeginPhaseRequest{Intent: intentRef(in),
		Stage: string(lastv1alpha1.StageCreate), SubjectHash: in.Status.Plan.Hash})
	switch {
	case status.Code(err) == codes.AlreadyExists:
		return r.abandonPhase(ctx, in, "A phase from an earlier attempt was still open")
	case terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, "The gateway refused to open the phase: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Opened phase", "plan", in.Status.Plan.Hash)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonPhaseOpen, "Running plan "+in.Status.Plan.Hash)
	}); err != nil {
		return ctrl.Result{}, err // the next attempt finds the phase open (AlreadyExists) and abandons it
	}
	return r.runSteps(ctx, in)
}

// runSteps runs every step in order, resolving ${steps…} references from the
// results so far, then creates the ManagedResources and writes their outputs.
func (r *IntentReconciler) runSteps(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	results := plan.Results{}
	for _, step := range in.Status.Plan.Steps {
		cmd, err := plan.Resolve(step, results)
		if err != nil {
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %v", step.ID, err), true)
		}
		raw, err := json.Marshal(cmd)
		if err != nil {
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %v", step.ID, err), false)
		}
		res, err := r.runStep(ctx, in, step.ID, raw)
		switch {
		case ctx.Err() != nil:
			return ctrl.Result{}, ctx.Err() // shutting down; the next process abandons the phase
		case err != nil:
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %s", step.ID, describe(err)), false)
		case res.GetExitCode() != 0 || res.GetError() != "":
			return r.endFailed(ctx, in, stepFailure(step.ID, res), true)
		}
		results[step.ID] = runnerapi.Result{ID: step.ID, Stdout: res.GetStdout(), StdoutTruncated: res.GetStdoutTruncated()}
	}
	if err := r.ensureManagedResources(ctx, in); err != nil {
		return r.endFailed(ctx, in, "Could not create ManagedResources: "+err.Error(), false)
	}
	if err := r.writeOutputs(ctx, in); err != nil {
		return r.endFailed(ctx, in, "Could not write an output Secret: "+err.Error(), false)
	}
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonStepsSucceeded, "Every step succeeded; closing the phase")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return r.finishPhase(ctx, in)
}

// runStep calls RunCommand, retrying while another call of the phase holds it (Aborted).
func (r *IntentReconciler) runStep(ctx context.Context, in *lastv1alpha1.Intent, id string, raw []byte) (*gatewayv1.CommandResult, error) {
	req := &gatewayv1.RunCommandRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash, StepId: id, CommandJson: raw}
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

// stepFailure describes a failed step for the replanning conversation.
func stepFailure(id string, res *gatewayv1.CommandResult) string {
	if res.GetError() != "" {
		return fmt.Sprintf("step %s could not run: %s", id, res.GetError())
	}
	return fmt.Sprintf("step %s exited %d: %s", id, res.GetExitCode(), tail(res.GetStderr(), 512))
}

// tail returns the last n bytes of s, cut on a UTF-8 boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// writeOutputs writes each resource's declared outputs (spec §15).
func (r *IntentReconciler) writeOutputs(ctx context.Context, in *lastv1alpha1.Intent) error {
	for i, res := range in.Status.Plan.Resources {
		if len(res.Outputs) == 0 {
			continue
		}
		name := managedResourceName(in.Name, i)
		req := &gatewayv1.WriteOutputSecretRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash,
			ManagedResource: name, Values: res.Outputs}
		for attempt := 1; ; attempt++ {
			_, err := r.Gateway.WriteOutputSecret(ctx, req)
			if err == nil {
				break
			}
			if status.Code(err) != codes.Aborted || attempt == retries {
				return fmt.Errorf("%s: %s", name, describe(err))
			}
			if err := sleep(ctx, r.RetryDelay); err != nil {
				return err
			}
		}
	}
	return nil
}

// endPhase calls EndPhase, retrying Unavailable with the same outcome: the
// gateway treats a repeated EndPhase as a retry of its record save.
func (r *IntentReconciler) endPhase(ctx context.Context, in *lastv1alpha1.Intent, succeeded bool, reason string) error {
	req := &gatewayv1.EndPhaseRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash, Succeeded: succeeded, Reason: reason}
	for attempt := 1; ; attempt++ {
		_, err := r.Gateway.EndPhase(ctx, req)
		if status.Code(err) != codes.Unavailable || attempt == retries {
			return err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return err
		}
	}
}

// endFailed closes the phase as failed. A failure of the plan itself returns
// the Intent to Planning with the failure as context (spec §8.3); anything
// else fails the Intent.
func (r *IntentReconciler) endFailed(ctx context.Context, in *lastv1alpha1.Intent, reason string, replan bool) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Ending phase as failed", "plan", in.Status.Plan.Hash, "reason", clip(reason))
	if err := r.endPhase(ctx, in, false, reason); err != nil && status.Code(err) != codes.FailedPrecondition {
		return ctrl.Result{}, err // still PhaseOpen: the next attempt abandons the phase
	}
	if replan {
		return r.startPlanning(ctx, in, reason)
	}
	return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, reason)
}

// abandonPhase closes a phase this process cannot continue and fails the Intent.
func (r *IntentReconciler) abandonPhase(ctx context.Context, in *lastv1alpha1.Intent, why string) (ctrl.Result, error) {
	if err := r.endPhase(ctx, in, false, why); err != nil && status.Code(err) != codes.FailedPrecondition {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.fail(ctx, in, ReasonInterrupted, why+"; edit the Intent's spec to plan again")
}

// finishPhase closes a phase whose steps all succeeded. If the gateway no
// longer has the phase open, an earlier EndPhase may have succeeded before its
// result was recorded here, or a gateway restart interrupted the phase:
// RunProbes reports which plan the gateway last executed.
func (r *IntentReconciler) finishPhase(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	err := r.endPhase(ctx, in, true, "")
	if status.Code(err) == codes.FailedPrecondition {
		executed, perr := r.executedPlan(ctx, in)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if executed != in.Status.Plan.Hash {
			return ctrl.Result{}, r.fail(ctx, in, ReasonInterrupted,
				"The phase ended before it succeeded ("+describe(err)+"); edit the Intent's spec to plan again")
		}
		err = nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Executed plan", "plan", in.Status.Plan.Hash)
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionTrue, ReasonPhaseEnded, "Plan "+in.Status.Plan.Hash+" executed")
		setPhase(in, lastv1alpha1.PhaseVerifying, ReasonVerifying, "Running every probe once")
	})
}

// executedPlan returns the hash of the plan the gateway last executed for the
// Intent, or "" when it has none.
func (r *IntentReconciler) executedPlan(ctx context.Context, in *lastv1alpha1.Intent) (string, error) {
	resp, err := r.Gateway.RunProbes(ctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	switch {
	case err == nil:
		return resp.GetPlanHash(), nil
	case terminal(err):
		return "", nil
	default:
		return "", err
	}
}
```

In `internal/controller/intent_controller.go`, add the Executing case to the `switch` in `Reconcile`, after the `PhaseAwaitingApproval` case:

```go
	case lastv1alpha1.PhaseExecuting:
		return r.execute(ctx, &in)
```

- [ ] **Step 6: Run the tests to see them pass**

Run: `make test`
Expected: PASS (`Intent planning` and `Intent execution`).

- [ ] **Step 7: Lint, commit**

```bash
make lint-fix && make test && make lint
git add internal/controller/
git commit -m "feat(operator): execute approved plans with restart checkpoints"
```

---

### Task 7: Verifying, Ready and Drifted

**Files:**
- Create: `internal/controller/intent_verify.go`
- Modify: `internal/controller/managedresources.go` (`ownedManagedResources`, `applyProbeOutcomes`)
- Modify: `internal/controller/intent_controller.go` (dispatch `Verifying`, `Ready` and `Drifted`)
- Modify: `internal/controller/helpers_test.go` (`setInSync`)
- Test: `internal/controller/intent_verify_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§7)

**Interfaces:**
- Produces:
  - `ownedManagedResources(ctx, c client.Reader, in) ([]lastv1alpha1.ManagedResource, error)`;
  - `applyProbeOutcomes(ctx, c client.Client, in, outcomes []*gatewayv1.ProbeOutcome, at time.Time) error`, which sets each owned ManagedResource's `status.observed` (`{"probes": {"<id>": {"passed", "exitCode", "error"}}}`), `status.lastProbeTime` and `InSync`. Task 8 uses it.
- Produces: the `Ready` ↔ `Drifted` transitions, which follow the ManagedResources' `InSync`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/helpers_test.go`:

```go
// setInSync sets a ManagedResource's InSync condition, as a probe run would.
func setInSync(ns, name string, s metav1.ConditionStatus) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mr := getMR(ns, name)
		setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, s, ReasonProbesFailed, "set by the test")
		return k8sClient.Status().Update(ctx, mr)
	})).To(Succeed())
}
```

Create `internal/controller/intent_verify_test.go`:

```go
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var _ = Describe("Intent verification", func() {
	var key types.NamespacedName
	probe := func(id string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "true"}
	}
	resource := lastv1alpha1.ManagedResourceSpec{Description: "web", Probes: []lastv1alpha1.Probe{probe("p1"), probe("p2")}}
	steps := []lastv1alpha1.Command{argvStep("s1", "true")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// run scripts the gateway, plans, and approves the plan.
	run := func(s *script) {
		s.llm = turns(answer(steps, resource))
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
	}

	It("runs every probe once and becomes Ready", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(readyCondition(key).Status).To(Equal(metav1.ConditionTrue))
		Expect(gw.calls(key)).To(Equal([]string{"CallLLM", "BeginPhase", "RunCommand:s1", "EndPhase:succeeded", "RunProbes"}))

		mr := getMR(key.Namespace, "web-0")
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(mr.Status.LastProbeTime).NotTo(BeNil())
		Expect(mr.Status.Observed.Raw).To(MatchJSON(`{"probes":{"p1":{"passed":true,"exitCode":0},"p2":{"passed":true,"exitCode":0}}}`))
	})

	It("fails verification when a probe fails", func() {
		run(&script{failing: map[string]bool{"p2": true}})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonVerificationFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("p2"))

		c := meta.FindStatusCondition(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Message).To(Equal("Probes failed: p2"))
	})

	It("follows its ManagedResources between Ready and Drifted", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		setInSync(key.Namespace, "web-0", metav1.ConditionFalse)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseDrifted))
		Expect(readyReason(key)).To(Equal(ReasonDrifted))

		setInSync(key.Namespace, "web-0", metav1.ConditionTrue)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
	})

	It("ignores a spec edit at Ready until the Update stage exists", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		n := len(gw.calls(key))

		editRequest(key, "something else")
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.calls(key)).To(HaveLen(n))
	})
})
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `make test`
Expected: FAIL. The verification specs time out in `Verifying`.

- [ ] **Step 3: Implement probe outcomes on ManagedResources**

Append to `internal/controller/managedresources.go` (and add the `encoding/json`, `strings`, `time`, `runtime`, `retry`, `client` and `gatewayv1` imports):

```go
// ownedManagedResources lists the ManagedResources the Intent controls.
func ownedManagedResources(ctx context.Context, c client.Reader, in *lastv1alpha1.Intent) ([]lastv1alpha1.ManagedResource, error) {
	var list lastv1alpha1.ManagedResourceList
	if err := c.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return nil, err
	}
	var owned []lastv1alpha1.ManagedResource
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], in) {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

// observedProbe is one probe's outcome in ManagedResource.status.observed.
// Probe output stays in the audit log (spec §10.2); status carries the verdict.
type observedProbe struct {
	Passed   bool   `json:"passed"`
	ExitCode int32  `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// applyProbeOutcomes records a probe run on the Intent's ManagedResources.
// Each gets the outcomes of its own probes, matched by probe ID, and an InSync
// condition. A ManagedResource with no probe in the run is left alone.
func applyProbeOutcomes(ctx context.Context, c client.Client, in *lastv1alpha1.Intent, outcomes []*gatewayv1.ProbeOutcome, at time.Time) error {
	byID := make(map[string]*gatewayv1.ProbeOutcome, len(outcomes))
	for _, o := range outcomes {
		byID[o.GetId()] = o
	}
	mrs, err := ownedManagedResources(ctx, c, in)
	if err != nil {
		return err
	}
	for i := range mrs {
		observed := map[string]observedProbe{}
		var failed []string
		for _, p := range mrs[i].Spec.Probes {
			o, ok := byID[p.ID]
			if !ok {
				continue
			}
			observed[p.ID] = observedProbe{Passed: o.GetPassed(), ExitCode: o.GetResult().GetExitCode(), Error: clip(o.GetError())}
			if !o.GetPassed() {
				failed = append(failed, p.ID)
			}
		}
		if len(observed) == 0 {
			continue
		}
		raw, err := json.Marshal(map[string]any{"probes": observed})
		if err != nil {
			return err
		}
		key := client.ObjectKeyFromObject(&mrs[i])
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var mr lastv1alpha1.ManagedResource
			if err := c.Get(ctx, key, &mr); err != nil {
				return err
			}
			mr.Status.Observed = &runtime.RawExtension{Raw: raw}
			mr.Status.LastProbeTime = &metav1.Time{Time: at}
			if len(failed) == 0 {
				setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, metav1.ConditionTrue, ReasonProbesPassed, "Every probe passed")
			} else {
				setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, metav1.ConditionFalse, ReasonProbesFailed,
					"Probes failed: "+strings.Join(failed, ", "))
			}
			return c.Status().Update(ctx, &mr)
		}); err != nil {
			return err
		}
	}
	return nil
}

// failedProbes returns the IDs of the probes that did not pass.
func failedProbes(outcomes []*gatewayv1.ProbeOutcome) []string {
	var failed []string
	for _, o := range outcomes {
		if !o.GetPassed() {
			failed = append(failed, o.GetId())
		}
	}
	return failed
}
```

- [ ] **Step 4: Implement Verifying and drift following**

Create `internal/controller/intent_verify.go`:

```go
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// verify runs every probe of the executed plan once; all must pass (spec §7).
// A failure here surfaces a bad probe at creation time instead of as drift.
func (r *IntentReconciler) verify(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The Intent has no plan to verify")
	}
	start := time.Now()
	resp, err := r.Gateway.RunProbes(ctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	switch {
	case status.Code(err) == codes.Aborted:
		return ctrl.Result{RequeueAfter: r.RetryDelay}, nil // a probe run is in flight; never overlap
	case terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The gateway refused the probe run: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	case resp.GetPlanHash() != in.Status.Plan.Hash:
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed,
			fmt.Sprintf("The gateway probed plan %s, not %s", resp.GetPlanHash(), in.Status.Plan.Hash))
	}
	if err := applyProbeOutcomes(ctx, r.Client, in, resp.GetOutcomes(), start); err != nil {
		return ctrl.Result{}, err
	}
	if failed := failedProbes(resp.GetOutcomes()); len(failed) > 0 {
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "Probes failed: "+strings.Join(failed, ", "))
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setPhase(in, lastv1alpha1.PhaseReady, ReasonReady, "Every probe passed")
	})
}

// followProbes moves a Ready Intent to Drifted when any of its ManagedResources
// is out of sync, and back when all are in sync again. Remediation arrives in M5.
func (r *IntentReconciler) followProbes(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	mrs, err := ownedManagedResources(ctx, r.Client, in)
	if err != nil {
		return ctrl.Result{}, err
	}
	var drifted []string
	for i := range mrs {
		if meta.IsStatusConditionFalse(mrs[i].Status.Conditions, CondInSync) {
			drifted = append(drifted, mrs[i].Name)
		}
	}
	switch {
	case len(drifted) > 0 && in.Status.Phase == lastv1alpha1.PhaseReady:
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			setPhase(in, lastv1alpha1.PhaseDrifted, ReasonDrifted, "Probes failed on "+strings.Join(drifted, ", "))
		})
	case len(drifted) == 0 && in.Status.Phase == lastv1alpha1.PhaseDrifted:
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			setPhase(in, lastv1alpha1.PhaseReady, ReasonReady, "Every probe passed")
		})
	}
	return ctrl.Result{}, nil
}
```

In `internal/controller/intent_controller.go`, add these cases to the `switch` in `Reconcile`, after the `PhaseExecuting` case:

```go
	case lastv1alpha1.PhaseVerifying:
		return r.verify(ctx, &in)
	case lastv1alpha1.PhaseReady, lastv1alpha1.PhaseDrifted:
		return r.followProbes(ctx, &in)
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `make test`
Expected: PASS (planning, execution and verification specs).

- [ ] **Step 6: Amend the spec**

In §7, after the "**Delete.**" bullet, add:

```markdown
- **Failures (M2).** A step that exits non-zero sends the Intent back to
  Planning with the failure as context. Any other failure moves it to
  `Failed`, with the reason in its `Ready` condition:
  - `PlanningFailed`
  - `ApprovalDenied`
  - `ExecutionFailed`
  - `ExecutionInterrupted`
  - `VerificationFailed`

  A spec edit retries a failed Intent. Automatic retry with backoff
  arrives with Auto modes (M5). Until then every replan needs a fresh
  approval.
- **Operator restarts during Executing.** Step results live only in the
  operator's memory and the gateway's record, so the operator never
  resumes a phase. An `Executed` condition checkpoints the phase:
  - **Phase open:** the operator ends it as failed and marks the Intent
    `ExecutionInterrupted`.
  - **Steps succeeded but the end is unrecorded:** the operator retries
    `EndPhase`. If the gateway has already closed the phase, the operator
    accepts the execution only when `RunProbes` reports the plan's hash as
    the last executed plan.
```

In the "**Operator restarts during Planning:**" paragraph, append: "Until `QueryAudit` arrives (M3), a restart during Planning starts the conversation over."

- [ ] **Step 7: Lint, commit**

```bash
make lint-fix && make test && make lint
git add internal/controller/ docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(operator): verify executed plans and follow drift"
```

---
### Task 8: Fixed-interval probe runs (ManagedResource controller)

**Files:**
- Replace: `internal/controller/managedresource_controller.go`
- Create: `internal/controller/schedule.go` (`nextRun`)
- Create: `internal/controller/metrics.go`
- Modify: `internal/controller/fakegateway_test.go` (`updateScript`)
- Test: `internal/controller/schedule_test.go` (plain `testing`)
- Test: `internal/controller/managedresource_controller_test.go` (Ginkgo)
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§16)

**Interfaces:**
- Consumes: `policy.ProbeInterval` (Task 2), `applyProbeOutcomes` and `ownedManagedResources` (Task 7), `intentRef` (Task 6).
- Produces the reconciler:

  ```go
  type ManagedResourceReconciler struct {
  	client.Client
  	Scheme  *runtime.Scheme
  	Gateway gatewayv1.GatewayServiceClient
  	Now     func() time.Time // nil means time.Now
  	// unexported: mu, next map[types.NamespacedName]time.Time
  }
  ```

  Its reconcile key is the **Intent**. It watches Intents only: one probe run covers all of an Intent's ManagedResources.
- Produces: `nextRun(start, finish time.Time, interval time.Duration) (time.Time, int)`.
- Produces the metrics `last_operator_probe_runs_total{namespace,intent,result}` (result is `passed`, `failed` or `error`) and `last_operator_probe_runs_skipped_total{namespace,intent}`. Both are registered with the controller-runtime metrics registry.

- [ ] **Step 1: Write the failing schedule test**

Create `internal/controller/schedule_test.go`:

```go
package controller

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		took    time.Duration
		next    time.Duration
		skipped int
	}{
		{0, 5 * time.Minute, 0},
		{4 * time.Minute, 5 * time.Minute, 0},
		{5 * time.Minute, 5 * time.Minute, 0}, // finished exactly when the next run is due: it still runs
		{6 * time.Minute, 10 * time.Minute, 1},
		{11 * time.Minute, 15 * time.Minute, 2},
	}
	for _, tc := range cases {
		next, skipped := nextRun(start, start.Add(tc.took), 5*time.Minute)
		if !next.Equal(start.Add(tc.next)) || skipped != tc.skipped {
			t.Errorf("run took %v: nextRun = +%v, %d skipped; want +%v, %d", tc.took, next.Sub(start), skipped, tc.next, tc.skipped)
		}
	}
	if next, _ := nextRun(start, start, 0); !next.After(start) {
		t.Errorf("a zero interval scheduled the next run at %v", next)
	}
}
```

Run: `go test ./internal/controller/ -run TestNextRun`
Expected: FAIL to compile. `undefined: nextRun`.

- [ ] **Step 2: Implement the schedule and metrics**

Create `internal/controller/schedule.go`:

```go
package controller

import (
	"time"

	"github.com/moogie-labs/last-operator/internal/policy"
)

// nextRun returns when the run after one that started at start and finished at
// finish is due, on the fixed grid start + k·interval. Grid points that passed
// while the run was in flight are skipped and counted: an Intent never has two
// probe runs in flight (spec §10.1).
func nextRun(start, finish time.Time, interval time.Duration) (time.Time, int) {
	if interval <= 0 {
		interval = policy.DefaultMinProbeInterval
	}
	next := start.Add(interval)
	skipped := 0
	for next.Before(finish) {
		next = next.Add(interval)
		skipped++
	}
	return next, skipped
}
```

Create `internal/controller/metrics.go`:

```go
package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	probeRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_total",
		Help: "Scheduled probe runs, by result (passed, failed, error).",
	}, []string{"namespace", "intent", "result"})
	probeRunsSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_skipped_total",
		Help: "Scheduled probe runs skipped because the previous run was still in flight (spec §10.1).",
	}, []string{"namespace", "intent"})
)

func init() {
	metrics.Registry.MustRegister(probeRuns, probeRunsSkipped)
}
```

Run: `go test ./internal/controller/ -run TestNextRun -v`
Expected: PASS (the Ginkgo suite also runs; its specs pass).

- [ ] **Step 3: Write the failing controller tests**

Append to `internal/controller/fakegateway_test.go`:

```go
// updateScript changes an Intent's script in place.
func (f *fakeGateway) updateScript(key types.NamespacedName, change func(*script)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scripts[key] == nil {
		f.scripts[key] = &script{}
	}
	change(f.scripts[key])
}
```

Create `internal/controller/managedresource_controller_test.go`:

```go
package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// These specs call Reconcile directly with a controlled clock; the suite's
// manager runs only the Intent controller.
var _ = Describe("ManagedResource probe schedule", func() {
	var (
		key  types.NamespacedName
		r    *ManagedResourceReconciler
		now  time.Time
		last time.Time // the Verifying run's start, as recorded on the ManagedResource
	)

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
		r = &ManagedResourceReconciler{Client: k8sClient, Scheme: scheme.Scheme, Gateway: gw, Now: func() time.Time { return now }}
		resource := lastv1alpha1.ManagedResourceSpec{Probes: []lastv1alpha1.Probe{{ID: "p1",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "true"}}}
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{argvStep("s1", "true")}, resource))})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		last = getMR(key.Namespace, "web-0").Status.LastProbeTime.Time
	})

	reconcileAt := func(t time.Time) ctrl.Result {
		now = t
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	probeCalls := func() int {
		n := 0
		for _, c := range gw.calls(key) {
			if c == "RunProbes" {
				n++
			}
		}
		return n
	}

	It("runs the probes when they are due and not before", func() {
		before := probeCalls()
		Expect(reconcileAt(last.Add(time.Minute)).RequeueAfter).To(Equal(4 * time.Minute))
		Expect(probeCalls()).To(Equal(before))

		Expect(reconcileAt(last.Add(5 * time.Minute)).RequeueAfter).To(Equal(5 * time.Minute))
		Expect(probeCalls()).To(Equal(before + 1))
		mr := getMR(key.Namespace, "web-0")
		Expect(mr.Status.LastProbeTime.Time).To(BeTemporally("~", last.Add(5*time.Minute), time.Second))
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "passed"))).To(Equal(1.0))
	})

	It("records drift when a scheduled run fails", func() {
		gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
		reconcileAt(last.Add(5 * time.Minute))
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseDrifted))
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "failed"))).To(Equal(1.0))
	})

	It("counts the runs that fell due while a run was in flight as skipped", func() {
		start := last.Add(5 * time.Minute)
		gw.updateScript(key, func(s *script) { s.onProbe = func() { now = start.Add(11 * time.Minute) } })
		// Runs were due at start+5m and start+10m while this one took 11 minutes; the next is at start+15m.
		Expect(reconcileAt(start).RequeueAfter).To(Equal(4 * time.Minute))
		Expect(testutil.ToFloat64(probeRunsSkipped.WithLabelValues(key.Namespace, key.Name))).To(Equal(2.0))
	})

	It("skips a run when the gateway already has one in flight", func() {
		gw.updateScript(key, func(s *script) {
			s.probeErr = status.Error(codes.Aborted, "a probe run for this intent is already in flight")
		})
		Expect(reconcileAt(last.Add(5 * time.Minute)).RequeueAfter).To(Equal(5 * time.Minute))
		Expect(testutil.ToFloat64(probeRunsSkipped.WithLabelValues(key.Namespace, key.Name))).To(Equal(1.0))
		Expect(getMR(key.Namespace, "web-0").Status.LastProbeTime.Time).To(BeTemporally("~", last, time.Second))
	})

	It("does not probe an Intent that is not Ready", func() {
		other := types.NamespacedName{Namespace: key.Namespace, Name: "unplanned"}
		createIntent(other, "no script")
		Eventually(phase(other)).Should(Equal(lastv1alpha1.PhaseFailed))
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: other})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{}))
		Expect(gw.calls(other)).NotTo(ContainElement("RunProbes"))
	})
})
```

Run: `make test`
Expected: FAIL to compile. `ManagedResourceReconciler` has no field `Gateway` or `Now`.

- [ ] **Step 4: Implement the controller**

Replace `internal/controller/managedresource_controller.go` with:

```go
package controller

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/policy"
)

// probeTimeout bounds one RunProbes call, including the wait for a sandbox slot.
const probeTimeout = 15 * time.Minute

// ManagedResourceReconciler runs each Ready or Drifted Intent's probes on a
// fixed interval and records the outcomes on its ManagedResources (spec §10,
// §10.1). One RunProbes call covers all of an Intent's ManagedResources, so the
// reconcile key is the Intent. It never calls the LLM.
type ManagedResourceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Gateway gatewayv1.GatewayServiceClient
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu   sync.Mutex
	next map[types.NamespacedName]time.Time // when each Intent's next probe run is due
}

// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intentpolicies;operatorconfigs,verbs=get;list;watch

// Reconcile runs the Intent's probes when they are due and requeues for the next run.
func (r *ManagedResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var in lastv1alpha1.Intent
	if err := r.Get(ctx, req.NamespacedName, &in); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if (in.Status.Phase != lastv1alpha1.PhaseReady && in.Status.Phase != lastv1alpha1.PhaseDrifted) || !hasProbes(in.Status.Plan) {
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	interval, err := r.interval(ctx, in.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	due, ok := r.due(req.NamespacedName)
	if !ok {
		last, err := r.lastProbe(ctx, &in)
		if err != nil {
			return ctrl.Result{}, err
		}
		due = last.Add(interval)
		r.setDue(req.NamespacedName, due)
	}
	if now.Before(due) {
		return ctrl.Result{RequeueAfter: due.Sub(now)}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(&in)})
	cancel()
	finish := r.now()
	next, skipped := nextRun(now, finish, interval)
	r.setDue(req.NamespacedName, next)
	switch {
	case status.Code(err) == codes.Aborted:
		skipped++ // a run for this Intent is still in flight: never overlap
	case err != nil:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Error(err, "Could not run probes")
	case resp.GetPlanHash() != in.Status.Plan.Hash:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Info("Gateway probed a plan other than the Intent's", "probed", resp.GetPlanHash(), "plan", in.Status.Plan.Hash)
	default:
		result := "passed"
		if len(failedProbes(resp.GetOutcomes())) > 0 {
			result = "failed"
		}
		probeRuns.WithLabelValues(in.Namespace, in.Name, result).Inc()
		if err := applyProbeOutcomes(ctx, r.Client, &in, resp.GetOutcomes(), now); err != nil {
			return ctrl.Result{}, err
		}
	}
	if skipped > 0 {
		probeRunsSkipped.WithLabelValues(in.Namespace, in.Name).Add(float64(skipped))
	}
	return ctrl.Result{RequeueAfter: next.Sub(finish)}, nil
}

func (r *ManagedResourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ManagedResourceReconciler) due(key types.NamespacedName) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.next[key]
	return t, ok
}

func (r *ManagedResourceReconciler) setDue(key types.NamespacedName, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == nil {
		r.next = map[types.NamespacedName]time.Time{}
	}
	r.next[key] = t
}

func (r *ManagedResourceReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.next, key)
}

// interval resolves the namespace's fixed probe interval (spec §10.1).
func (r *ManagedResourceReconciler) interval(ctx context.Context, ns string) (time.Duration, error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default"}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return 0, err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	return policy.ProbeInterval(op, policies.Items), nil
}

// lastProbe returns the newest probe time among the Intent's ManagedResources,
// or the zero time when none was probed. After a restart, the schedule resumes from it.
func (r *ManagedResourceReconciler) lastProbe(ctx context.Context, in *lastv1alpha1.Intent) (time.Time, error) {
	mrs, err := ownedManagedResources(ctx, r.Client, in)
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, mr := range mrs {
		if t := mr.Status.LastProbeTime; t != nil && t.After(last) {
			last = t.Time
		}
	}
	return last, nil
}

func hasProbes(p *lastv1alpha1.Plan) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Resources {
		if len(r.Probes) > 0 {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager. It watches
// Intents: a phase change to Ready starts the schedule, and RequeueAfter keeps it.
func (r *ManagedResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("managedresource").
		Watches(&lastv1alpha1.Intent{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}
```

Run: `make test`
Expected: PASS (`ManagedResource probe schedule` and every earlier spec).

- [ ] **Step 5: Amend the spec**

In §16, change the bullet "Probe runs skipped due to overlap" to:

```markdown
- Probe runs skipped due to overlap (`last_operator_probe_runs_skipped_total`)
```

and the bullet "Probe runs, failures, and drift events" to:

```markdown
- Probe runs, failures, and drift events (`last_operator_probe_runs_total`, by result)
```

- [ ] **Step 6: Lint, commit**

```bash
make manifests generate
make lint-fix && make test && make lint
git add internal/controller/ config/rbac/role.yaml docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(operator): run probes on a fixed interval without overlap"
```

---

### Task 9: Operator deployment, RBAC and network policy

**Files:**
- Modify: `cmd/operator/main.go` (gateway flags and reconciler wiring)
- Create: `charts/last-operator/files/operator-clusterrole.yaml`
- Modify: `charts/last-operator/templates/operator.yaml`
- Modify: `charts/last-operator/templates/gateway.yaml` (gRPC ingress `podSelector`)
- Modify: `charts/last-operator/values.yaml` (`operator.resources`)
- Modify: `Makefile` (`helm-lint` checks)
- Test: `test/admission/operator_rbac_test.go`
- Modify: `docs/superpowers/specs/2026-09-26-last-operator-design.md` (§4.1 RBAC, §4.2 NetworkPolicy)

**Interfaces:**
- Consumes: `gatewayclient.Dial` (Task 3), `agent.Planner` (Task 4), `IntentReconciler` (Tasks 5–7), `ManagedResourceReconciler` (Task 8).
- Produces the operator flags `--gateway-address` (default `last-gateway.last-gateway.svc:9090`) and `--gateway-token-file` (default `/var/run/secrets/last-operator/gateway/token`).
- Produces the chart objects:
  - ClusterRole `last-operator` (from `files/operator-clusterrole.yaml`);
  - Role `last-operator-leader-election` in the release namespace;
  - Deployment `last-operator`, with pod label `app.kubernetes.io/name: last-operator` and a projected token of audience `last-gateway`.

- [ ] **Step 1: Write the failing RBAC test**

Create `test/admission/operator_rbac_test.go`:

```go
package admission_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// TestOperatorRBAC checks the chart's operator ClusterRole (spec §4.1, §17.1).
// The untrusted operator reads approvals and policy, writes only its own CRs,
// and touches nothing else. The role is bound to a fresh ServiceAccount, so
// grants made by other tests in this package cannot leak in.
func TestOperatorRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "last-operator", "files", "operator-clusterrole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	const sa = "rbac-check"
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "operator-rbac-check"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: "last-system"}}}
	for _, o := range []client.Object{&role, binding} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	kube := kubernetes.NewForConfigOrDie(adminCfg)
	can := func(a authzv1.ResourceAttributes) bool {
		t.Helper()
		res, err := kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:last-system:" + sa,
			Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:last-system", "system:authenticated"},
			ResourceAttributes: &a}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Status.Allowed
	}
	attr := func(group, ns, resource, sub, verb string) authzv1.ResourceAttributes {
		return authzv1.ResourceAttributes{Group: group, Namespace: ns, Resource: resource, Subresource: sub, Verb: verb}
	}
	last := lastv1alpha1.GroupVersion.Group

	allowed := []authzv1.ResourceAttributes{
		attr(last, "", "intents", "", "watch"),
		attr(last, "team-x", "intents", "status", "update"),
		attr(last, "team-x", "managedresources", "", "create"),
		attr(last, "team-x", "managedresources", "status", "update"),
		attr(last, "", "intentapprovals", "", "list"),
		attr(last, "", "intentpolicies", "", "list"),
		attr(last, "", "operatorconfigs", "", "get"),
		attr("events.k8s.io", "team-x", "events", "", "create"),
	}
	// The binding reaches the authorizer through an informer; wait until it applies.
	deadline := time.Now().Add(30 * time.Second)
	for !can(allowed[0]) {
		if time.Now().After(deadline) {
			t.Fatal("the operator role never took effect")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, a := range allowed {
		if !can(a) {
			t.Errorf("operator cannot %s %s/%s in %q", a.Verb, a.Resource, a.Subresource, a.Namespace)
		}
	}

	var denied []authzv1.ResourceAttributes
	for _, ns := range []string{"", "team-x", "last-gateway", "last-sandbox"} {
		for _, verb := range []string{"get", "list", "watch", "create", "update"} {
			denied = append(denied, attr("", ns, "secrets", "", verb))
		}
	}
	denied = append(denied,
		attr("", "last-gateway", "configmaps", "", "get"),
		attr("", "last-gateway", "configmaps", "", "list"),
		attr("", "last-sandbox", "pods", "", "get"),
		attr("", "last-sandbox", "pods", "", "list"),
		attr("", "last-sandbox", "pods", "log", "get"),
		attr("", "last-sandbox", "pods", "exec", "create"),
		attr("", "", "users", "", "impersonate"),
		attr("", "", "serviceaccounts", "", "impersonate"),
		attr("", "last-gateway", "serviceaccounts", "token", "create"),
		attr("networking.k8s.io", "last-sandbox", "networkpolicies", "", "create"),
		attr(last, "", "intents", "", "create"),
		attr(last, "team-x", "intents", "", "update"),
		attr(last, "team-x", "intents", "", "delete"),
		attr(last, "team-x", "managedresources", "", "delete"),
		attr(last, "team-x", "managedresources", "", "deletecollection"),
	)
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		denied = append(denied, attr(last, "team-x", "intentapprovals", "", verb))
		for _, r := range []string{"intentpolicies", "commandrules"} {
			denied = append(denied, attr(last, "team-x", r, "", verb))
		}
		for _, r := range []string{"operatorconfigs", "providers", "llmproviders", "clustercommandrules"} {
			denied = append(denied, attr(last, "", r, "", verb))
		}
		for _, r := range []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"} {
			denied = append(denied, attr("admissionregistration.k8s.io", "", r, "", verb))
		}
	}
	for _, verb := range []string{"create", "update", "escalate", "bind"} {
		for _, r := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
			denied = append(denied, attr("rbac.authorization.k8s.io", "", r, "", verb))
		}
	}
	for _, a := range denied {
		if can(a) {
			t.Errorf("operator can %s %s %s/%s in %q", a.Verb, a.Group, a.Resource, a.Subresource, a.Namespace)
		}
	}
}
```

Run: `go test ./test/admission/ -run TestOperatorRBAC` (with `KUBEBUILDER_ASSETS="$(make -s envtest-assets)"`)
Expected: FAIL. `open ../../charts/last-operator/files/operator-clusterrole.yaml: no such file or directory`.

- [ ] **Step 2: Write the operator ClusterRole**

Create `charts/last-operator/files/operator-clusterrole.yaml`:

```yaml
# The operator's cluster-wide permissions (spec §4.1). The operator is
# untrusted: it reads approvals, policy and configuration, and writes only
# Intent status and ManagedResources. It has no access to Secrets, Pods,
# ConfigMaps or anything in the gateway, sandbox or audit namespaces, cannot
# impersonate or mint tokens, and cannot delete ManagedResources (that would
# garbage-collect output Secrets unaudited). test/admission checks this file.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: last-operator
rules:
  - apiGroups: [last.moogie-labs.com]
    resources: [intents]
    verbs: [get, list, watch]
  - apiGroups: [last.moogie-labs.com]
    resources: [intents/status]
    verbs: [get, update, patch]
  - apiGroups: [last.moogie-labs.com]
    resources: [intents/finalizers]
    verbs: [update]
  - apiGroups: [last.moogie-labs.com]
    resources: [managedresources]
    verbs: [get, list, watch, create, update, patch]
  - apiGroups: [last.moogie-labs.com]
    resources: [managedresources/status]
    verbs: [get, update, patch]
  - apiGroups: [last.moogie-labs.com]
    resources: [intentapprovals, intentpolicies, operatorconfigs]
    verbs: [get, list, watch]
  - apiGroups: [events.k8s.io]
    resources: [events]
    verbs: [create, patch]
```

Run: `KUBEBUILDER_ASSETS="$(make -s envtest-assets)" go test ./test/admission/ -v -count=1`
Expected: PASS (`TestOperatorRBAC` and the existing admission tests).

- [ ] **Step 3: Deploy the operator from the chart**

Replace `charts/last-operator/templates/operator.yaml` with:

```yaml
# The operator's identity. The gateway accepts gRPC calls only from this
# ServiceAccount's projected token (audience last-gateway; spec §4.2).
apiVersion: v1
kind: ServiceAccount
metadata:
  name: last-operator
  namespace: {{ .Release.Namespace }}
  labels: {{- include "last.labels" . | nindent 4 }}
---
{{ .Files.Get "files/operator-clusterrole.yaml" }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: last-operator
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: last-operator}
subjects:
  - {kind: ServiceAccount, name: last-operator, namespace: {{ .Release.Namespace }}}
---
# Leader election, in the operator's own namespace only.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: last-operator-leader-election
  namespace: {{ .Release.Namespace }}
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [""]
    resources: [events]
    verbs: [create, patch]
  - apiGroups: [events.k8s.io]
    resources: [events]
    verbs: [create, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: last-operator-leader-election
  namespace: {{ .Release.Namespace }}
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: last-operator-leader-election}
subjects:
  - {kind: ServiceAccount, name: last-operator, namespace: {{ .Release.Namespace }}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: last-operator
  namespace: {{ .Release.Namespace }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: last-operator
  template:
    metadata:
      labels:
        # The gateway's NetworkPolicy admits gRPC from pods with this label only.
        app.kubernetes.io/name: last-operator
        {{- include "last.labels" . | nindent 8 }}
    spec:
      serviceAccountName: last-operator
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: operator
          image: {{ include "last.image" (list . "operator") }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            - --leader-elect
            - --health-probe-bind-address=:8081
            - --metrics-bind-address=:8080
            - --metrics-secure=false
            - --gateway-address={{ printf "last-gateway.%s.svc:9090" .Values.namespaces.gateway }}
            - --gateway-token-file=/var/run/secrets/last-operator/gateway/token
          ports:
            - {name: metrics, containerPort: 8080}
            - {name: health, containerPort: 8081}
          readinessProbe:
            httpGet: {path: /readyz, port: health}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /healthz, port: health}
            periodSeconds: 20
          volumeMounts:
            - {name: gateway-token, mountPath: /var/run/secrets/last-operator/gateway, readOnly: true}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          resources: {{- toYaml .Values.operator.resources | nindent 12 }}
      volumes:
        # The gateway authenticates every call with this token (spec §4.2).
        - name: gateway-token
          projected:
            sources:
              - serviceAccountToken:
                  audience: last-gateway
                  expirationSeconds: 3600
                  path: token
```

In `charts/last-operator/values.yaml`, add after the `gateway:` block:

```yaml
operator:
  resources:
    requests: {cpu: 50m, memory: 64Mi}
    limits: {memory: 256Mi}
```

In `charts/last-operator/templates/gateway.yaml`, narrow the gRPC ingress item of the `last-gateway` NetworkPolicy to the operator's pods. The `podSelector` goes in the **same** `from:` element as the `namespaceSelector`, so both must match:

```yaml
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: {{ .Release.Namespace }}
          podSelector:
            matchLabels:
              app.kubernetes.io/name: last-operator
      ports:
        - {port: 9090, protocol: TCP}
```

- [ ] **Step 4: Wire the reconcilers to the gateway**

In `cmd/operator/main.go`:

(a) Add imports. `"time"` goes in the standard-library group. These go above `// +kubebuilder:scaffold:imports`:

```go
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/gatewayclient"
```

(b) Add the flags next to the other `flag.StringVar` calls, declaring `var gatewayAddr, gatewayTokenFile string` with the other `var`s:

```go
	flag.StringVar(&gatewayAddr, "gateway-address", "last-gateway.last-gateway.svc:9090", "The gateway's gRPC address.")
	flag.StringVar(&gatewayTokenFile, "gateway-token-file", "/var/run/secrets/last-operator/gateway/token",
		"The projected ServiceAccount token (audience last-gateway) sent with every gateway call.")
```

(c) Replace the two scaffolded `SetupWithManager` blocks (above `// +kubebuilder:scaffold:builder`) with:

```go
	conn, err := gatewayclient.Dial(gatewayAddr, gatewayTokenFile)
	if err != nil {
		setupLog.Error(err, "Failed to create gateway client", "address", gatewayAddr)
		os.Exit(1)
	}
	gw := gatewayv1.NewGatewayServiceClient(conn)
	if err := (&controller.IntentReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Gateway:    gw,
		Planner:    &agent.Planner{LLM: gw},
		Recorder:   mgr.GetEventRecorder("last-operator"),
		RetryDelay: 5 * time.Second,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "intent")
		os.Exit(1)
	}
	if err := (&controller.ManagedResourceReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Gateway: gw,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "managedresource")
		os.Exit(1)
	}
```

Run: `go build ./cmd/operator/ && go run ./cmd/operator --help 2>&1 | grep -A1 gateway-`
Expected: the build succeeds, and the help lists `-gateway-address` and `-gateway-token-file`.

- [ ] **Step 5: Check the chart in `helm-lint`**

In the `Makefile`, append to the `helm-lint` recipe (tab-indented, like its other lines):

```make
	helm template last charts/last-operator -n last-system | grep -q 'image: ghcr.io/moogie-labs/last-operator:'
	helm template last charts/last-operator -n last-system | grep -q 'audience: last-gateway'
	helm template last charts/last-operator -n last-system | grep -q -- '--gateway-address=last-gateway.last-gateway.svc:9090'
	helm template last charts/last-operator -n last-system | grep -q 'name: last-operator-leader-election'
	@# The gateway's gRPC port admits only the operator's pods: the podSelector shares the namespaceSelector's from: element.
	helm template last charts/last-operator -n last-system | grep -A1 'kubernetes.io/metadata.name: last-system' | grep -q '^          podSelector:'
	@# The operator never gets Secrets or ManagedResource deletes (spec §4.1; test/admission checks the full list).
	! grep -Eq '^[[:space:]]+(resources|verbs): \[.*(secrets|delete)' charts/last-operator/files/operator-clusterrole.yaml
```

(The check matches only `resources:` and `verbs:` lists, because the file's header comment mentions Secrets and delete.)

Run: `make helm-lint`
Expected: exits 0.

- [ ] **Step 6: Amend the spec**

In §4.1, replace the **RBAC** bullet list with:

```markdown
**RBAC** (one ClusterRole, `charts/last-operator/files/operator-clusterrole.yaml`,
checked by `test/admission`; plus Leases and Events in its own namespace for
leader election):

- get/list/watch on `Intent`; update/patch on `intents/status`; update on
  `intents/finalizers`.
- get/list/watch/create/update/patch on `ManagedResource` and its status.
  **No delete**: deleting a ManagedResource garbage-collects its output
  Secret with no audit entry.
- get/list/watch on `IntentApproval`, `IntentPolicy`, and `OperatorConfig`.
- create/patch on `events.k8s.io` Events.
- Later milestones add:
  - create on `CommandRuleSuggestion` (M7). The `spec.decision` field is
    excluded by admission (§9.5).
  - get/list/watch on `CommandRule` and `ClusterCommandRule` (M6).
  - update/patch on `Intent`, for finalizers (M5).
- **No** create on `IntentApproval`, `CommandRule`, or
  `ClusterCommandRule`. **No** writes to `IntentPolicy`, `OperatorConfig`,
  `Provider`, `LLMProvider`, RBAC, or admission policies.
- **No** access to Secrets, Pods, ConfigMaps, NetworkPolicies,
  ServiceAccount tokens, impersonation, or anything else.
```

In §4.2, replace "A NetworkPolicy limits ingress to the gRPC port to pods in `last-system`." with "A NetworkPolicy limits ingress to the gRPC port to the operator's pods in `last-system`."

- [ ] **Step 7: Lint, test, commit**

```bash
make lint-fix && make test && make lint && make helm-lint
git add cmd/operator/main.go charts/last-operator/ Makefile test/admission/operator_rbac_test.go docs/superpowers/specs/2026-09-26-last-operator-design.md
git commit -m "feat(chart): deploy the operator with least-privilege RBAC and a gateway token"
```

---

### Task 10: Scripted-LLM web-server e2e

**Files:**
- Create: `test/e2e/operator_e2e_test.go`
- Modify: `test/e2e/lifecycle_e2e_test.go` (pause the operator while it drives the gateway by hand)
- Modify: `hack/e2e.sh` (build and load the operator image; preload nginx)
- Modify: `docs/superpowers/followups/2026-10-05-m2b-followups.md`
- Create: `docs/superpowers/followups/2026-10-05-m2c-followups.md`

**Interfaces:**
- Consumes these e2e helpers: `kubectl`, `secretValue`, `portForward`, `runLast`, `env` (`audit_e2e_test.go`); `intentEntryTypes`, `containsInOrder` (`lifecycle_e2e_test.go`).
- Produces: `TestOperatorWebServer`, the M2 done-criterion test. It also produces the helpers `pauseOperator(t)`, `verifyAuditChain(t)` and `e2eClients(t)`.

- [ ] **Step 1: Build and load the operator image in the e2e script**

In `hack/e2e.sh`:

(a) After the `REGISTRY=` line, add:

```bash
# Must match nginxImage in test/e2e/operator_e2e_test.go. Preloaded so the
# cluster never pulls from a public registry during the test.
NGINX_IMAGE="nginx:1.27-alpine"
```

(b) Replace the `make images …` line and the `for c in …` loop with:

```bash
make images IMAGE_TAG="$TAG" IMAGE_REGISTRY="$REGISTRY" COMPONENTS="operator gateway audit-init"
for c in operator gateway audit-init sandbox-base; do
  kind load docker-image --name "$CLUSTER" "$REGISTRY/last-$c:$TAG"
done
docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1 || docker pull "$NGINX_IMAGE"
kind load docker-image --name "$CLUSTER" "$NGINX_IMAGE"
```

(c) After the `helm upgrade --install …` command, add:

```bash
# A rebuilt image under the same tag is only picked up by new pods.
kubectl rollout restart -n last-system deploy/last-operator
kubectl rollout status -n last-system deploy/last-operator --timeout 5m
```

- [ ] **Step 2: Keep the operator away from the lifecycle test's hand-driven Intent**

`TestGatewayLifecycle` writes an Intent's status itself and calls the gateway as the operator. With the operator deployed, the operator would also reconcile that Intent: it would plan it, fail for lack of an LLM script, and overwrite the plan. Pause the operator for the test.

Append to `test/e2e/lifecycle_e2e_test.go`:

```go
// pauseOperator scales the operator to zero for the rest of the test, so a test
// that drives the gateway by hand owns its Intents.
func pauseOperator(t *testing.T) {
	t.Helper()
	kubectl(t, "scale", "-n", "last-system", "deploy/last-operator", "--replicas=0")
	kubectl(t, "wait", "-n", "last-system", "--for=delete", "pod", "-l", "app.kubernetes.io/name=last-operator", "--timeout=2m")
	t.Cleanup(func() {
		kubectl(t, "scale", "-n", "last-system", "deploy/last-operator", "--replicas=1")
		kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	})
}
```

In `TestGatewayLifecycle`, call it right after the first `kubectl(t, "rollout", "status", …)` line:

```go
	pauseOperator(t)
```

`kubectl wait --for=delete` returns at once when no pod matches.

- [ ] **Step 3: Write the web-server e2e**

Create `test/e2e/operator_e2e_test.go`:

```go
//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// nginxImage must match NGINX_IMAGE in hack/e2e.sh, which preloads it into kind.
const nginxImage = "nginx:1.27-alpine"

func e2eClients(t *testing.T) (client.Client, kubernetes.Interface) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = lastv1alpha1.AddToScheme(sch)
	crc, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	return crc, kubernetes.NewForConfigOrDie(cfg)
}

// waitFor polls cond every two seconds until it returns true or timeout passes.
// cond's string describes the current state for the failure message.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s; last state: %s", timeout, what, state)
		}
		time.Sleep(2 * time.Second)
	}
}

// verifyAuditChain runs `kubectl last audit verify` against the bundled store.
func verifyAuditChain(t *testing.T) {
	t.Helper()
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	pub := filepath.Join(t.TempDir(), "pub.pem")
	pubPEM := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-pubkey", "-o", `jsonpath={.data.ed25519\.pub}`)
	if err := os.WriteFile(pub, []byte(pubPEM+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runLast(env{
		"LAST_AUDIT_ENDPOINT": "http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000"),
		"LAST_AUDIT_BUCKET":   "last-audit", "LAST_AUDIT_CHAIN_ID": chain,
		"AWS_ACCESS_KEY_ID":     secretValue(t, "last-audit", "last-audit-reader", "accessKey"),
		"AWS_SECRET_ACCESS_KEY": secretValue(t, "last-audit", "last-audit-reader", "secretKey"),
	}, "audit", "verify", "--pubkey-file", pub)
	if err != nil {
		t.Fatalf("audit verify: %v\n%s", err, out)
	}
}

// operatorCan asks the API server whether the operator's ServiceAccount may do something.
func operatorCan(t *testing.T, args ...string) bool {
	t.Helper()
	out, _ := exec.Command("kubectl", append([]string{"auth", "can-i", "--as", "system:serviceaccount:last-system:last-operator"}, args...)...).Output()
	return strings.TrimSpace(string(out)) == "yes"
}

// TestOperatorWebServer is M2's done-criterion (spec §20): a scripted-LLM web
// server Intent goes Pending → Ready via kubectl in a sandbox, with a verified
// audit chain.
func TestOperatorWebServer(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	ctx := context.Background()
	crc, kube := e2eClients(t)
	// A unique suffix keeps this run's objects, and so its audit entries, distinct from any earlier run.
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	ns, provider, llmName := "e2e-web-"+suffix, "e2e-web-"+suffix, "e2e-web-"+suffix
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	// OperatorConfig default: allow the Provider and use the scripted LLM; restored in cleanup.
	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		create(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	} else if err != nil {
		t.Fatal(err)
	}
	var prevLLM *lastv1alpha1.LocalRef
	t.Cleanup(func() {
		bg := context.Background()
		for _, o := range []client.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "last-gateway"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"}},
			&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: provider}},
			&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}},
		} {
			if err := crc.Delete(bg, o); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup %T: %v", o, err)
			}
		}
		for range 5 {
			var cur lastv1alpha1.OperatorConfig
			if err := crc.Get(bg, client.ObjectKey{Name: "default"}, &cur); err != nil {
				t.Errorf("cleanup: read OperatorConfig: %v", err)
				return
			}
			if cur.Spec.Defaults != nil {
				cur.Spec.Defaults.Providers = slices.DeleteFunc(cur.Spec.Defaults.Providers, func(n string) bool { return n == provider })
			}
			if cur.Spec.LLMProviderRef != nil && cur.Spec.LLMProviderRef.Name == llmName {
				cur.Spec.LLMProviderRef = prevLLM
			}
			if err := crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not restore OperatorConfig")
	})

	// The namespace, a Provider that can edit only that namespace, and a 1m probe interval there.
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "last-gateway"}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "provider-edit", Namespace: ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: provider, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: provider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: provider}}}})
	create(&lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "probes", Namespace: ns}, Spec: lastv1alpha1.IntentPolicySpec{
		Probes: &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: time.Minute}}}})

	// The plan the scripted LLM answers with.
	k8s := []string{provider}
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: nginx
          image: %[2]s
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 80}]
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: %[1]s}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 80}]
`, ns, nginxImage)
	steps := []lastv1alpha1.Command{
		{ID: "apply", Reason: "Create the nginx Deployment and its Service",
			Run:   lastv1alpha1.CommandRun{Argv: []string{"kubectl", "apply", "-f", "/work/web.yaml"}},
			Files: map[string]string{"/work/web.yaml": manifest}, Providers: k8s, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
		{ID: "rollout", Reason: "Wait until nginx is ready",
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "rollout", "status", "deployment/web", "-n", ns, "--timeout=3m"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: 4 * time.Minute}},
		{ID: "svc", Reason: "Read the Service",
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "service", "web", "-n", ns, "-o", "json"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: time.Minute}},
	}
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "nginx web server " + ns + "/web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","namespace":"` + ns + `","name":"web"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "web-ready",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deployment", "web", "-n", ns, "-o", "json"}}, Providers: k8s},
			Assert:  "result.exitCode == 0 && result.json.status.readyReplicas == 1"}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"clusterIP": "${steps.svc.json.spec.clusterIP}"},
	}}
	answer, err := json.Marshal(map[string]any{"summary": "nginx Deployment and Service web in " + ns, "steps": steps, "resources": resources})
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal([]string{string(answer)})
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"},
		Data: map[string]string{llm.ScriptKey(ns + "/web"): string(script)}})
	create(&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}, Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "scripted", Scripted: &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: llmName}}}})
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	prevLLM = op.Spec.LLMProviderRef
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: llmName}
	if op.Spec.Defaults == nil {
		op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
	}
	op.Spec.Defaults.Providers = append(op.Spec.Defaults.Providers, provider)
	if err := crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}

	// The Intent, as a user writes it.
	create(&lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: lastv1alpha1.IntentSpec{Request: "Run an nginx web server named web in namespace " + ns}})
	key := client.ObjectKey{Namespace: ns, Name: "web"}
	var intent lastv1alpha1.Intent
	state := func() string {
		c := meta.FindStatusCondition(intent.Status.Conditions, "Ready")
		if c == nil {
			return string(intent.Status.Phase)
		}
		return fmt.Sprintf("%s (%s: %s)", intent.Status.Phase, c.Reason, c.Message)
	}
	waitFor(t, 3*time.Minute, "the plan to await approval", func() (bool, string) {
		if err := crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		return intent.Status.Phase == lastv1alpha1.PhaseAwaitingApproval, state()
	})
	if intent.Status.Plan.Hash != wantHash {
		t.Fatalf("plan hash = %s, want %s (the scripted plan)", intent.Status.Plan.Hash, wantHash)
	}

	// Approve as ourselves: the admission policy requires approvedBy to be the creator.
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	create(&lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: "web-create", Namespace: ns},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: wantHash, Decision: lastv1alpha1.DecisionApprove, ApprovedBy: review.Status.UserInfo.Username}})
	waitFor(t, 10*time.Minute, "the Intent to become Ready", func() (bool, string) {
		if err := crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		if intent.Status.Phase == lastv1alpha1.PhaseFailed {
			t.Fatalf("the Intent failed: %s", state())
		}
		return intent.Status.Phase == lastv1alpha1.PhaseReady, state()
	})

	// The world matches the plan.
	if got := kubectl(t, "get", "deployment", "web", "-n", ns, "-o", "jsonpath={.status.readyReplicas}"); got != "1" {
		t.Fatalf("nginx readyReplicas = %q", got)
	}
	clusterIP := kubectl(t, "get", "service", "web", "-n", ns, "-o", "jsonpath={.spec.clusterIP}")
	if got := secretValue(t, ns, "web-outputs", "clusterIP"); got != clusterIP {
		t.Fatalf("output secret clusterIP = %q, want %q", got, clusterIP)
	}
	var mr lastv1alpha1.ManagedResource
	if err := crc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-0"}, &mr); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&mr, &intent) || !meta.IsStatusConditionTrue(mr.Status.Conditions, "InSync") || mr.Status.LastProbeTime == nil {
		t.Fatalf("managed resource = %+v", mr)
	}

	// The ManagedResource controller probes again on the 1m schedule.
	first := mr.Status.LastProbeTime.Time
	waitFor(t, 4*time.Minute, "a scheduled probe run", func() (bool, string) {
		if err := crc.Get(ctx, client.ObjectKeyFromObject(&mr), &mr); err != nil {
			return false, err.Error()
		}
		return mr.Status.LastProbeTime.After(first), mr.Status.LastProbeTime.String()
	})

	// The operator stayed within its RBAC: it cannot touch Secrets, sandbox pods, or approvals.
	for _, args := range [][]string{
		{"get", "secrets", "-n", ns},
		{"list", "pods", "-n", "last-sandbox"},
		{"create", "intentapprovals.last.moogie-labs.com", "-n", ns},
		{"delete", "managedresources.last.moogie-labs.com", "-n", ns},
	} {
		if operatorCan(t, args...) {
			t.Errorf("the operator can %v", args)
		}
	}

	// Every step is in the WORM chain, in order, and the chain verifies.
	types := intentEntryTypes(t, ns+"/web")
	want := []string{audit.TypeLLMRequest, audit.TypeLLMResponse, audit.TypeApprovalObserved, audit.TypePhaseBegin,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeOutputWritten, audit.TypePhaseEnd,
		audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult}
	if !containsInOrder(types, want) {
		t.Fatalf("chain entries for the intent = %v, want %v in order", types, want)
	}
	verifyAuditChain(t)
}
```

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output.

- [ ] **Step 4: Run the e2e suite on kind**

Run: `KIND_CLUSTER=last-e2e make test-e2e`

This uses the cluster `last-e2e`. It creates it if missing; never delete a kind cluster this work did not create.

Expected: PASS for every e2e test, including `TestOperatorWebServer` (about 3–6 minutes) and `TestGatewayLifecycle` (with the operator paused).

On failure, inspect with:
- `kubectl -n last-system logs deploy/last-operator`;
- `kubectl -n last-gateway logs deploy/last-gateway`;
- `kubectl get intent -A -o yaml`.

- [ ] **Step 5: Record the follow-ups**

In `docs/superpowers/followups/2026-10-05-m2b-followups.md`, replace the heading `## M2c (operator side) must respect or build` with `## M2c (operator side) must respect or build — done in M2c` and add this line directly under it:

```markdown
All items are addressed by `docs/superpowers/plans/2026-10-05-m2c-operator-lifecycle.md`. The Intent identity decision is deferred: see the M2c follow-ups.
```

Create `docs/superpowers/followups/2026-10-05-m2c-followups.md`:

```markdown
# M2c (operator lifecycle) follow-ups

Rulings and deferred items from `docs/superpowers/plans/2026-10-05-m2c-operator-lifecycle.md`.

## Rulings made in M2c

- **Pending.** An Intent with no phase is Pending; the first reconcile moves it straight to Planning.
- **Failed is terminal until a spec edit.** Automatic retry with backoff (spec §7) arrives with Auto modes (M5), along with a cap on replans after step failures. Until then every replan needs a fresh human approval.
- **Restarts during Planning** start the conversation over. Reloading it from `QueryAudit` arrives in M3.
- **Restarts during Executing.** A phase the operator lost track of is ended as failed and the Intent becomes `ExecutionInterrupted`. Phases are never resumed, because step results are not persisted outside the gateway.
- **Unavailable during `RunCommand`** is treated as a failed step without replanning: the gateway may or may not have run it.
- **Plan-declared outputs.** `ManagedResourceSpec.outputs` is part of the approved plan, and the gateway writes exactly those values.
- **ManagedResource naming.** ManagedResources are named `<intent>-<index>`. An Intent name longer than 251 characters fails at ManagedResource creation.
- **Probe schedule.** The schedule is kept in memory. After a restart it resumes from the newest `lastProbeTime`. Probe outcomes are matched to ManagedResources by probe ID from the ManagedResource's spec, which only affects display; the gateway runs the executed plan's probes.

## Deferred

- **Intent identity (from the M2b follow-ups).** A deleted and recreated Intent with the same name shares the gateway record and approvals, which are keyed by namespace and name. A recreated Intent whose plan is identical to an executed one is refused with "already opened a phase". Fix it with the "approval replay" item: tie approvals and the record to the Intent UID.
- **Stale ManagedResources.** A ManagedResource created by a failed phase, or by an earlier plan with more resources, stays (with `InSync` unset) until the M5 Delete stage. The operator has no delete on ManagedResources by design.
- **Deletion.** Deleting an Intent garbage-collects its ManagedResources and their output Secrets without an audit entry. Finalizers and teardown arrive in M5.
- **Long reconciles.** Planning and execution run inside one reconcile, so a long step holds a worker. Four workers per controller suffice for M2. Revisit with real LLMs (M3).
- **Status size.** The plan is stored in `Intent.status` and is capped at 256 KiB of canonical JSON. Large manifests in `files` count toward that cap.
- **Events.** Events are emitted on phase transitions only. Per-step Events could help `kubectl describe`.
```

- [ ] **Step 6: Lint, test, commit**

```bash
make lint-fix && make test && make lint && make helm-lint
git add hack/e2e.sh test/e2e/ docs/superpowers/followups/
git commit -m "test(e2e): drive a scripted web-server Intent from Pending to Ready"
```

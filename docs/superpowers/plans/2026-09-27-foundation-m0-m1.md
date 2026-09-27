# Foundation (M0 Scaffolding + M1 Audit) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scaffold the kubebuilder project with every CRD type from the spec, and ship the write-once audit foundation: the hash-chained, signed audit library, a leader-elected gateway that writes to a bundled RustFS WORM store and fails closed, a WORM conformance check, and `kubectl last audit verify|log|conformance`.

**Architecture:** One Go module with three binaries in M1: `gateway`, `audit-init`, and `kubectl-last`. The `operator` binary is scaffolded but runs no controllers until M2. `internal/audit` is the core: entry sealing, a `Sink` interface (in-memory for tests, S3 for real), a verifier, and a `Writer` whose `Do` method is the fail-closed primitive. A Helm chart installs the CRDs, a single-node RustFS StatefulSet (`last-audit`), an init Job that creates the Object Lock bucket and least-privilege users, and the gateway (`last-gateway`).

**Tech Stack:** Go 1.27, kubebuilder v4 (controller-runtime, controller-gen, envtest), aws-sdk-go-v2 (S3 + SigV4), gowebpki/jcs (RFC 8785), client-go leader election, Prometheus client, cobra, testcontainers-go, RustFS `rustfs/rustfs:1.0.0`, Helm 4, kind.

**Spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md` (read §3, §4, §5, §14 before starting).

## Global Constraints

- Go module path: `github.com/moogie-labs/last-operator`
- API group/version: `last.moogie-labs.com/v1alpha1` (kubebuilder `--domain moogie-labs.com`, `--group last`)
- Namespaces: operator `last-system` (Helm release namespace), gateway `last-gateway`, audit store `last-audit`
- Bundled audit store image: `rustfs/rustfs:1.0.0`; S3 API on port 9000; health `/health`, readiness `/health/ready`; root credentials via env `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY`; data dir env `RUSTFS_VOLUMES=/data`; runs as UID/GID 10001
- RustFS admin API: native paths `/rustfs/admin/v3/*` take **plain JSON** bodies, SigV4-signed with service `s3`, region `us-east-1`. Never use the `/minio/admin/*` paths (they require encrypted bodies)
- No AGPL dependencies (e.g. do **not** import `github.com/minio/madmin-go` or `minio-go`)
- Every S3 client must set `RequestChecksumCalculation` and `ResponseChecksumValidation` to `WhenRequired`, and use path-style addressing for RustFS
- Audit constants: default retention 365 days (`8760h`); checkpoint every 1000 entries or 10 minutes; entry key `chains/<chainId>/<seq %020d>.json`; checkpoint index key `checkpoints/<chainId>/<seq %020d>.json`; genesis `prevHash` = `sha256:` + 64 zeros; timestamps formatted `2006-01-02T15:04:05.000000000Z` (UTC)
- Tests for our own packages use the standard `testing` package (no Ginkgo)
- Integration tests (Docker required) use build tag `integration`; e2e tests use build tag `e2e`
- Commit messages: conventional style (`feat:`, `test:`, `chore:`…). **Do not add `Co-authored-by` or other attribution trailers.**
- Start Docker before Tasks 9–11 and 15 (integration and e2e tests)

## File Map

| Path | Responsibility |
|---|---|
| `api/v1alpha1/common_types.go` | Shared types: `Mode`, `Stage`, `StageModes`, `Command`, `Probe`, `LocalRef`, … |
| `api/v1alpha1/{intent,intentapproval,managedresource}_types.go` | Lifecycle CRDs |
| `api/v1alpha1/{commandrule,clustercommandrule,commandrulesuggestion}_types.go` | Allow-rule CRDs |
| `api/v1alpha1/{intentpolicy,operatorconfig,provider,llmprovider}_types.go` | Configuration CRDs |
| `api/v1alpha1/*_test.go` | envtest CRD validation tests |
| `internal/audit/entry.go`, `canonical.go`, `sign.go` | Entry model, RFC 8785 hashing, Ed25519 sealing, PEM helpers |
| `internal/audit/sink.go`, `keys.go` | `Sink` interface, object key layout |
| `internal/audit/audittest/memsink.go` | In-memory `Sink` with tamper helpers |
| `internal/audit/verify.go` | Chain verifier |
| `internal/audit/writer.go`, `guard.go` | Chain writer, checkpoints, fail-closed `Do` |
| `internal/audit/s3sink.go`, `bucket.go`, `lock.go` | S3 `Sink`, bucket creation, lock-config check |
| `internal/audit/conformance.go` | WORM conformance checks |
| `internal/testutil/rustfstest/rustfs.go` | RustFS testcontainer harness (integration tag) |
| `internal/rustfsadmin/client.go` | Minimal SigV4 client for the RustFS admin API |
| `internal/auditinit/init.go` | Bucket + policy + user provisioning |
| `cmd/audit-init/main.go` | Init Job entry point |
| `internal/gateway/{config,signingkey,metrics,server}.go` | Gateway config, key bootstrap, metrics, leader-elected audit writer |
| `cmd/gateway/main.go` | Gateway entry point |
| `internal/cli/{root,audit}.go` | `kubectl last` commands |
| `cmd/kubectl-last/main.go` | CLI entry point |
| `cmd/operator/main.go` | Operator entry point (moved from kubebuilder's `cmd/main.go`) |
| `Dockerfile` | One parameterized image build (`--build-arg CMD=<binary>`) |
| `charts/last-operator/` | Helm chart |
| `hack/e2e.sh`, `test/e2e/audit_e2e_test.go` | kind end-to-end suite |
| `.github/workflows/*.yml` | CI |

---

### Task 1: Toolchain and kubebuilder scaffold

**Files:**
- Create (via kubebuilder): `go.mod`, `Makefile`, `PROJECT`, `Dockerfile`, `cmd/main.go`, `config/**`, `api/v1alpha1/*_types.go` (placeholder types), `.github/workflows/*`, `hack/boilerplate.go.txt`, `test/**`
- Move: `cmd/main.go` → `cmd/operator/main.go`
- Modify: `Makefile`, `Dockerfile`

**Interfaces:**
- Produces: `make test` (unit + envtest), `make manifests generate`, and a new target `make envtest-assets` that prints the `KUBEBUILDER_ASSETS` path. Scaffolded placeholder files `api/v1alpha1/<kind>_types.go` for all ten kinds, which Tasks 2–4 overwrite.

- [ ] **Step 1: Install kubebuilder and kind**

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
curl -fsSL -o "$(go env GOPATH)/bin/kubebuilder" "https://go.kubebuilder.io/dl/latest/$(go env GOOS)/$(go env GOARCH)"
chmod +x "$(go env GOPATH)/bin/kubebuilder"
go install sigs.k8s.io/kind@latest
kubebuilder version && kind version
```
Expected: both print versions.

- [ ] **Step 2: Initialize the project**

```bash
cd /Users/gregkrause/Documents/business/internal/moogie-labs/last-operator
kubebuilder init --domain moogie-labs.com --repo github.com/moogie-labs/last-operator --project-name last-operator --license none --owner "Moogie Labs"
```
If kubebuilder refuses because the directory is not empty, run `mv docs /tmp/last-docs`, re-run the command, then run `mv /tmp/last-docs docs`.
Expected: `go.mod`, `Makefile`, `PROJECT`, `cmd/main.go`, and `config/` exist.

- [ ] **Step 3: Scaffold all ten API types (no controllers)**

```bash
for k in Intent IntentApproval ManagedResource CommandRule CommandRuleSuggestion IntentPolicy; do
  kubebuilder create api --group last --version v1alpha1 --kind "$k" --resource --controller=false
done
for k in ClusterCommandRule OperatorConfig Provider LLMProvider; do
  kubebuilder create api --group last --version v1alpha1 --kind "$k" --resource --controller=false --namespaced=false
done
ls api/v1alpha1
```
Expected: ten `*_types.go` files plus `groupversion_info.go`. `grep 'Group:' api/v1alpha1/groupversion_info.go` shows `last.moogie-labs.com`.

- [ ] **Step 4: Move the operator entry point to `cmd/operator`**

```bash
mkdir -p cmd/operator && git mv cmd/main.go cmd/operator/main.go 2>/dev/null || mv cmd/main.go cmd/operator/main.go
sed -i '' 's#cmd/main.go#cmd/operator/main.go#g' Makefile Dockerfile
grep -n 'cmd/operator/main.go' Makefile Dockerfile
```
Expected: the `build` and `run` targets in the Makefile and the `COPY`/`go build` lines in the Dockerfile reference `cmd/operator/main.go`.

- [ ] **Step 5: Add the `envtest-assets` helper target**

Append to `Makefile`:
```make
.PHONY: envtest-assets
envtest-assets: ## Print the KUBEBUILDER_ASSETS path (downloads envtest binaries if needed).
	@$(MAKE) -s envtest >/dev/null
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path
```

- [ ] **Step 6: Generate, build, and test the scaffold**

Run: `make manifests generate && make build && make test`
Expected: all succeed. `ls config/crd/bases` lists ten CRDs, each named `last.moogie-labs.com_<plural>.yaml`. Then run `export KUBEBUILDER_ASSETS="$(make -s envtest-assets)" && echo $KUBEBUILDER_ASSETS`, which should print a directory path.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "chore: scaffold kubebuilder project with all v1alpha1 API kinds"
```

### Task 2: Shared types and lifecycle CRDs (Intent, IntentApproval, ManagedResource)

**Files:**
- Create: `api/v1alpha1/common_types.go`, `api/v1alpha1/suite_test.go`, `api/v1alpha1/lifecycle_validation_test.go`
- Overwrite: `api/v1alpha1/intent_types.go`, `api/v1alpha1/intentapproval_types.go`, `api/v1alpha1/managedresource_types.go`
- Regenerated: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`

**Interfaces:**
- Consumes: scaffold from Task 1.
- Produces (package `v1alpha1`): `Mode` (`ModeAuto`, `ModeManual`), `Stage` (`StageDiscovery`, `StageCreate`, `StageUpdate`, `StageRemediate`, `StageDelete`), `StageModes`, `StageFlags`, `ArgvCommand`, `CommandRun`, `ImageSpec`, `Command`, `Probe`, `LocalRef`, `Phase`, `Intent{Spec IntentSpec; Status IntentStatus}`, `Plan`, `Generalization`, `PendingCommand`, `Question`, `Answer`, `IntentApproval{Spec IntentApprovalSpec}`, `Decision` (`DecisionApprove`, `DecisionDeny`), `ManagedResource{Spec ManagedResourceSpec; Status ManagedResourceStatus}`. Test helpers in `v1alpha1_test`: `k8sClient`, `ctx`, `meta()`, `clusterMeta()`, `expectInvalid(t, err, substr)`.

- [ ] **Step 1: Write the envtest suite and failing validation tests**

`api/v1alpha1/suite_test.go`:
```go
package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var (
	k8sClient client.Client
	ctx       = context.Background()
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\nset KUBEBUILDER_ASSETS: export KUBEBUILDER_ASSETS=\"$(make -s envtest-assets)\"\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := lastv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func meta() metav1.ObjectMeta        { return metav1.ObjectMeta{GenerateName: "t-", Namespace: "default"} }
func clusterMeta() metav1.ObjectMeta { return metav1.ObjectMeta{GenerateName: "t-"} }

// expectInvalid asserts err is an API "Invalid" error whose message contains substr.
func expectInvalid(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error containing %q, got nil", substr)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got: %v", err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}
```

`api/v1alpha1/lifecycle_validation_test.go`:
```go
package v1alpha1_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const validHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestIntentRequiresRequest(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta()}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.request")
}

func TestIntentAcceptsModes(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta(), Spec: lastv1alpha1.IntentSpec{
		Request: "run nginx serving team-x-site",
		Answers: []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}},
		Modes:   &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeManual, Reconcile: lastv1alpha1.ModeAuto},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestIntentRejectsUnknownMode(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta(), Spec: lastv1alpha1.IntentSpec{
		Request: "x",
		Modes:   &lastv1alpha1.StageModes{Create: "Sometimes"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.modes.create")
}

func managedResource(run lastv1alpha1.CommandRun) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{ObjectMeta: meta(), Spec: lastv1alpha1.ManagedResourceSpec{
		IntentRef: &lastv1alpha1.LocalRef{Name: "web"},
		Probes: []lastv1alpha1.Probe{{
			ID:      "replicas",
			Command: lastv1alpha1.Command{Run: run},
			Assert:  "result.exitCode == 0",
		}},
	}}
}

func TestCommandRunRequiresExactlyOneOfArgvOrScript(t *testing.T) {
	both := managedResource(lastv1alpha1.CommandRun{Argv: []string{"kubectl"}, Script: "echo hi"})
	expectInvalid(t, k8sClient.Create(ctx, both), "exactly one of argv or script")

	neither := managedResource(lastv1alpha1.CommandRun{})
	expectInvalid(t, k8sClient.Create(ctx, neither), "exactly one of argv or script")

	argv := managedResource(lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods"}})
	if err := k8sClient.Create(ctx, argv); err != nil {
		t.Fatalf("argv-only command rejected: %v", err)
	}
}

func TestManagedResourceKeepsFreeFormDesiredState(t *testing.T) {
	obj := managedResource(lastv1alpha1.CommandRun{Argv: []string{"true"}})
	obj.Spec.Desired = &runtime.RawExtension{Raw: []byte(`{"replicas":2,"image":"nginx:1.27"}`)}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &lastv1alpha1.ManagedResource{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(got.Spec.Desired.Raw), `"replicas":2`) {
		t.Fatalf("desired state was pruned: %s", got.Spec.Desired.Raw)
	}
}

func validApproval() *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{ObjectMeta: meta(), Spec: lastv1alpha1.IntentApprovalSpec{
		IntentRef:   lastv1alpha1.LocalRef{Name: "web"},
		Stage:       lastv1alpha1.StageCreate,
		SubjectHash: validHash,
		Decision:    lastv1alpha1.DecisionApprove,
		ApprovedBy:  "alice@example.com",
	}}
}

func TestIntentApprovalRejectsMalformedHash(t *testing.T) {
	obj := validApproval()
	obj.Spec.SubjectHash = "abc"
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.subjectHash")
}

func TestIntentApprovalIsImmutable(t *testing.T) {
	obj := validApproval()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	obj.Spec.Reason = "changed my mind"
	expectInvalid(t, k8sClient.Update(ctx, obj), "spec is immutable")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export KUBEBUILDER_ASSETS="$(make -s envtest-assets)" && go test ./api/v1alpha1/ -count=1`
Expected: compile errors such as `undefined: lastv1alpha1.StageModes` and `unknown field Request`.

- [ ] **Step 3: Write the shared types**

`api/v1alpha1/common_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode selects whether a lifecycle stage needs human approval.
// +kubebuilder:validation:Enum=Auto;Manual
type Mode string

const (
	ModeAuto   Mode = "Auto"
	ModeManual Mode = "Manual"
)

// Stage identifies what an approval subject or allow rule applies to.
// +kubebuilder:validation:Enum=Discovery;Create;Update;Remediate;Delete
type Stage string

const (
	StageDiscovery Stage = "Discovery"
	StageCreate    Stage = "Create"
	StageUpdate    Stage = "Update"
	StageRemediate Stage = "Remediate"
	StageDelete    Stage = "Delete"
)

// StageModes sets Auto/Manual per lifecycle stage. Create covers the Create and Update stages.
type StageModes struct {
	// +optional
	Discovery Mode `json:"discovery,omitempty"`
	// +optional
	Create Mode `json:"create,omitempty"`
	// +optional
	Reconcile Mode `json:"reconcile,omitempty"`
	// +optional
	Delete Mode `json:"delete,omitempty"`
}

// StageFlags sets a boolean per lifecycle stage.
type StageFlags struct {
	// +optional
	Discovery bool `json:"discovery,omitempty"`
	// +optional
	Create bool `json:"create,omitempty"`
	// +optional
	Reconcile bool `json:"reconcile,omitempty"`
	// +optional
	Delete bool `json:"delete,omitempty"`
}

// ArgvCommand is a command executed directly, without a shell.
type ArgvCommand struct {
	// +kubebuilder:validation:MinItems=1
	Argv []string `json:"argv"`
}

// CommandRun is exactly one of argv (exec'd directly) or script (run with /bin/sh -c).
// +kubebuilder:validation:XValidation:rule="has(self.argv) != has(self.script)",message="exactly one of argv or script must be set"
type CommandRun struct {
	// +optional
	// +kubebuilder:validation:MinItems=1
	Argv []string `json:"argv,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	Script string `json:"script,omitempty"`
}

// ImageSpec lists install commands baked into the command's image at build time (spec §11.2).
type ImageSpec struct {
	// +kubebuilder:validation:MinItems=1
	Install []ArgvCommand `json:"install"`
}

// Command is the unit of action: one sandboxed execution (spec §8.1).
type Command struct {
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	Run    CommandRun `json:"run"`
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
	// Files are written under /work before the command runs, keyed by absolute path.
	// +optional
	Files map[string]string `json:"files,omitempty"`
	// Providers names the Provider credentials to inject.
	// +optional
	Providers []string `json:"providers,omitempty"`
	// Egress lists host:port or CIDR:port destinations beyond what providers imply.
	// +optional
	Egress []string `json:"egress,omitempty"`
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// Probe is a command plus a CEL assertion over its result (spec §10).
type Probe struct {
	// +kubebuilder:validation:MinLength=1
	ID      string  `json:"id"`
	Command Command `json:"command"`
	// +kubebuilder:validation:MinLength=1
	Assert string `json:"assert"`
}

// LocalRef names an object in the same namespace (or a cluster-scoped object).
type LocalRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}
```

- [ ] **Step 4: Write the lifecycle types**

`api/v1alpha1/intent_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is where an Intent is in its lifecycle (spec §7).
// +kubebuilder:validation:Enum=Pending;Clarifying;Planning;AwaitingDiscoveryApproval;AwaitingApproval;Executing;Verifying;Ready;Drifted;Remediating;TearingDown;Failed
type Phase string

const (
	PhasePending                   Phase = "Pending"
	PhaseClarifying                Phase = "Clarifying"
	PhasePlanning                  Phase = "Planning"
	PhaseAwaitingDiscoveryApproval Phase = "AwaitingDiscoveryApproval"
	PhaseAwaitingApproval          Phase = "AwaitingApproval"
	PhaseExecuting                 Phase = "Executing"
	PhaseVerifying                 Phase = "Verifying"
	PhaseReady                     Phase = "Ready"
	PhaseDrifted                   Phase = "Drifted"
	PhaseRemediating               Phase = "Remediating"
	PhaseTearingDown               Phase = "TearingDown"
	PhaseFailed                    Phase = "Failed"
)

// Answer answers one of status.questions.
type Answer struct {
	// +kubebuilder:validation:MinLength=1
	ID    string `json:"id"`
	Value string `json:"value"`
}

// IntentSpec is a natural-language request plus the requester's inputs.
type IntentSpec struct {
	// +kubebuilder:validation:MinLength=1
	Request string `json:"request"`
	// +optional
	Answers []Answer `json:"answers,omitempty"`
	// Modes may only be as strict as, or stricter than, the effective policy.
	// +optional
	Modes *StageModes `json:"modes,omitempty"`
}

// Question is a clarifying question from the agent.
type Question struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// +optional
	Options []string `json:"options,omitempty"`
}

// Generalization is an LLM-proposed wildcard pattern for an allow rule (advice only).
type Generalization struct {
	Argv []string `json:"argv"`
	// +optional
	Lint []string `json:"lint,omitempty"`
}

// PendingCommand is a discovery command awaiting approval.
type PendingCommand struct {
	Hash    string  `json:"hash"`
	Command Command `json:"command"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Generalization *Generalization `json:"generalization,omitempty"`
}

// Plan is the agent's reviewable plan (spec §8.2).
type Plan struct {
	Hash string `json:"hash"`
	// +optional
	Summary string    `json:"summary,omitempty"`
	Steps   []Command `json:"steps"`
	// +optional
	Resources []ManagedResourceSpec `json:"resources,omitempty"`
	// +optional
	UncoveredSteps []string `json:"uncoveredSteps,omitempty"`
	// +optional
	Generalizations map[string]Generalization `json:"generalizations,omitempty"`
}

// IntentStatus is written by the operator.
type IntentStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Stage Stage `json:"stage,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Questions []Question `json:"questions,omitempty"`
	// +optional
	PendingCommand *PendingCommand `json:"pendingCommand,omitempty"`
	// +optional
	Plan *Plan `json:"plan,omitempty"`
	// +optional
	SuggestedRules []string `json:"suggestedRules,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Intent is a natural-language provisioning request.
type Intent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentSpec `json:"spec"`
	// +optional
	Status IntentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IntentList contains a list of Intent.
type IntentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Intent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Intent{}, &IntentList{})
}
```

`api/v1alpha1/intentapproval_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Decision is an approver's verdict.
// +kubebuilder:validation:Enum=Approve;Deny
type Decision string

const (
	DecisionApprove Decision = "Approve"
	DecisionDeny    Decision = "Deny"
)

// IntentApprovalSpec binds a human decision to exactly one approval subject.
type IntentApprovalSpec struct {
	IntentRef LocalRef `json:"intentRef"`
	Stage     Stage    `json:"stage"`
	// SubjectHash is the plan hash, or the pending command hash for Discovery.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SubjectHash string   `json:"subjectHash"`
	Decision    Decision `json:"decision"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// ApprovedBy must equal the creating user; enforced by admission policy in M2.
	// +kubebuilder:validation:MinLength=1
	ApprovedBy string `json:"approvedBy"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="By",type=string,JSONPath=`.spec.approvedBy`

// IntentApproval is an immutable approval or denial of one plan or discovery command.
type IntentApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec IntentApprovalSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// IntentApprovalList contains a list of IntentApproval.
type IntentApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IntentApproval{}, &IntentApprovalList{})
}
```

`api/v1alpha1/managedresource_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ManagedResourceSpec describes one provisioned thing. It is also used as a
// template inside Plan.resources, where IntentRef is omitted.
type ManagedResourceSpec struct {
	// +optional
	IntentRef *LocalRef `json:"intentRef,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
	// Identity is free-form: how humans and the agent find this thing again.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Identity *runtime.RawExtension `json:"identity,omitempty"`
	// Desired is the approved desired-state snapshot.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Desired *runtime.RawExtension `json:"desired,omitempty"`
	// +optional
	Probes []Probe `json:"probes,omitempty"`
	// +optional
	OutputsSecretRef *LocalRef `json:"outputsSecretRef,omitempty"`
	// +optional
	Teardown []Command `json:"teardown,omitempty"`
}

// ManagedResourceStatus holds the latest probe observations.
type ManagedResourceStatus struct {
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Observed *runtime.RawExtension `json:"observed,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`
// +kubebuilder:printcolumn:name="InSync",type=string,JSONPath=`.status.conditions[?(@.type=="InSync")].status`

// ManagedResource is one thing an Intent provisioned.
type ManagedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ManagedResourceSpec `json:"spec"`
	// +optional
	Status ManagedResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedResourceList contains a list of ManagedResource.
type ManagedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagedResource{}, &ManagedResourceList{})
}
```

- [ ] **Step 5: Regenerate and run the tests**

Run: `make manifests generate && go test ./api/v1alpha1/ -count=1 -v -run 'Intent|Command|Managed'`
Expected: PASS for all seven tests. If controller-gen reports a CEL cost-budget error, add `// +kubebuilder:validation:MaxItems=64` to the list field it names and regenerate.

- [ ] **Step 6: Commit**

```bash
git add api config
git commit -m "feat(api): add shared types and Intent, IntentApproval, ManagedResource CRDs"
```

### Task 3: Allow-rule CRDs (CommandRule, ClusterCommandRule, CommandRuleSuggestion)

**Files:**
- Overwrite: `api/v1alpha1/commandrule_types.go`, `api/v1alpha1/clustercommandrule_types.go`, `api/v1alpha1/commandrulesuggestion_types.go`
- Create: `api/v1alpha1/rules_validation_test.go`

**Interfaces:**
- Consumes: `Stage`, `LocalRef`, `Command` (Task 2); test helpers `k8sClient`, `ctx`, `meta()`, `clusterMeta()`, `expectInvalid` (Task 2).
- Produces: `ArgvPattern{Argv []string}`, `RuleOrigin` (`RuleOriginApproval`, `RuleOriginSuggestion`, `RuleOriginManual`), `RuleSource`, `CommandRuleSpec`, `CommandRule`, `ClusterCommandRule`, `RuleScope` (`RuleScopeNamespace`, `RuleScopeCluster`), `SuggestionTrigger` (`TriggerPlanTime`, `TriggerRepeatedApproval`, `TriggerPeriodicReview`), `SuggestionDecision` (`SuggestionPending`, `SuggestionAccepted`, `SuggestionDenied`), `CommandRuleSuggestionSpec`, `CommandRuleSuggestion`.

- [ ] **Step 1: Write the failing validation tests**

`api/v1alpha1/rules_validation_test.go`:
```go
package v1alpha1_test

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func ruleSpec(argv ...string) lastv1alpha1.CommandRuleSpec {
	return lastv1alpha1.CommandRuleSpec{
		Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
		Providers: []string{"aws-prod"},
		Match:     lastv1alpha1.ArgvPattern{Argv: argv},
	}
}

func TestCommandRuleAcceptsWildcards(t *testing.T) {
	obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "ec2", "describe-*", "re:^--region=[a-z0-9-]+$", "...")}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestCommandRuleRequiresLiteralBinary(t *testing.T) {
	for _, first := range []string{"*", "aw?", "[a]ws", "re:^aws$", "..."} {
		obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec(first, "ec2")}
		expectInvalid(t, k8sClient.Create(ctx, obj), "argv[0] must be a literal")
	}
}

func TestCommandRuleEllipsisOnlyLast(t *testing.T) {
	obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "...", "describe")}
	expectInvalid(t, k8sClient.Create(ctx, obj), "only allowed in the last position")

	twice := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "...", "...")}
	expectInvalid(t, k8sClient.Create(ctx, twice), "only allowed in the last position")
}

func TestCommandRuleRequiresStagesAndProviders(t *testing.T) {
	noStages := ruleSpec("aws")
	noStages.Stages = nil
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: noStages}), "spec.stages")

	noProviders := ruleSpec("aws")
	noProviders.Providers = nil
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: noProviders}), "spec.providers")
}

func TestCommandRuleValidatesInstallPatterns(t *testing.T) {
	spec := ruleSpec("aws", "...")
	spec.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{"*", "install"}}}
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: spec}), "argv[0] must be a literal")
}

func TestClusterCommandRuleSharesValidation(t *testing.T) {
	obj := &lastv1alpha1.ClusterCommandRule{ObjectMeta: clusterMeta(), Spec: ruleSpec("...")}
	expectInvalid(t, k8sClient.Create(ctx, obj), "argv[0] must be a literal")

	ok := &lastv1alpha1.ClusterCommandRule{ObjectMeta: clusterMeta(), Spec: ruleSpec("kubectl", "get", "...")}
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })
}

func suggestion() *lastv1alpha1.CommandRuleSuggestion {
	return &lastv1alpha1.CommandRuleSuggestion{ObjectMeta: meta(), Spec: lastv1alpha1.CommandRuleSuggestionSpec{
		TargetScope: lastv1alpha1.RuleScopeNamespace,
		Trigger:     lastv1alpha1.TriggerPlanTime,
		Rationale:   "lets drift remediation run unattended",
		Proposed:    ruleSpec("kubectl", "get", "..."),
	}}
}

func TestSuggestionDefaultsToPending(t *testing.T) {
	obj := suggestion()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &lastv1alpha1.CommandRuleSuggestion{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Fatalf("decision = %q, want Pending", got.Spec.Decision)
	}
}

func TestSuggestionDecisionRequiresDecidedBy(t *testing.T) {
	obj := suggestion()
	obj.Spec.Decision = lastv1alpha1.SuggestionDenied
	expectInvalid(t, k8sClient.Create(ctx, obj), "decidedBy is required once decided")
}

func TestSuggestionDecisionCannotChangeOnceDecided(t *testing.T) {
	obj := suggestion()
	obj.Spec.Decision = lastv1alpha1.SuggestionDenied
	obj.Spec.DecidedBy = "alice@example.com"
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	obj.Spec.Decision = lastv1alpha1.SuggestionAccepted
	expectInvalid(t, k8sClient.Update(ctx, obj), "decision cannot change once decided")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./api/v1alpha1/ -count=1 -run 'Rule|Suggestion'`
Expected: compile errors (`undefined: lastv1alpha1.ArgvPattern`).

- [ ] **Step 3: Write the rule types**

`api/v1alpha1/commandrule_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ArgvPattern matches a command's resolved argv position by position (spec §9.2):
// a literal, a glob within one argument (*, ?, [...]), "re:<RE2>" anchored to one
// argument, or "..." (last position only) for any remaining arguments.
// +kubebuilder:validation:XValidation:rule="size(self.argv) == 0 || (!self.argv[0].contains('*') && !self.argv[0].contains('?') && !self.argv[0].contains('[') && !self.argv[0].startsWith('re:') && self.argv[0] != '...')",message="argv[0] must be a literal"
// +kubebuilder:validation:XValidation:rule="!self.argv.exists(a, a == '...') || (self.argv[size(self.argv) - 1] == '...' && size(self.argv.filter(a, a == '...')) == 1)",message="'...' is only allowed in the last position"
type ArgvPattern struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=1024
	Argv []string `json:"argv"`
}

// RuleOrigin records how a rule was created.
// +kubebuilder:validation:Enum=approval;suggestion;manual
type RuleOrigin string

const (
	RuleOriginApproval   RuleOrigin = "approval"
	RuleOriginSuggestion RuleOrigin = "suggestion"
	RuleOriginManual     RuleOrigin = "manual"
)

// RuleSource is provenance set by the CLI.
type RuleSource struct {
	CreatedFrom RuleOrigin `json:"createdFrom"`
	// +optional
	SuggestionRef *LocalRef `json:"suggestionRef,omitempty"`
}

// CommandRuleSpec lets matching commands skip approval (spec §9).
type CommandRuleSpec struct {
	// +optional
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Stages []Stage `json:"stages"`
	// A command's providers must be a subset of this list.
	// +kubebuilder:validation:MinItems=1
	Providers []string    `json:"providers"`
	Match     ArgvPattern `json:"match"`
	// Every image.install command of a matching command must match one of these.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Install []ArgvPattern `json:"install,omitempty"`
	// A command's declared egress must be a subset of this list.
	// +optional
	Egress []string `json:"egress,omitempty"`
	// +optional
	Source *RuleSource `json:"source,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Stages",type=string,JSONPath=`.spec.stages`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.match.argv`

// CommandRule is a namespace-scoped allow rule.
type CommandRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommandRuleSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// CommandRuleList contains a list of CommandRule.
type CommandRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CommandRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CommandRule{}, &CommandRuleList{})
}
```

`api/v1alpha1/clustercommandrule_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Stages",type=string,JSONPath=`.spec.stages`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.match.argv`

// ClusterCommandRule is a cluster-wide allow rule.
type ClusterCommandRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommandRuleSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ClusterCommandRuleList contains a list of ClusterCommandRule.
type ClusterCommandRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCommandRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterCommandRule{}, &ClusterCommandRuleList{})
}
```

`api/v1alpha1/commandrulesuggestion_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuleScope is which kind of rule a suggestion would create.
// +kubebuilder:validation:Enum=Namespace;Cluster
type RuleScope string

const (
	RuleScopeNamespace RuleScope = "Namespace"
	RuleScopeCluster   RuleScope = "Cluster"
)

// SuggestionTrigger is why the operator made a suggestion (spec §9.5).
// +kubebuilder:validation:Enum=PlanTime;RepeatedApproval;PeriodicReview
type SuggestionTrigger string

const (
	TriggerPlanTime         SuggestionTrigger = "PlanTime"
	TriggerRepeatedApproval SuggestionTrigger = "RepeatedApproval"
	TriggerPeriodicReview   SuggestionTrigger = "PeriodicReview"
)

// SuggestionDecision is a human's verdict on a suggestion.
// +kubebuilder:validation:Enum=Pending;Accepted;Denied
type SuggestionDecision string

const (
	SuggestionPending  SuggestionDecision = "Pending"
	SuggestionAccepted SuggestionDecision = "Accepted"
	SuggestionDenied   SuggestionDecision = "Denied"
)

// CommandRuleSuggestionSpec is an operator-proposed allow rule awaiting a human decision.
// +kubebuilder:validation:XValidation:rule="self.decision == 'Pending' || has(self.decidedBy)",message="decidedBy is required once decided"
type CommandRuleSuggestionSpec struct {
	TargetScope RuleScope         `json:"targetScope"`
	Trigger     SuggestionTrigger `json:"trigger"`
	// +optional
	Rationale string          `json:"rationale,omitempty"`
	Proposed  CommandRuleSpec `json:"proposed"`
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Examples []Command `json:"examples,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=20
	BreadthWarnings []string `json:"breadthWarnings,omitempty"`
	// +kubebuilder:default=Pending
	// +optional
	Decision SuggestionDecision `json:"decision,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecisionReason string `json:"decisionReason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.proposed.match.argv`

// CommandRuleSuggestion is an allow rule the operator proposes; only humans decide it.
type CommandRuleSuggestion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="oldSelf.decision == 'Pending' || self.decision == oldSelf.decision",message="decision cannot change once decided"
	Spec CommandRuleSuggestionSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// CommandRuleSuggestionList contains a list of CommandRuleSuggestion.
type CommandRuleSuggestionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CommandRuleSuggestion `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CommandRuleSuggestion{}, &CommandRuleSuggestionList{})
}
```

- [ ] **Step 4: Regenerate and run the tests**

Run: `make manifests generate && go test ./api/v1alpha1/ -count=1 -v -run 'Rule|Suggestion'`
Expected: PASS for all nine tests. If the API server rejects a CRD with "estimated rule cost exceeds budget", add the `MaxItems`/`items:MaxLength` markers the error names and regenerate. Do not remove the rules.

- [ ] **Step 5: Commit**

```bash
git add api config
git commit -m "feat(api): add CommandRule, ClusterCommandRule, CommandRuleSuggestion CRDs"
```

### Task 4: Configuration CRDs (IntentPolicy, OperatorConfig, Provider, LLMProvider)

**Files:**
- Overwrite: `api/v1alpha1/intentpolicy_types.go`, `api/v1alpha1/operatorconfig_types.go`, `api/v1alpha1/provider_types.go`, `api/v1alpha1/llmprovider_types.go`
- Create: `api/v1alpha1/config_validation_test.go`

**Interfaces:**
- Consumes: `StageModes`, `StageFlags`, `LocalRef` (Task 2); test helpers (Task 2).
- Produces: `IntentPolicySpec`, `EgressPolicy`, `ProbePolicy`, `IntentPolicy`, `OperatorConfigSpec`, `SandboxConfig`, `RegistryMode` (`RegistryBundled`, `RegistryExternal`), `RegistryConfig`, `ExternalRegistry`, `ImagesConfig`, `ProbeLimits`, `SuggestionsConfig`, `OperatorConfig` (must be named `default`), `ProviderType` (`ProviderKubernetes`, `ProviderEnvSecret`), `KubernetesProvider`, `ProviderSpec`, `Provider`, `LLMProviderType` (`LLMAnthropic`, `LLMOpenAI`, `LLMOpenAICompatible`), `LLMProviderSpec`, `LLMProvider`.

- [ ] **Step 1: Write the failing validation tests**

`api/v1alpha1/config_validation_test.go`:
```go
package v1alpha1_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const pinnedImage = "ghcr.io/moogie-labs/last-sandbox-base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestIntentPolicyAcceptsFullSpec(t *testing.T) {
	allow := true
	obj := &lastv1alpha1.IntentPolicy{ObjectMeta: meta(), Spec: lastv1alpha1.IntentPolicySpec{
		Modes:          &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeManual, Create: lastv1alpha1.ModeManual},
		AllowAutoModes: &lastv1alpha1.StageFlags{Reconcile: true},
		AllowScripts:   &allow,
		Providers:      []string{"cluster-team-x"},
		Egress:         &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}},
		Probes:         &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: 5 * time.Minute}, FastInterval: &metav1.Duration{Duration: time.Minute}},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestOperatorConfigMustBeNamedDefault(t *testing.T) {
	bad := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	expectInvalid(t, k8sClient.Create(ctx, bad), "OperatorConfig must be named 'default'")

	good := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if err := k8sClient.Create(ctx, good); err != nil {
		t.Fatalf("create default: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, good) })
}

func TestOperatorConfigRequiresDigestPinnedBaseImage(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: lastv1alpha1.OperatorConfigSpec{
		Sandbox: &lastv1alpha1.SandboxConfig{BaseImage: "ghcr.io/moogie-labs/last-sandbox-base:latest"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.sandbox.baseImage")
}

func TestOperatorConfigExternalRegistryRequiresDetails(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: lastv1alpha1.OperatorConfigSpec{
		Registry: &lastv1alpha1.RegistryConfig{Mode: lastv1alpha1.RegistryExternal},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "external is required when mode is external")
}

func TestOperatorConfigDefaults(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: lastv1alpha1.OperatorConfigSpec{
		Sandbox:  &lastv1alpha1.SandboxConfig{BaseImage: pinnedImage},
		Registry: &lastv1alpha1.RegistryConfig{},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	got := &lastv1alpha1.OperatorConfig{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Sandbox.MaxConcurrent != 20 {
		t.Errorf("maxConcurrent = %d, want 20", got.Spec.Sandbox.MaxConcurrent)
	}
	if got.Spec.Registry.Mode != lastv1alpha1.RegistryBundled {
		t.Errorf("registry mode = %q, want bundled", got.Spec.Registry.Mode)
	}
}

func TestProviderTypeRequiresMatchingFields(t *testing.T) {
	k8s := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{Type: lastv1alpha1.ProviderKubernetes}}
	expectInvalid(t, k8sClient.Create(ctx, k8s), "kubernetes is required when type is kubernetes")

	env := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{Type: lastv1alpha1.ProviderEnvSecret}}
	expectInvalid(t, k8sClient.Create(ctx, env), "secretRef is required when type is env-secret")

	ok := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{
		Type:       lastv1alpha1.ProviderKubernetes,
		Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "team-x-deployer"}},
	}}
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })
}

func TestLLMProviderOpenAICompatibleRequiresEndpoint(t *testing.T) {
	obj := &lastv1alpha1.LLMProvider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMOpenAICompatible, Model: "llama", SecretRef: lastv1alpha1.LocalRef{Name: "key"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "endpoint is required for openai-compatible")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./api/v1alpha1/ -count=1 -run 'Policy|OperatorConfig|Provider'`
Expected: compile errors (`undefined: lastv1alpha1.EgressPolicy`).

- [ ] **Step 3: Write the configuration types**

`api/v1alpha1/intentpolicy_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressPolicy bounds sandbox egress.
type EgressPolicy struct {
	// Allowed lists CIDR:port or host:port patterns (host names need Cilium).
	// +optional
	Allowed []string `json:"allowed,omitempty"`
}

// ProbePolicy sets drift-check cadence (spec §10.1).
type ProbePolicy struct {
	// Steady-state interval (the adaptive ceiling).
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// Interval used right after a change or drift.
	// +optional
	FastInterval *metav1.Duration `json:"fastInterval,omitempty"`
}

// IntentPolicySpec can only tighten OperatorConfig.defaults (spec §6).
type IntentPolicySpec struct {
	// +optional
	Modes *StageModes `json:"modes,omitempty"`
	// +optional
	AllowAutoModes *StageFlags `json:"allowAutoModes,omitempty"`
	// +optional
	AllowScripts *bool `json:"allowScripts,omitempty"`
	// +optional
	Providers []string `json:"providers,omitempty"`
	// +optional
	Egress *EgressPolicy `json:"egress,omitempty"`
	// +optional
	Probes *ProbePolicy `json:"probes,omitempty"`
}

// +kubebuilder:object:root=true

// IntentPolicy is a namespace's policy for Intents.
type IntentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true

// IntentPolicyList contains a list of IntentPolicy.
type IntentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IntentPolicy{}, &IntentPolicyList{})
}
```

`api/v1alpha1/operatorconfig_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxConfig configures sandbox pods.
type SandboxConfig struct {
	// BaseImage must be pinned by digest.
	// +kubebuilder:validation:Pattern=`@sha256:[0-9a-f]{64}$`
	BaseImage string `json:"baseImage"`
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}

// RegistryMode selects the bundled or an external image registry.
// +kubebuilder:validation:Enum=bundled;external
type RegistryMode string

const (
	RegistryBundled  RegistryMode = "bundled"
	RegistryExternal RegistryMode = "external"
)

// ExternalRegistry points at a customer-managed registry.
type ExternalRegistry struct {
	// +kubebuilder:validation:MinLength=1
	Repository    string   `json:"repository"`
	PushSecretRef LocalRef `json:"pushSecretRef"`
	PullSecretRef LocalRef `json:"pullSecretRef"`
}

// RegistryConfig configures where built images live (spec §11.3).
// +kubebuilder:validation:XValidation:rule="self.mode != 'external' || has(self.external)",message="external is required when mode is external"
type RegistryConfig struct {
	// +kubebuilder:default=bundled
	// +optional
	Mode RegistryMode `json:"mode,omitempty"`
	// +optional
	External *ExternalRegistry `json:"external,omitempty"`
}

// ImagesConfig configures automatic image builds.
type ImagesConfig struct {
	// +optional
	BuildEgress []string `json:"buildEgress,omitempty"`
	// +optional
	UnusedRetention *metav1.Duration `json:"unusedRetention,omitempty"`
}

// ProbeLimits bounds probe intervals cluster-wide.
type ProbeLimits struct {
	// +optional
	MinInterval *metav1.Duration `json:"minInterval,omitempty"`
}

// SuggestionsConfig tunes rule suggestions (spec §9.5).
type SuggestionsConfig struct {
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	RepeatThreshold int32 `json:"repeatThreshold,omitempty"`
	// +optional
	ReviewInterval *metav1.Duration `json:"reviewInterval,omitempty"`
}

// OperatorConfigSpec holds cluster defaults.
type OperatorConfigSpec struct {
	// +optional
	LLMProviderRef *LocalRef `json:"llmProviderRef,omitempty"`
	// +optional
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`
	// +optional
	Registry *RegistryConfig `json:"registry,omitempty"`
	// +optional
	Images *ImagesConfig `json:"images,omitempty"`
	// +optional
	Probes *ProbeLimits `json:"probes,omitempty"`
	// +optional
	Suggestions *SuggestionsConfig `json:"suggestions,omitempty"`
	// +optional
	Defaults *IntentPolicySpec `json:"defaults,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="OperatorConfig must be named 'default'"

// OperatorConfig is the cluster-wide singleton configuration.
type OperatorConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec OperatorConfigSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// OperatorConfigList contains a list of OperatorConfig.
type OperatorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OperatorConfig{}, &OperatorConfigList{})
}
```

`api/v1alpha1/provider_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderType selects how credentials are materialized in a sandbox.
// +kubebuilder:validation:Enum=kubernetes;env-secret
type ProviderType string

const (
	ProviderKubernetes ProviderType = "kubernetes"
	ProviderEnvSecret  ProviderType = "env-secret"
)

// KubernetesProvider mints short-lived tokens for a ServiceAccount in last-gateway.
type KubernetesProvider struct {
	ServiceAccountRef LocalRef `json:"serviceAccountRef"`
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`
}

// ProviderSpec describes one set of platform credentials (spec §13).
// +kubebuilder:validation:XValidation:rule="self.type != 'kubernetes' || has(self.kubernetes)",message="kubernetes is required when type is kubernetes"
// +kubebuilder:validation:XValidation:rule="self.type != 'env-secret' || has(self.secretRef)",message="secretRef is required when type is env-secret"
type ProviderSpec struct {
	Type ProviderType `json:"type"`
	// +optional
	Description string `json:"description,omitempty"`
	// +optional
	Kubernetes *KubernetesProvider `json:"kubernetes,omitempty"`
	// SecretRef names a Secret in last-gateway.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// Env maps sandbox env var name -> Secret key.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// Files maps Secret key -> file path in the sandbox.
	// +optional
	Files map[string]string `json:"files,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`

// Provider is a set of platform credentials held only by the gateway.
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ProviderList contains a list of Provider.
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Provider{}, &ProviderList{})
}
```

`api/v1alpha1/llmprovider_types.go`:
```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LLMProviderType selects a gateway LLM adapter.
// +kubebuilder:validation:Enum=anthropic;openai;openai-compatible
type LLMProviderType string

const (
	LLMAnthropic        LLMProviderType = "anthropic"
	LLMOpenAI           LLMProviderType = "openai"
	LLMOpenAICompatible LLMProviderType = "openai-compatible"
)

// LLMProviderSpec configures one model endpoint.
// +kubebuilder:validation:XValidation:rule="self.type != 'openai-compatible' || has(self.endpoint)",message="endpoint is required for openai-compatible"
type LLMProviderSpec struct {
	Type LLMProviderType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef names a Secret in last-gateway holding the API key.
	SecretRef LocalRef `json:"secretRef"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`

// LLMProvider is a model endpoint the gateway can call.
type LLMProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LLMProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// LLMProviderList contains a list of LLMProvider.
type LLMProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMProvider{}, &LLMProviderList{})
}
```

- [ ] **Step 4: Regenerate and run the whole API suite**

Run: `make manifests generate && go test ./api/v1alpha1/ -count=1 -v`
Expected: PASS for all tests from Tasks 2–4. Also run `make test`, which should pass.

- [ ] **Step 5: Commit**

```bash
git add api config
git commit -m "feat(api): add IntentPolicy, OperatorConfig, Provider, LLMProvider CRDs"
```

### Task 5: Audit entries, canonical hashing, and signing

**Files:**
- Create: `internal/audit/entry.go`, `internal/audit/canonical.go`, `internal/audit/sign.go`, `internal/audit/entry_test.go`

**Interfaces:**
- Produces (package `audit`): the entry-type constants `Type*` (spec §14.2), `GenesisHash`, `TimeFormat`, `Authorization{Kind, Name}`, `Entry` (JSON fields `seq, chainId, prevHash, ts, type, actor, intent, stage, subjectHash, authorizedBy, payload, hash, sig`), `Draft{Type, Actor, Intent, Stage, SubjectHash string; AuthorizedBy *Authorization; Payload any}`, `CheckpointPayload{PrevSeq uint64; PrevHash string}`, `CheckpointIndex{Seq uint64; Hash string}`, `Canonical(v any) ([]byte, error)`, `ComputeHash(e Entry) (string, error)`, `Signer`, `NewSigner(ed25519.PrivateKey) *Signer`, `(*Signer).Public() ed25519.PublicKey`, `(*Signer).Seal(*Entry) error`, `CheckSeal(ed25519.PublicKey, Entry) error`, `ErrBadHash`, `ErrBadSignature`, `Fingerprint(ed25519.PublicKey) string`, `MarshalPrivateKeyPEM`, `ParsePrivateKeyPEM`, `MarshalPublicKeyPEM`, `ParsePublicKeyPEM`. Test helper in `audit_test`: `newSigner(t) *audit.Signer`.

- [ ] **Step 1: Add the RFC 8785 dependency**

Run: `go get github.com/gowebpki/jcs@latest`
Expected: `go.mod` lists `github.com/gowebpki/jcs`.

- [ ] **Step 2: Write the failing tests**

`internal/audit/entry_test.go`:
```go
package audit_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func newSigner(t *testing.T) *audit.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return audit.NewSigner(key)
}

func sampleEntry() audit.Entry {
	return audit.Entry{
		Seq: 7, ChainID: "c1", PrevHash: audit.GenesisHash,
		TS: "2026-09-27T00:00:00.000000000Z", Type: audit.TypeCommandStart,
		Actor: "operator", Intent: "team-x/web",
		Payload: json.RawMessage(`{"argv":["kubectl","get","pods"]}`),
	}
}

func TestCanonicalSortsKeys(t *testing.T) {
	got, err := audit.Canonical(map[string]int{"b": 1, "a": 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2,"b":1}` {
		t.Fatalf("canonical = %s", got)
	}
}

func TestComputeHashIgnoresHashAndSig(t *testing.T) {
	e := sampleEntry()
	h1, err := audit.ComputeHash(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Hash, e.Sig = "sha256:whatever", "ed25519:whatever"
	h2, _ := audit.ComputeHash(e)
	if h1 != h2 {
		t.Fatalf("hash changed when only hash/sig changed: %s vs %s", h1, h2)
	}
}

func TestComputeHashCoversPayload(t *testing.T) {
	a := sampleEntry()
	b := sampleEntry()
	b.Payload = json.RawMessage(`{"argv":["kubectl","delete","pods"]}`)
	ha, _ := audit.ComputeHash(a)
	hb, _ := audit.ComputeHash(b)
	if ha == hb {
		t.Fatal("different payloads produced the same hash")
	}
}

func TestSealSurvivesJSONRoundTripAndWhitespace(t *testing.T) {
	s := newSigner(t)
	e := sampleEntry()
	e.Payload = json.RawMessage(`{ "b": 1,   "a": [1, 2] }`)
	if err := s.Seal(&e); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back audit.Entry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), back); err != nil {
		t.Fatalf("round-tripped entry failed CheckSeal: %v", err)
	}
}

func TestCheckSealDetectsTampering(t *testing.T) {
	s := newSigner(t)
	e := sampleEntry()
	if err := s.Seal(&e); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), e); err != nil {
		t.Fatalf("fresh entry: %v", err)
	}

	tampered := e
	tampered.Payload = json.RawMessage(`{"argv":["rm","-rf","/"]}`)
	if err := audit.CheckSeal(s.Public(), tampered); !errors.Is(err, audit.ErrBadHash) {
		t.Fatalf("tampered payload: got %v, want ErrBadHash", err)
	}

	badSig := e
	badSig.Sig = "ed25519:AAAA"
	if err := audit.CheckSeal(s.Public(), badSig); !errors.Is(err, audit.ErrBadSignature) {
		t.Fatalf("bad sig: got %v, want ErrBadSignature", err)
	}

	forged := sampleEntry()
	if err := newSigner(t).Seal(&forged); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), forged); !errors.Is(err, audit.ErrBadSignature) {
		t.Fatalf("other key: got %v, want ErrBadSignature", err)
	}
}

func TestKeyPEMRoundTrip(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	privPEM, err := audit.MarshalPrivateKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := audit.ParsePrivateKeyPEM(privPEM)
	if err != nil || !back.Equal(key) {
		t.Fatalf("private key round trip failed: %v", err)
	}
	pubPEM, err := audit.MarshalPublicKeyPEM(key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := audit.ParsePublicKeyPEM(pubPEM)
	if err != nil || !pub.Equal(key.Public()) {
		t.Fatalf("public key round trip failed: %v", err)
	}
	if _, err := audit.ParsePublicKeyPEM([]byte("junk")); err == nil {
		t.Fatal("parsed junk as a public key")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -count=1`
Expected: compile errors (`no non-test Go files` or `undefined: audit.Entry`).

- [ ] **Step 4: Implement**

`internal/audit/entry.go`:
```go
// Package audit implements the write-once, hash-chained audit log (spec §14).
package audit

import "encoding/json"

// Entry types (spec §14.2).
const (
	TypeGatewayStart      = "gateway.start"
	TypeCheckpoint        = "checkpoint"
	TypePhaseBegin        = "phase.begin"
	TypePhaseEnd          = "phase.end"
	TypeLLMRequest        = "llm.request"
	TypeLLMResponse       = "llm.response"
	TypeCommandStart      = "command.start"
	TypeCommandResult     = "command.result"
	TypeCredentialIssued  = "credential.issued"
	TypeEgressGranted     = "egress.granted"
	TypeEgressRevoked     = "egress.revoked"
	TypeProbeStart        = "probe.start"
	TypeProbeResult       = "probe.result"
	TypeImageBuildStart   = "image.build.start"
	TypeImageBuildResult  = "image.build.result"
	TypeImageDeleted      = "image.deleted"
	TypeImageGCStart      = "image.gc.start"
	TypeImageGCResult     = "image.gc.result"
	TypeApprovalObserved  = "approval.observed"
	TypeRuleObserved      = "rule.observed"
	TypeSuggestionDecided = "suggestion.decided"
	TypePolicyDenied      = "policy.denied"
)

// GenesisHash is the prevHash of the entry at seq 0.
const GenesisHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// TimeFormat is fixed-width so an entry's bytes never depend on trailing-zero trimming.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// Authorization records what allowed an action.
type Authorization struct {
	Kind string `json:"kind"` // approval | rule | auto
	Name string `json:"name,omitempty"`
}

// Entry is one signed link in the audit chain.
type Entry struct {
	Seq          uint64          `json:"seq"`
	ChainID      string          `json:"chainId"`
	PrevHash     string          `json:"prevHash"`
	TS           string          `json:"ts"`
	Type         string          `json:"type"`
	Actor        string          `json:"actor"`
	Intent       string          `json:"intent,omitempty"`
	Stage        string          `json:"stage,omitempty"`
	SubjectHash  string          `json:"subjectHash,omitempty"`
	AuthorizedBy *Authorization  `json:"authorizedBy,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Hash         string          `json:"hash,omitempty"`
	Sig          string          `json:"sig,omitempty"`
}

// Draft is what callers supply; the Writer fills in the chain fields.
type Draft struct {
	Type         string
	Actor        string
	Intent       string
	Stage        string
	SubjectHash  string
	AuthorizedBy *Authorization
	Payload      any
}

// CheckpointPayload is the payload of a checkpoint entry.
type CheckpointPayload struct {
	PrevSeq  uint64 `json:"prevSeq"`
	PrevHash string `json:"prevHash"`
}

// CheckpointIndex is the body of checkpoints/<chainId>/<seq>.json.
type CheckpointIndex struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}
```

`internal/audit/canonical.go`:
```go
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gowebpki/jcs"
)

// Canonical returns the RFC 8785 canonical JSON encoding of v.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal: %w", err)
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize: %w", err)
	}
	return out, nil
}

// ComputeHash returns "sha256:<hex>" over the canonical entry with hash and sig removed.
func ComputeHash(e Entry) (string, error) {
	e.Hash, e.Sig = "", ""
	b, err := Canonical(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
```

`internal/audit/sign.go`:
```go
package audit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrBadHash      = errors.New("audit: entry hash does not match its contents")
	ErrBadSignature = errors.New("audit: entry signature is invalid")
)

// Signer seals entries with the gateway's Ed25519 key.
type Signer struct{ key ed25519.PrivateKey }

func NewSigner(key ed25519.PrivateKey) *Signer { return &Signer{key: key} }

func (s *Signer) Public() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// Seal fills e.Hash and e.Sig.
func (s *Signer) Seal(e *Entry) error {
	h, err := ComputeHash(*e)
	if err != nil {
		return err
	}
	e.Hash = h
	e.Sig = "ed25519:" + base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(h)))
	return nil
}

// CheckSeal verifies e's hash, then its signature over that hash.
func CheckSeal(pub ed25519.PublicKey, e Entry) error {
	h, err := ComputeHash(e)
	if err != nil || h != e.Hash {
		return ErrBadHash
	}
	b64, ok := strings.CutPrefix(e.Sig, "ed25519:")
	if !ok {
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !ed25519.Verify(pub, []byte(e.Hash), sig) {
		return ErrBadSignature
	}
	return nil
}

// Fingerprint identifies a public key in logs and ConfigMaps.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func MarshalPrivateKeyPEM(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParsePrivateKeyPEM(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("audit: no PEM block in private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("audit: private key is %T, want ed25519", k)
	}
	return key, nil
}

func MarshalPublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func ParsePublicKeyPEM(b []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("audit: no PEM block in public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("audit: public key is %T, want ed25519", k)
	}
	return pub, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/audit/ -count=1 -v`
Expected: PASS (6 tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/audit
git commit -m "feat(audit): add entry model, RFC 8785 hashing, and Ed25519 sealing"
```

### Task 6: Sink contract, key layout, and in-memory sink

**Files:**
- Create: `internal/audit/sink.go`, `internal/audit/keys.go`, `internal/audit/keys_test.go`, `internal/audit/audittest/memsink.go`, `internal/audit/audittest/memsink_test.go`

**Interfaces:**
- Consumes: nothing beyond the standard library.
- Produces (package `audit`): `ErrExists`, `ErrNotFound`, `ObjectInfo{Key string; VersionIDs []string /* oldest first */; DeleteMarkers int}`, `LockConfig{Enabled bool; Mode string; DefaultRetention time.Duration}`, and
  ```go
  type Sink interface {
      Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error // create-only; ErrExists if key exists
      Get(ctx context.Context, key, versionID string) ([]byte, error)               // "" = oldest version; ErrNotFound
      List(ctx context.Context, prefix, startAfter string) ([]ObjectInfo, error)     // keys > startAfter, sorted
      LockConfig(ctx context.Context) (LockConfig, error)
  }
  ```
  `EntryKey(chainID string, seq uint64) string`, `EntryPrefix(chainID string) string`, `CheckpointKey(chainID string, seq uint64) string`, `CheckpointPrefix(chainID string) string`, `SeqFromKey(key string) (uint64, error)`.
- Produces (package `audittest`): `NewMemSink() *MemSink` (implements `audit.Sink`; defaults to a COMPLIANCE lock with 365-day retention), `SetLockConfig(audit.LockConfig)`, `SetFailPuts(bool)`, `Keys() []string`, and the tamper helpers `TamperDelete(key)`, `TamperReplace(key, body)`, `TamperAddVersion(key, body)`, `TamperAddDeleteMarker(key)`, `TamperPut(key, body)`.

- [ ] **Step 1: Write the failing tests**

`internal/audit/keys_test.go`:
```go
package audit_test

import (
	"testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func TestKeyLayout(t *testing.T) {
	if got := audit.EntryKey("c1", 42); got != "chains/c1/00000000000000000042.json" {
		t.Fatalf("EntryKey = %s", got)
	}
	if got := audit.CheckpointKey("c1", 7); got != "checkpoints/c1/00000000000000000007.json" {
		t.Fatalf("CheckpointKey = %s", got)
	}
	if audit.EntryPrefix("c1") != "chains/c1/" || audit.CheckpointPrefix("c1") != "checkpoints/c1/" {
		t.Fatal("prefixes wrong")
	}
}

func TestSeqFromKey(t *testing.T) {
	seq, err := audit.SeqFromKey(audit.EntryKey("c1", 1234))
	if err != nil || seq != 1234 {
		t.Fatalf("SeqFromKey = %d, %v", seq, err)
	}
	for _, bad := range []string{"chains/c1/12.json", "chains/c1/0000000000000000001x.json", "chains/c1/00000000000000000001.txt"} {
		if _, err := audit.SeqFromKey(bad); err == nil {
			t.Errorf("SeqFromKey(%q) accepted a malformed key", bad)
		}
	}
}
```

`internal/audit/audittest/memsink_test.go`:
```go
package audittest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

var ctx = context.Background()

func TestMemSinkPutIsCreateOnly(t *testing.T) {
	m := audittest.NewMemSink()
	if err := m.Put(ctx, "k", []byte("a"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, "k", []byte("b"), time.Time{}); !errors.Is(err, audit.ErrExists) {
		t.Fatalf("second put = %v, want ErrExists", err)
	}
	got, err := m.Get(ctx, "k", "")
	if err != nil || string(got) != "a" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if _, err := m.Get(ctx, "missing", ""); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("missing get = %v, want ErrNotFound", err)
	}
}

func TestMemSinkListSortedWithStartAfter(t *testing.T) {
	m := audittest.NewMemSink()
	for _, k := range []string{"p/3", "p/1", "p/2", "q/1"} {
		_ = m.Put(ctx, k, []byte(k), time.Time{})
	}
	objs, err := m.List(ctx, "p/", "p/1")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Key != "p/2" || objs[1].Key != "p/3" {
		t.Fatalf("list = %+v", objs)
	}
}

func TestMemSinkTamperHelpersAreVisible(t *testing.T) {
	m := audittest.NewMemSink()
	_ = m.Put(ctx, "k", []byte("a"), time.Time{})
	m.TamperAddVersion("k", []byte("b"))
	m.TamperAddDeleteMarker("k")
	objs, _ := m.List(ctx, "", "")
	if len(objs) != 1 || len(objs[0].VersionIDs) != 2 || objs[0].DeleteMarkers != 1 {
		t.Fatalf("list = %+v", objs)
	}
	if got, _ := m.Get(ctx, "k", objs[0].VersionIDs[1]); string(got) != "b" {
		t.Fatalf("second version = %q", got)
	}
}

func TestMemSinkFailPuts(t *testing.T) {
	m := audittest.NewMemSink()
	m.SetFailPuts(true)
	if err := m.Put(ctx, "k", []byte("a"), time.Time{}); err == nil {
		t.Fatal("put succeeded while failing")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/... -count=1`
Expected: compile errors (`undefined: audit.EntryKey`, and no `audittest` package).

- [ ] **Step 3: Implement**

`internal/audit/sink.go`:
```go
package audit

import (
	"context"
	"errors"
	"time"
)

var (
	ErrExists   = errors.New("audit: object already exists")
	ErrNotFound = errors.New("audit: object not found")
)

// ObjectInfo describes every stored version of one key.
type ObjectInfo struct {
	Key           string
	VersionIDs    []string // oldest first
	DeleteMarkers int
}

// LockConfig is a bucket's default Object Lock configuration.
type LockConfig struct {
	Enabled          bool
	Mode             string // "COMPLIANCE", "GOVERNANCE", or ""
	DefaultRetention time.Duration
}

// Sink is WORM object storage for audit entries.
type Sink interface {
	// Put writes body at key only if key does not exist (If-None-Match: *), locked in
	// compliance mode until retainUntil. It returns ErrExists if key already exists.
	Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error
	// Get reads one version of key; versionID "" means the oldest (original) version.
	Get(ctx context.Context, key, versionID string) ([]byte, error)
	// List returns objects under prefix whose keys sort after startAfter, sorted by key.
	List(ctx context.Context, prefix, startAfter string) ([]ObjectInfo, error)
	// LockConfig reports the bucket's default Object Lock configuration.
	LockConfig(ctx context.Context) (LockConfig, error)
}
```

`internal/audit/keys.go`:
```go
package audit

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

func EntryPrefix(chainID string) string      { return "chains/" + chainID + "/" }
func CheckpointPrefix(chainID string) string { return "checkpoints/" + chainID + "/" }

func EntryKey(chainID string, seq uint64) string {
	return fmt.Sprintf("%s%020d.json", EntryPrefix(chainID), seq)
}

func CheckpointKey(chainID string, seq uint64) string {
	return fmt.Sprintf("%s%020d.json", CheckpointPrefix(chainID), seq)
}

// SeqFromKey parses the zero-padded sequence number from an entry or checkpoint key.
func SeqFromKey(key string) (uint64, error) {
	digits, ok := strings.CutSuffix(path.Base(key), ".json")
	if !ok || len(digits) != 20 {
		return 0, fmt.Errorf("audit: malformed key %q", key)
	}
	seq, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("audit: malformed key %q: %w", key, err)
	}
	return seq, nil
}
```

`internal/audit/audittest/memsink.go`:
```go
// Package audittest provides an in-memory audit.Sink with tamper helpers for tests.
package audittest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type version struct {
	id   string
	body []byte
}

type object struct {
	versions      []version
	deleteMarkers int
}

// MemSink is an in-memory audit.Sink that honors create-only semantics.
type MemSink struct {
	mu       sync.Mutex
	objects  map[string]*object
	lock     audit.LockConfig
	failPuts bool
	nextID   int
}

func NewMemSink() *MemSink {
	return &MemSink{
		objects: map[string]*object{},
		lock:    audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 365 * 24 * time.Hour},
	}
}

func (m *MemSink) SetLockConfig(lc audit.LockConfig) { m.mu.Lock(); defer m.mu.Unlock(); m.lock = lc }
func (m *MemSink) SetFailPuts(fail bool)             { m.mu.Lock(); defer m.mu.Unlock(); m.failPuts = fail }

func (m *MemSink) Put(_ context.Context, key string, body []byte, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPuts {
		return errors.New("memsink: store unavailable")
	}
	if o, ok := m.objects[key]; ok && len(o.versions) > 0 {
		return audit.ErrExists
	}
	m.addVersionLocked(key, body)
	return nil
}

func (m *MemSink) addVersionLocked(key string, body []byte) {
	o := m.objects[key]
	if o == nil {
		o = &object{}
		m.objects[key] = o
	}
	m.nextID++
	o.versions = append(o.versions, version{id: fmt.Sprintf("v%d", m.nextID), body: bytes.Clone(body)})
}

func (m *MemSink) Get(_ context.Context, key, versionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok || len(o.versions) == 0 {
		return nil, audit.ErrNotFound
	}
	if versionID == "" {
		return bytes.Clone(o.versions[0].body), nil
	}
	for _, v := range o.versions {
		if v.id == versionID {
			return bytes.Clone(v.body), nil
		}
	}
	return nil, audit.ErrNotFound
}

func (m *MemSink) List(_ context.Context, prefix, startAfter string) ([]audit.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []audit.ObjectInfo
	for k, o := range m.objects {
		if !strings.HasPrefix(k, prefix) || k <= startAfter {
			continue
		}
		info := audit.ObjectInfo{Key: k, DeleteMarkers: o.deleteMarkers}
		for _, v := range o.versions {
			info.VersionIDs = append(info.VersionIDs, v.id)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemSink) LockConfig(context.Context) (audit.LockConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lock, nil
}

// Keys returns every stored key, sorted.
func (m *MemSink) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The Tamper* helpers bypass Object Lock to simulate a compromised or non-conformant store.

func (m *MemSink) TamperDelete(key string) { m.mu.Lock(); defer m.mu.Unlock(); delete(m.objects, key) }

func (m *MemSink) TamperReplace(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok && len(o.versions) > 0 {
		o.versions[0].body = bytes.Clone(body)
	}
}

func (m *MemSink) TamperAddVersion(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addVersionLocked(key, body)
}

func (m *MemSink) TamperAddDeleteMarker(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok {
		o.deleteMarkers++
	}
}

// TamperPut writes key as a brand-new object even if it already exists.
func (m *MemSink) TamperPut(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	m.addVersionLocked(key, body)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/audit/... -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit
git commit -m "feat(audit): add Sink contract, key layout, and in-memory sink"
```

### Task 7: Chain verifier with tamper tests

**Files:**
- Create: `internal/audit/verify.go`, `internal/audit/verify_test.go`

**Interfaces:**
- Consumes: `Entry`, `CheckSeal`, `ErrBadHash`, `ErrBadSignature`, `GenesisHash`, `CheckpointPayload`, `CheckpointIndex`, `TypeCheckpoint` (Task 5); `Sink`, `EntryKey`, `EntryPrefix`, `CheckpointPrefix`, `SeqFromKey` (Task 6); `audittest.MemSink` (Task 6); `newSigner` (Task 5 test file).
- Produces: `ProblemKind` constants `ProblemGap`, `ProblemMalformed`, `ProblemSeqMismatch`, `ProblemChainMismatch`, `ProblemBadHash`, `ProblemBadSignature`, `ProblemBrokenLink`, `ProblemExtraVersion`, `ProblemDeleteMarker`, `ProblemBadCheckpoint`, `ProblemTruncated`; `Problem{Seq uint64; Kind ProblemKind; Detail string}`; `VerifyOptions{FromSeq uint64; ExpectHeadAtLeast *uint64}`; `Report{Entries uint64; First, Head *Entry; Problems []Problem}` with `OK() bool` and `FirstBad() (Problem, bool)`; `Verify(ctx, sink Sink, pub ed25519.PublicKey, chainID string, opts VerifyOptions) (Report, error)`. Test helpers in `audit_test`: `const chainID = "test-chain"`, `buildChain`, `putEntry`, `mustMarshal`, `verifyChain`, `requireProblem`.

- [ ] **Step 1: Write the failing tamper tests**

`internal/audit/verify_test.go`:
```go
package audit_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

const chainID = "test-chain"

var bg = context.Background()

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putEntry(t *testing.T, sink *audittest.MemSink, e audit.Entry) {
	t.Helper()
	if err := sink.Put(bg, audit.EntryKey(chainID, e.Seq), mustMarshal(t, e), time.Time{}); err != nil {
		t.Fatal(err)
	}
}

// buildChain seals n entries (seq 0..n-1) straight into sink, bypassing Writer.
// mutate, if non-nil, may alter each entry (PrevHash is already set) before sealing.
func buildChain(t *testing.T, sink *audittest.MemSink, s *audit.Signer, n int, mutate func(*audit.Entry)) []audit.Entry {
	t.Helper()
	prev := audit.GenesisHash
	out := make([]audit.Entry, 0, n)
	for i := 0; i < n; i++ {
		e := audit.Entry{
			Seq: uint64(i), ChainID: chainID, PrevHash: prev,
			TS: "2026-09-27T00:00:00.000000000Z", Type: audit.TypeCommandStart,
			Actor: "operator", Intent: "team-x/web",
			Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		}
		if mutate != nil {
			mutate(&e)
		}
		if err := s.Seal(&e); err != nil {
			t.Fatal(err)
		}
		putEntry(t, sink, e)
		prev = e.Hash
		out = append(out, e)
	}
	return out
}

func verifyChain(t *testing.T, sink audit.Sink, s *audit.Signer, opts audit.VerifyOptions) audit.Report {
	t.Helper()
	rep, err := audit.Verify(bg, sink, s.Public(), chainID, opts)
	if err != nil {
		t.Fatalf("verify returned an error: %v", err)
	}
	return rep
}

func requireProblem(t *testing.T, rep audit.Report, seq uint64, kind audit.ProblemKind) {
	t.Helper()
	p, ok := rep.FirstBad()
	if !ok {
		t.Fatalf("expected %s at seq %d, but the chain verified clean", kind, seq)
	}
	if p.Seq != seq || p.Kind != kind {
		t.Fatalf("first problem = %+v, want %s at seq %d (all: %+v)", p, kind, seq, rep.Problems)
	}
}

func TestVerifyCleanChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	rep := verifyChain(t, sink, s, audit.VerifyOptions{})
	if !rep.OK() || rep.Entries != 10 || rep.First.Seq != 0 || rep.Head.Seq != 9 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyEmptyChain(t *testing.T) {
	s := newSigner(t)
	rep := verifyChain(t, audittest.NewMemSink(), s, audit.VerifyOptions{})
	if !rep.OK() || rep.Entries != 0 || rep.Head != nil {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyDetectsDeletedEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperDelete(audit.EntryKey(chainID, 3))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 3, audit.ProblemGap)
}

func TestVerifyDetectsReorderedEntries(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), mustMarshal(t, es[3]))
	sink.TamperReplace(audit.EntryKey(chainID, 3), mustMarshal(t, es[2]))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 2, audit.ProblemSeqMismatch)
}

func TestVerifyDetectsModifiedPayload(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[4]
	e.Payload = json.RawMessage(`{"n":999}`)
	sink.TamperReplace(audit.EntryKey(chainID, 4), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 4, audit.ProblemBadHash)
}

func TestVerifyDetectsInvalidSignature(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[5]
	e.Sig = "ed25519:" + base64.StdEncoding.EncodeToString(make([]byte, 64))
	sink.TamperReplace(audit.EntryKey(chainID, 5), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadSignature)
}

func TestVerifyDetectsEntryResignedWithAnotherKey(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, nil)
	e := es[6]
	e.Payload = json.RawMessage(`{"n":"forged"}`)
	if err := newSigner(t).Seal(&e); err != nil {
		t.Fatal(err)
	}
	sink.TamperReplace(audit.EntryKey(chainID, 6), mustMarshal(t, e))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 6, audit.ProblemBadSignature)
}

func TestVerifyDetectsBrokenLink(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, func(e *audit.Entry) {
		if e.Seq == 4 {
			e.PrevHash = "sha256:" + strings.Repeat("f", 64)
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 4, audit.ProblemBrokenLink)
}

func TestVerifyDetectsBadGenesis(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 3, func(e *audit.Entry) {
		if e.Seq == 0 {
			e.PrevHash = "sha256:" + strings.Repeat("1", 64)
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 0, audit.ProblemBrokenLink)
}

func TestVerifyDetectsExtraVersion(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperAddVersion(audit.EntryKey(chainID, 5), []byte(`{}`))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemExtraVersion)
}

func TestVerifyDetectsDeleteMarker(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperAddDeleteMarker(audit.EntryKey(chainID, 7))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 7, audit.ProblemDeleteMarker)
}

func TestVerifyDetectsMalformedEntry(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 5, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("not json"))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 2, audit.ProblemMalformed)
}

func TestVerifyRejectsUnknownFields(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 5, nil)
	raw := strings.TrimSuffix(string(mustMarshal(t, es[1])), "}") + `,"evil":true}`
	sink.TamperReplace(audit.EntryKey(chainID, 1), []byte(raw))
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 1, audit.ProblemMalformed)
}

func checkpointAt(seq uint64) func(*audit.Entry) {
	return func(e *audit.Entry) {
		if e.Seq == seq {
			e.Type = audit.TypeCheckpoint
			b, _ := json.Marshal(audit.CheckpointPayload{PrevSeq: seq - 1, PrevHash: e.PrevHash})
			e.Payload = b
		}
	}
}

func TestVerifyAcceptsConsistentCheckpoint(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	es := buildChain(t, sink, s, 10, checkpointAt(5))
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 5), mustMarshal(t, audit.CheckpointIndex{Seq: 5, Hash: es[5].Hash}), time.Time{})
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestVerifyDetectsInconsistentCheckpointPayload(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, func(e *audit.Entry) {
		if e.Seq == 5 {
			e.Type = audit.TypeCheckpoint
			b, _ := json.Marshal(audit.CheckpointPayload{PrevSeq: 3, PrevHash: "sha256:x"})
			e.Payload = b
		}
	})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 5, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsForgedCheckpointIndex(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	forged := audit.CheckpointIndex{Seq: 3, Hash: "sha256:" + strings.Repeat("a", 64)}
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 3), mustMarshal(t, forged), time.Time{})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 3, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsCheckpointIndexBeyondHead(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	_ = sink.Put(bg, audit.CheckpointKey(chainID, 42), mustMarshal(t, audit.CheckpointIndex{Seq: 42, Hash: "sha256:x"}), time.Time{})
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{}), 42, audit.ProblemBadCheckpoint)
}

func TestVerifyDetectsTruncatedTailOnlyWithExpectation(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperDelete(audit.EntryKey(chainID, 9))
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("without an expectation, tail truncation is undetectable by design; got %+v", rep.Problems)
	}
	want := uint64(9)
	requireProblem(t, verifyChain(t, sink, s, audit.VerifyOptions{ExpectHeadAtLeast: &want}), 9, audit.ProblemTruncated)
}

func TestVerifyFromSeqSkipsEarlierEntries(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	buildChain(t, sink, s, 10, nil)
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("garbage"))
	rep := verifyChain(t, sink, s, audit.VerifyOptions{FromSeq: 5})
	if !rep.OK() || rep.Entries != 5 || rep.First.Seq != 5 {
		t.Fatalf("report = %+v", rep)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -count=1 -run Verify`
Expected: compile errors (`undefined: audit.Verify`).

- [ ] **Step 3: Implement the verifier**

`internal/audit/verify.go`:
```go
package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ProblemKind classifies a verification failure.
type ProblemKind string

const (
	ProblemGap           ProblemKind = "gap"
	ProblemMalformed     ProblemKind = "malformed"
	ProblemSeqMismatch   ProblemKind = "seq-mismatch"
	ProblemChainMismatch ProblemKind = "chain-mismatch"
	ProblemBadHash       ProblemKind = "bad-hash"
	ProblemBadSignature  ProblemKind = "bad-signature"
	ProblemBrokenLink    ProblemKind = "broken-link"
	ProblemExtraVersion  ProblemKind = "extra-version"
	ProblemDeleteMarker  ProblemKind = "delete-marker"
	ProblemBadCheckpoint ProblemKind = "bad-checkpoint"
	ProblemTruncated     ProblemKind = "truncated"
)

// Problem is one verification failure, anchored at a sequence number.
type Problem struct {
	Seq    uint64
	Kind   ProblemKind
	Detail string
}

// VerifyOptions narrows or strengthens verification.
type VerifyOptions struct {
	// FromSeq starts verification at this seq (normally a checkpoint).
	FromSeq uint64
	// ExpectHeadAtLeast, if set, fails verification when the chain ends before this seq.
	ExpectHeadAtLeast *uint64
}

// Report is the outcome of Verify.
type Report struct {
	Entries  uint64
	First    *Entry
	Head     *Entry
	Problems []Problem
}

func (r Report) OK() bool { return len(r.Problems) == 0 }

// FirstBad returns the problem with the lowest seq.
func (r Report) FirstBad() (Problem, bool) {
	if len(r.Problems) == 0 {
		return Problem{}, false
	}
	return r.Problems[0], true
}

func (r *Report) add(seq uint64, kind ProblemKind, format string, args ...any) {
	r.Problems = append(r.Problems, Problem{Seq: seq, Kind: kind, Detail: fmt.Sprintf(format, args...)})
}

// Verify checks the chain from opts.FromSeq to its head: sequence continuity,
// one version and no delete markers per entry, hashes, signatures, hash links,
// checkpoint payloads, and checkpoint index objects. It returns an error only
// when the store itself cannot be read.
func Verify(ctx context.Context, sink Sink, pub ed25519.PublicKey, chainID string, opts VerifyOptions) (Report, error) {
	var rep Report
	startAfter := ""
	if opts.FromSeq > 0 {
		startAfter = EntryKey(chainID, opts.FromSeq-1)
	}
	objs, err := sink.List(ctx, EntryPrefix(chainID), startAfter)
	if err != nil {
		return rep, fmt.Errorf("audit: list chain: %w", err)
	}

	hashes := map[uint64]string{}
	types := map[uint64]string{}
	expected := opts.FromSeq
	var prev *Entry
	for _, o := range objs {
		seq, err := SeqFromKey(o.Key)
		if err != nil {
			rep.add(expected, ProblemMalformed, "unexpected key %s", o.Key)
			continue
		}
		if seq != expected {
			rep.add(expected, ProblemGap, "missing seq %d..%d", expected, seq-1)
			prev = nil
		}
		expected = seq + 1
		if o.DeleteMarkers > 0 {
			rep.add(seq, ProblemDeleteMarker, "%d delete marker(s)", o.DeleteMarkers)
		}
		if len(o.VersionIDs) != 1 {
			rep.add(seq, ProblemExtraVersion, "%d versions", len(o.VersionIDs))
		}
		if len(o.VersionIDs) == 0 {
			prev = nil
			continue
		}
		body, err := sink.Get(ctx, o.Key, o.VersionIDs[0])
		if err != nil {
			return rep, fmt.Errorf("audit: read %s: %w", o.Key, err)
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			rep.add(seq, ProblemMalformed, "%v", err)
			prev = nil
			continue
		}
		if e.Seq != seq {
			rep.add(seq, ProblemSeqMismatch, "key says %d, entry says %d", seq, e.Seq)
		}
		if e.ChainID != chainID {
			rep.add(seq, ProblemChainMismatch, "entry chain %q", e.ChainID)
		}
		switch err := CheckSeal(pub, e); {
		case errors.Is(err, ErrBadHash):
			rep.add(seq, ProblemBadHash, "hash does not match contents")
		case errors.Is(err, ErrBadSignature):
			rep.add(seq, ProblemBadSignature, "signature invalid for this public key")
		}
		switch {
		case seq == 0 && e.PrevHash != GenesisHash:
			rep.add(seq, ProblemBrokenLink, "seq 0 must link to the genesis hash")
		case prev != nil && e.PrevHash != prev.Hash:
			rep.add(seq, ProblemBrokenLink, "prevHash does not match seq %d", prev.Seq)
		}
		if e.Type == TypeCheckpoint {
			var cp CheckpointPayload
			if err := json.Unmarshal(e.Payload, &cp); err != nil {
				rep.add(seq, ProblemBadCheckpoint, "payload: %v", err)
			} else if prev != nil && (cp.PrevSeq != prev.Seq || cp.PrevHash != prev.Hash) {
				rep.add(seq, ProblemBadCheckpoint, "payload does not match seq %d", prev.Seq)
			}
		}
		hashes[seq], types[seq] = e.Hash, e.Type
		entry := e
		if rep.First == nil {
			rep.First = &entry
		}
		rep.Head, prev = &entry, &entry
		rep.Entries++
	}

	if err := verifyCheckpointIndex(ctx, sink, chainID, opts.FromSeq, hashes, types, &rep); err != nil {
		return rep, err
	}

	if want := opts.ExpectHeadAtLeast; want != nil && (rep.Head == nil || rep.Head.Seq < *want) {
		head := "none"
		if rep.Head != nil {
			head = fmt.Sprint(rep.Head.Seq)
		}
		rep.add(*want, ProblemTruncated, "chain ends at seq %s, expected at least %d", head, *want)
	}
	sort.SliceStable(rep.Problems, func(i, j int) bool { return rep.Problems[i].Seq < rep.Problems[j].Seq })
	return rep, nil
}

func verifyCheckpointIndex(ctx context.Context, sink Sink, chainID string, fromSeq uint64, hashes, types map[uint64]string, rep *Report) error {
	idx, err := sink.List(ctx, CheckpointPrefix(chainID), "")
	if err != nil {
		return fmt.Errorf("audit: list checkpoints: %w", err)
	}
	for _, o := range idx {
		seq, err := SeqFromKey(o.Key)
		if err != nil {
			rep.add(0, ProblemBadCheckpoint, "unexpected key %s", o.Key)
			continue
		}
		if seq < fromSeq {
			continue
		}
		if len(o.VersionIDs) != 1 {
			rep.add(seq, ProblemBadCheckpoint, "index has %d versions", len(o.VersionIDs))
			continue
		}
		body, err := sink.Get(ctx, o.Key, o.VersionIDs[0])
		if err != nil {
			return fmt.Errorf("audit: read %s: %w", o.Key, err)
		}
		var ci CheckpointIndex
		if err := json.Unmarshal(body, &ci); err != nil {
			rep.add(seq, ProblemBadCheckpoint, "index: %v", err)
			continue
		}
		if ci.Seq != seq || hashes[seq] == "" || ci.Hash != hashes[seq] || types[seq] != TypeCheckpoint {
			rep.add(seq, ProblemBadCheckpoint, "index does not match a checkpoint entry in the chain")
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/audit/ -count=1 -v -run Verify`
Expected: PASS (19 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/audit
git commit -m "feat(audit): add chain verifier with tamper-detection tests"
```

### Task 8: Chain writer, checkpoints, and the fail-closed guard

**Files:**
- Create: `internal/audit/writer.go`, `internal/audit/guard.go`, `internal/audit/writer_test.go`

**Interfaces:**
- Consumes: Tasks 5–7 (`Signer`, `Entry`, `Draft`, `Sink`, keys, `Verify`, `CheckpointPayload`, `CheckpointIndex`); test helpers `chainID`, `bg`, `newSigner`, `verifyChain` (Tasks 5 and 7).
- Produces:
  ```go
  var ErrWriterBroken, ErrChainConflict, ErrAuditUnavailable error
  type WriterConfig struct {
      ChainID            string
      Retention          time.Duration    // per-object compliance retention
      CheckpointEvery    uint64           // default 1000
      CheckpointInterval time.Duration    // default 10m
      PutAttempts        int              // default 3
      RetryBackoff       time.Duration    // default 200ms
      Now                func() time.Time // default time.Now
      Actor              string           // default "gateway"
  }
  func Open(ctx context.Context, sink Sink, signer *Signer, cfg WriterConfig, startPayload any) (*Writer, error)
  func (w *Writer) Append(ctx context.Context, d Draft) (Entry, error)
  func (w *Writer) CheckpointIfDue(ctx context.Context) error
  func (w *Writer) Do(ctx context.Context, before Draft, action func(context.Context) (any, error), after func(result any, actionErr error) Draft) error
  func (w *Writer) Broken() bool
  func (w *Writer) Head() (seq uint64, hash string, ok bool)
  func (w *Writer) SetOnWrite(func(Entry))
  func (w *Writer) SetOnFailure(func(error))
  ```

- [ ] **Step 1: Write the failing tests**

`internal/audit/writer_test.go`:
```go
package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)} }

func writerConfig(c *fakeClock) audit.WriterConfig {
	return audit.WriterConfig{
		ChainID: chainID, Retention: time.Hour,
		CheckpointEvery: 1000, CheckpointInterval: 10 * time.Minute,
		PutAttempts: 3, RetryBackoff: time.Millisecond,
		Now: c.Now, Actor: "gateway",
	}
}

func openWriter(t *testing.T, sink audit.Sink, s *audit.Signer, cfg audit.WriterConfig) *audit.Writer {
	t.Helper()
	w, err := audit.Open(bg, sink, s, cfg, map[string]string{"pod": "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return w
}

func readEntry(t *testing.T, sink audit.Sink, seq uint64) audit.Entry {
	t.Helper()
	body, err := sink.Get(bg, audit.EntryKey(chainID, seq), "")
	if err != nil {
		t.Fatalf("get seq %d: %v", seq, err)
	}
	var e audit.Entry
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func cmd(n int) audit.Draft {
	return audit.Draft{Type: audit.TypeCommandStart, Actor: "operator", Intent: "team-x/web", Payload: map[string]int{"n": n}}
}

func TestOpenEmptyChainStartsAtGenesis(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	openWriter(t, sink, s, writerConfig(newClock()))
	e := readEntry(t, sink, 0)
	if e.Type != audit.TypeGatewayStart || e.PrevHash != audit.GenesisHash || e.Actor != "gateway" {
		t.Fatalf("first entry = %+v", e)
	}
	if e.TS != "2026-09-27T12:00:00.000000000Z" {
		t.Fatalf("ts = %s", e.TS)
	}
}

func TestAppendChainsAndVerifies(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	for i := 0; i < 10; i++ {
		if _, err := w.Append(bg, cmd(i)); err != nil {
			t.Fatal(err)
		}
	}
	rep := verifyChain(t, sink, s, audit.VerifyOptions{})
	if !rep.OK() || rep.Head.Seq != 10 {
		t.Fatalf("report = %+v", rep)
	}
	if seq, hash, ok := w.Head(); !ok || seq != 10 || hash != rep.Head.Hash {
		t.Fatalf("Head() = %d %s %v", seq, hash, ok)
	}
}

func TestOpenResumesAfterHead(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	w := openWriter(t, sink, s, cfg)
	for i := 0; i < 3; i++ {
		_, _ = w.Append(bg, cmd(i))
	}
	openWriter(t, sink, s, cfg)
	e := readEntry(t, sink, 4)
	if e.Type != audit.TypeGatewayStart || e.PrevHash != readEntry(t, sink, 3).Hash {
		t.Fatalf("resumed entry = %+v", e)
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestOpenRefusesTamperedChain(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	w := openWriter(t, sink, s, cfg)
	for i := 0; i < 5; i++ {
		_, _ = w.Append(bg, cmd(i))
	}
	sink.TamperReplace(audit.EntryKey(chainID, 2), []byte("garbage"))
	_, err := audit.Open(bg, sink, s, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "seq 2") {
		t.Fatalf("open on tampered chain = %v, want refusal naming seq 2", err)
	}
}

func TestCheckpointEveryN(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 5
	w := openWriter(t, sink, s, cfg) // seq 0
	for i := 0; i < 4; i++ {         // seq 1..4, then checkpoint at 5
		_, _ = w.Append(bg, cmd(i))
	}
	cp := readEntry(t, sink, 5)
	if cp.Type != audit.TypeCheckpoint {
		t.Fatalf("seq 5 type = %s, want checkpoint", cp.Type)
	}
	var p audit.CheckpointPayload
	_ = json.Unmarshal(cp.Payload, &p)
	if p.PrevSeq != 4 || p.PrevHash != readEntry(t, sink, 4).Hash {
		t.Fatalf("checkpoint payload = %+v", p)
	}
	if _, err := sink.Get(bg, audit.CheckpointKey(chainID, 5), ""); err != nil {
		t.Fatalf("checkpoint index missing: %v", err)
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestCheckpointIfDueUsesInterval(t *testing.T) {
	s, sink, clk := newSigner(t), audittest.NewMemSink(), newClock()
	w := openWriter(t, sink, s, writerConfig(clk)) // seq 0
	if err := w.CheckpointIfDue(bg); err != nil {
		t.Fatal(err)
	}
	if len(sink.Keys()) != 1 {
		t.Fatalf("checkpoint written before the interval elapsed: %v", sink.Keys())
	}
	clk.Advance(10 * time.Minute)
	if err := w.CheckpointIfDue(bg); err != nil {
		t.Fatal(err)
	}
	if readEntry(t, sink, 1).Type != audit.TypeCheckpoint {
		t.Fatal("no checkpoint after the interval")
	}
	clk.Advance(time.Hour)
	_ = w.CheckpointIfDue(bg)
	if _, err := sink.Get(bg, audit.EntryKey(chainID, 2), ""); err == nil {
		t.Fatal("checkpoint written with nothing new since the last one")
	}
}

func TestOpenVerifiesFromLatestCheckpoint(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	cfg := writerConfig(newClock())
	cfg.CheckpointEvery = 4
	w := openWriter(t, sink, s, cfg) // seq 0
	for i := 0; i < 5; i++ {         // seq 1,2,3 -> checkpoint 4 -> seq 5,6
		_, _ = w.Append(bg, cmd(i))
	}
	if readEntry(t, sink, 4).Type != audit.TypeCheckpoint {
		t.Fatal("expected checkpoint at seq 4")
	}
	sink.TamperReplace(audit.EntryKey(chainID, 1), []byte("garbage"))
	if _, err := audit.Open(bg, sink, s, cfg, nil); err != nil {
		t.Fatalf("entries before the latest checkpoint are trusted at startup, got: %v", err)
	}
	sink.TamperReplace(audit.EntryKey(chainID, 5), []byte("garbage"))
	if _, err := audit.Open(bg, sink, s, cfg, nil); err == nil {
		t.Fatal("open accepted tampering after the latest checkpoint")
	}
}

func TestPersistentPutFailureBreaksWriter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	var failures int
	w.SetOnFailure(func(error) { failures++ })
	sink.SetFailPuts(true)
	if _, err := w.Append(bg, cmd(1)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("append = %v, want ErrWriterBroken", err)
	}
	sink.SetFailPuts(false)
	if _, err := w.Append(bg, cmd(2)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("broken writer accepted an append: %v", err)
	}
	if !w.Broken() || failures != 1 {
		t.Fatalf("broken=%v failures=%d", w.Broken(), failures)
	}
}

// flakySink stores the object but reports failure once, like a timeout after the write landed.
type flakySink struct {
	*audittest.MemSink
	failed bool
}

func (f *flakySink) Put(ctx context.Context, key string, body []byte, until time.Time) error {
	if err := f.MemSink.Put(ctx, key, body, until); err != nil {
		return err
	}
	if !f.failed {
		f.failed = true
		return errors.New("timeout after write")
	}
	return nil
}

func TestAmbiguousWriteIsRecovered(t *testing.T) {
	s := newSigner(t)
	sink := &flakySink{MemSink: audittest.NewMemSink()}
	w := openWriter(t, sink, s, writerConfig(newClock()))
	if w.Broken() {
		t.Fatal("writer broke on a write that actually landed")
	}
	if rep := verifyChain(t, sink, s, audit.VerifyOptions{}); !rep.OK() {
		t.Fatalf("problems: %+v", rep.Problems)
	}
}

func TestChainConflictBreaksWriter(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	sink.TamperPut(audit.EntryKey(chainID, 1), []byte(`{"another":"writer"}`))
	if _, err := w.Append(bg, cmd(1)); !errors.Is(err, audit.ErrChainConflict) {
		t.Fatalf("append = %v, want ErrChainConflict", err)
	}
	if _, err := w.Append(bg, cmd(2)); !errors.Is(err, audit.ErrWriterBroken) {
		t.Fatalf("append after conflict = %v, want ErrWriterBroken", err)
	}
}

func after(result any, err error) audit.Draft {
	payload := map[string]any{"result": result}
	if err != nil {
		payload["error"] = err.Error()
	}
	return audit.Draft{Type: audit.TypeCommandResult, Actor: "gateway", Payload: payload}
}

func TestDoRecordsBeforeRunningAction(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	var sawBefore bool
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) {
		_, getErr := sink.Get(bg, audit.EntryKey(chainID, 1), "")
		sawBefore = getErr == nil
		return map[string]int{"exitCode": 0}, nil
	}, after)
	if err != nil || !sawBefore {
		t.Fatalf("err=%v sawBefore=%v", err, sawBefore)
	}
	if readEntry(t, sink, 2).Type != audit.TypeCommandResult {
		t.Fatal("outcome not recorded")
	}
}

func TestDoRefusesActionWhenAuditUnavailable(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	sink.SetFailPuts(true)
	called := false
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { called = true; return nil, nil }, after)
	if !errors.Is(err, audit.ErrAuditUnavailable) || called {
		t.Fatalf("err=%v called=%v; want ErrAuditUnavailable and no action", err, called)
	}
}

func TestDoRecordsActionFailure(t *testing.T) {
	s, sink := newSigner(t), audittest.NewMemSink()
	w := openWriter(t, sink, s, writerConfig(newClock()))
	actionErr := errors.New("exit status 1")
	err := w.Do(bg, cmd(1), func(context.Context) (any, error) { return nil, actionErr }, after)
	if !errors.Is(err, actionErr) {
		t.Fatalf("Do = %v, want the action's error", err)
	}
	if !strings.Contains(string(readEntry(t, sink, 2).Payload), "exit status 1") {
		t.Fatal("failure not recorded in the outcome entry")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/audit/ -count=1 -run 'Open|Append|Checkpoint|Put|Ambiguous|Conflict|Do'`
Expected: compile errors (`undefined: audit.Open`).

- [ ] **Step 3: Implement the writer**

`internal/audit/writer.go`:
```go
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrWriterBroken     = errors.New("audit: writer is broken; restart the gateway to resynchronize")
	ErrChainConflict    = errors.New("audit: chain conflict: a different entry already exists at this seq")
	ErrAuditUnavailable = errors.New("audit: entry could not be recorded; action refused")
)

// WriterConfig configures a Writer. Zero values get defaults.
type WriterConfig struct {
	ChainID            string
	Retention          time.Duration
	CheckpointEvery    uint64
	CheckpointInterval time.Duration
	PutAttempts        int
	RetryBackoff       time.Duration
	Now                func() time.Time
	Actor              string
}

func (c WriterConfig) withDefaults() WriterConfig {
	if c.CheckpointEvery == 0 {
		c.CheckpointEvery = 1000
	}
	if c.CheckpointInterval == 0 {
		c.CheckpointInterval = 10 * time.Minute
	}
	if c.PutAttempts == 0 {
		c.PutAttempts = 3
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = 200 * time.Millisecond
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Actor == "" {
		c.Actor = "gateway"
	}
	return c
}

// Writer appends sealed entries to one chain. It is safe for concurrent use.
// Once broken it refuses every write until a new Writer is opened.
type Writer struct {
	cfg    WriterConfig
	sink   Sink
	signer *Signer

	mu              sync.Mutex
	nextSeq         uint64
	prevHash        string
	sinceCheckpoint uint64
	lastCheckpoint  time.Time
	broken          bool
	onWrite         func(Entry)
	onFailure       func(error)
}

// Open verifies the chain from its latest checkpoint, resumes after its head, and
// records a gateway.start entry. It refuses a chain that fails verification.
func Open(ctx context.Context, sink Sink, signer *Signer, cfg WriterConfig, startPayload any) (*Writer, error) {
	cfg = cfg.withDefaults()
	from, err := latestCheckpointSeq(ctx, sink, cfg.ChainID)
	if err != nil {
		return nil, err
	}
	rep, err := Verify(ctx, sink, signer.Public(), cfg.ChainID, VerifyOptions{FromSeq: from})
	if err != nil {
		return nil, err
	}
	if p, bad := rep.FirstBad(); bad {
		return nil, fmt.Errorf("audit: chain failed verification at seq %d (%s): %s", p.Seq, p.Kind, p.Detail)
	}
	w := &Writer{cfg: cfg, sink: sink, signer: signer, prevHash: GenesisHash, lastCheckpoint: cfg.Now()}
	if rep.Head != nil {
		w.nextSeq, w.prevHash = rep.Head.Seq+1, rep.Head.Hash
	}
	if _, err := w.Append(ctx, Draft{Type: TypeGatewayStart, Actor: cfg.Actor, Payload: startPayload}); err != nil {
		return nil, err
	}
	return w, nil
}

func latestCheckpointSeq(ctx context.Context, sink Sink, chainID string) (uint64, error) {
	idx, err := sink.List(ctx, CheckpointPrefix(chainID), "")
	if err != nil {
		return 0, fmt.Errorf("audit: list checkpoints: %w", err)
	}
	if len(idx) == 0 {
		return 0, nil
	}
	return SeqFromKey(idx[len(idx)-1].Key)
}

func (w *Writer) SetOnWrite(f func(Entry))   { w.mu.Lock(); defer w.mu.Unlock(); w.onWrite = f }
func (w *Writer) SetOnFailure(f func(error)) { w.mu.Lock(); defer w.mu.Unlock(); w.onFailure = f }

func (w *Writer) Broken() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.broken }

// Head returns the last written entry's seq and hash.
func (w *Writer) Head() (uint64, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nextSeq == 0 {
		return 0, "", false
	}
	return w.nextSeq - 1, w.prevHash, true
}

// Append seals d as the next entry and stores it. A checkpoint may follow it;
// if that checkpoint fails, the error is returned even though d was stored.
func (w *Writer) Append(ctx context.Context, d Draft) (Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, err := w.appendLocked(ctx, d)
	if err != nil {
		return Entry{}, err
	}
	return e, w.checkpointLocked(ctx, false)
}

// CheckpointIfDue writes a checkpoint if CheckpointInterval has passed since the
// last one and something was written since. Call it periodically.
func (w *Writer) CheckpointIfDue(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkpointLocked(ctx, true)
}

func (w *Writer) appendLocked(ctx context.Context, d Draft) (Entry, error) {
	if w.broken {
		return Entry{}, ErrWriterBroken
	}
	payload, err := marshalPayload(d.Payload)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		Seq: w.nextSeq, ChainID: w.cfg.ChainID, PrevHash: w.prevHash,
		TS:   w.cfg.Now().UTC().Format(TimeFormat),
		Type: d.Type, Actor: d.Actor, Intent: d.Intent, Stage: d.Stage,
		SubjectHash: d.SubjectHash, AuthorizedBy: d.AuthorizedBy, Payload: payload,
	}
	if err := w.signer.Seal(&e); err != nil {
		return Entry{}, err
	}
	body, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	if err := w.putLocked(ctx, EntryKey(w.cfg.ChainID, e.Seq), body); err != nil {
		return Entry{}, err
	}
	w.nextSeq++
	w.prevHash = e.Hash
	if e.Type != TypeCheckpoint {
		w.sinceCheckpoint++
	}
	if w.onWrite != nil {
		w.onWrite(e)
	}
	return e, nil
}

func (w *Writer) checkpointLocked(ctx context.Context, byTime bool) error {
	due := w.sinceCheckpoint >= w.cfg.CheckpointEvery ||
		(byTime && w.sinceCheckpoint > 0 && w.cfg.Now().Sub(w.lastCheckpoint) >= w.cfg.CheckpointInterval)
	if !due {
		return nil
	}
	cp, err := w.appendLocked(ctx, Draft{
		Type: TypeCheckpoint, Actor: w.cfg.Actor,
		Payload: CheckpointPayload{PrevSeq: w.nextSeq - 1, PrevHash: w.prevHash},
	})
	if err != nil {
		return err
	}
	idx, err := json.Marshal(CheckpointIndex{Seq: cp.Seq, Hash: cp.Hash})
	if err != nil {
		return err
	}
	if err := w.putLocked(ctx, CheckpointKey(w.cfg.ChainID, cp.Seq), idx); err != nil {
		return err
	}
	w.sinceCheckpoint = 0
	w.lastCheckpoint = w.cfg.Now()
	return nil
}

// putLocked writes body, retrying the identical bytes. An ErrExists whose stored
// bytes match is an earlier attempt that landed; anything else breaks the writer.
func (w *Writer) putLocked(ctx context.Context, key string, body []byte) error {
	retainUntil := w.cfg.Now().Add(w.cfg.Retention)
	var lastErr error
	for attempt := 0; attempt < w.cfg.PutAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return w.fail(fmt.Errorf("%w: %v", ErrWriterBroken, ctx.Err()))
			case <-time.After(w.cfg.RetryBackoff):
			}
		}
		err := w.sink.Put(ctx, key, body, retainUntil)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrExists) {
			existing, getErr := w.sink.Get(ctx, key, "")
			if getErr == nil && bytes.Equal(existing, body) {
				return nil
			}
			return w.fail(fmt.Errorf("%w (key %s)", ErrChainConflict, key))
		}
		lastErr = err
	}
	return w.fail(fmt.Errorf("%w: %v", ErrWriterBroken, lastErr))
}

func (w *Writer) fail(err error) error {
	w.broken = true
	if w.onFailure != nil {
		w.onFailure(err)
	}
	return err
}

func marshalPayload(p any) (json.RawMessage, error) {
	switch v := p.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return v, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("audit: marshal payload: %w", err)
		}
		return b, nil
	}
}
```

`internal/audit/guard.go`:
```go
package audit

import (
	"context"
	"errors"
	"fmt"
)

// Do is the fail-closed primitive (spec §14.1): it records before, runs action only
// if that record is durable, then records the outcome built by after.
func (w *Writer) Do(ctx context.Context, before Draft, action func(context.Context) (any, error), after func(result any, actionErr error) Draft) error {
	if _, err := w.Append(ctx, before); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	result, actionErr := action(ctx)
	if _, err := w.Append(ctx, after(result, actionErr)); err != nil {
		return errors.Join(actionErr, fmt.Errorf("audit: outcome not recorded: %w", err))
	}
	return actionErr
}
```

- [ ] **Step 4: Run the whole audit package**

Run: `go test ./internal/audit/... -count=1 -v`
Expected: PASS for all tests from Tasks 5–8.

- [ ] **Step 5: Commit**

```bash
git add internal/audit
git commit -m "feat(audit): add chain writer, checkpoints, and fail-closed Do"
```

### Task 9: S3 sink, bucket setup, and RustFS integration tests

**Files:**
- Create: `internal/audit/lock.go`, `internal/audit/lock_test.go`, `internal/audit/s3sink.go`, `internal/audit/bucket.go`, `internal/testutil/rustfstest/rustfs.go`, `internal/audit/s3_integration_test.go`
- Modify: `Makefile` (add `test-integration`)

**Interfaces:**
- Consumes: `Sink`, `ObjectInfo`, `LockConfig`, `ErrExists`, `ErrNotFound` (Task 6); `Writer`, `Open`, `Do`, `ErrAuditUnavailable` (Task 8); `Verify` (Task 7); test helpers from Tasks 5, 7 and 8.
- Produces:
  ```go
  func CheckLockConfig(lc LockConfig, retention time.Duration) error
  type S3Config struct{ Endpoint, Region, Bucket, AccessKey, SecretKey string; PathStyle bool }
  func NewS3Client(ctx context.Context, cfg S3Config) (*s3.Client, error)
  func NewS3Sink(client *s3.Client, bucket string) *S3Sink // implements Sink
  func EnsureLockedBucket(ctx context.Context, client *s3.Client, bucket string, retentionDays int32) error
  func IsAccessDenied(err error) bool
  ```
  Package `rustfstest` (build tag `integration`): `Image`, `RootAccessKey`, `RootSecretKey`, `Start(t) *Server`, `(*Server).Endpoint`, `(*Server).Client(t, accessKey, secretKey) *s3.Client`, `(*Server).RootClient(t) *s3.Client`, `(*Server).Stop(t)`.

- [ ] **Step 1: Write the failing unit test for the lock check**

`internal/audit/lock_test.go`:
```go
package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func TestCheckLockConfig(t *testing.T) {
	year := 365 * 24 * time.Hour
	cases := []struct {
		name string
		lc   audit.LockConfig
		want string // substring of the error; "" = ok
	}{
		{"compliant", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: year}, ""},
		{"longer is fine", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 2 * year}, ""},
		{"not enabled", audit.LockConfig{}, "Object Lock"},
		{"governance", audit.LockConfig{Enabled: true, Mode: "GOVERNANCE", DefaultRetention: year}, "COMPLIANCE"},
		{"too short", audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 24 * time.Hour}, "shorter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := audit.CheckLockConfig(c.lc, year)
			if c.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails, then implement `lock.go`**

Run: `go test ./internal/audit/ -count=1 -run CheckLockConfig`
Expected: compile error (`undefined: audit.CheckLockConfig`).

`internal/audit/lock.go`:
```go
package audit

import (
	"errors"
	"fmt"
	"time"
)

// CheckLockConfig enforces the gateway's startup requirement (spec §14.4): Object
// Lock enabled, compliance mode, and default retention at least as long as configured.
func CheckLockConfig(lc LockConfig, retention time.Duration) error {
	switch {
	case !lc.Enabled:
		return errors.New("audit: bucket does not have Object Lock enabled")
	case lc.Mode != "COMPLIANCE":
		return fmt.Errorf("audit: bucket default retention mode is %q, want COMPLIANCE", lc.Mode)
	case lc.DefaultRetention < retention:
		return fmt.Errorf("audit: bucket default retention %s is shorter than configured %s", lc.DefaultRetention, retention)
	}
	return nil
}
```

Run: `go test ./internal/audit/ -count=1 -run CheckLockConfig`
Expected: PASS.

- [ ] **Step 3: Add dependencies**

```bash
go get github.com/aws/aws-sdk-go-v2@latest github.com/aws/aws-sdk-go-v2/config@latest \
  github.com/aws/aws-sdk-go-v2/credentials@latest github.com/aws/aws-sdk-go-v2/service/s3@latest \
  github.com/aws/smithy-go@latest github.com/testcontainers/testcontainers-go@latest
```

- [ ] **Step 4: Write the RustFS harness and failing integration tests**

`internal/testutil/rustfstest/rustfs.go`:
```go
//go:build integration

// Package rustfstest starts a disposable RustFS server for integration tests.
package rustfstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/moogie-labs/last-operator/internal/audit"
)

const (
	Image         = "rustfs/rustfs:1.0.0"
	RootAccessKey = "last-root"
	RootSecretKey = "last-root-secret-key-for-tests"
)

type Server struct {
	Endpoint  string
	container testcontainers.Container
}

// Start runs RustFS and waits until /health/ready answers. It is terminated at test cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"RUSTFS_ACCESS_KEY":     RootAccessKey,
				"RUSTFS_SECRET_KEY":     RootSecretKey,
				"RUSTFS_VOLUMES":        "/data",
				"RUSTFS_ADDRESS":        ":9000",
				"RUSTFS_CONSOLE_ENABLE": "false",
			},
			WaitingFor: wait.ForHTTP("/health/ready").WithPort("9000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start rustfs: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Endpoint: fmt.Sprintf("http://%s:%s", host, port.Port()), container: c}
}

// Client returns an S3 client for the given credentials.
func (s *Server) Client(t *testing.T, accessKey, secretKey string) *s3.Client {
	t.Helper()
	c, err := audit.NewS3Client(context.Background(), audit.S3Config{
		Endpoint: s.Endpoint, AccessKey: accessKey, SecretKey: secretKey, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *Server) RootClient(t *testing.T) *s3.Client {
	return s.Client(t, RootAccessKey, RootSecretKey)
}

// Stop halts the container without removing it, simulating an outage.
func (s *Server) Stop(t *testing.T) {
	t.Helper()
	timeout := 10 * time.Second
	if err := s.container.Stop(context.Background(), &timeout); err != nil {
		t.Fatal(err)
	}
}
```

`internal/audit/s3_integration_test.go`:
```go
//go:build integration

package audit_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/testutil/rustfstest"
)

func randomBucket(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "audit-" + hex.EncodeToString(b)
}

func lockedSink(t *testing.T, srv *rustfstest.Server) (*s3.Client, *audit.S3Sink, string) {
	t.Helper()
	client := srv.RootClient(t)
	bucket := randomBucket(t)
	if err := audit.EnsureLockedBucket(bg, client, bucket, 1); err != nil {
		t.Fatalf("EnsureLockedBucket: %v", err)
	}
	return client, audit.NewS3Sink(client, bucket), bucket
}

func TestIntegrationS3SinkContract(t *testing.T) {
	srv := rustfstest.Start(t)
	_, sink, _ := lockedSink(t, srv)
	key := audit.EntryKey("c", 0)
	until := time.Now().Add(time.Hour)
	if err := sink.Put(bg, key, []byte(`{"a":1}`), until); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := sink.Put(bg, key, []byte(`{"a":2}`), until); !errors.Is(err, audit.ErrExists) {
		t.Fatalf("second put = %v, want ErrExists", err)
	}
	got, err := sink.Get(bg, key, "")
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("get = %q, %v", got, err)
	}
	objs, err := sink.List(bg, audit.EntryPrefix("c"), "")
	if err != nil || len(objs) != 1 || len(objs[0].VersionIDs) != 1 || objs[0].DeleteMarkers != 0 {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	if _, err := sink.Get(bg, audit.EntryKey("c", 99), ""); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("missing get = %v, want ErrNotFound", err)
	}
}

func TestIntegrationEnsureLockedBucketIsIdempotent(t *testing.T) {
	srv := rustfstest.Start(t)
	client, sink, bucket := lockedSink(t, srv)
	if err := audit.EnsureLockedBucket(bg, client, bucket, 1); err != nil {
		t.Fatalf("second EnsureLockedBucket: %v", err)
	}
	lc, err := sink.LockConfig(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckLockConfig(lc, 24*time.Hour); err != nil {
		t.Fatalf("lock config %+v: %v", lc, err)
	}
}

func TestIntegrationWriterChainOnRustFS(t *testing.T) {
	srv := rustfstest.Start(t)
	_, sink, _ := lockedSink(t, srv)
	s := newSigner(t)
	cfg := audit.WriterConfig{ChainID: chainID, Retention: time.Hour, CheckpointEvery: 5, RetryBackoff: 10 * time.Millisecond}
	w, err := audit.Open(bg, sink, s, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := w.Append(bg, cmd(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	rep, err := audit.Verify(bg, sink, s.Public(), chainID, audit.VerifyOptions{})
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %+v", err, rep.Problems)
	}
	idx, _ := sink.List(bg, audit.CheckpointPrefix(chainID), "")
	if len(idx) < 4 {
		t.Fatalf("expected at least 4 checkpoint index objects, got %d", len(idx))
	}
	if _, err := audit.Open(bg, sink, s, cfg, nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestIntegrationFailClosedWhenStoreStops(t *testing.T) {
	srv := rustfstest.Start(t)
	_, sink, _ := lockedSink(t, srv)
	w, err := audit.Open(bg, sink, newSigner(t), audit.WriterConfig{ChainID: chainID, Retention: time.Hour, RetryBackoff: 10 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Stop(t)
	ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	called := false
	err = w.Do(ctx, cmd(1), func(context.Context) (any, error) { called = true; return nil, nil }, after)
	if !errors.Is(err, audit.ErrAuditUnavailable) || called {
		t.Fatalf("err=%v called=%v; want ErrAuditUnavailable and no action", err, called)
	}
}
```

Add to `Makefile`:
```make
.PHONY: test-integration
test-integration: ## Run integration tests (requires Docker).
	go test -tags integration ./... -run Integration -count=1 -v -timeout 20m
```

Run: `go test -tags integration ./internal/audit/ -run Integration -count=1`
Expected: compile errors (`undefined: audit.NewS3Client`, `audit.EnsureLockedBucket`).

- [ ] **Step 5: Implement the S3 sink and bucket setup**

`internal/audit/s3sink.go`:
```go
package audit

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config locates an S3-compatible audit bucket.
type S3Config struct {
	Endpoint  string // empty = AWS endpoint resolution
	Region    string // default us-east-1
	Bucket    string
	AccessKey string // empty = AWS default credential chain
	SecretKey string
	PathStyle bool // required for RustFS
}

// NewS3Client builds a client for S3-compatible stores. It sends checksums only
// when an operation requires them, since many stores reject aws-chunked trailers.
func NewS3Client(ctx context.Context, cfg S3Config) (*s3.Client, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if cfg.AccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("audit: load aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}), nil
}

// S3Sink is a Sink backed by an Object Lock bucket.
type S3Sink struct {
	client *s3.Client
	bucket string
}

func NewS3Sink(client *s3.Client, bucket string) *S3Sink { return &S3Sink{client: client, bucket: bucket} }

func (s *S3Sink) Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error {
	sum := md5.Sum(body)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:                    aws.String(s.bucket),
		Key:                       aws.String(key),
		Body:                      bytes.NewReader(body),
		ContentLength:             aws.Int64(int64(len(body))),
		ContentMD5:                aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		ContentType:               aws.String("application/json"),
		IfNoneMatch:               aws.String("*"),
		ObjectLockMode:            types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: aws.Time(retainUntil.UTC()),
	})
	if err != nil {
		if hasCode(err, http.StatusPreconditionFailed, "PreconditionFailed", "ConditionalRequestConflict") {
			return ErrExists
		}
		return fmt.Errorf("audit: put %s: %w", key, err)
	}
	return nil
}

func (s *S3Sink) Get(ctx context.Context, key, versionID string) ([]byte, error) {
	if versionID == "" {
		objs, err := s.List(ctx, key, "")
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if o.Key == key && len(o.VersionIDs) > 0 {
				versionID = o.VersionIDs[0]
			}
		}
		if versionID == "" {
			return nil, ErrNotFound
		}
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), VersionId: aws.String(versionID),
	})
	if err != nil {
		if hasCode(err, http.StatusNotFound, "NoSuchKey", "NoSuchVersion", "NotFound") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("audit: get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// List uses ListObjectVersions so extra versions and delete markers are visible.
// S3 lists each key's versions newest first; we reverse them to oldest first.
func (s *S3Sink) List(ctx context.Context, prefix, startAfter string) ([]ObjectInfo, error) {
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)}
	if startAfter != "" {
		in.KeyMarker = aws.String(startAfter)
	}
	byKey := map[string]*ObjectInfo{}
	info := func(k string) *ObjectInfo {
		if byKey[k] == nil {
			byKey[k] = &ObjectInfo{Key: k}
		}
		return byKey[k]
	}
	for {
		page, err := s.client.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("audit: list %s: %w", prefix, err)
		}
		for _, v := range page.Versions {
			if k := aws.ToString(v.Key); k > startAfter {
				o := info(k)
				o.VersionIDs = append(o.VersionIDs, aws.ToString(v.VersionId))
			}
		}
		for _, d := range page.DeleteMarkers {
			if k := aws.ToString(d.Key); k > startAfter {
				info(k).DeleteMarkers++
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	out := make([]ObjectInfo, 0, len(byKey))
	for _, o := range byKey {
		for i, j := 0, len(o.VersionIDs)-1; i < j; i, j = i+1, j-1 {
			o.VersionIDs[i], o.VersionIDs[j] = o.VersionIDs[j], o.VersionIDs[i]
		}
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *S3Sink) LockConfig(ctx context.Context) (LockConfig, error) {
	out, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		if hasCode(err, 0, "ObjectLockConfigurationNotFoundError") {
			return LockConfig{}, nil
		}
		return LockConfig{}, fmt.Errorf("audit: get lock config: %w", err)
	}
	var lc LockConfig
	olc := out.ObjectLockConfiguration
	if olc == nil {
		return lc, nil
	}
	lc.Enabled = olc.ObjectLockEnabled == types.ObjectLockEnabledEnabled
	if olc.Rule != nil && olc.Rule.DefaultRetention != nil {
		dr := olc.Rule.DefaultRetention
		lc.Mode = string(dr.Mode)
		lc.DefaultRetention = time.Duration(aws.ToInt32(dr.Days))*24*time.Hour +
			time.Duration(aws.ToInt32(dr.Years))*365*24*time.Hour
	}
	return lc, nil
}

// IsAccessDenied reports whether err is an authorization refusal from the store.
func IsAccessDenied(err error) bool { return hasCode(err, http.StatusForbidden, "AccessDenied") }

// hasCode matches an S3 error by API error code or, if status > 0, HTTP status.
func hasCode(err error, status int, codes ...string) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		for _, c := range codes {
			if apiErr.ErrorCode() == c {
				return true
			}
		}
	}
	var re *awshttp.ResponseError
	return status > 0 && errors.As(err, &re) && re.HTTPStatusCode() == status
}
```

`internal/audit/bucket.go`:
```go
package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// EnsureLockedBucket creates bucket with Object Lock enabled if it is missing, and
// sets a compliance-mode default retention of at least retentionDays. It is idempotent.
func EnsureLockedBucket(ctx context.Context, client *s3.Client, bucket string, retentionDays int32) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                     aws.String(bucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	})
	if err != nil && !hasCode(err, 0, "BucketAlreadyOwnedByYou", "BucketAlreadyExists") {
		return fmt.Errorf("audit: create bucket %s: %w", bucket, err)
	}
	lc, err := NewS3Sink(client, bucket).LockConfig(ctx)
	if err != nil {
		return err
	}
	if !lc.Enabled {
		return fmt.Errorf("audit: bucket %s exists without Object Lock; use a new bucket", bucket)
	}
	want := time.Duration(retentionDays) * 24 * time.Hour
	if CheckLockConfig(lc, want) == nil {
		return nil
	}
	_, err = client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String(bucket),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{
				Mode: types.ObjectLockRetentionModeCompliance,
				Days: aws.Int32(retentionDays),
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("audit: set lock config on %s: %w", bucket, err)
	}
	return nil
}
```

- [ ] **Step 6: Run the integration tests (Docker must be running)**

Run: `make test-integration`
Expected: PASS for the four `TestIntegration*` tests in `internal/audit`.
If RustFS rejects an operation (for example `PutObjectLockConfiguration` without `Content-MD5`, or `IfNoneMatch` is ignored so the second put succeeds), **stop and report the exact error to the user**. These are the WORM guarantees the design depends on. Do not weaken the test.

- [ ] **Step 7: Run unit tests and commit**

Run: `go test ./internal/audit/... -count=1`
Expected: PASS. Integration files are excluded without the tag.

```bash
git add go.mod go.sum Makefile internal
git commit -m "feat(audit): add S3 Object Lock sink with RustFS integration tests"
```

### Task 10: WORM conformance check

**Files:**
- Create: `internal/audit/conformance.go`, `internal/audit/conformance_integration_test.go`

**Interfaces:**
- Consumes: `NewS3Client`, `EnsureLockedBucket`, `hasCode` (Task 9); `rustfstest` (Task 9).
- Produces:
  ```go
  type CheckResult struct{ Name string; Passed bool; Detail string }
  func RunConformance(ctx context.Context, client *s3.Client, bucket string, retention time.Duration) ([]CheckResult, error)
  ```
  Check names, in order: `delete-version-refused`, `conditional-overwrite-refused`, `plain-overwrite-preserves-original`, `shorten-retention-refused`, `switch-to-governance-refused`, `delete-marker-preserves-original`, `delete-bucket-refused`. A setup failure (the probe object can't be written, or no version ID is returned) is returned as an error.

- [ ] **Step 1: Write the failing integration tests**

`internal/audit/conformance_integration_test.go`:
```go
//go:build integration

package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/testutil/rustfstest"
)

func TestIntegrationConformancePassesOnLockedBucketWithRootCredentials(t *testing.T) {
	srv := rustfstest.Start(t)
	client, _, bucket := lockedSink(t, srv)
	results, err := audit.RunConformance(context.Background(), client, bucket, 24*time.Hour)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(results) != 7 {
		t.Fatalf("got %d results, want 7", len(results))
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s FAILED: %s", r.Name, r.Detail)
		}
	}
}

func TestIntegrationConformanceRejectsBucketWithoutLock(t *testing.T) {
	srv := rustfstest.Start(t)
	client := srv.RootClient(t)
	bucket := randomBucket(t)
	if _, err := client.CreateBucket(bg, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	_, _ = client.PutBucketVersioning(bg, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	results, err := audit.RunConformance(context.Background(), client, bucket, time.Hour)
	if err == nil {
		for _, r := range results {
			if !r.Passed {
				return // a failed check is also an acceptable outcome
			}
		}
		t.Fatal("conformance passed on a bucket without Object Lock")
	}
}
```

Run: `go test -tags integration ./internal/audit/ -run Conformance -count=1`
Expected: compile error (`undefined: audit.RunConformance`).

- [ ] **Step 2: Implement**

`internal/audit/conformance.go`:
```go
package audit

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// CheckResult is one WORM conformance check.
type CheckResult struct {
	Name   string
	Passed bool
	Detail string
}

// RunConformance checks whether bucket really enforces compliance-mode Object Lock
// (spec §14.4). It writes one probe object under conformance/ that stays locked for
// retention. Run it with each set of credentials you want to evaluate.
func RunConformance(ctx context.Context, client *s3.Client, bucket string, retention time.Duration) ([]CheckResult, error) {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	key := fmt.Sprintf("conformance/%d-%s.json", time.Now().UnixNano(), hex.EncodeToString(suffix))
	original := []byte(`{"conformance":true}`)
	retainUntil := time.Now().Add(retention).UTC()

	put, err := client.PutObject(ctx, putInput(bucket, key, original, true, retainUntil))
	if err != nil {
		return nil, fmt.Errorf("conformance setup: write probe object: %w", err)
	}
	versionID := aws.ToString(put.VersionId)
	if versionID == "" {
		return nil, errors.New("conformance setup: store returned no version ID; versioning/Object Lock is not enabled")
	}

	var results []CheckResult
	refused := func(name string, err error) {
		detail := "allowed (should have been refused)"
		if err != nil {
			detail = "refused: " + errorCode(err)
		}
		results = append(results, CheckResult{Name: name, Passed: err != nil, Detail: detail})
	}
	intact := func(name string) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
		if err != nil {
			results = append(results, CheckResult{Name: name, Detail: "original version unreadable: " + errorCode(err)})
			return
		}
		body, _ := io.ReadAll(out.Body)
		out.Body.Close()
		ok := bytes.Equal(body, original)
		detail := "original version intact"
		if !ok {
			detail = "original version changed"
		}
		results = append(results, CheckResult{Name: name, Passed: ok, Detail: detail})
	}

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	refused("delete-version-refused", err)

	_, err = client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), true, retainUntil))
	refused("conditional-overwrite-refused", err)

	_, _ = client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), false, retainUntil))
	intact("plain-overwrite-preserves-original")

	_, err = client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(time.Now().Add(time.Minute).UTC())},
	})
	refused("shorten-retention-refused", err)

	_, err = client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(retainUntil)},
	})
	refused("switch-to-governance-refused", err)

	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	intact("delete-marker-preserves-original")

	_, err = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	refused("delete-bucket-refused", err)

	return results, nil
}

func putInput(bucket, key string, body []byte, ifNoneMatch bool, retainUntil time.Time) *s3.PutObjectInput {
	sum := md5.Sum(body)
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		ContentLength:             aws.Int64(int64(len(body))),
		ContentMD5:                aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		ObjectLockMode:            types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: aws.Time(retainUntil),
	}
	if ifNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	return in
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return err.Error()
}
```

- [ ] **Step 3: Run the integration tests**

Run: `go test -tags integration ./internal/audit/ -run Conformance -count=1 -v`
Expected: PASS. If any check fails against RustFS with root credentials, **stop and report the check name and detail to the user**. That result means the bundled store doesn't provide the WORM guarantee, which is a design-level finding. Do not change the check.

- [ ] **Step 4: Commit**

```bash
git add internal/audit
git commit -m "feat(audit): add WORM conformance check"
```

### Task 11: RustFS admin client and the audit-init Job binary

**Files:**
- Create: `internal/rustfsadmin/client.go`, `internal/auditinit/init.go`, `internal/auditinit/init_test.go`, `internal/auditinit/init_integration_test.go`, `cmd/audit-init/main.go`

**Interfaces:**
- Consumes: `NewS3Client`, `NewS3Sink`, `EnsureLockedBucket`, `IsAccessDenied`, `S3Config` (Task 9); `rustfstest` (Task 9).
- Produces:
  - `rustfsadmin.New(endpoint, accessKey, secretKey string) *Client`; `(*Client).AddCannedPolicy(ctx, name string, policy []byte) error`; `(*Client).AddUser(ctx, accessKey, secretKey string) error`; `(*Client).SetUserPolicy(ctx, policyName, user string) error`; `*rustfsadmin.APIError{Status int; Body string}`.
  - `auditinit.Config{Endpoint, Bucket string; RetentionDays int32; RootAccessKey, RootSecretKey, WriterAccessKey, WriterSecretKey, ReaderAccessKey, ReaderSecretKey string; ReadyTimeout time.Duration}`; `auditinit.ConfigFromEnv(getenv func(string) string) (Config, error)`; `auditinit.Run(ctx, Config) error`; `auditinit.WriterPolicyDocument(bucket string) []byte`; `auditinit.ReaderPolicyDocument(bucket string) []byte`; constants `WriterPolicy = "last-audit-writer"`, `ReaderPolicy = "last-audit-reader"`.
  - The env var names that the Helm chart (Task 14) must set: `LAST_AUDIT_ENDPOINT`, `LAST_AUDIT_BUCKET`, `LAST_AUDIT_RETENTION_DAYS`, `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY`, `LAST_AUDIT_WRITER_ACCESS_KEY`, `LAST_AUDIT_WRITER_SECRET_KEY`, `LAST_AUDIT_READER_ACCESS_KEY`, `LAST_AUDIT_READER_SECRET_KEY`.

- [ ] **Step 1: Write the failing unit tests**

`internal/auditinit/init_test.go`:
```go
package auditinit_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/moogie-labs/last-operator/internal/auditinit"
)

type policyDoc struct {
	Statement []struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	} `json:"Statement"`
}

func actions(t *testing.T, doc []byte) []string {
	t.Helper()
	var p policyDoc
	if err := json.Unmarshal(doc, &p); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	var out []string
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			t.Fatalf("unexpected effect %q", s.Effect)
		}
		out = append(out, s.Action...)
	}
	sort.Strings(out)
	return out
}

func TestWriterPolicyIsLeastPrivilege(t *testing.T) {
	got := actions(t, auditinit.WriterPolicyDocument("last-audit"))
	want := []string{
		"s3:GetBucketObjectLockConfiguration", "s3:GetObject", "s3:GetObjectVersion",
		"s3:ListBucket", "s3:ListBucketVersions", "s3:PutObject", "s3:PutObjectRetention",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("writer actions = %v, want %v", got, want)
	}
}

func TestReaderPolicyCannotWrite(t *testing.T) {
	for _, a := range actions(t, auditinit.ReaderPolicyDocument("last-audit")) {
		if strings.HasPrefix(a, "s3:Put") || strings.HasPrefix(a, "s3:Delete") {
			t.Fatalf("reader policy grants %s", a)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"LAST_AUDIT_ENDPOINT": "http://last-rustfs.last-audit.svc:9000", "LAST_AUDIT_BUCKET": "last-audit",
		"RUSTFS_ACCESS_KEY": "root", "RUSTFS_SECRET_KEY": "rootsecret",
		"LAST_AUDIT_WRITER_ACCESS_KEY": "w", "LAST_AUDIT_WRITER_SECRET_KEY": "wsecret",
		"LAST_AUDIT_READER_ACCESS_KEY": "r", "LAST_AUDIT_READER_SECRET_KEY": "rsecret",
	}
	cfg, err := auditinit.ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetentionDays != 365 || cfg.ReadyTimeout == 0 || cfg.WriterAccessKey != "w" {
		t.Fatalf("cfg = %+v", cfg)
	}
	delete(env, "LAST_AUDIT_WRITER_SECRET_KEY")
	if _, err := auditinit.ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "LAST_AUDIT_WRITER_SECRET_KEY") {
		t.Fatalf("missing var error = %v", err)
	}
}
```

Run: `go test ./internal/auditinit/ -count=1`
Expected: compile error (package not found).

- [ ] **Step 2: Implement the admin client**

`internal/rustfsadmin/client.go`:
```go
// Package rustfsadmin is a minimal client for RustFS's native admin API
// (/rustfs/admin/v3/*), which takes plain JSON bodies signed with SigV4.
package rustfsadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// APIError is a non-2xx admin API response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("rustfs admin: HTTP %d: %s", e.Status, e.Body) }

type Client struct {
	endpoint string
	creds    aws.Credentials
	signer   *v4.Signer
	http     *http.Client
}

func New(endpoint, accessKey, secretKey string) *Client {
	return &Client{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		creds:    aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		signer:   v4.NewSigner(),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// AddCannedPolicy creates or replaces a named IAM policy.
func (c *Client) AddCannedPolicy(ctx context.Context, name string, policy []byte) error {
	return c.do(ctx, "add-canned-policy", url.Values{"name": {name}}, policy)
}

// AddUser creates or updates an enabled user.
func (c *Client) AddUser(ctx context.Context, accessKey, secretKey string) error {
	body, err := json.Marshal(map[string]string{"secretKey": secretKey, "status": "enabled"})
	if err != nil {
		return err
	}
	return c.do(ctx, "add-user", url.Values{"accessKey": {accessKey}}, body)
}

// SetUserPolicy attaches a named policy to a user.
func (c *Client) SetUserPolicy(ctx context.Context, policyName, user string) error {
	return c.do(ctx, "set-user-or-group-policy",
		url.Values{"policyName": {policyName}, "userOrGroup": {user}, "isGroup": {"false"}}, nil)
}

func (c *Client) do(ctx context.Context, op string, query url.Values, body []byte) error {
	u := c.endpoint + "/rustfs/admin/v3/" + op + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := c.signer.SignHTTP(ctx, c.creds, req, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
		return fmt.Errorf("rustfs admin: sign: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rustfs admin: %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	return nil
}
```

- [ ] **Step 3: Implement provisioning**

`internal/auditinit/init.go`:
```go
// Package auditinit provisions the bundled RustFS audit store: an Object Lock
// bucket plus least-privilege writer and reader users (spec §14.4).
package auditinit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/rustfsadmin"
)

const (
	WriterPolicy = "last-audit-writer"
	ReaderPolicy = "last-audit-reader"
)

type Config struct {
	Endpoint                         string
	Bucket                           string
	RetentionDays                    int32
	RootAccessKey, RootSecretKey     string
	WriterAccessKey, WriterSecretKey string
	ReaderAccessKey, ReaderSecretKey string
	ReadyTimeout                     time.Duration
}

func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{ReadyTimeout: 5 * time.Minute, RetentionDays: 365}
	required := map[string]*string{
		"LAST_AUDIT_ENDPOINT":          &cfg.Endpoint,
		"LAST_AUDIT_BUCKET":            &cfg.Bucket,
		"RUSTFS_ACCESS_KEY":            &cfg.RootAccessKey,
		"RUSTFS_SECRET_KEY":            &cfg.RootSecretKey,
		"LAST_AUDIT_WRITER_ACCESS_KEY": &cfg.WriterAccessKey,
		"LAST_AUDIT_WRITER_SECRET_KEY": &cfg.WriterSecretKey,
		"LAST_AUDIT_READER_ACCESS_KEY": &cfg.ReaderAccessKey,
		"LAST_AUDIT_READER_SECRET_KEY": &cfg.ReaderSecretKey,
	}
	for name, dst := range required {
		if *dst = getenv(name); *dst == "" {
			return Config{}, fmt.Errorf("auditinit: %s is required", name)
		}
	}
	if v := getenv("LAST_AUDIT_RETENTION_DAYS"); v != "" {
		days, err := strconv.ParseInt(v, 10, 32)
		if err != nil || days < 1 {
			return Config{}, fmt.Errorf("auditinit: LAST_AUDIT_RETENTION_DAYS=%q is not a positive integer", v)
		}
		cfg.RetentionDays = int32(days)
	}
	return cfg, nil
}

func policy(bucket string, actions []string) []byte {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":   "Allow",
			"Action":   actions,
			"Resource": []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"},
		}},
	}
	b, _ := json.Marshal(doc)
	return b
}

// WriterPolicyDocument lets the gateway append and read, never delete or relax locks.
// PutObjectRetention is needed to set retention on each write; compliance mode
// still refuses any shortening.
func WriterPolicyDocument(bucket string) []byte {
	return policy(bucket, []string{
		"s3:PutObject", "s3:PutObjectRetention", "s3:GetObject", "s3:GetObjectVersion",
		"s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketObjectLockConfiguration",
	})
}

// ReaderPolicyDocument is for `kubectl last audit`.
func ReaderPolicyDocument(bucket string) []byte {
	return policy(bucket, []string{
		"s3:GetObject", "s3:GetObjectVersion", "s3:ListBucket", "s3:ListBucketVersions",
		"s3:GetBucketObjectLockConfiguration",
	})
}

// Run is idempotent: safe to execute on every install and upgrade.
func Run(ctx context.Context, cfg Config) error {
	if err := waitReady(ctx, cfg.Endpoint, cfg.ReadyTimeout); err != nil {
		return err
	}
	client, err := audit.NewS3Client(ctx, audit.S3Config{
		Endpoint: cfg.Endpoint, AccessKey: cfg.RootAccessKey, SecretKey: cfg.RootSecretKey, PathStyle: true,
	})
	if err != nil {
		return err
	}
	if err := audit.EnsureLockedBucket(ctx, client, cfg.Bucket, cfg.RetentionDays); err != nil {
		return err
	}
	admin := rustfsadmin.New(cfg.Endpoint, cfg.RootAccessKey, cfg.RootSecretKey)
	users := []struct{ policy, ak, sk string; doc []byte }{
		{WriterPolicy, cfg.WriterAccessKey, cfg.WriterSecretKey, WriterPolicyDocument(cfg.Bucket)},
		{ReaderPolicy, cfg.ReaderAccessKey, cfg.ReaderSecretKey, ReaderPolicyDocument(cfg.Bucket)},
	}
	for _, u := range users {
		if err := admin.AddCannedPolicy(ctx, u.policy, u.doc); err != nil {
			return fmt.Errorf("auditinit: policy %s: %w", u.policy, err)
		}
		if err := admin.AddUser(ctx, u.ak, u.sk); err != nil {
			return fmt.Errorf("auditinit: user %s: %w", u.ak, err)
		}
		if err := admin.SetUserPolicy(ctx, u.policy, u.ak); err != nil {
			return fmt.Errorf("auditinit: attach %s to %s: %w", u.policy, u.ak, err)
		}
	}
	return nil
}

func waitReady(ctx context.Context, endpoint string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health/ready", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("auditinit: %s not ready after %s", endpoint, timeout)
		case <-time.After(2 * time.Second):
		}
	}
}
```

`cmd/audit-init/main.go`:
```go
// Command audit-init provisions the bundled RustFS audit store (Helm Job).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/moogie-labs/last-operator/internal/auditinit"
)

func main() {
	cfg, err := auditinit.ConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if err := auditinit.Run(context.Background(), cfg); err != nil {
		slog.Error("audit store provisioning failed", "err", err)
		os.Exit(1)
	}
	slog.Info("audit store ready", "bucket", cfg.Bucket, "retentionDays", cfg.RetentionDays)
}
```

Run: `go test ./internal/auditinit/ -count=1 -v && go build ./cmd/audit-init`
Expected: PASS and a successful build.

- [ ] **Step 4: Write the least-privilege integration test**

`internal/auditinit/init_integration_test.go`:
```go
//go:build integration

package auditinit_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/auditinit"
	"github.com/moogie-labs/last-operator/internal/testutil/rustfstest"
)

func expectDenied(t *testing.T, what string, err error) {
	t.Helper()
	if !audit.IsAccessDenied(err) {
		t.Fatalf("%s: want AccessDenied, got %v", what, err)
	}
}

func TestIntegrationInitProvisionsLeastPrivilegeUsers(t *testing.T) {
	ctx := context.Background()
	srv := rustfstest.Start(t)
	cfg := auditinit.Config{
		Endpoint: srv.Endpoint, Bucket: "last-audit", RetentionDays: 1,
		RootAccessKey: rustfstest.RootAccessKey, RootSecretKey: rustfstest.RootSecretKey,
		WriterAccessKey: "writer", WriterSecretKey: "writer-secret-0123456789",
		ReaderAccessKey: "reader", ReaderSecretKey: "reader-secret-0123456789",
		ReadyTimeout: time.Minute,
	}
	if err := auditinit.Run(ctx, cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := auditinit.Run(ctx, cfg); err != nil {
		t.Fatalf("second run (must be idempotent): %v", err)
	}

	writer := srv.Client(t, cfg.WriterAccessKey, cfg.WriterSecretKey)
	wsink := audit.NewS3Sink(writer, cfg.Bucket)
	key := audit.EntryKey("c", 0)
	if err := wsink.Put(ctx, key, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writer put: %v", err)
	}
	objs, err := wsink.List(ctx, audit.EntryPrefix("c"), "")
	if err != nil || len(objs) != 1 {
		t.Fatalf("writer list: %+v %v", objs, err)
	}
	if _, err := wsink.LockConfig(ctx); err != nil {
		t.Fatalf("writer lock config: %v", err)
	}
	_, err = writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key), VersionId: aws.String(objs[0].VersionIDs[0])})
	expectDenied(t, "writer delete version", err)
	_, err = writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key)})
	expectDenied(t, "writer delete (marker)", err)

	reader := srv.Client(t, cfg.ReaderAccessKey, cfg.ReaderSecretKey)
	rsink := audit.NewS3Sink(reader, cfg.Bucket)
	if _, err := rsink.Get(ctx, key, ""); err != nil {
		t.Fatalf("reader get: %v", err)
	}
	expectDenied(t, "reader put", rsink.Put(ctx, audit.EntryKey("c", 1), []byte(`{}`), time.Now().Add(time.Hour)))
}
```

- [ ] **Step 5: Run the integration test**

Run: `go test -tags integration ./internal/auditinit/ -count=1 -v`
Expected: PASS.
If an admin call returns an `APIError`, compare the path and query names against the RustFS source (`rustfs/src/admin/handlers/{user,policies}.rs`) and fix the client. If RustFS doesn't enforce the policies (for example, the writer is allowed to delete), **stop and report to the user**. Per spec §14.4, compliance mode still protects integrity in that case, but the restricted-user layer of defense would be missing.

- [ ] **Step 6: Commit**

```bash
git add internal/rustfsadmin internal/auditinit cmd/audit-init
git commit -m "feat(audit): provision RustFS bucket and least-privilege users"
```

### Task 12: Gateway — config, signing key, leader-elected audit writer, health and metrics

**Files:**
- Create: `internal/gateway/config.go`, `internal/gateway/config_test.go`, `internal/gateway/signingkey.go`, `internal/gateway/signingkey_test.go`, `internal/gateway/metrics.go`, `internal/gateway/server.go`, `internal/gateway/server_test.go`, `cmd/gateway/main.go`

**Interfaces:**
- Consumes: `audit.S3Config`, `NewS3Client`, `NewS3Sink`, `CheckLockConfig`, `Open`, `WriterConfig`, `Writer`, `Signer`, PEM helpers, `Fingerprint` (Tasks 5–9); `audittest.MemSink` (Task 6).
- Produces:
  - `gateway.Config{Namespace, PodName string; Audit audit.S3Config; ChainID string; Retention time.Duration; SigningKeySecret, PublicKeyConfigMap, LeaseName, HTTPAddr string}` and `gateway.ConfigFromEnv(getenv func(string) string) (Config, error)`. Env vars: `POD_NAMESPACE`, `POD_NAME`, `LAST_AUDIT_ENDPOINT`, `LAST_AUDIT_REGION`, `LAST_AUDIT_BUCKET`, `LAST_AUDIT_ACCESS_KEY`, `LAST_AUDIT_SECRET_KEY`, `LAST_AUDIT_PATH_STYLE` (default `true`), `LAST_AUDIT_CHAIN_ID`, `LAST_AUDIT_RETENTION` (default `8760h`), `LAST_HTTP_ADDR` (default `:8080`).
  - `gateway.EnsureSigningKey(ctx, kubernetes.Interface, ns, secretName, configMapName string) (ed25519.PrivateKey, error)`. The Secret `last-audit-signing` holds key `ed25519.pem`; the ConfigMap `last-audit-pubkey` holds keys `ed25519.pub` and `fingerprint`.
  - `gateway.New(cfg Config, kube kubernetes.Interface, sink audit.Sink, signer *audit.Signer) *Gateway`, `(*Gateway).Run(ctx) error`, `(*Gateway).Handler() http.Handler` (`/healthz`, `/readyz`, `/metrics`). Metrics: `last_audit_writes_total{type}`, `last_audit_write_failures_total`, `last_audit_chain_seq`.
  - Lease name `last-gateway-audit-writer`. The Helm RBAC in Task 14 must allow exactly these objects.

- [ ] **Step 1: Write the failing config and signing-key tests**

`internal/gateway/config_test.go`:
```go
package gateway

import (
	"strings"
	"testing"
	"time"
)

func baseEnv() map[string]string {
	return map[string]string{
		"POD_NAMESPACE": "last-gateway", "POD_NAME": "gw-0",
		"LAST_AUDIT_ENDPOINT": "http://last-rustfs.last-audit.svc:9000",
		"LAST_AUDIT_BUCKET":   "last-audit", "LAST_AUDIT_CHAIN_ID": "chain-1",
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	env := baseEnv()
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention != 8760*time.Hour || !cfg.Audit.PathStyle || cfg.Audit.Region != "us-east-1" ||
		cfg.LeaseName != "last-gateway-audit-writer" || cfg.HTTPAddr != ":8080" ||
		cfg.SigningKeySecret != "last-audit-signing" || cfg.PublicKeyConfigMap != "last-audit-pubkey" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigFromEnvRequiresChainAndBucket(t *testing.T) {
	for _, missing := range []string{"LAST_AUDIT_CHAIN_ID", "LAST_AUDIT_BUCKET", "POD_NAMESPACE", "POD_NAME"} {
		env := baseEnv()
		delete(env, missing)
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: err = %v", missing, err)
		}
	}
}

func TestConfigFromEnvRejectsBadValues(t *testing.T) {
	env := baseEnv()
	env["LAST_AUDIT_RETENTION"] = "forever"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("accepted retention=forever")
	}
	env = baseEnv()
	env["LAST_AUDIT_PATH_STYLE"] = "maybe"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("accepted path style=maybe")
	}
}
```

`internal/gateway/signingkey_test.go`:
```go
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/audit"
)

const ns = "last-gateway"

func TestEnsureSigningKeyCreatesThenReuses(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	k1, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("second call returned a different key (err=%v)", err)
	}
	sec, _ := cs.CoreV1().Secrets(ns).Get(ctx, "last-audit-signing", metav1.GetOptions{})
	if sec.Immutable == nil || !*sec.Immutable {
		t.Fatal("signing key Secret must be immutable")
	}
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	pub, err := audit.ParsePublicKeyPEM([]byte(cm.Data["ed25519.pub"]))
	if err != nil || !pub.Equal(k1.Public()) {
		t.Fatalf("published key does not match (err=%v)", err)
	}
	if cm.Data["fingerprint"] != audit.Fingerprint(pub) {
		t.Fatal("fingerprint mismatch")
	}
}

func TestEnsureSigningKeyRepairsPublishedKey(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	key, _ := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPEM, _ := audit.MarshalPublicKeyPEM(other)
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	cm.Data["ed25519.pub"] = string(otherPEM)
	_, _ = cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})

	if _, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey"); err != nil {
		t.Fatal(err)
	}
	cm, _ = cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	pub, _ := audit.ParsePublicKeyPEM([]byte(cm.Data["ed25519.pub"]))
	if !pub.Equal(key.Public()) {
		t.Fatal("published key was not repaired to match the signing key")
	}
}
```

Run: `go test ./internal/gateway/ -count=1`
Expected: compile errors (package missing).

- [ ] **Step 2: Implement config and signing key**

`internal/gateway/config.go`:
```go
// Package gateway is the trusted component that holds credentials and writes the audit log.
package gateway

import (
	"fmt"
	"strconv"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type Config struct {
	Namespace          string
	PodName            string
	Audit              audit.S3Config
	ChainID            string
	Retention          time.Duration
	SigningKeySecret   string
	PublicKeyConfigMap string
	LeaseName          string
	HTTPAddr           string
}

func ConfigFromEnv(getenv func(string) string) (Config, error) {
	or := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	cfg := Config{
		Namespace: getenv("POD_NAMESPACE"),
		PodName:   getenv("POD_NAME"),
		ChainID:   getenv("LAST_AUDIT_CHAIN_ID"),
		Audit: audit.S3Config{
			Endpoint:  getenv("LAST_AUDIT_ENDPOINT"),
			Region:    or("LAST_AUDIT_REGION", "us-east-1"),
			Bucket:    getenv("LAST_AUDIT_BUCKET"),
			AccessKey: getenv("LAST_AUDIT_ACCESS_KEY"),
			SecretKey: getenv("LAST_AUDIT_SECRET_KEY"),
		},
		SigningKeySecret:   "last-audit-signing",
		PublicKeyConfigMap: "last-audit-pubkey",
		LeaseName:          "last-gateway-audit-writer",
		HTTPAddr:           or("LAST_HTTP_ADDR", ":8080"),
	}
	for name, v := range map[string]string{
		"POD_NAMESPACE": cfg.Namespace, "POD_NAME": cfg.PodName,
		"LAST_AUDIT_CHAIN_ID": cfg.ChainID, "LAST_AUDIT_BUCKET": cfg.Audit.Bucket,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("gateway: %s is required", name)
		}
	}
	pathStyle, err := strconv.ParseBool(or("LAST_AUDIT_PATH_STYLE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("gateway: LAST_AUDIT_PATH_STYLE: %w", err)
	}
	cfg.Audit.PathStyle = pathStyle
	cfg.Retention, err = time.ParseDuration(or("LAST_AUDIT_RETENTION", "8760h"))
	if err != nil || cfg.Retention <= 0 {
		return Config{}, fmt.Errorf("gateway: LAST_AUDIT_RETENTION must be a positive duration: %v", err)
	}
	return cfg, nil
}
```

`internal/gateway/signingkey.go`:
```go
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// EnsureSigningKey loads the audit signing key from its Secret, creating it on first
// start, and publishes the public key to a ConfigMap for verifiers.
func EnsureSigningKey(ctx context.Context, cs kubernetes.Interface, ns, secretName, cmName string) (ed25519.PrivateKey, error) {
	key, err := loadOrCreateKey(ctx, cs, ns, secretName)
	if err != nil {
		return nil, err
	}
	pub := key.Public().(ed25519.PublicKey)
	pubPEM, err := audit.MarshalPublicKeyPEM(pub)
	if err != nil {
		return nil, err
	}
	want := map[string]string{"ed25519.pub": string(pubPEM), "fingerprint": audit.Fingerprint(pub)}
	cms := cs.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, cmName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}, Data: want}, metav1.CreateOptions{})
	case err == nil && (cm.Data["ed25519.pub"] != want["ed25519.pub"] || cm.Data["fingerprint"] != want["fingerprint"]):
		cm.Data = want
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: publish public key: %w", err)
	}
	return key, nil
}

func loadOrCreateKey(ctx context.Context, cs kubernetes.Interface, ns, name string) (ed25519.PrivateKey, error) {
	secrets := cs.CoreV1().Secrets(ns)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return audit.ParsePrivateKeyPEM(sec.Data["ed25519.pem"])
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("gateway: read signing key: %w", err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pemBytes, err := audit.MarshalPrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	immutable := true
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Immutable:  &immutable,
		Data:       map[string][]byte{"ed25519.pem": pemBytes},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return loadOrCreateKey(ctx, cs, ns, name) // lost a creation race; use the winner's key
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: create signing key: %w", err)
	}
	return key, nil
}
```

Run: `go test ./internal/gateway/ -count=1 -v`
Expected: PASS (5 tests).

- [ ] **Step 3: Write the failing server tests**

`internal/gateway/server_test.go`:
```go
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

func testGateway(t *testing.T, sink audit.Sink) *Gateway {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Namespace: ns, PodName: "gw-0", ChainID: "chain-1", Retention: 365 * 24 * time.Hour, HTTPAddr: ":0", LeaseName: "l"}
	return New(cfg, fake.NewClientset(), sink, audit.NewSigner(key))
}

func get(t *testing.T, g *Gateway, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestLeadRefusesBucketWithoutComplianceLock(t *testing.T) {
	sink := audittest.NewMemSink()
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: "GOVERNANCE", DefaultRetention: 365 * 24 * time.Hour})
	err := testGateway(t, sink).lead(context.Background())
	if err == nil || !strings.Contains(err.Error(), "COMPLIANCE") {
		t.Fatalf("lead = %v, want refusal mentioning COMPLIANCE", err)
	}
	if len(sink.Keys()) != 0 {
		t.Fatalf("wrote to a non-compliant bucket: %v", sink.Keys())
	}
}

func TestLeadOpensChainAndReportsReady(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink)
	if code, _ := get(t, g, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before leading = %d, want 503", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := get(t, g, "/readyz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, err := sink.Get(context.Background(), audit.EntryKey("chain-1", 0), "")
	if err != nil {
		t.Fatalf("no gateway.start entry: %v", err)
	}
	var e audit.Entry
	_ = json.Unmarshal(body, &e)
	if e.Type != audit.TypeGatewayStart {
		t.Fatalf("first entry type = %s", e.Type)
	}
	if _, metrics := get(t, g, "/metrics"); !strings.Contains(metrics, "last_audit_chain_seq 0") ||
		!strings.Contains(metrics, `last_audit_writes_total{type="gateway.start"} 1`) {
		t.Fatalf("metrics missing audit series:\n%s", metrics)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead returned %v after cancel", err)
	}
	if code, _ := get(t, g, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after stepping down = %d, want 503", code)
	}
}
```

Run: `go test ./internal/gateway/ -count=1 -run Lead`
Expected: compile errors (`undefined: New`).

- [ ] **Step 4: Implement metrics and the server**

`internal/gateway/metrics.go`:
```go
package gateway

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type Metrics struct {
	registry *prometheus.Registry
	writes   *prometheus.CounterVec
	failures prometheus.Counter
	seq      prometheus.Gauge
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_audit_writes_total", Help: "Audit entries written, by type.",
		}, []string{"type"}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "last_audit_write_failures_total", Help: "Audit writes that failed and broke the writer.",
		}),
		seq: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "last_audit_chain_seq", Help: "Sequence number of the last audit entry written.",
		}),
	}
	m.registry.MustRegister(m.writes, m.failures, m.seq)
	return m
}

func (m *Metrics) ObserveWrite(e audit.Entry) {
	m.writes.WithLabelValues(e.Type).Inc()
	m.seq.Set(float64(e.Seq))
}

func (m *Metrics) ObserveFailure(error) { m.failures.Inc() }
```

`internal/gateway/server.go`:
```go
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/moogie-labs/last-operator/internal/audit"
)

var errLostLeadership = errors.New("gateway: lost audit-writer leadership")

// Version is set at build time with -ldflags.
var Version = "dev"

type Gateway struct {
	cfg     Config
	kube    kubernetes.Interface
	sink    audit.Sink
	signer  *audit.Signer
	metrics *Metrics
	writer  atomic.Pointer[audit.Writer]
}

func New(cfg Config, kube kubernetes.Interface, sink audit.Sink, signer *audit.Signer) *Gateway {
	return &Gateway{cfg: cfg, kube: kube, sink: sink, signer: signer, metrics: NewMetrics()}
}

// Handler serves liveness, readiness (leader with a healthy writer), and metrics.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if wr := g.writer.Load(); wr == nil || wr.Broken() {
			http.Error(w, "audit writer not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(g.metrics.registry, promhttp.HandlerOpts{}))
	return mux
}

// Run serves HTTP and competes for the audit-writer lease. It returns an error
// (so the process exits and restarts) if leading fails or leadership is lost.
func (g *Gateway) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	srv := &http.Server{Addr: g.cfg.HTTPAddr, Handler: g.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("gateway: http server: %w", err))
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: g.cfg.LeaseName, Namespace: g.cfg.Namespace},
		Client:     g.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: g.cfg.PodName},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				if err := g.lead(lctx); err != nil {
					cancel(err)
				}
			},
			OnStoppedLeading: func() { cancel(errLostLeadership) },
		},
	})
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// lead checks the bucket's lock, opens the chain (verifying it), and writes
// checkpoints until ctx ends. It refuses to write to a non-compliant bucket.
func (g *Gateway) lead(ctx context.Context) error {
	lc, err := g.sink.LockConfig(ctx)
	if err != nil {
		return fmt.Errorf("gateway: read bucket lock config: %w", err)
	}
	if err := audit.CheckLockConfig(lc, g.cfg.Retention); err != nil {
		return err
	}
	w, err := audit.Open(ctx, g.sink, g.signer, audit.WriterConfig{
		ChainID: g.cfg.ChainID, Retention: g.cfg.Retention, Actor: "gateway",
	}, map[string]string{
		"pod": g.cfg.PodName, "version": Version, "keyFingerprint": audit.Fingerprint(g.signer.Public()),
	})
	if err != nil {
		return err
	}
	// Metrics hooks are set after Open, so count gateway.start explicitly.
	if seq, _, ok := w.Head(); ok {
		g.metrics.ObserveWrite(audit.Entry{Seq: seq, Type: audit.TypeGatewayStart})
	}
	w.SetOnWrite(g.metrics.ObserveWrite)
	w.SetOnFailure(g.metrics.ObserveFailure)
	g.writer.Store(w)
	defer g.writer.Store(nil)
	slog.Info("audit writer ready", "chain", g.cfg.ChainID, "pod", g.cfg.PodName)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.CheckpointIfDue(ctx); err != nil {
				return fmt.Errorf("gateway: checkpoint: %w", err)
			}
		}
	}
}
```

`cmd/gateway/main.go`:
```go
// Command gateway is the trusted last-operator component that holds credentials
// and writes the write-once audit log.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := gateway.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	key, err := gateway.EnsureSigningKey(ctx, kube, cfg.Namespace, cfg.SigningKeySecret, cfg.PublicKeyConfigMap)
	if err != nil {
		return err
	}
	client, err := audit.NewS3Client(ctx, cfg.Audit)
	if err != nil {
		return err
	}
	g := gateway.New(cfg, kube, audit.NewS3Sink(client, cfg.Audit.Bucket), audit.NewSigner(key))
	return g.Run(ctx)
}
```

- [ ] **Step 5: Run the gateway tests and build**

Run: `go test ./internal/gateway/ -count=1 -v && go build ./cmd/gateway`
Expected: PASS (7 tests) and a successful build.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway cmd/gateway go.mod go.sum
git commit -m "feat(gateway): leader-elected audit writer with signing key bootstrap and health"
```

### Task 13: `kubectl last audit verify | log | conformance`

**Files:**
- Create: `internal/cli/root.go`, `internal/cli/audit.go`, `internal/cli/audit_test.go`, `cmd/kubectl-last/main.go`

**Interfaces:**
- Consumes: `audit.Verify`, `VerifyOptions`, `Report`, `ParsePublicKeyPEM`, `S3Config`, `NewS3Client`, `NewS3Sink`, `RunConformance`, `EntryPrefix`, `Entry`, `Open`, `WriterConfig`, `Draft` (Tasks 5–10); `audittest.MemSink` (Task 6).
- Produces: `cli.Deps{Out, Err io.Writer; Getenv func(string) string; NewSink func(context.Context, audit.S3Config) (audit.Sink, error); NewS3Client func(context.Context, audit.S3Config) (*s3.Client, error)}`, `cli.DefaultDeps() Deps`, `cli.NewRootCommand(Deps) *cobra.Command`, `cli.ErrVerifyFailed`.
  - Flags on `audit`: `--endpoint` (env `LAST_AUDIT_ENDPOINT`), `--bucket` (env `LAST_AUDIT_BUCKET`, default `last-audit`), `--region` (env `LAST_AUDIT_REGION`, default `us-east-1`), `--chain` (env `LAST_AUDIT_CHAIN_ID`), `--path-style` (default `true`). Credentials come from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`.
  - `verify --pubkey-file F [--from-checkpoint N] [--expect-head N]` prints `OK: verified <n> entries (seq <a>..<b>), head <hash>` on success.
  - `log [--intent ns/name] [--follow] [--interval 2s]`.
  - `conformance [--retention 24h]` uses primary credentials from `AWS_*`, plus optional admin credentials from `LAST_ADMIN_ACCESS_KEY` / `LAST_ADMIN_SECRET_KEY`.

- [ ] **Step 1: Add cobra**

Run: `go get github.com/spf13/cobra@latest`

- [ ] **Step 2: Write the failing CLI tests**

`internal/cli/audit_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

func testDeps(sink audit.Sink, out *bytes.Buffer) Deps {
	return Deps{
		Out: out, Err: out,
		Getenv:  func(string) string { return "" },
		NewSink: func(context.Context, audit.S3Config) (audit.Sink, error) { return sink, nil },
	}
}

func execute(d Deps, args ...string) error {
	cmd := NewRootCommand(d)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(context.Background())
}

// writeChain creates chain "c1": gateway.start + 3 command entries across two intents.
func writeChain(t *testing.T) (*audittest.MemSink, string) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.NewSigner(key)
	sink := audittest.NewMemSink()
	w, err := audit.Open(context.Background(), sink, signer, audit.WriterConfig{ChainID: "c1", Retention: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"team-x/web", "team-y/db", "team-x/web"} {
		if _, err := w.Append(context.Background(), audit.Draft{Type: audit.TypeCommandStart, Actor: "operator", Intent: intent, Stage: "Create"}); err != nil {
			t.Fatal(err)
		}
	}
	pubPEM, _ := audit.MarshalPublicKeyPEM(signer.Public())
	pubFile := filepath.Join(t.TempDir(), "pub.pem")
	if err := os.WriteFile(pubFile, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return sink, pubFile
}

func TestAuditVerifyOK(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "OK: verified 4 entries (seq 0..3)") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestAuditVerifyReportsFirstBadSeq(t *testing.T) {
	sink, pub := writeChain(t)
	sink.TamperDelete(audit.EntryKey("c1", 2))
	var out bytes.Buffer
	err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub)
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("err = %v, want ErrVerifyFailed", err)
	}
	if !strings.Contains(out.String(), "first problem at seq 2 (gap)") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestAuditVerifyExpectHead(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub, "--expect-head", "50")
	if !errors.Is(err, ErrVerifyFailed) || !strings.Contains(out.String(), "truncated") {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
}

func TestAuditVerifyRequiresChain(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "verify", "--pubkey-file", pub); err == nil || !strings.Contains(err.Error(), "--chain") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuditLogFiltersByIntent(t *testing.T) {
	sink, _ := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "log", "--chain", "c1", "--intent", "team-x/web"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, "team-x/web") != 2 || strings.Contains(s, "team-y/db") || strings.Contains(s, "gateway.start") {
		t.Fatalf("output = %s", s)
	}
}
```

Run: `go test ./internal/cli/ -count=1`
Expected: compile errors (package missing).

- [ ] **Step 3: Implement**

`internal/cli/root.go`:
```go
// Package cli implements the kubectl-last plugin.
package cli

import (
	"context"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// Deps are the CLI's external dependencies, injectable for tests.
type Deps struct {
	Out, Err    io.Writer
	Getenv      func(string) string
	NewSink     func(context.Context, audit.S3Config) (audit.Sink, error)
	NewS3Client func(context.Context, audit.S3Config) (*s3.Client, error)
}

func DefaultDeps() Deps {
	return Deps{
		Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv,
		NewS3Client: audit.NewS3Client,
		NewSink: func(ctx context.Context, cfg audit.S3Config) (audit.Sink, error) {
			c, err := audit.NewS3Client(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return audit.NewS3Sink(c, cfg.Bucket), nil
		},
	}
}

func NewRootCommand(d Deps) *cobra.Command {
	root := &cobra.Command{
		Use:           "kubectl-last",
		Short:         "Work with last-operator",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	root.AddCommand(newAuditCommand(d))
	return root
}
```

`internal/cli/audit.go`:
```go
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/moogie-labs/last-operator/internal/audit"
)

var ErrVerifyFailed = errors.New("audit chain failed verification")

type auditFlags struct {
	endpoint, bucket, region, chain string
	pathStyle                       bool
}

func envOr(d Deps, key, def string) string {
	if v := d.Getenv(key); v != "" {
		return v
	}
	return def
}

func (f *auditFlags) s3Config(d Deps) audit.S3Config {
	return audit.S3Config{
		Endpoint: f.endpoint, Region: f.region, Bucket: f.bucket, PathStyle: f.pathStyle,
		AccessKey: d.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: d.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
}

func (f *auditFlags) requireChain() error {
	if f.chain == "" {
		return errors.New("--chain is required (or set LAST_AUDIT_CHAIN_ID; see ConfigMap last-gateway/last-audit-config)")
	}
	return nil
}

func newAuditCommand(d Deps) *cobra.Command {
	f := &auditFlags{}
	cmd := &cobra.Command{Use: "audit", Short: "Read and verify the write-once audit log"}
	pf := cmd.PersistentFlags()
	pf.StringVar(&f.endpoint, "endpoint", d.Getenv("LAST_AUDIT_ENDPOINT"), "S3 endpoint of the audit store (env LAST_AUDIT_ENDPOINT)")
	pf.StringVar(&f.bucket, "bucket", envOr(d, "LAST_AUDIT_BUCKET", "last-audit"), "audit bucket (env LAST_AUDIT_BUCKET)")
	pf.StringVar(&f.region, "region", envOr(d, "LAST_AUDIT_REGION", "us-east-1"), "S3 region (env LAST_AUDIT_REGION)")
	pf.StringVar(&f.chain, "chain", d.Getenv("LAST_AUDIT_CHAIN_ID"), "chain ID (env LAST_AUDIT_CHAIN_ID)")
	pf.BoolVar(&f.pathStyle, "path-style", true, "use path-style S3 addressing (required for RustFS)")
	cmd.AddCommand(newVerifyCommand(d, f), newLogCommand(d, f), newConformanceCommand(d, f))
	return cmd
}

func newVerifyCommand(d Deps, f *auditFlags) *cobra.Command {
	var pubkeyFile string
	var fromSeq uint64
	var expectHead int64
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify signatures, hash links, and sequence continuity of the audit chain",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.requireChain(); err != nil {
				return err
			}
			pemBytes, err := os.ReadFile(pubkeyFile)
			if err != nil {
				return err
			}
			pub, err := audit.ParsePublicKeyPEM(pemBytes)
			if err != nil {
				return err
			}
			sink, err := d.NewSink(cmd.Context(), f.s3Config(d))
			if err != nil {
				return err
			}
			opts := audit.VerifyOptions{FromSeq: fromSeq}
			if expectHead >= 0 {
				h := uint64(expectHead)
				opts.ExpectHeadAtLeast = &h
			}
			rep, err := audit.Verify(cmd.Context(), sink, pub, f.chain, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if first, bad := rep.FirstBad(); bad {
				fmt.Fprintf(out, "FAILED: first problem at seq %d (%s): %s\n", first.Seq, first.Kind, first.Detail)
				for _, p := range rep.Problems {
					fmt.Fprintf(out, "  seq %-8d %-15s %s\n", p.Seq, p.Kind, p.Detail)
				}
				return ErrVerifyFailed
			}
			if rep.Head == nil {
				fmt.Fprintln(out, "OK: chain is empty")
				return nil
			}
			fmt.Fprintf(out, "OK: verified %d entries (seq %d..%d), head %s\n", rep.Entries, rep.First.Seq, rep.Head.Seq, rep.Head.Hash)
			return nil
		},
	}
	cmd.Flags().StringVar(&pubkeyFile, "pubkey-file", "", "PEM Ed25519 public key (ConfigMap last-gateway/last-audit-pubkey, key ed25519.pub)")
	_ = cmd.MarkFlagRequired("pubkey-file")
	cmd.Flags().Uint64Var(&fromSeq, "from-checkpoint", 0, "start verification at this seq (a checkpoint)")
	cmd.Flags().Int64Var(&expectHead, "expect-head", -1, "fail if the chain ends before this seq")
	return cmd
}

func newLogCommand(d Deps, f *auditFlags) *cobra.Command {
	var intent string
	var follow bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Print audit entries, optionally for one Intent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.requireChain(); err != nil {
				return err
			}
			sink, err := d.NewSink(cmd.Context(), f.s3Config(d))
			if err != nil {
				return err
			}
			after := ""
			for {
				objs, err := sink.List(cmd.Context(), audit.EntryPrefix(f.chain), after)
				if err != nil {
					return err
				}
				for _, o := range objs {
					after = o.Key
					if len(o.VersionIDs) == 0 {
						continue
					}
					body, err := sink.Get(cmd.Context(), o.Key, o.VersionIDs[0])
					if err != nil {
						return err
					}
					var e audit.Entry
					if err := json.Unmarshal(body, &e); err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "%s  <malformed: %v>\n", o.Key, err)
						continue
					}
					if intent == "" || e.Intent == intent {
						printEntry(cmd.OutOrStdout(), e)
					}
				}
				if !follow {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "only entries for this Intent (namespace/name)")
	cmd.Flags().BoolVar(&follow, "follow", false, "keep polling for new entries")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval with --follow")
	return cmd
}

func printEntry(w io.Writer, e audit.Entry) {
	payload := string(e.Payload)
	if len(payload) > 120 {
		payload = payload[:117] + "..."
	}
	fmt.Fprintf(w, "%8d  %s  %-20s %-28s %-10s %s\n", e.Seq, e.TS, e.Type, e.Intent, e.Stage, payload)
}

func newConformanceCommand(d Deps, f *auditFlags) *cobra.Command {
	var retention time.Duration
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Check that the audit bucket really enforces compliance-mode Object Lock",
		Long: "Writes one locked probe object under conformance/ and tries to delete, overwrite, and unlock it.\n" +
			"Runs with AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, and again with LAST_ADMIN_ACCESS_KEY/LAST_ADMIN_SECRET_KEY if set.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			type credSet struct{ name, ak, sk string }
			sets := []credSet{{"primary", d.Getenv("AWS_ACCESS_KEY_ID"), d.Getenv("AWS_SECRET_ACCESS_KEY")}}
			if ak := d.Getenv("LAST_ADMIN_ACCESS_KEY"); ak != "" {
				sets = append(sets, credSet{"admin", ak, d.Getenv("LAST_ADMIN_SECRET_KEY")})
			}
			out := cmd.OutOrStdout()
			failed := false
			for _, cs := range sets {
				cfg := f.s3Config(d)
				cfg.AccessKey, cfg.SecretKey = cs.ak, cs.sk
				client, err := d.NewS3Client(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				results, err := audit.RunConformance(cmd.Context(), client, f.bucket, retention)
				if err != nil {
					fmt.Fprintf(out, "[%s] SETUP FAILED: %v\n", cs.name, err)
					failed = true
					continue
				}
				for _, r := range results {
					status := "PASS"
					if !r.Passed {
						status, failed = "FAIL", true
					}
					fmt.Fprintf(out, "[%s] %s  %-36s %s\n", cs.name, status, r.Name, r.Detail)
				}
			}
			if failed {
				return errors.New("WORM conformance failed")
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&retention, "retention", 24*time.Hour, "how long the probe object stays locked")
	return cmd
}
```

`cmd/kubectl-last/main.go`:
```go
// Command kubectl-last is the last-operator kubectl plugin.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/moogie-labs/last-operator/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := cli.NewRootCommand(cli.DefaultDeps()).ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run the tests and build**

Run: `go test ./internal/cli/ -count=1 -v && go build -o bin/kubectl-last ./cmd/kubectl-last && ./bin/kubectl-last audit --help`
Expected: PASS (5 tests). The help output lists `conformance`, `log`, and `verify`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli cmd/kubectl-last go.mod go.sum
git commit -m "feat(cli): add kubectl last audit verify, log, and conformance"
```

### Task 14: Container images and the Helm chart

**Files:**
- Overwrite: `Dockerfile`
- Modify: `Makefile` (add `images`, `helm-crds`, `helm-lint`)
- Create: `charts/last-operator/Chart.yaml`, `charts/last-operator/values.yaml`, `charts/last-operator/templates/_helpers.tpl`, `templates/namespaces.yaml`, `templates/audit-secrets.yaml`, `templates/rustfs.yaml`, `templates/audit-init-job.yaml`, `templates/gateway.yaml`, and `charts/last-operator/crds/` (copied)

**Interfaces:**
- Consumes: the binaries `cmd/gateway`, `cmd/audit-init`, `cmd/operator`; the env var contracts from Tasks 11 and 12; `config/crd/bases/*.yaml` (Tasks 2–4).
- Produces: images `$(IMAGE_REGISTRY)/last-<component>:$(IMAGE_TAG)` for `operator`, `gateway`, `audit-init`. The chart installs into release namespace `last-system` and creates:
  - `last-gateway`: Deployment/SA `last-gateway`, ConfigMap `last-audit-config` (keys `chainId`, `bucket`, `endpoint`), Secret `last-audit-writer`
  - `last-audit`: Service/StatefulSet `last-rustfs` (port 9000), Job `last-audit-init-<revision>`, Secrets `last-audit-root` (keys `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY`), `last-audit-writer`, `last-audit-reader` (keys `accessKey`, `secretKey`)
  - The gateway creates Secret `last-audit-signing` and ConfigMap `last-audit-pubkey` at runtime (Task 12).

- [ ] **Step 1: Replace the Dockerfile with a parameterized build**

`Dockerfile`:
```dockerfile
# Build any last-operator binary: docker build --build-arg CMD=gateway .
FROM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CMD=operator
ARG VERSION=dev
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X github.com/moogie-labs/last-operator/internal/gateway.Version=${VERSION}" \
    -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
```

- [ ] **Step 2: Add Makefile targets**

Append to `Makefile`:
```make
IMAGE_REGISTRY ?= ghcr.io/moogie-labs
IMAGE_TAG ?= dev
COMPONENTS ?= operator gateway audit-init

.PHONY: images
images: ## Build container images for all components.
	@for c in $(COMPONENTS); do \
		$(CONTAINER_TOOL) build --build-arg CMD=$$c --build-arg VERSION=$(IMAGE_TAG) \
			-t $(IMAGE_REGISTRY)/last-$$c:$(IMAGE_TAG) . || exit 1; \
	done

.PHONY: helm-crds
helm-crds: manifests ## Copy generated CRDs into the Helm chart.
	mkdir -p charts/last-operator/crds
	rm -f charts/last-operator/crds/*.yaml
	cp config/crd/bases/*.yaml charts/last-operator/crds/

.PHONY: helm-lint
helm-lint: helm-crds ## Lint and render the Helm chart in bundled and external modes.
	helm lint --strict charts/last-operator
	helm template last charts/last-operator -n last-system >/dev/null
	helm template last charts/last-operator -n last-system \
		--set audit.bundled.enabled=false \
		--set audit.external.endpoint=https://s3.example.com \
		--set audit.external.existingSecret=audit-creds >/dev/null
```

Run: `make images IMAGE_TAG=dev`
Expected: three images build. Check with `docker images | grep last-`.

- [ ] **Step 3: Write the chart metadata and values**

`charts/last-operator/Chart.yaml`:
```yaml
apiVersion: v2
name: last-operator
description: AI-driven Kubernetes provisioning operator with a write-once audit log
type: application
version: 0.1.0
appVersion: "0.1.0"
```

`charts/last-operator/values.yaml`:
```yaml
image:
  registry: ghcr.io/moogie-labs
  tag: ""            # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent

namespaces:
  gateway: last-gateway
  audit: last-audit

audit:
  bucket: last-audit
  retentionDays: 365
  bundled:
    enabled: true
    image: rustfs/rustfs:1.0.0
    storageClassName: ""   # "" = cluster default
    size: 10Gi
    resources:
      requests: {cpu: 100m, memory: 512Mi}
      limits: {memory: 2Gi}
  external:
    endpoint: ""
    region: us-east-1
    pathStyle: false
    existingSecret: ""     # Secret in the gateway namespace with keys accessKey, secretKey

gateway:
  resources:
    requests: {cpu: 50m, memory: 64Mi}
    limits: {memory: 256Mi}
```

- [ ] **Step 4: Write the templates**

`charts/last-operator/templates/_helpers.tpl`:
```yaml
{{- define "last.image" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}
{{ $root.Values.image.registry }}/last-{{ $name }}:{{ $root.Values.image.tag | default $root.Chart.AppVersion }}
{{- end }}

{{/* Reuse a Secret value across upgrades; generate one on first install. */}}
{{- define "last.persistedSecretValue" -}}
{{- $ns := index . 0 -}}{{- $name := index . 1 -}}{{- $key := index . 2 -}}
{{- $existing := lookup "v1" "Secret" $ns $name -}}
{{- if and $existing $existing.data (hasKey $existing.data $key) -}}
{{- index $existing.data $key | b64dec -}}
{{- else -}}
{{- randAlphaNum 40 -}}
{{- end -}}
{{- end }}

{{- define "last.auditEndpoint" -}}
{{- if .Values.audit.bundled.enabled -}}
http://last-rustfs.{{ .Values.namespaces.audit }}.svc:9000
{{- else -}}
{{ required "audit.external.endpoint is required when audit.bundled.enabled=false" .Values.audit.external.endpoint }}
{{- end -}}
{{- end }}

{{- define "last.writerSecret" -}}
{{- if .Values.audit.bundled.enabled -}}
last-audit-writer
{{- else -}}
{{ required "audit.external.existingSecret is required when audit.bundled.enabled=false" .Values.audit.external.existingSecret }}
{{- end -}}
{{- end }}

{{- define "last.labels" -}}
app.kubernetes.io/part-of: last-operator
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
```

`charts/last-operator/templates/namespaces.yaml`:
```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Values.namespaces.gateway }}
  labels: {{- include "last.labels" . | nindent 4 }}
{{- if .Values.audit.bundled.enabled }}
---
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Values.namespaces.audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations:
    # Deleting this namespace would delete the audit store's volume.
    helm.sh/resource-policy: keep
{{- end }}
```

`charts/last-operator/templates/audit-secrets.yaml`:
```yaml
{{- if .Values.audit.bundled.enabled }}
{{- $audit := .Values.namespaces.audit }}
{{- $gw := .Values.namespaces.gateway }}
{{- $root := include "last.persistedSecretValue" (list $audit "last-audit-root" "RUSTFS_SECRET_KEY") }}
{{- $writer := include "last.persistedSecretValue" (list $audit "last-audit-writer" "secretKey") }}
{{- $reader := include "last.persistedSecretValue" (list $audit "last-audit-reader" "secretKey") }}
apiVersion: v1
kind: Secret
metadata:
  name: last-audit-root
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations: {helm.sh/resource-policy: keep}
type: Opaque
stringData:
  RUSTFS_ACCESS_KEY: last-root
  RUSTFS_SECRET_KEY: {{ $root | quote }}
---
apiVersion: v1
kind: Secret
metadata:
  name: last-audit-writer
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations: {helm.sh/resource-policy: keep}
type: Opaque
stringData:
  accessKey: last-gateway-writer
  secretKey: {{ $writer | quote }}
---
apiVersion: v1
kind: Secret
metadata:
  name: last-audit-writer
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations: {helm.sh/resource-policy: keep}
type: Opaque
stringData:
  accessKey: last-gateway-writer
  secretKey: {{ $writer | quote }}
---
apiVersion: v1
kind: Secret
metadata:
  name: last-audit-reader
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations: {helm.sh/resource-policy: keep}
type: Opaque
stringData:
  accessKey: last-audit-reader
  secretKey: {{ $reader | quote }}
{{- end }}
```

`charts/last-operator/templates/rustfs.yaml`:
```yaml
{{- if .Values.audit.bundled.enabled }}
{{- $audit := .Values.namespaces.audit }}
apiVersion: v1
kind: Service
metadata:
  name: last-rustfs
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  selector:
    app.kubernetes.io/name: last-rustfs
  ports:
    - name: s3
      port: 9000
      targetPort: s3
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: last-rustfs
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  serviceName: last-rustfs
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: last-rustfs
  template:
    metadata:
      labels:
        app.kubernetes.io/name: last-rustfs
        {{- include "last.labels" . | nindent 8 }}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
        runAsGroup: 10001
        fsGroup: 10001
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: rustfs
          image: {{ .Values.audit.bundled.image }}
          env:
            - {name: RUSTFS_VOLUMES, value: /data}
            - {name: RUSTFS_ADDRESS, value: ":9000"}
            - {name: RUSTFS_CONSOLE_ENABLE, value: "false"}
          envFrom:
            - secretRef: {name: last-audit-root}
          ports:
            - {name: s3, containerPort: 9000}
          readinessProbe:
            httpGet: {path: /health/ready, port: s3}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /health, port: s3}
            periodSeconds: 20
            failureThreshold: 6
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
          resources: {{- toYaml .Values.audit.bundled.resources | nindent 12 }}
          volumeMounts:
            - {name: data, mountPath: /data}
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        {{- with .Values.audit.bundled.storageClassName }}
        storageClassName: {{ . }}
        {{- end }}
        resources:
          requests:
            storage: {{ .Values.audit.bundled.size }}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: last-rustfs
  namespace: {{ $audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: last-rustfs
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: {{ .Values.namespaces.gateway }}
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: last-audit-init
      ports:
        - {port: 9000, protocol: TCP}
{{- end }}
```

`charts/last-operator/templates/audit-init-job.yaml`:
```yaml
{{- if .Values.audit.bundled.enabled }}
apiVersion: batch/v1
kind: Job
metadata:
  # A new Job per revision: Job templates are immutable, and init is idempotent.
  name: last-audit-init-{{ .Release.Revision }}
  namespace: {{ .Values.namespaces.audit }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  backoffLimit: 10
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels:
        app.kubernetes.io/name: last-audit-init
        {{- include "last.labels" . | nindent 8 }}
    spec:
      restartPolicy: OnFailure
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: init
          image: {{ include "last.image" (list . "audit-init") }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          env:
            - {name: LAST_AUDIT_ENDPOINT, value: {{ include "last.auditEndpoint" . | quote }}}
            - {name: LAST_AUDIT_BUCKET, value: {{ .Values.audit.bucket | quote }}}
            - {name: LAST_AUDIT_RETENTION_DAYS, value: {{ .Values.audit.retentionDays | quote }}}
            - name: RUSTFS_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: last-audit-root, key: RUSTFS_ACCESS_KEY}}
            - name: RUSTFS_SECRET_KEY
              valueFrom: {secretKeyRef: {name: last-audit-root, key: RUSTFS_SECRET_KEY}}
            - name: LAST_AUDIT_WRITER_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: last-audit-writer, key: accessKey}}
            - name: LAST_AUDIT_WRITER_SECRET_KEY
              valueFrom: {secretKeyRef: {name: last-audit-writer, key: secretKey}}
            - name: LAST_AUDIT_READER_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: last-audit-reader, key: accessKey}}
            - name: LAST_AUDIT_READER_SECRET_KEY
              valueFrom: {secretKeyRef: {name: last-audit-reader, key: secretKey}}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
{{- end }}
```

`charts/last-operator/templates/gateway.yaml`:
```yaml
{{- $gw := .Values.namespaces.gateway }}
{{- $cfg := lookup "v1" "ConfigMap" $gw "last-audit-config" }}
{{- $chainId := uuidv4 }}
{{- if and $cfg $cfg.data (hasKey $cfg.data "chainId") }}{{ $chainId = index $cfg.data "chainId" }}{{ end }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: last-audit-config
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
  annotations:
    # The chain ID must never change for an existing audit bucket.
    helm.sh/resource-policy: keep
data:
  chainId: {{ $chainId | quote }}
  bucket: {{ .Values.audit.bucket | quote }}
  endpoint: {{ include "last.auditEndpoint" . | quote }}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: last-gateway
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: last-gateway
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: [secrets, configmaps]
    verbs: [create]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [last-audit-signing]
    verbs: [get]
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [last-audit-pubkey]
    verbs: [get, update]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [create]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    resourceNames: [last-gateway-audit-writer]
    verbs: [get, update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: last-gateway
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: last-gateway
subjects:
  - kind: ServiceAccount
    name: last-gateway
    namespace: {{ $gw }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: last-gateway
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels:
      app.kubernetes.io/name: last-gateway
  template:
    metadata:
      labels:
        app.kubernetes.io/name: last-gateway
        {{- include "last.labels" . | nindent 8 }}
    spec:
      serviceAccountName: last-gateway
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: gateway
          image: {{ include "last.image" (list . "gateway") }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          env:
            - name: POD_NAMESPACE
              valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
            - name: POD_NAME
              valueFrom: {fieldRef: {fieldPath: metadata.name}}
            - {name: LAST_AUDIT_ENDPOINT, value: {{ include "last.auditEndpoint" . | quote }}}
            - {name: LAST_AUDIT_REGION, value: {{ .Values.audit.external.region | quote }}}
            - {name: LAST_AUDIT_PATH_STYLE, value: {{ ternary "true" (toString .Values.audit.external.pathStyle) .Values.audit.bundled.enabled | quote }}}
            - {name: LAST_AUDIT_BUCKET, value: {{ .Values.audit.bucket | quote }}}
            - {name: LAST_AUDIT_RETENTION, value: {{ printf "%dh" (mul .Values.audit.retentionDays 24) | quote }}}
            - name: LAST_AUDIT_CHAIN_ID
              valueFrom: {configMapKeyRef: {name: last-audit-config, key: chainId}}
            - name: LAST_AUDIT_ACCESS_KEY
              valueFrom: {secretKeyRef: {name: {{ include "last.writerSecret" . }}, key: accessKey}}
            - name: LAST_AUDIT_SECRET_KEY
              valueFrom: {secretKeyRef: {name: {{ include "last.writerSecret" . }}, key: secretKey}}
          ports:
            - {name: http, containerPort: 8080}
          readinessProbe:
            httpGet: {path: /readyz, port: http}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /healthz, port: http}
            periodSeconds: 20
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          resources: {{- toYaml .Values.gateway.resources | nindent 12 }}
---
apiVersion: v1
kind: Service
metadata:
  name: last-gateway
  namespace: {{ $gw }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  selector:
    app.kubernetes.io/name: last-gateway
  ports:
    - {name: http, port: 8080, targetPort: http}
```

- [ ] **Step 5: Lint and render**

Run: `make helm-lint`
Expected: `helm lint` reports `0 chart(s) failed`, and both `helm template` renders succeed. `ls charts/last-operator/crds` lists ten CRDs.
Then check that the rendered RBAC matches the gateway's needs:
```bash
helm template last charts/last-operator -n last-system | grep -A3 'resourceNames'
```
Expected: `last-audit-signing`, `last-audit-pubkey`, and `last-gateway-audit-writer` appear.

- [ ] **Step 6: Commit**

```bash
git add Dockerfile Makefile charts
git commit -m "feat(chart): add images and Helm chart with bundled RustFS audit store and gateway"
```

### Task 15: End-to-end suite on kind

**Files:**
- Create: `hack/e2e.sh`, `test/e2e/audit_e2e_test.go`
- Delete: kubebuilder's scaffolded Ginkgo e2e files (`test/e2e/e2e_suite_test.go`, `test/e2e/e2e_test.go`) and `test/utils/` if nothing else imports it
- Modify: `Makefile` (`test-e2e` target)

**Interfaces:**
- Consumes: `make images` and the chart (Task 14); `kubectl-last` (Task 13); the object names listed in Task 14's Interfaces.
- Produces: `make test-e2e`, which creates (or reuses) kind cluster `last-e2e`, installs the chart, and runs `go test -tags e2e ./test/e2e/`.

- [ ] **Step 1: Remove the scaffolded e2e suite and point `test-e2e` at our script**

```bash
git rm -q test/e2e/e2e_suite_test.go test/e2e/e2e_test.go
grep -rl 'test/utils' --include=*.go . || git rm -rq test/utils
```
In `Makefile`, replace the body of the existing `test-e2e` target with `./hack/e2e.sh`. Delete the `setup-test-e2e` and `cleanup-test-e2e` targets if they exist, and remove `setup-test-e2e` from `test-e2e`'s prerequisites.

- [ ] **Step 2: Write the e2e driver script**

`hack/e2e.sh`:
```bash
#!/usr/bin/env bash
# Build images, install the chart on kind, and run the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER="${KIND_CLUSTER:-last-e2e}"
TAG="${IMAGE_TAG:-e2e}"
REGISTRY="${IMAGE_REGISTRY:-ghcr.io/moogie-labs}"

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kubectl config use-context "kind-$CLUSTER"

make images IMAGE_TAG="$TAG" IMAGE_REGISTRY="$REGISTRY" COMPONENTS="gateway audit-init"
for c in gateway audit-init; do
  kind load docker-image --name "$CLUSTER" "$REGISTRY/last-$c:$TAG"
done

make helm-crds
helm upgrade --install last charts/last-operator -n last-system --create-namespace \
  --set image.tag="$TAG" --set image.registry="$REGISTRY" --set image.pullPolicy=Never \
  --set audit.bundled.size=1Gi \
  --wait --wait-for-jobs --timeout 10m

go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m
```
Run: `chmod +x hack/e2e.sh`

- [ ] **Step 3: Write the e2e test**

`test/e2e/audit_e2e_test.go`:
```go
//go:build e2e

package e2e

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	kubectlLast string
	tmpDir      string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "last-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tmpDir = dir
	kubectlLast = filepath.Join(dir, "kubectl-last")
	build := exec.Command("go", "build", "-o", kubectlLast, "../../cmd/kubectl-last")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build kubectl-last:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func secretValue(t *testing.T, ns, name, key string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(kubectl(t, "get", "secret", "-n", ns, name, "-o", "jsonpath={.data."+key+"}"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// portForward forwards a random local port to svc/last-rustfs and returns its URL.
func portForward(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("kubectl", "port-forward", "-n", "last-audit", "svc/last-rustfs", ":9000")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	re := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() { // keep draining so port-forward never blocks on stdout
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case port := <-found:
		return "http://127.0.0.1:" + port
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward did not start")
		return ""
	}
}

type env map[string]string

func runLast(e env, args ...string) (string, error) {
	cmd := exec.Command(kubectlLast, args...)
	cmd.Env = os.Environ()
	for k, v := range e {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var headRe = regexp.MustCompile(`seq \d+\.\.(\d+)`)

func headSeq(t *testing.T, verifyOutput string) uint64 {
	t.Helper()
	m := headRe.FindStringSubmatch(verifyOutput)
	if m == nil {
		t.Fatalf("no head seq in: %s", verifyOutput)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}

func TestAuditChain(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	pub := filepath.Join(tmpDir, "pub.pem")
	pubPEM := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-pubkey", "-o", `jsonpath={.data.ed25519\.pub}`)
	if err := os.WriteFile(pub, []byte(pubPEM+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := func(endpoint, ns, secret string) env {
		return env{
			"LAST_AUDIT_ENDPOINT": endpoint, "LAST_AUDIT_BUCKET": "last-audit", "LAST_AUDIT_CHAIN_ID": chain,
			"AWS_ACCESS_KEY_ID":     secretValue(t, ns, secret, "accessKey"),
			"AWS_SECRET_ACCESS_KEY": secretValue(t, ns, secret, "secretKey"),
		}
	}
	endpoint := portForward(t)
	reader := creds(endpoint, "last-audit", "last-audit-reader")

	var before uint64
	t.Run("chain verifies after install", func(t *testing.T) {
		out, err := runLast(reader, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify: %v\n%s", err, out)
		}
		before = headSeq(t, out)
	})

	t.Run("gateway restart extends the chain", func(t *testing.T) {
		kubectl(t, "rollout", "restart", "-n", "last-gateway", "deploy/last-gateway")
		kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
		out, err := runLast(reader, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify: %v\n%s", err, out)
		}
		if after := headSeq(t, out); after <= before {
			t.Fatalf("head did not advance after restart: before=%d after=%d", before, after)
		}
	})

	t.Run("bundled store passes WORM conformance", func(t *testing.T) {
		e := creds(endpoint, "last-gateway", "last-audit-writer")
		e["LAST_ADMIN_ACCESS_KEY"] = secretValue(t, "last-audit", "last-audit-root", "RUSTFS_ACCESS_KEY")
		e["LAST_ADMIN_SECRET_KEY"] = secretValue(t, "last-audit", "last-audit-root", "RUSTFS_SECRET_KEY")
		out, err := runLast(e, "audit", "conformance")
		if err != nil || strings.Contains(out, "FAIL") {
			t.Fatalf("conformance: %v\n%s", err, out)
		}
	})

	t.Run("gateway refuses to start while the store is down", func(t *testing.T) {
		kubectl(t, "scale", "-n", "last-audit", "statefulset/last-rustfs", "--replicas=0")
		kubectl(t, "wait", "-n", "last-audit", "--for=delete", "pod/last-rustfs-0", "--timeout=2m")
		kubectl(t, "rollout", "restart", "-n", "last-gateway", "deploy/last-gateway")
		time.Sleep(45 * time.Second)
		ready := kubectl(t, "get", "deploy", "-n", "last-gateway", "last-gateway", "-o", "jsonpath={.status.readyReplicas}")
		if ready != "" && ready != "0" {
			t.Fatalf("gateway reported ready (%s) with the audit store down", ready)
		}

		kubectl(t, "scale", "-n", "last-audit", "statefulset/last-rustfs", "--replicas=1")
		kubectl(t, "rollout", "status", "-n", "last-audit", "statefulset/last-rustfs", "--timeout=5m")
		kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=10m")
		fresh := creds(portForward(t), "last-audit", "last-audit-reader") // the old forward died with the pod
		out, err := runLast(fresh, "audit", "verify", "--pubkey-file", pub)
		if err != nil {
			t.Fatalf("verify after recovery: %v\n%s", err, out)
		}
	})
}
```

- [ ] **Step 4: Run the suite (Docker must be running)**

Run: `make test-e2e`
Expected: the kind cluster comes up, `helm upgrade --install` completes with the init Job succeeding, and `TestAuditChain` passes all four subtests.
Troubleshooting:
- **Init Job fails:** run `kubectl logs -n last-audit job/last-audit-init-1`.
- **Gateway won't become ready:** run `kubectl logs -n last-gateway deploy/last-gateway`. Look for a lock-config or chain-verification error.

- [ ] **Step 5: Commit**

```bash
git add hack/e2e.sh test Makefile
git commit -m "test(e2e): verify audit chain, restart, conformance, and fail-closed startup on kind"
```

### Task 16: CI and README

**Files:**
- Overwrite: `.github/workflows/lint.yml`, `.github/workflows/test.yml`, `.github/workflows/test-e2e.yml`
- Overwrite: `README.md`

**Interfaces:**
- Consumes: the make targets `lint`, `test`, `test-integration`, `helm-lint`, `test-e2e` (Tasks 1, 9, 14, 15).
- Produces: CI that runs unit, envtest, integration, chart lint, CRD-sync, and kind e2e on every push and PR.

- [ ] **Step 1: Write the workflows**

`.github/workflows/lint.yml`:
```yaml
name: Lint
on: [push, pull_request]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version-file: go.mod}
      - run: make lint
      - uses: azure/setup-helm@v4
      - name: Chart lint and CRD sync
        run: |
          make helm-lint
          git diff --exit-code charts/ config/ || (echo "Run 'make helm-crds manifests' and commit the result." && exit 1)
```

`.github/workflows/test.yml`:
```yaml
name: Tests
on: [push, pull_request]
jobs:
  unit:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version-file: go.mod}
      - run: make test
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version-file: go.mod}
      - run: make test-integration
```

`.github/workflows/test-e2e.yml`:
```yaml
name: E2E
on: [push, pull_request]
jobs:
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version-file: go.mod}
      - uses: azure/setup-helm@v4
      - run: go install sigs.k8s.io/kind@latest
      - run: make test-e2e
```

- [ ] **Step 2: Write the README**

`README.md`:
````markdown
# last-operator

An AI-driven Kubernetes operator that turns natural-language requests into
reviewed, audited, continuously reconciled infrastructure.
Design: [`docs/superpowers/specs/2026-09-26-last-operator-design.md`](docs/superpowers/specs/2026-09-26-last-operator-design.md).

**Status:** foundation (M0–M1). The CRDs and the write-once audit log are in
place; the agent, sandboxes, and approvals arrive in later milestones.

## What works today

- All `last.moogie-labs.com/v1alpha1` CRDs, with validation.
- The **gateway**, the only component that will ever act, writes a hash-chained,
  Ed25519-signed audit log to S3 Object Lock storage in compliance mode, and
  refuses to start if the store is unreachable or not locked correctly.
- A bundled single-node **RustFS** audit store, provisioned with least-privilege users.
- `kubectl last audit verify | log | conformance`.

## Quick start (kind)

```bash
make test-e2e            # builds images, installs the chart on kind, runs the suite
```

Verify the audit chain yourself:

```bash
kubectl port-forward -n last-audit svc/last-rustfs 9000:9000 &
export LAST_AUDIT_ENDPOINT=http://127.0.0.1:9000
export LAST_AUDIT_CHAIN_ID=$(kubectl get cm -n last-gateway last-audit-config -o jsonpath='{.data.chainId}')
export AWS_ACCESS_KEY_ID=$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.accessKey}' | base64 -d)
export AWS_SECRET_ACCESS_KEY=$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.secretKey}' | base64 -d)
kubectl get cm -n last-gateway last-audit-pubkey -o jsonpath='{.data.ed25519\.pub}' > pub.pem
go run ./cmd/kubectl-last audit verify --pubkey-file pub.pem
```

## Production audit storage

The bundled RustFS store is the easy default. For production compliance use,
point the chart at an external S3 bucket (or Ceph RGW) that has Object Lock in
compliance mode, and run `kubectl last audit conformance` against it:

```bash
helm install last charts/last-operator -n last-system --create-namespace \
  --set audit.bundled.enabled=false \
  --set audit.external.endpoint=https://s3.us-east-1.amazonaws.com \
  --set audit.external.existingSecret=last-audit-writer   # keys: accessKey, secretKey
```

## Development

| Command | What it runs |
|---|---|
| `make test` | unit + envtest |
| `make test-integration` | integration tests against RustFS (Docker) |
| `make helm-lint` | chart lint and render |
| `make test-e2e` | kind end-to-end suite |
````

- [ ] **Step 3: Run everything locally**

Run: `make lint test helm-lint test-integration`
Expected: all pass. `make lint` uses kubebuilder's golangci-lint config; fix any findings in our code rather than disabling linters.

- [ ] **Step 4: Commit**

```bash
git add .github README.md
git commit -m "ci: run unit, envtest, integration, chart lint, and kind e2e; add README"
```

---

## Spec coverage (M0 + M1)

| Spec requirement | Task |
|---|---|
| All CRD types with validation (§5) | 2, 3, 4 |
| Entry format, hashing, Ed25519 signatures (§14.2) | 5 |
| Object key layout, checkpoint index objects (§14.2–14.3) | 6, 8 |
| Verifier: gaps, links, signatures, versions, delete markers, checkpoints, `--expect-head` (§14.5) | 7, 13 |
| Genesis hash, recovery from unclear write failures, broken-writer fail-closed (§14.3) | 8 |
| Fail-closed guard (§14.1) | 8, 9 |
| Compliance-mode Object Lock sink, `If-None-Match`, startup lock check (§14.4) | 9, 12 |
| WORM conformance in CI and on demand, never at startup (§14.4) | 10, 13, 15 |
| Bundled RustFS, least-privilege writer and reader users (§14.4) | 11, 14 |
| Gateway leader election, startup verification from the latest checkpoint, checkpoints every 1000 entries or 10 minutes (§14.3) | 8, 12 |
| Chain-seq metric (§14.5 truncation witness, §16) | 12 |
| `kubectl last audit verify/log` (§14.5) | 13 |
| Namespaces and RBAC isolation (§3) | 14 |

Deferred to later plans: admission policies for `IntentApproval`, rules, and suggestions (M2, M6); Kubernetes API audit cross-check docs (§14.6, M2); every gateway RPC except the audit writer (M2+).

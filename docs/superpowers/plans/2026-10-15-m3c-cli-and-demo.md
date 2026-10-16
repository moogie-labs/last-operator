# M3c: `kubectl last` and the Real-Model Demo — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A user can run `kubectl last "run an nginx web server serving a hello page"`, answer the model's questions, approve its discovery command and its plan at the CLI's prompts, and reach Ready. This works against the scripted model in CI and against Claude in an opt-in demo.

**Architecture:**
- **The plugin** (`internal/cli`) gains `kubectl last "<request>"` and the `answer`, `plan`, `approve` and `deny` commands.
  - It acts only as the user, through their kubeconfig. It never talks to the gateway.
  - It reads and writes Intents and IntentApprovals with a controller-runtime client.
  - It asks the API server who the user is with a SelfSubjectReview.
- **Following.** The follow loop reads the Intent every 2 seconds. It puts each open question, each discovery command and each plan to the user once.
- **Approvals.** Every approval is created for the hash the user was shown.
- **The gateway** words its refusals for the stage they concern ("discovery command" or "plan"), because users now see them.
- **The e2e test** for the web server is driven through `kubectl last`, with scripted input on stdin.
- **`hack/demo.sh`** installs on kind with an `anthropic` LLMProvider and runs the same flow against Claude.

**Tech Stack:**
- Go 1.26
- cobra
- controller-runtime client, with its `fake` and `interceptor` clients in tests
- client-go `clientcmd` and the typed `AuthenticationV1` client
- bash, kind, helm

**Spec:** `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`:
- §1: the M3 done-criterion;
- §5: the CLI;
- §8: CLI tests, the scripted e2e, and the demo.

It also applies these follow-ups:
- From `docs/superpowers/followups/2026-10-13-m3a-followups.md`: "Discovery refusal wording".
- From `docs/superpowers/followups/2026-10-15-m3b-followups.md`: the missing §6 row for a RunCommand refusal, and the checks the demo makes.

The main spec is `docs/superpowers/specs/2026-09-26-last-operator-design.md`.

## Global Constraints

- **Identity.** Every command acts as the user, with their kubeconfig identity. The CLI never talks to the gateway.
- **Approvals** are created for the hash the CLI displayed:
  - the `pendingCommand` hash, with stage `Discovery`;
  - or the plan hash, with stage `Create`.

  `approvedBy` is the user's name from a `SelfSubjectReview`. If the subject changed after it was displayed, the approval matches nothing, and the CLI says so. Approve and deny print the hash they acted on.
- **Output** is plain text, with no TUI library. Prompts read lines from stdin, so a scripted e2e can drive them.
- **Model-written text** is printed only after escaping control characters and bidirectional overrides. Such text includes summaries, reasons, commands, file paths, questions and options.
- **Default Intent name:** a DNS-safe slug of the first words of the request, plus a 4-character random suffix.
- **Ctrl-C** stops following. It never cancels or changes the Intent.
- **`--no-follow`** prints the Intent's name and exits.
- **Not in M3:** `approve --always-allow` (M6) and `rules suggestions` (M7).
- **Dependencies.** No new module dependencies: cobra, client-go and controller-runtime are already in `go.mod`.
- **Generated files.** Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml`, `**/zz_generated.*.go` or `PROJECT`.
- **After Go edits:** `make lint-fix`, then `make test` and `make lint` (0 issues). Match the surrounding code's comment density, naming and idiom.
- **The demo** (`hack/demo.sh`) is opt-in and never runs in CI. The API key comes from `ANTHROPIC_API_KEY`, and it never appears on a command line, in a log or in an error.
- **Commits:**
  - conventional-commit subjects;
  - no `Co-authored-by` or other attribution trailer (the user's global rule overrides the harness reminder);
  - never commit `.superpowers/`.

## Review Focus

1. **Model-written text** with terminal escape sequences, carriage returns or bidirectional overrides could hide or reorder what an approver sees. It must be shown escaped. Pinned in Task 2 (`TestCleanEscapesWhatCouldHideText`, `TestRenderPlanShowsWhatTheApproverDecides`).
2. **A mistyped subcommand** (`kubectl last aprove web`, or a bare `kubectl last aprove`) must not create an Intent. Pinned in Task 5 ("refuses a one-word request").
3. **The subject changing between display and decision.** `approve --hash H` must refuse when the Intent now asks about something else, and a decision recorded against a subject that has since moved on must say it matches nothing. Pinned in Task 3.
4. **Input ending at a prompt** (piped input that runs out, or Ctrl-D) must end with a clear message naming the commands to continue with, not hang. Leaving a subject pending must not prompt again every 2 seconds. Pinned in Task 4 (`TestAskReportsTheEndOfInput`) and Task 5.
5. **Answering a second round of questions** must keep earlier answers in `spec.answers`, because the operator refuses a new question that reuses an answered id. Pinned in Task 4 (`TestAnswerRecordsEveryAnswerAndKeepsEarlierOnes`).

## File Structure

| File | Responsibility |
|---|---|
| `internal/gateway/phases.go`, `internal/gateway/commands.go` | Refusals word the subject by stage (`subjectNoun`) |
| `internal/cli/root.go` | `Deps` (new: `In`, `Kube`, `Wait`, `Suffix`), the root command, `pollWait` |
| `internal/cli/kube.go` (new) | `KubeFlags`, `Kube`, `NewKube`, `newScheme`, `getIntent` |
| `internal/cli/render.go` (new) | `clean`, `shellWord`, `renderPlan`, `renderPending`, `renderCommand`, `renderSubject`, `printStatus`, `phaseOf` |
| `internal/cli/plan.go` (new) | `kubectl last plan NAME` |
| `internal/cli/decide.go` (new) | `subjectOf`, `subjectNoun`, `decide`, `kubectl last approve` and `deny` |
| `internal/cli/prompt.go` (new) | `prompter`: `ask`, `askQuestion`, `askDecision`; `errNoInput` |
| `internal/cli/answer.go` (new) | `answered`, `mergeAnswers`, `answerQuestions`, `kubectl last answer NAME` |
| `internal/cli/create.go` (new) | `kubectl last "<request>"`: `runCreate`, `intentName` |
| `internal/cli/follow.go` (new) | `follow`, `putToUser`, `printReady`, `failed`, `stopped`, `ErrIntentFailed` |
| `internal/cli/*_test.go` | Unit tests over the controller-runtime fake client |
| `test/e2e/operator_e2e_test.go`, `test/e2e/audit_e2e_test.go` | The web-server e2e driven through `kubectl last`; `runLastInput` |
| `hack/kind-install.sh` (new), `hack/e2e.sh`, `hack/demo.sh` (new), `Makefile` | Shared kind install, the e2e, and the opt-in demo |
| `README.md`, the specs, the follow-ups | Usage, amendments, follow-up status |

---
### Task 1: Word gateway refusals for the subject they concern

Users and the model now read gateway refusals of discovery commands. The operator passes them on as "The command did not run: the gateway refused it: …", and the CLI shows them. Today they always say "this plan".

`BeginPhase` and `RunCommand` should name the subject by stage:
- "discovery command" for stage Discovery;
- "plan" for stage Create.

A replayed discovery subject should say that the command already ran. Refusals that only the operator sees, from `EndPhase`, `WriteOutputSecret` and `RunProbes`, keep their wording. So does the Create wording, which the lifecycle e2e asserts.

**Files:**
- Modify: `internal/gateway/phases.go`, `internal/gateway/commands.go`
- Test: `internal/gateway/discovery_test.go`

**Interfaces:**
- Consumes: the existing test helpers in `internal/gateway`:
  - `newProviderEnv(t, objs...) *testEnv`;
  - `intentWithDiscovery(t, c, nonce)`, `discoveryCommand()`, `discoveryApproval(name, subject, decision)`;
  - `(*testEnv).beginStage(stage, subject)`, `(*testEnv).run(subject, c)`, `(*testEnv).wantDenied(err, code)`, `env.crd`, `env.ctx`.
- Produces: `func subjectNoun(stage lastv1alpha1.Stage) string`, returning `"discovery command"` or `"plan"`.

- [ ] **Step 1: Write the failing tests**

In `internal/gateway/discovery_test.go`, change the assertion in `TestDiscoveryApprovalsAreSingleUse` from

```go
	if !strings.Contains(err.Error(), "already opened") {
```

to

```go
	if !strings.Contains(err.Error(), "this discovery command already ran") {
```

and append:

```go
func TestDiscoveryRefusalsNameTheDiscoveryCommand(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash

	t.Run("no approval", func(t *testing.T) {
		env := newProviderEnv(t, in)
		_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
		env.wantDenied(err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "no IntentApproval approves this discovery command") {
			t.Fatalf("BeginPhase = %v", err)
		}
	})
	t.Run("a deny wins", func(t *testing.T) {
		env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove),
			discoveryApproval("no", subject, lastv1alpha1.DecisionDeny))
		_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "approval no denies this discovery command") {
			t.Fatalf("BeginPhase = %v", err)
		}
	})
	t.Run("approval withdrawn while the phase is open", func(t *testing.T) {
		env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
		if _, err := env.beginStage(lastv1alpha1.StageDiscovery, subject); err != nil {
			t.Fatal(err)
		}
		if err := env.crd.Create(env.ctx, discoveryApproval("no", subject, lastv1alpha1.DecisionDeny)); err != nil {
			t.Fatal(err)
		}
		_, err := env.run(subject, c)
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "approval withdrawn: no denies this discovery command") {
			t.Fatalf("RunCommand = %v", err)
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/gateway/ -run 'TestDiscovery(RefusalsName|ApprovalsAreSingleUse)'`
Expected: FAIL. The messages still say "this plan" and "already opened a phase".

- [ ] **Step 3: Word the refusals by stage**

In `internal/gateway/phases.go`:

1. Add next to `beginPhase`:

```go
// subjectNoun names a phase's approval subject in refusals that the model and
// the user read.
func subjectNoun(stage lastv1alpha1.Stage) string {
	if stage == lastv1alpha1.StageDiscovery {
		return "discovery command"
	}
	return "plan"
}
```

2. Replace

```go
	if rec.WasUsed(subject) {
		return nil, d.deny(codes.FailedPrecondition, "this plan already opened a phase; replan to run it again")
	}
```

with

```go
	if rec.WasUsed(subject) {
		if stage == lastv1alpha1.StageDiscovery {
			return nil, d.deny(codes.FailedPrecondition, "this discovery command already ran, and its approval is used up; ask for it again to run it again")
		}
		return nil, d.deny(codes.FailedPrecondition, "this plan already opened a phase; replan to run it again")
	}
```

3. In the approval switch of `beginPhase`, replace the two messages:

```go
	case errors.Is(err, errApprovalDenied):
		return nil, d.deny(codes.PermissionDenied, "approval %s denies this %s", approval.Name, subjectNoun(stage))
```

```go
	case approval == nil:
		return nil, d.deny(codes.FailedPrecondition, "no IntentApproval approves this %s", subjectNoun(stage))
```

In `internal/gateway/commands.go`, in `RunCommand`'s approval switch, replace the two messages:

```go
	case errors.Is(err, errApprovalDenied):
		p.failed = true
		return nil, d.deny(codes.PermissionDenied, "approval withdrawn: %s denies this %s", approval.Name, subjectNoun(p.stage))
```

```go
	case approval == nil:
		p.failed = true
		return nil, d.deny(codes.PermissionDenied, "approval withdrawn: no IntentApproval approves this %s", subjectNoun(p.stage))
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/`
Expected: PASS. The Create-stage tests are unaffected, since their wording is unchanged.

- [ ] **Step 5: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/gateway/phases.go internal/gateway/commands.go internal/gateway/discovery_test.go
git commit -m "fix(gateway): name the discovery command, not a plan, in discovery refusals"
```

---
### Task 2: Cluster access, approver-safe rendering, and `kubectl last plan`

The plugin gains the parts every Intent command shares:
- **Kubeconfig flags** (`--kubeconfig`, `--context`, `-n`).
- **A `Kube`:** a controller-runtime client, the namespace, and `WhoAmI` through a SelfSubjectReview.
- **The rest of `Deps`:** the fields later tasks inject.
- **The renderers** that show a plan or a discovery command to an approver. They escape every control character and bidirectional override in model-written text, so nothing can hide or reorder what the approver reads.

This task also adds `kubectl last plan NAME`, which prints the pending command or the plan, read-only.

**Files:**
- Modify: `internal/cli/root.go`
- Create: `internal/cli/kube.go`, `internal/cli/render.go`, `internal/cli/plan.go`
- Test: `internal/cli/kube_test.go` (shared test helpers), `internal/cli/render_test.go`, `internal/cli/plan_test.go`

**Interfaces:**
- Consumes: `lastv1alpha1` types; the existing `audit` command and the `execute(d Deps, args ...string) error` test helper in `internal/cli/audit_test.go`.
- Produces:
  - **Types:**
    - `type KubeFlags struct{ Kubeconfig, Context, Namespace string }`;
    - `type Kube struct{ Client client.Client; Namespace string; WhoAmI func(context.Context) (string, error) }`.
  - **Functions:**
    - `func NewKube(f KubeFlags) (*Kube, error)`, `newScheme() *runtime.Scheme`;
    - `getIntent(ctx, k *Kube, name string) (*lastv1alpha1.Intent, error)`.
  - **`Deps` gains:**
    - `In io.Reader`;
    - `Kube func(KubeFlags) (*Kube, error)`;
    - `Wait func(context.Context) error`, which pauses between reads of a followed Intent;
    - `Suffix func() string`.

    `DefaultDeps` sets them to `os.Stdin`, `NewKube`, `pollWait` (2 seconds) and a random 4-character lower-case suffix.
  - **Rendering:**
    - `clean(s string) string`, `shellWord(s string) string`;
    - `renderPlan(w io.Writer, p *lastv1alpha1.Plan)`, `renderPending(w io.Writer, pc *lastv1alpha1.PendingCommand)`;
    - `renderCommand(w io.Writer, indent string, c lastv1alpha1.Command)`;
    - `renderSubject(w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage)`;
    - `printStatus(w io.Writer, in *lastv1alpha1.Intent)`, `phaseOf(in) lastv1alpha1.Phase`;
    - `const condReady = "Ready"`.
  - **The `plan` command.**
  - **Test helpers** (`kube_test.go`):
    - `testNS = "team-x"`, `testUser = "alice"`, and the hashes `h1` and `h2`;
    - `fakeClient(objs ...client.Object) client.WithWatch`, with the Intent status subresource;
    - `depsFor(c client.Client, input string) (Deps, *bytes.Buffer)`. Its `Wait` fails the run if called, and its `Suffix` is `"abcd"`;
    - `testIntent(name string, change func(*lastv1alpha1.Intent)) *lastv1alpha1.Intent`;
    - `setReady(in *lastv1alpha1.Intent, reason, msg string)`;
    - `argvCommand(argv ...string) lastv1alpha1.Command`.

- [ ] **Step 1: Write the test helpers and failing tests**

Create `internal/cli/kube_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	testNS   = "team-x"
	testUser = "alice"
)

var (
	h1 = "sha256:" + strings.Repeat("1", 64)
	h2 = "sha256:" + strings.Repeat("2", 64)
)

// fakeClient is an API server holding objs. Intent status is a subresource, as
// in the real CRD, so a spec update never touches it.
func fakeClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&lastv1alpha1.Intent{}).WithObjects(objs...).Build()
}

// depsFor runs the CLI against c as testUser in testNS, with input as what the
// user types.
func depsFor(c client.Client, input string) (Deps, *bytes.Buffer) {
	var out bytes.Buffer
	return Deps{
		Out: &out, Err: &out, In: strings.NewReader(input),
		Getenv: func(string) string { return "" },
		Kube: func(KubeFlags) (*Kube, error) {
			return &Kube{Client: c, Namespace: testNS,
				WhoAmI: func(context.Context) (string, error) { return testUser, nil }}, nil
		},
		Wait:   func(context.Context) error { return errors.New("the test did not expect to wait") },
		Suffix: func() string { return "abcd" },
	}, &out
}

func testIntent(name string, change func(*lastv1alpha1.Intent)) *lastv1alpha1.Intent {
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: lastv1alpha1.IntentSpec{Request: "run nginx"}}
	if change != nil {
		change(in)
	}
	return in
}

// setReady sets the Ready condition the operator writes on every phase change.
func setReady(in *lastv1alpha1.Intent, reason, msg string) {
	meta.SetStatusCondition(&in.Status.Conditions, metav1.Condition{Type: condReady, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg})
}

func argvCommand(argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}
}
```

Create `internal/cli/render_test.go`:

```go
package cli

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func TestCleanEscapesWhatCouldHideText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":       "plain text",
		"red\x1b[31mtext":  `red\u001b[31mtext`,
		"line\nbreak":      `line\u000abreak`,
		"carriage\rreturn": `carriage\u000dreturn`,
		"evil\u202etxt":    `evil\u202etxt`,
		"café ✓":           "café ✓",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellWordQuotesAmbiguousArguments(t *testing.T) {
	for in, want := range map[string]string{
		"kubectl":        "kubectl",
		"/work/web.yaml": "/work/web.yaml",
		"--timeout=3m":   "--timeout=3m",
		"a b":            `"a b"`,
		"$(rm -rf /)":    `"$(rm -rf /)"`,
		"":               `""`,
		"x\u202ey":       `"x\u202ey"`,
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderPlanShowsWhatTheApproverDecides(t *testing.T) {
	p := &lastv1alpha1.Plan{Hash: h2, Summary: "nginx \x1b[8mhidden", Steps: []lastv1alpha1.Command{
		{ID: "apply", Reason: "create it", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "apply", "-f", "/work/web.yaml"}},
			Files: map[string]string{"/work/web.yaml": "kind: Deployment\n"}, Providers: []string{"k8s"},
			Egress: []string{"10.0.0.1:443"}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
		{ID: "check", Run: lastv1alpha1.CommandRun{Script: "set -e\necho done \x1b[2K"}},
	}, Resources: []lastv1alpha1.ManagedResourceSpec{{
		Description: "web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "ready", Command: argvCommand("kubectl", "get", "deploy", "web"),
			Assert: "result.exitCode == 0"}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"ip": "${steps.svc.json.spec.clusterIP}"},
	}}}
	var b strings.Builder
	renderPlan(&b, p)
	got := b.String()
	for _, want := range []string{
		"Plan " + h2 + " (stage Create)\n",
		"Summary: nginx \\u001b[8mhidden\n",
		"  1. apply: create it\n",
		"     run:       kubectl apply -f /work/web.yaml\n",
		"     file:      /work/web.yaml (17 bytes)\n",
		"     providers: k8s\n",
		"     egress:    10.0.0.1:443\n",
		"     timeout:   2m0s\n",
		"  2. check\n",
		"     script:\n",
		"       | set -e\n",
		"       | echo done \\u001b[2K\n",
		"  - web\n",
		"    identity: {\"kind\":\"Deployment\"}\n",
		"    probe ready, assert result.exitCode == 0\n",
		"      run:       kubectl get deploy web\n",
		"    outputs to Secret web-outputs:\n",
		"      ip = ${steps.svc.json.spec.clusterIP}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Error("an escape sequence reached the terminal")
	}
	if strings.Contains(got, "kind: Deployment") {
		t.Error("file content is shown; files are listed by path and size")
	}
}

func TestRenderPendingShowsTheCommandAndItsReason(t *testing.T) {
	var b strings.Builder
	renderPending(&b, &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand("kubectl", "get", "ns")})
	want := "Discovery command " + h1 + " (stage Discovery)\nReason: see what exists\n  run:       kubectl get ns\n"
	if b.String() != want {
		t.Fatalf("pending command =\n%s\nwant\n%s", b.String(), want)
	}
}
```

Create `internal/cli/plan_test.go`:

```go
package cli

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func TestPlanShowsThePendingCommandOrThePlan(t *testing.T) {
	look := testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand("kubectl", "get", "ns")}
		setReady(in, "AwaitingDiscoveryApproval", "Waiting for an IntentApproval of discovery command "+h1)
	})
	web := testIntent("web", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: "nginx", Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}}}
	})
	fresh := testIntent("fresh", nil)
	d, out := depsFor(fakeClient(look, web, fresh), "")

	if err := execute(d, "plan", "look"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Intent team-x/look: AwaitingDiscoveryApproval (Waiting for an IntentApproval of discovery command " + h1 + ")\n",
		"Discovery command " + h1} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan look lacks %q:\n%s", want, out)
		}
	}
	out.Reset()
	if err := execute(d, "plan", "web"); err != nil || !strings.Contains(out.String(), "Plan "+h2) {
		t.Fatalf("plan web = %v:\n%s", err, out)
	}
	out.Reset()
	if err := execute(d, "plan", "fresh"); err != nil || out.String() != "Intent team-x/fresh: Pending\nNo plan or pending command yet.\n" {
		t.Fatalf("plan fresh = %v: %q", err, out)
	}
	if err := execute(d, "plan", "gone"); !apierrors.IsNotFound(err) {
		t.Fatalf("plan gone = %v, want NotFound", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/`
Expected: FAIL to compile. These are undefined: `newScheme`, `KubeFlags`, `Kube`, `condReady`, `clean`, `shellWord`, `renderPlan`, `renderPending`, and the `Deps` fields.

- [ ] **Step 3: Write the cluster access**

Create `internal/cli/kube.go`:

```go
package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// KubeFlags are the kubectl connection flags the Intent commands take.
type KubeFlags struct {
	Kubeconfig, Context, Namespace string
}

func (f *KubeFlags) addTo(cmd *cobra.Command) {
	fs := cmd.PersistentFlags()
	fs.StringVar(&f.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&f.Context, "context", "", "the kubeconfig context to use")
	fs.StringVarP(&f.Namespace, "namespace", "n", "", "the Intent's namespace (default: the context's namespace)")
}

// Kube is the cluster as the user sees it: every call acts with their
// kubeconfig identity, never the gateway's (M3 spec §5).
type Kube struct {
	Client    client.Client
	Namespace string
	// WhoAmI returns the user's name as the API server knows it.
	WhoAmI func(context.Context) (string, error)
}

// NewKube connects with the user's kubeconfig.
func NewKube(f KubeFlags) (*Kube, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = f.Kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: f.Context})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load the kubeconfig: %w", err)
	}
	ns := f.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, fmt.Errorf("load the kubeconfig: %w", err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Kube{Client: c, Namespace: ns, WhoAmI: func(ctx context.Context) (string, error) {
		r, err := kc.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("ask the API server who you are (SelfSubjectReview): %w", err)
		}
		return r.Status.UserInfo.Username, nil
	}}, nil
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(lastv1alpha1.AddToScheme(s))
	return s
}

// getIntent reads one Intent in the user's namespace.
func getIntent(ctx context.Context, k *Kube, name string) (*lastv1alpha1.Intent, error) {
	var in lastv1alpha1.Intent
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: name}, &in); err != nil {
		return nil, err
	}
	return &in, nil
}
```

In `internal/cli/root.go`:

1. Add the imports `"crypto/rand"`, `"strings"` and `"time"`.
2. Replace `Deps`, `DefaultDeps` and `NewRootCommand` with:

```go
// pollInterval is how often a followed Intent is read again.
const pollInterval = 2 * time.Second

// Deps are the CLI's external dependencies, injectable for tests.
type Deps struct {
	Out, Err    io.Writer
	In          io.Reader
	Getenv      func(string) string
	NewSink     func(context.Context, audit.S3Config) (audit.Sink, error)
	NewS3Client func(context.Context, audit.S3Config) (*s3.Client, error)
	// Kube connects to the cluster as the user.
	Kube func(KubeFlags) (*Kube, error)
	// Wait pauses between reads of a followed Intent and returns ctx's error if it ends first.
	Wait func(context.Context) error
	// Suffix returns the random part of a generated Intent name.
	Suffix func() string
}

func DefaultDeps() Deps {
	return Deps{
		Out: os.Stdout, Err: os.Stderr, In: os.Stdin, Getenv: os.Getenv,
		NewS3Client: audit.NewS3Client,
		NewSink: func(ctx context.Context, cfg audit.S3Config) (audit.Sink, error) {
			c, err := audit.NewS3Client(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return audit.NewS3Sink(c, cfg.Bucket), nil
		},
		Kube:   NewKube,
		Wait:   pollWait,
		Suffix: func() string { return strings.ToLower(rand.Text()[:4]) },
	}
}

func pollWait(ctx context.Context) error {
	t := time.NewTimer(pollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func NewRootCommand(d Deps) *cobra.Command {
	kf := &KubeFlags{}
	root := &cobra.Command{
		Use:           "kubectl-last",
		Short:         "Work with last-operator",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	kf.addTo(root)
	root.AddCommand(newAuditCommand(d), newPlanCommand(d, kf))
	return root
}
```

- [ ] **Step 4: Write the renderers and the `plan` command**

Create `internal/cli/render.go`:

```go
package cli

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/api/meta"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// condReady is the Intent condition whose message says what the Intent waits for.
const condReady = "Ready"

// clean makes text the model wrote safe to show an approver. Control
// characters (escape sequences, carriage returns, newlines) and bidirectional
// overrides could hide or reorder what is shown, so they are written as
// \uXXXX.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || isBidi(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isBidi(r rune) bool {
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWord writes one argv element, quoted when it would be ambiguous
// unquoted. strconv.Quote escapes control characters and overrides too.
func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// renderPlan writes a plan as its approver reviews it (M3 spec §5): the
// summary, each step, and each resource with its probes and outputs.
func renderPlan(w io.Writer, p *lastv1alpha1.Plan) {
	fmt.Fprintf(w, "Plan %s (stage Create)\n", p.Hash)
	if p.Summary != "" {
		fmt.Fprintf(w, "Summary: %s\n", clean(p.Summary))
	}
	fmt.Fprintln(w, "Steps:")
	for i, s := range p.Steps {
		fmt.Fprintf(w, "  %d. %s", i+1, clean(s.ID))
		if s.Reason != "" {
			fmt.Fprintf(w, ": %s", clean(s.Reason))
		}
		fmt.Fprintln(w)
		renderCommand(w, "     ", s)
	}
	if len(p.Resources) > 0 {
		fmt.Fprintln(w, "Resources:")
	}
	for _, r := range p.Resources {
		fmt.Fprintf(w, "  - %s\n", clean(r.Description))
		if r.Identity != nil {
			fmt.Fprintf(w, "    identity: %s\n", clean(string(r.Identity.Raw)))
		}
		if r.Desired != nil {
			fmt.Fprintf(w, "    desired:  %s\n", clean(string(r.Desired.Raw)))
		}
		for _, pr := range r.Probes {
			fmt.Fprintf(w, "    probe %s, assert %s\n", clean(pr.ID), clean(pr.Assert))
			renderCommand(w, "      ", pr.Command)
		}
		if r.OutputsSecretRef != nil {
			fmt.Fprintf(w, "    outputs to Secret %s:\n", clean(r.OutputsSecretRef.Name))
			for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
				fmt.Fprintf(w, "      %s = %s\n", clean(k), clean(r.Outputs[k]))
			}
		}
	}
}

// renderPending writes a discovery command awaiting approval, with the
// model's reason for it.
func renderPending(w io.Writer, pc *lastv1alpha1.PendingCommand) {
	fmt.Fprintf(w, "Discovery command %s (stage Discovery)\n", pc.Hash)
	if pc.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", clean(pc.Reason))
	}
	renderCommand(w, "  ", pc.Command)
}

// renderCommand writes one command: argv or script, files by path and size,
// Providers, egress and timeout.
func renderCommand(w io.Writer, indent string, c lastv1alpha1.Command) {
	if len(c.Run.Argv) > 0 {
		words := make([]string, len(c.Run.Argv))
		for i, a := range c.Run.Argv {
			words[i] = shellWord(a)
		}
		fmt.Fprintf(w, "%srun:       %s\n", indent, strings.Join(words, " "))
	} else {
		fmt.Fprintf(w, "%sscript:\n", indent)
		for _, line := range strings.Split(c.Run.Script, "\n") {
			fmt.Fprintf(w, "%s  | %s\n", indent, clean(line))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(c.Files)) {
		fmt.Fprintf(w, "%sfile:      %s (%d bytes)\n", indent, clean(path), len(c.Files[path]))
	}
	if len(c.Providers) > 0 {
		fmt.Fprintf(w, "%sproviders: %s\n", indent, clean(strings.Join(c.Providers, ", ")))
	}
	if len(c.Egress) > 0 {
		fmt.Fprintf(w, "%segress:    %s\n", indent, clean(strings.Join(c.Egress, ", ")))
	}
	if c.Timeout != nil {
		fmt.Fprintf(w, "%stimeout:   %s\n", indent, c.Timeout.Duration)
	}
}

// renderSubject writes what the Intent asks its user to decide in stage.
func renderSubject(w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) {
	if stage == lastv1alpha1.StageDiscovery {
		renderPending(w, in.Status.PendingCommand)
		return
	}
	renderPlan(w, in.Status.Plan)
}

// printStatus writes the Intent's phase and, from its Ready condition, why.
func printStatus(w io.Writer, in *lastv1alpha1.Intent) {
	fmt.Fprintf(w, "Intent %s/%s: %s", in.Namespace, in.Name, phaseOf(in))
	if c := meta.FindStatusCondition(in.Status.Conditions, condReady); c != nil && c.Message != "" {
		fmt.Fprintf(w, " (%s)", clean(c.Message))
	}
	fmt.Fprintln(w)
}

// phaseOf is the Intent's phase; a new Intent the operator has not seen yet is Pending.
func phaseOf(in *lastv1alpha1.Intent) lastv1alpha1.Phase {
	if in.Status.Phase == "" {
		return lastv1alpha1.PhasePending
	}
	return in.Status.Phase
}
```

Create `internal/cli/plan.go`:

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func newPlanCommand(d Deps, kf *KubeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "plan NAME",
		Short: "Show an Intent's pending discovery command or its plan (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			printStatus(d.Out, in)
			switch {
			case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil:
				renderPending(d.Out, in.Status.PendingCommand)
			case in.Status.Plan != nil:
				renderPlan(d.Out, in.Status.Plan)
			default:
				fmt.Fprintln(d.Out, "No plan or pending command yet.")
			}
			return nil
		},
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/`
Expected: PASS, including the existing audit tests.

Run: `go build ./...`
Expected: no output.

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/cli/root.go internal/cli/kube.go internal/cli/render.go internal/cli/plan.go \
  internal/cli/kube_test.go internal/cli/render_test.go internal/cli/plan_test.go
git commit -m "feat(cli): connect as the user, render plans safely for approvers, and add kubectl last plan"
```

---
### Task 3: `kubectl last approve` and `deny`

`approve NAME` and `deny NAME --reason R` decide exactly what `plan NAME` shows:
- the pending command, in phase AwaitingDiscoveryApproval, with stage Discovery;
- the plan, in phase AwaitingApproval, with stage Create.

The command:
1. prints the subject;
2. creates an IntentApproval for that hash, as the user (`approvedBy` comes from `WhoAmI`);
3. prints the hash it acted on;
4. reads the Intent again, and says so if the Intent now asks about something else, because the decision then matches nothing.

`--hash H` lets a script pin the subject. If the Intent asks about anything other than H, nothing is recorded.

The follow loop (Task 5) reuses `subjectOf` and `decide`.

**Files:**
- Create: `internal/cli/decide.go`
- Modify: `internal/cli/root.go` (register the commands)
- Test: `internal/cli/decide_test.go`

**Interfaces:**
- Consumes (Task 2): `Kube`, `KubeFlags`, `getIntent`, `renderSubject`, `phaseOf`, `Deps.Kube`; in tests, `fakeClient`, `depsFor`, `testIntent`, `argvCommand`, `h1`, `h2`, `testUser`, `testNS` and `execute`.
- Produces:
  - `func subjectOf(in *lastv1alpha1.Intent) (lastv1alpha1.Stage, string, bool)`;
  - `func subjectNoun(stage lastv1alpha1.Stage) string`, returning `"discovery command"` or `"plan"`;
  - `func decide(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string, decision lastv1alpha1.Decision, reason string) error`. It prints `Approved <noun> <subject> as <user> (IntentApproval <name>)`, or `Denied …`.
  - In tests: `h3`, `pendingIntent()`, `plannedIntent()`, `approvalsIn(t, c)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/decide_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var h3 = "sha256:" + strings.Repeat("3", 64)

func pendingIntent() *lastv1alpha1.Intent {
	return testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand("kubectl", "get", "ns")}
	})
}

func plannedIntent() *lastv1alpha1.Intent {
	return testIntent("web", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: "nginx", Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}}}
	})
}

func approvalsIn(t *testing.T, c client.Client) []lastv1alpha1.IntentApproval {
	t.Helper()
	var list lastv1alpha1.IntentApprovalList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestApproveRecordsTheDisplayedSubjectAsTheUser(t *testing.T) {
	for _, tc := range []struct {
		in             *lastv1alpha1.Intent
		stage          lastv1alpha1.Stage
		subject, shown string
	}{
		{pendingIntent(), lastv1alpha1.StageDiscovery, h1, "Discovery command " + h1},
		{plannedIntent(), lastv1alpha1.StageCreate, h2, "Plan " + h2},
	} {
		c := fakeClient(tc.in)
		d, out := depsFor(c, "")
		if err := execute(d, "approve", tc.in.Name); err != nil {
			t.Fatalf("approve %s: %v", tc.in.Name, err)
		}
		got := approvalsIn(t, c)
		if len(got) != 1 {
			t.Fatalf("approve %s created %d IntentApprovals", tc.in.Name, len(got))
		}
		s := got[0].Spec
		if s.IntentRef.Name != tc.in.Name || s.Stage != tc.stage || s.SubjectHash != tc.subject ||
			s.Decision != lastv1alpha1.DecisionApprove || s.ApprovedBy != testUser || s.Reason != "" {
			t.Fatalf("approval = %+v", s)
		}
		want := "Approved " + subjectNoun(tc.stage) + " " + tc.subject + " as " + testUser
		for _, w := range []string{tc.shown, want} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("approve %s output lacks %q:\n%s", tc.in.Name, w, out)
			}
		}
	}
}

func TestDenyNeedsAReason(t *testing.T) {
	c := fakeClient(pendingIntent())
	d, out := depsFor(c, "")
	if err := execute(d, "deny", "look"); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("deny without --reason = %v", err)
	}
	if err := execute(d, "deny", "look", "--reason", "  "); err == nil {
		t.Fatal("deny with a blank reason succeeded")
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Fatalf("%d IntentApprovals after refused denials", n)
	}
	if err := execute(d, "deny", "look", "--reason", "too broad"); err != nil {
		t.Fatal(err)
	}
	got := approvalsIn(t, c)
	if len(got) != 1 || got[0].Spec.Decision != lastv1alpha1.DecisionDeny || got[0].Spec.Reason != "too broad" ||
		got[0].Spec.Stage != lastv1alpha1.StageDiscovery || got[0].Spec.SubjectHash != h1 {
		t.Fatalf("approvals = %+v", got)
	}
	if !strings.Contains(out.String(), "Denied discovery command "+h1) {
		t.Errorf("deny output:\n%s", out)
	}
}

func TestDecisionsNeedSomethingToDecide(t *testing.T) {
	planning := testIntent("web", func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhasePlanning })
	d, _ := depsFor(fakeClient(planning), "")
	if err := execute(d, "approve", "web"); err == nil || !strings.Contains(err.Error(), "has nothing to decide (phase Planning)") {
		t.Fatalf("approve while planning = %v", err)
	}
}

func TestApproveRefusesAHashThatIsNotTheCurrentSubject(t *testing.T) {
	c := fakeClient(pendingIntent())
	d, _ := depsFor(c, "")
	err := execute(d, "approve", "look", "--hash", h2)
	if err == nil || !strings.Contains(err.Error(), "asks about "+h1) {
		t.Fatalf("approve --hash of another subject = %v", err)
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Fatalf("%d IntentApprovals after a refused approval", n)
	}
	if err := execute(d, "approve", "look", "--hash", h1); err != nil {
		t.Fatalf("approve --hash of the current subject = %v", err)
	}
}

func TestDecideSaysWhenTheSubjectMovedOn(t *testing.T) {
	base := fakeClient(pendingIntent())
	// The model asks for another command while the user decides.
	c := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := cl.Create(ctx, obj, opts...); err != nil {
			return err
		}
		var in lastv1alpha1.Intent
		if err := cl.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "look"}, &in); err != nil {
			return err
		}
		in.Status.PendingCommand.Hash = h3
		return cl.Status().Update(ctx, &in)
	}})
	d, out := depsFor(c, "")
	if err := execute(d, "approve", "look"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approved discovery command " + h1, "now asks about " + h3, "matches nothing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'Approve|Deny|Decision|Decide'`
Expected: FAIL to compile (`undefined: subjectNoun`), or, once that compiles, `unknown command "approve"`.

- [ ] **Step 3: Write the decisions**

Create `internal/cli/decide.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// subjectOf returns what the Intent asks its user to decide: the pending
// discovery command in AwaitingDiscoveryApproval, the plan in AwaitingApproval.
func subjectOf(in *lastv1alpha1.Intent) (lastv1alpha1.Stage, string, bool) {
	switch {
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil:
		return lastv1alpha1.StageDiscovery, in.Status.PendingCommand.Hash, true
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && in.Status.Plan != nil:
		return lastv1alpha1.StageCreate, in.Status.Plan.Hash, true
	}
	return "", "", false
}

func subjectNoun(stage lastv1alpha1.Stage) string {
	if stage == lastv1alpha1.StageDiscovery {
		return "discovery command"
	}
	return "plan"
}

// decide records the user's decision on exactly subject, the hash they were
// shown, never on whatever the Intent holds by now (M3 spec §5). If the
// Intent has moved on to another subject, the decision matches nothing, and
// decide says so.
func decide(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage,
	subject string, decision lastv1alpha1.Decision, reason string) error {
	user, err := k.WhoAmI(ctx)
	if err != nil {
		return err
	}
	a := &lastv1alpha1.IntentApproval{
		ObjectMeta: metav1.ObjectMeta{GenerateName: in.Name + "-" + strings.ToLower(string(stage)) + "-", Namespace: in.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: in.Name}, Stage: stage,
			SubjectHash: subject, Decision: decision, Reason: reason, ApprovedBy: user},
	}
	if err := k.Client.Create(ctx, a); err != nil {
		return fmt.Errorf("record the decision: %w", err)
	}
	verb := "Approved"
	if decision == lastv1alpha1.DecisionDeny {
		verb = "Denied"
	}
	fmt.Fprintf(w, "%s %s %s as %s (IntentApproval %s)\n", verb, subjectNoun(stage), subject, user, a.Name)
	var cur lastv1alpha1.Intent
	if err := k.Client.Get(ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
		return err
	}
	if _, now, ok := subjectOf(&cur); ok && now != subject {
		fmt.Fprintf(w, "The Intent changed while you decided: it now asks about %s, so this decision matches nothing. "+
			"See it with kubectl last plan %s.\n", now, in.Name)
	}
	return nil
}

// newDecisionCommand builds approve or deny: both decide exactly what
// kubectl last plan shows.
func newDecisionCommand(d Deps, kf *KubeFlags, decision lastv1alpha1.Decision) *cobra.Command {
	var hash, reason string
	cmd := &cobra.Command{
		Use:   "approve NAME",
		Short: "Approve the plan or discovery command that kubectl last plan shows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if decision == lastv1alpha1.DecisionDeny && strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required: the model reads it")
			}
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			stage, subject, ok := subjectOf(in)
			if !ok {
				return fmt.Errorf("intent %s/%s has nothing to decide (phase %s)", in.Namespace, in.Name, phaseOf(in))
			}
			if hash != "" && hash != subject {
				return fmt.Errorf("intent %s/%s asks about %s, not %s; nothing was recorded", in.Namespace, in.Name, subject, hash)
			}
			renderSubject(d.Out, in, stage)
			return decide(cmd.Context(), k, d.Out, in, stage, subject, decision, reason)
		},
	}
	cmd.Flags().StringVar(&hash, "hash", "", "decide only if the Intent still asks about this hash")
	if decision == lastv1alpha1.DecisionDeny {
		cmd.Use = "deny NAME --reason REASON"
		cmd.Short = "Deny the plan or discovery command that kubectl last plan shows"
		cmd.Flags().StringVar(&reason, "reason", "", "why (required; the model reads it)")
	}
	return cmd
}
```

In `internal/cli/root.go`, change the `AddCommand` line in `NewRootCommand` to:

```go
	root.AddCommand(newAuditCommand(d), newPlanCommand(d, kf),
		newDecisionCommand(d, kf, lastv1alpha1.DecisionApprove), newDecisionCommand(d, kf, lastv1alpha1.DecisionDeny))
```

and add the import `lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/cli/decide.go internal/cli/decide_test.go internal/cli/root.go
git commit -m "feat(cli): approve and deny exactly the subject kubectl last plan shows"
```

---
### Task 4: Prompts and `kubectl last answer`

**The prompter** reads the user's answers line by line from `Deps.In`.
- A goroutine does the reading, so a waiting prompt still gives way to Ctrl-C (the context).
- If input ends, the prompt returns `errNoInput`.
- `askQuestion` shows a numbered menu when a question has options (a number or the exact option text answers it), and asks for a free-text line otherwise. It asks again until it gets a usable answer.
- `askDecision` asks `Approve? [y/N/d]`:
  - `y` approves;
  - `n` or Enter leaves the subject pending;
  - `d` asks for a reason, then denies.

**`kubectl last answer NAME`** asks the open questions that have no answer yet. It records the answers in `spec.answers` and keeps the earlier ones. The operator refuses a new question that reuses an answered id, so it relies on that record.

**Files:**
- Create: `internal/cli/prompt.go`, `internal/cli/answer.go`
- Modify: `internal/cli/root.go` (register `answer`)
- Test: `internal/cli/prompt_test.go`, `internal/cli/answer_test.go`

**Interfaces:**
- Consumes:
  - Task 2: `Kube`, `KubeFlags`, `getIntent`, `clean`, `phaseOf`, `Deps.In`, `Deps.Kube`; in tests, `fakeClient`, `depsFor`, `testIntent` and `execute`.
  - Task 3: the `AddCommand` line in `root.go`.
- Produces:
  - `type prompter struct{ w io.Writer; lines chan string }`, with `newPrompter(r io.Reader, w io.Writer) *prompter` and these methods:
    - `(p *prompter) ask(ctx, prompt string) (string, error)`;
    - `askQuestion(ctx, q lastv1alpha1.Question) (string, error)`;
    - `askDecision(ctx) (lastv1alpha1.Decision, string, error)`, where an empty decision means "leave pending".
  - `var errNoInput`.
  - `func hasAnswer(in *lastv1alpha1.Intent, id string) bool`.
  - `func mergeAnswers(old, add []lastv1alpha1.Answer) []lastv1alpha1.Answer`.
  - `func answerQuestions(ctx context.Context, k *Kube, p *prompter, in *lastv1alpha1.Intent) error`.
  - The `answer` command.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/prompt_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func newTestPrompter(input string) (*prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return newPrompter(strings.NewReader(input), &out), &out
}

func TestAskQuestionWithOptions(t *testing.T) {
	q := lastv1alpha1.Question{ID: "q1", Text: "Which tier?", Options: []string{"internal", "public"}}
	p, out := newTestPrompter("9\nfoo\n2\n")
	got, err := p.askQuestion(context.Background(), q)
	if err != nil || got != "public" {
		t.Fatalf("askQuestion = %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "Which tier?\n  1) internal\n  2) public\n") || !strings.Contains(out.String(), "Answer [1-2]: ") {
		t.Errorf("menu:\n%s", out)
	}
	if n := strings.Count(out.String(), "Enter a number from 1 to 2."); n != 2 {
		t.Errorf("re-asked %d times, want 2:\n%s", n, out)
	}
	p, _ = newTestPrompter("internal\n")
	if got, err := p.askQuestion(context.Background(), q); err != nil || got != "internal" {
		t.Fatalf("askQuestion by option text = %q, %v", got, err)
	}
}

func TestAskQuestionFreeText(t *testing.T) {
	p, out := newTestPrompter("\n  port 80  \n")
	got, err := p.askQuestion(context.Background(), lastv1alpha1.Question{ID: "q2", Text: "Which port?"})
	if err != nil || got != "port 80" {
		t.Fatalf("askQuestion = %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "An answer cannot be empty.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAskDecision(t *testing.T) {
	for input, want := range map[string]struct {
		decision lastv1alpha1.Decision
		reason   string
	}{
		"y\n":                       {lastv1alpha1.DecisionApprove, ""},
		"YES\n":                     {lastv1alpha1.DecisionApprove, ""},
		"\n":                        {"", ""},
		"n\n":                       {"", ""},
		"maybe\nd\n\ntoo broad\n":   {lastv1alpha1.DecisionDeny, "too broad"},
	} {
		p, out := newTestPrompter(input)
		decision, reason, err := p.askDecision(context.Background())
		if err != nil || decision != want.decision || reason != want.reason {
			t.Errorf("input %q: askDecision = %q, %q, %v", input, decision, reason, err)
		}
		if input == "maybe\nd\n\ntoo broad\n" &&
			(!strings.Contains(out.String(), "Answer y to approve") || !strings.Contains(out.String(), "A reason cannot be empty.")) {
			t.Errorf("re-asks:\n%s", out)
		}
	}
}

func TestAskGivesWayToCtrlC(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newPrompter(r, io.Discard).ask(ctx, "? "); !errors.Is(err, context.Canceled) {
		t.Fatalf("ask after Ctrl-C = %v", err)
	}
}

func TestAskReportsTheEndOfInput(t *testing.T) {
	p, _ := newTestPrompter("")
	if _, err := p.ask(context.Background(), "? "); !errors.Is(err, errNoInput) {
		t.Fatalf("ask at the end of input = %v", err)
	}
}
```

Create `internal/cli/answer_test.go`:

```go
package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func clarifying(answers ...lastv1alpha1.Answer) *lastv1alpha1.Intent {
	return testIntent("web", func(in *lastv1alpha1.Intent) {
		in.Spec.Answers = answers
		in.Status.Phase = lastv1alpha1.PhaseClarifying
		in.Status.Questions = []lastv1alpha1.Question{
			{ID: "q1", Text: "Which tier?", Options: []string{"internal", "public"}},
			{ID: "q2", Text: "Which port?"},
		}
	})
}

func answersOf(t *testing.T, c client.Client) []lastv1alpha1.Answer {
	t.Helper()
	var in lastv1alpha1.Intent
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "web"}, &in); err != nil {
		t.Fatal(err)
	}
	return in.Spec.Answers
}

func TestAnswerRecordsEveryAnswerAndKeepsEarlierOnes(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q0", Value: "an earlier round"}))
	d, out := depsFor(c, "1\nport 80\n")
	if err := execute(d, "answer", "web"); err != nil {
		t.Fatal(err)
	}
	want := []lastv1alpha1.Answer{{ID: "q0", Value: "an earlier round"}, {ID: "q1", Value: "internal"}, {ID: "q2", Value: "port 80"}}
	if got := answersOf(t, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("spec.answers = %+v, want %+v", got, want)
	}
	if !strings.Contains(out.String(), "Recorded 2 answers.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAnswerAsksOnlyUnansweredQuestions(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q1", Value: "public"}))
	d, out := depsFor(c, "port 80\n")
	if err := execute(d, "answer", "web"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Which tier?") {
		t.Errorf("asked an answered question:\n%s", out)
	}
	want := []lastv1alpha1.Answer{{ID: "q1", Value: "public"}, {ID: "q2", Value: "port 80"}}
	if got := answersOf(t, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("spec.answers = %+v, want %+v", got, want)
	}
}

func TestAnswerWhenEveryQuestionHasAnAnswer(t *testing.T) {
	c := fakeClient(clarifying(lastv1alpha1.Answer{ID: "q1", Value: "public"}, lastv1alpha1.Answer{ID: "q2", Value: "80"}))
	d, out := depsFor(c, "")
	if err := execute(d, "answer", "web"); err != nil || !strings.Contains(out.String(), "Every question already has an answer") {
		t.Fatalf("answer = %v:\n%s", err, out)
	}
}

func TestAnswerNeedsOpenQuestions(t *testing.T) {
	planning := testIntent("web", func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhasePlanning })
	d, _ := depsFor(fakeClient(planning), "")
	if err := execute(d, "answer", "web"); err == nil || !strings.Contains(err.Error(), "has no open questions (phase Planning)") {
		t.Fatalf("answer while planning = %v", err)
	}
}

func TestMergeAnswersReplacesByID(t *testing.T) {
	got := mergeAnswers([]lastv1alpha1.Answer{{ID: "a", Value: "1"}, {ID: "b", Value: "2"}},
		[]lastv1alpha1.Answer{{ID: "b", Value: "3"}, {ID: "c", Value: "4"}})
	want := []lastv1alpha1.Answer{{ID: "a", Value: "1"}, {ID: "b", Value: "3"}, {ID: "c", Value: "4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeAnswers = %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'Ask|Answer|MergeAnswers'`
Expected: FAIL to compile (`undefined: newPrompter`, `undefined: errNoInput`, `undefined: mergeAnswers`).

- [ ] **Step 3: Write the prompter**

Create `internal/cli/prompt.go`:

```go
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// errNoInput means the input ended while a prompt waited for an answer.
var errNoInput = errors.New("input ended")

// prompter reads the user's answers line by line. A goroutine does the
// reading, so a waiting prompt still gives way to Ctrl-C.
type prompter struct {
	w     io.Writer
	lines chan string // closed at the end of input
}

func newPrompter(r io.Reader, w io.Writer) *prompter {
	p := &prompter{w: w, lines: make(chan string)}
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			p.lines <- s.Text()
		}
		close(p.lines)
	}()
	return p
}

// ask prints prompt and returns the next line, trimmed.
func (p *prompter) ask(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(p.w, prompt)
	select {
	case <-ctx.Done():
		fmt.Fprintln(p.w)
		return "", ctx.Err()
	case line, ok := <-p.lines:
		if !ok {
			fmt.Fprintln(p.w)
			return "", errNoInput
		}
		return strings.TrimSpace(line), nil
	}
}

// askQuestion asks one clarifying question: a numbered menu when it has
// options, otherwise a free-text line. It asks again until the answer is usable.
func (p *prompter) askQuestion(ctx context.Context, q lastv1alpha1.Question) (string, error) {
	fmt.Fprintf(p.w, "\n%s\n", clean(q.Text))
	for i, o := range q.Options {
		fmt.Fprintf(p.w, "  %d) %s\n", i+1, clean(o))
	}
	prompt := "Answer: "
	if len(q.Options) > 0 {
		prompt = fmt.Sprintf("Answer [1-%d]: ", len(q.Options))
	}
	for {
		line, err := p.ask(ctx, prompt)
		if err != nil {
			return "", err
		}
		if len(q.Options) == 0 {
			if line != "" {
				return line, nil
			}
			fmt.Fprintln(p.w, "An answer cannot be empty.")
			continue
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1], nil
		}
		if slices.Contains(q.Options, line) {
			return line, nil
		}
		fmt.Fprintf(p.w, "Enter a number from 1 to %d.\n", len(q.Options))
	}
}

// askDecision asks whether to approve what was just shown. It returns
// DecisionApprove, DecisionDeny with a reason, or "" to leave it pending.
func (p *prompter) askDecision(ctx context.Context) (lastv1alpha1.Decision, string, error) {
	for {
		line, err := p.ask(ctx, "Approve? [y/N/d]: ")
		if err != nil {
			return "", "", err
		}
		switch strings.ToLower(line) {
		case "y", "yes":
			return lastv1alpha1.DecisionApprove, "", nil
		case "", "n", "no":
			return "", "", nil
		case "d", "deny":
			for {
				reason, err := p.ask(ctx, "Reason for denying (the model reads it): ")
				if err != nil {
					return "", "", err
				}
				if reason != "" {
					return lastv1alpha1.DecisionDeny, reason, nil
				}
				fmt.Fprintln(p.w, "A reason cannot be empty.")
			}
		}
		fmt.Fprintln(p.w, "Answer y to approve, n to leave it pending, or d to deny.")
	}
}
```

- [ ] **Step 4: Write `answer`**

Create `internal/cli/answer.go`:

```go
package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func hasAnswer(in *lastv1alpha1.Intent, id string) bool {
	return slices.ContainsFunc(in.Spec.Answers, func(a lastv1alpha1.Answer) bool { return a.ID == id })
}

// mergeAnswers returns old with each answer in add replacing the one with its
// id, or appended.
func mergeAnswers(old, add []lastv1alpha1.Answer) []lastv1alpha1.Answer {
	out := slices.Clone(old)
	for _, a := range add {
		if i := slices.IndexFunc(out, func(o lastv1alpha1.Answer) bool { return o.ID == a.ID }); i >= 0 {
			out[i] = a
			continue
		}
		out = append(out, a)
	}
	return out
}

// answerQuestions asks every open question that has no answer yet, then
// records the answers in spec.answers. Earlier answers stay: the operator
// refuses a new question that reuses an answered id, and relies on that record.
func answerQuestions(ctx context.Context, k *Kube, p *prompter, in *lastv1alpha1.Intent) error {
	var add []lastv1alpha1.Answer
	for _, q := range in.Status.Questions {
		if hasAnswer(in, q.ID) {
			continue
		}
		v, err := p.askQuestion(ctx, q)
		if err != nil {
			return err
		}
		add = append(add, lastv1alpha1.Answer{ID: q.ID, Value: v})
	}
	if len(add) == 0 {
		fmt.Fprintln(p.w, "Every question already has an answer; the operator continues planning.")
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur lastv1alpha1.Intent
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
			return err
		}
		cur.Spec.Answers = mergeAnswers(cur.Spec.Answers, add)
		return k.Client.Update(ctx, &cur)
	})
	if err != nil {
		return fmt.Errorf("record the answers: %w", err)
	}
	fmt.Fprintf(p.w, "Recorded %d answers.\n", len(add))
	return nil
}

func newAnswerCommand(d Deps, kf *KubeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "answer NAME",
		Short: "Answer an Intent's open clarifying questions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			if in.Status.Phase != lastv1alpha1.PhaseClarifying || len(in.Status.Questions) == 0 {
				return fmt.Errorf("intent %s/%s has no open questions (phase %s)", in.Namespace, in.Name, phaseOf(in))
			}
			return answerQuestions(cmd.Context(), k, newPrompter(d.In, d.Out), in)
		},
	}
}
```

In `internal/cli/root.go`, add `newAnswerCommand(d, kf)` to the `AddCommand` call.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/cli/prompt.go internal/cli/answer.go internal/cli/prompt_test.go internal/cli/answer_test.go internal/cli/root.go
git commit -m "feat(cli): prompt for answers and decisions, and add kubectl last answer"
```

---
### Task 5: `kubectl last "<request>"`: create and follow

**`kubectl last "<request>" [-n NS] [--name NAME] [--no-follow]`** creates an Intent.
- The default name is a slug of the request's first words (up to 30 characters) plus a random 4-character suffix.
- A request without a space is refused, so a mistyped subcommand never creates an Intent.

**Following.** Unless `--no-follow` is set, the command reads the Intent every `Deps.Wait` (2 seconds) and prints each phase change. What it does next depends on the phase:

| Phase | What follow does |
|---|---|
| Clarifying | Asks the open questions that have no answer yet. |
| AwaitingDiscoveryApproval | Shows the pending command and asks `Approve? [y/N/d]`. |
| AwaitingApproval | Shows the plan and asks the same. |
| Ready | Lists the Intent's ManagedResources and output Secrets, and returns nil. |
| Failed | Prints the reason and returns `ErrIntentFailed`, so the exit code is 1. |

- Each subject is put to the user once: leaving it pending does not ask again two seconds later.
- When input ends at a prompt, the error names the commands to continue with.
- Ctrl-C stops following and leaves the Intent alone.

**Files:**
- Create: `internal/cli/create.go`, `internal/cli/follow.go`
- Modify: `internal/cli/root.go` (the root command takes the request)
- Test: `internal/cli/create_test.go`, `internal/cli/follow_test.go`

**Interfaces:**
- Consumes:
  - Task 2: `Kube`, `KubeFlags`, `printStatus`, `clean`, `phaseOf`, `renderSubject`, `condReady`, `Deps.Wait`, `Deps.Suffix`, `Deps.In`; the test helpers `fakeClient`, `depsFor`, `setReady`, `argvCommand`, `h1`, `h2`, `testNS`, `testUser`, `execute`.
  - Task 3: `subjectOf`, `decide`, `approvalsIn(t, c)` (test).
  - Task 4: `newPrompter`, `prompter` (`askDecision`, field `w`), `answerQuestions`, `hasAnswer`, `errNoInput`.
- Produces:
  - `var ErrIntentFailed`;
  - `type createFlags struct{ name string; noFollow bool }`;
  - `func runCreate(ctx context.Context, d Deps, kf KubeFlags, cf createFlags, request string) error`;
  - `func intentName(request, suffix string) string`;
  - `func follow(ctx context.Context, d Deps, k *Kube, key client.ObjectKey) error`;
  - `func answered(in *lastv1alpha1.Intent) bool`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/create_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func TestIntentNameSlugsTheFirstWords(t *testing.T) {
	for request, want := range map[string]string{
		"Run an nginx web server serving a hello page": "run-an-nginx-web-server-abcd",
		"  Deploy   Redis!! v7 ":                       "deploy-redis-v7-abcd",
		"!!! ???":                                      "intent-abcd",
		strings.Repeat("a", 50) + " x":                 strings.Repeat("a", 30) + "-abcd",
	} {
		if got := intentName(request, "abcd"); got != want {
			t.Errorf("intentName(%q) = %q, want %q", request, got, want)
		}
	}
}

func intents(t *testing.T, c client.Client) []lastv1alpha1.Intent {
	t.Helper()
	var list lastv1alpha1.IntentList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestCreateWithoutFollowing(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	if err := execute(d, "run nginx", "--no-follow"); err != nil {
		t.Fatal(err)
	}
	got := intents(t, c)
	if len(got) != 1 || got[0].Name != "run-nginx-abcd" || got[0].Spec.Request != "run nginx" {
		t.Fatalf("intents = %+v", got)
	}
	if out.String() != "Created Intent team-x/run-nginx-abcd\n" {
		t.Fatalf("output = %q", out)
	}
}

func TestCreateRefusesAOneWordRequest(t *testing.T) {
	c := fakeClient()
	d, _ := depsFor(c, "")
	for _, args := range [][]string{{"aprove"}, {"aprove", "web"}, {"  "}} {
		if err := execute(d, args...); err == nil {
			t.Errorf("kubectl last %q succeeded", args)
		}
	}
	if n := len(intents(t, c)); n != 0 {
		t.Fatalf("%d Intents created", n)
	}
}
```

Create `internal/cli/follow_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// world plays the operator in follow tests: at every pause between reads it
// applies its next step, once that step's precondition holds.
type world struct {
	t     *testing.T
	c     client.Client
	steps []step
	waits int
}

type step struct {
	ready func(in *lastv1alpha1.Intent) bool // nil: at once
	apply func(in *lastv1alpha1.Intent)
}

func (w *world) wait(ctx context.Context) error {
	if w.waits++; w.waits > 50 {
		return errors.New("the follow loop did not finish")
	}
	if len(w.steps) == 0 {
		return nil
	}
	var in lastv1alpha1.Intent
	if err := w.c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "web"}, &in); err != nil {
		return err
	}
	s := w.steps[0]
	if s.ready != nil && !s.ready(&in) {
		return nil
	}
	s.apply(&in)
	w.steps = w.steps[1:]
	return w.c.Status().Update(ctx, &in)
}

// decided reports whether the user decided on subject.
func (w *world) decided(subject string) func(*lastv1alpha1.Intent) bool {
	return func(*lastv1alpha1.Intent) bool {
		for _, a := range approvalsIn(w.t, w.c) {
			if a.Spec.SubjectHash == subject {
				return true
			}
		}
		return false
	}
}

func TestFollowAsksDecidesAndEndsReady(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "2\ny\ny\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseClarifying
			in.Status.Questions = []lastv1alpha1.Question{{ID: "q1", Text: "Which tier?", Options: []string{"internal", "public"}}}
		}},
		{ready: func(in *lastv1alpha1.Intent) bool { return len(in.Spec.Answers) == 1 }, apply: func(in *lastv1alpha1.Intent) {
			in.Status.Questions = nil
			in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
			in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand("kubectl", "get", "ns")}
		}},
		{ready: w.decided(h1), apply: func(in *lastv1alpha1.Intent) {
			in.Status.PendingCommand = nil
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: "nginx", Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}}}
		}},
		{ready: w.decided(h2), apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseExecuting }},
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseReady
			mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: testNS},
				Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: "web"}, Description: "nginx web",
					OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"}}}
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatal(err)
			}
		}},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", "web"); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}

	var in lastv1alpha1.Intent
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "web"}, &in); err != nil {
		t.Fatal(err)
	}
	if want := []lastv1alpha1.Answer{{ID: "q1", Value: "public"}}; !reflect.DeepEqual(in.Spec.Answers, want) {
		t.Errorf("spec.answers = %+v", in.Spec.Answers)
	}
	approvals := approvalsIn(t, c)
	if len(approvals) != 2 {
		t.Fatalf("approvals = %+v", approvals)
	}
	subjects := map[lastv1alpha1.Stage]string{}
	for _, a := range approvals {
		s := a.Spec
		if s.Decision != lastv1alpha1.DecisionApprove || s.ApprovedBy != testUser || s.IntentRef.Name != "web" {
			t.Errorf("approval = %+v", s)
		}
		subjects[s.Stage] = s.SubjectHash
	}
	if want := map[lastv1alpha1.Stage]string{lastv1alpha1.StageDiscovery: h1, lastv1alpha1.StageCreate: h2}; !reflect.DeepEqual(subjects, want) {
		t.Errorf("approved subjects = %v, want %v", subjects, want)
	}
	for _, want := range []string{
		"Created Intent team-x/web\n", "Intent team-x/web: Clarifying", "Which tier?", "2) public", "Recorded 1 answers.",
		"Discovery command " + h1, "Reason: see what exists", "Approved discovery command " + h1,
		"Plan " + h2, "Approved plan " + h2, "Intent team-x/web: Executing",
		"Intent team-x/web is Ready.", "ManagedResource web-0: nginx web", "outputs: Secret web-outputs",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFollowAsksAboutEachSubjectOnce(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
			in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Command: argvCommand("kubectl", "get", "ns")}
		}},
		{apply: func(*lastv1alpha1.Intent) {}}, // a read later, still waiting for the same decision
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseFailed
			setReady(in, "PlanningFailed", "planning exceeded 20 model turns")
		}},
	}
	d.Wait = w.wait
	err := execute(d, "run an nginx web server", "--name", "web")
	if !errors.Is(err, ErrIntentFailed) {
		t.Fatalf("kubectl last = %v, want ErrIntentFailed", err)
	}
	if n := strings.Count(out.String(), "Approve? [y/N/d]"); n != 1 {
		t.Errorf("asked %d times:\n%s", n, out)
	}
	for _, want := range []string{"Left pending; decide later with kubectl last approve web",
		"Intent team-x/web failed: PlanningFailed: planning exceeded 20 model turns"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Errorf("%d IntentApprovals for a subject left pending", n)
	}
}

func TestFollowStopsAtTheEndOfInput(t *testing.T) {
	c := fakeClient()
	d, _ := depsFor(c, "")
	w := &world{t: t, c: c, steps: []step{{apply: func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseClarifying
		in.Status.Questions = []lastv1alpha1.Question{{ID: "q1", Text: "Which port?"}}
	}}}}
	d.Wait = w.wait
	err := execute(d, "run an nginx web server", "--name", "web")
	if !errors.Is(err, errNoInput) || !strings.Contains(err.Error(), "kubectl last plan web") {
		t.Fatalf("kubectl last = %v", err)
	}
}

func TestCtrlCStopsFollowingAndLeavesTheIntent(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	d.Wait = func(context.Context) error { return context.Canceled }
	if err := execute(d, "run an nginx web server", "--name", "web"); err != nil {
		t.Fatalf("kubectl last = %v", err)
	}
	if !strings.Contains(out.String(), "Stopped following Intent team-x/web; it carries on.") {
		t.Errorf("output:\n%s", out)
	}
	if n := len(intents(t, c)); n != 1 {
		t.Fatalf("%d Intents after Ctrl-C, want the one created", n)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'IntentName|Create|Follow|CtrlC'`
Expected: FAIL to compile (`undefined: intentName`, `undefined: ErrIntentFailed`).

- [ ] **Step 3: Write create**

Create `internal/cli/create.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// maxSlug bounds the words of a generated Intent name, before its suffix.
const maxSlug = 30

type createFlags struct {
	name     string
	noFollow bool
}

// runCreate creates an Intent for request and, unless told not to, follows it.
func runCreate(ctx context.Context, d Deps, kf KubeFlags, cf createFlags, request string) error {
	request = strings.TrimSpace(request)
	if request == "" {
		return errors.New("the request is empty")
	}
	if !strings.ContainsFunc(request, unicode.IsSpace) {
		return fmt.Errorf("unknown command %q; to create an Intent, quote a request in your own words, "+
			`e.g. kubectl last "run an nginx web server"`, request)
	}
	k, err := d.Kube(kf)
	if err != nil {
		return err
	}
	name := cf.name
	if name == "" {
		name = intentName(request, d.Suffix())
	}
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Namespace},
		Spec: lastv1alpha1.IntentSpec{Request: request}}
	if err := k.Client.Create(ctx, in); err != nil {
		return fmt.Errorf("create the Intent: %w", err)
	}
	fmt.Fprintf(d.Out, "Created Intent %s/%s\n", in.Namespace, in.Name)
	if cf.noFollow {
		return nil
	}
	return follow(ctx, d, k, client.ObjectKeyFromObject(in))
}

// intentName is a DNS-safe slug of the request's first words, plus suffix.
func intentName(request, suffix string) string {
	words := strings.FieldsFunc(strings.ToLower(request), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	var b strings.Builder
	for _, w := range words {
		if b.Len() == 0 && len(w) > maxSlug {
			w = w[:maxSlug]
		}
		if b.Len() > 0 && b.Len()+1+len(w) > maxSlug {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 {
		b.WriteString("intent")
	}
	return b.String() + "-" + suffix
}
```

In `internal/cli/root.go`, replace `NewRootCommand` with:

```go
func NewRootCommand(d Deps) *cobra.Command {
	kf := &KubeFlags{}
	var cf createFlags
	root := &cobra.Command{
		Use:   `kubectl-last ["REQUEST"]`,
		Short: "Work with last-operator",
		Long: "Create an Intent from a request in your own words and follow it, answering its questions and " +
			"approving its discovery commands and plan as they come, or run one of the commands below.",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runCreate(cmd.Context(), d, *kf, cf, args[0])
		},
	}
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	kf.addTo(root)
	root.Flags().StringVar(&cf.name, "name", "", "the Intent's name (default: from the request, with a random suffix)")
	root.Flags().BoolVar(&cf.noFollow, "no-follow", false, "create the Intent and exit")
	root.AddCommand(newAuditCommand(d), newPlanCommand(d, kf), newAnswerCommand(d, kf),
		newDecisionCommand(d, kf, lastv1alpha1.DecisionApprove), newDecisionCommand(d, kf, lastv1alpha1.DecisionDeny))
	return root
}
```

- [ ] **Step 4: Write follow**

Create `internal/cli/follow.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// ErrIntentFailed is returned when a followed Intent fails, after its reason is printed.
var ErrIntentFailed = errors.New("the Intent failed")

// answered reports whether spec.answers answers every open question, as the
// operator checks it before planning on.
func answered(in *lastv1alpha1.Intent) bool {
	for _, q := range in.Status.Questions {
		if !hasAnswer(in, q.ID) {
			return false
		}
	}
	return true
}

// follow reads the Intent every Wait and acts on what it waits for (M3 spec
// §5): it asks the open questions, and puts each discovery command and plan to
// the user once. It returns at Ready, fails at Failed, and on Ctrl-C stops
// following without touching the Intent.
func follow(ctx context.Context, d Deps, k *Kube, key client.ObjectKey) error {
	p := newPrompter(d.In, d.Out)
	asked := map[string]bool{} // subjects already put to the user, decided or left pending
	var last lastv1alpha1.Phase
	for {
		var in lastv1alpha1.Intent
		if err := k.Client.Get(ctx, key, &in); err != nil {
			if ctx.Err() != nil {
				return stopped(d.Out, key)
			}
			return err
		}
		if phaseOf(&in) != last {
			printStatus(d.Out, &in)
			last = phaseOf(&in)
		}
		var err error
		switch in.Status.Phase {
		case lastv1alpha1.PhaseClarifying:
			if len(in.Status.Questions) > 0 && !answered(&in) {
				err = answerQuestions(ctx, k, p, &in)
			}
		case lastv1alpha1.PhaseAwaitingDiscoveryApproval, lastv1alpha1.PhaseAwaitingApproval:
			if stage, subject, ok := subjectOf(&in); ok && !asked[subject] {
				asked[subject] = true
				err = putToUser(ctx, k, p, &in, stage, subject)
			}
		case lastv1alpha1.PhaseReady:
			return printReady(ctx, k, d.Out, &in)
		case lastv1alpha1.PhaseFailed:
			return failed(d.Out, &in)
		}
		switch {
		case ctx.Err() != nil:
			return stopped(d.Out, key)
		case errors.Is(err, errNoInput):
			return fmt.Errorf("%w while Intent %s/%s waited for you; see where it is with kubectl last plan %s, "+
				"then use kubectl last answer, approve or deny", err, key.Namespace, key.Name, key.Name)
		case err != nil:
			return err
		}
		if err := d.Wait(ctx); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return stopped(d.Out, key)
			}
			return err
		}
	}
}

// putToUser shows a discovery command or plan and records the user's decision on it.
func putToUser(ctx context.Context, k *Kube, p *prompter, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) error {
	fmt.Fprintln(p.w)
	renderSubject(p.w, in, stage)
	decision, reason, err := p.askDecision(ctx)
	if err != nil {
		return err
	}
	if decision == "" {
		fmt.Fprintf(p.w, "Left pending; decide later with kubectl last approve %s or kubectl last deny %s --reason R\n", in.Name, in.Name)
		return nil
	}
	return decide(ctx, k, p.w, in, stage, subject, decision, reason)
}

// printReady lists what the Intent provisioned: its ManagedResources and their output Secrets.
func printReady(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent) error {
	fmt.Fprintf(w, "Intent %s/%s is Ready.\n", in.Namespace, in.Name)
	var list lastv1alpha1.ManagedResourceList
	if err := k.Client.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return err
	}
	slices.SortFunc(list.Items, func(a, b lastv1alpha1.ManagedResource) int { return strings.Compare(a.Name, b.Name) })
	for _, mr := range list.Items {
		if mr.Spec.IntentRef == nil || mr.Spec.IntentRef.Name != in.Name {
			continue
		}
		fmt.Fprintf(w, "  ManagedResource %s: %s\n", mr.Name, clean(mr.Spec.Description))
		if mr.Spec.OutputsSecretRef != nil {
			fmt.Fprintf(w, "    outputs: Secret %s\n", mr.Spec.OutputsSecretRef.Name)
		}
	}
	return nil
}

func failed(w io.Writer, in *lastv1alpha1.Intent) error {
	reason, msg := "", ""
	if c := meta.FindStatusCondition(in.Status.Conditions, condReady); c != nil {
		reason, msg = c.Reason, c.Message
	}
	fmt.Fprintf(w, "Intent %s/%s failed: %s: %s\nEdit its spec.request to try again.\n", in.Namespace, in.Name, clean(reason), clean(msg))
	return ErrIntentFailed
}

func stopped(w io.Writer, key client.ObjectKey) error {
	fmt.Fprintf(w, "\nStopped following Intent %s/%s; it carries on. See where it is with kubectl last plan %s.\n",
		key.Namespace, key.Name, key.Name)
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/cli/`
Expected: PASS.

Run: `go build -o /tmp/kubectl-last ./cmd/kubectl-last && /tmp/kubectl-last --help`
Expected: the help lists `answer`, `approve`, `audit`, `deny` and `plan`, plus the `--name`, `--no-follow`, `-n` and `--kubeconfig` flags.

- [ ] **Step 6: Lint and commit**

Run: `make lint-fix && make lint`
Expected: `0 issues.`

```bash
git add internal/cli/create.go internal/cli/follow.go internal/cli/create_test.go internal/cli/follow_test.go internal/cli/root.go
git commit -m "feat(cli): create an Intent from a request and follow it through its prompts"
```

---
### Task 6: Drive the web-server e2e through `kubectl last`

M3 spec §8 asks for the web-server e2e to be driven through `kubectl last`, with scripted input on stdin. The scripted model asks one clarifying question, asks for one discovery command, then plans. The test runs the real plugin binary with the input `1\ny\ny\n`:
- it answers the question with option 1;
- it approves the discovery command;
- it approves the plan.

The test then checks:
- the plugin's output;
- that the Intent is Ready, with the answer recorded;
- that both IntentApprovals are the test user's own;
- that the audit chain holds the question, the Discovery phase and the plan, in order, and verifies.

**Files:**
- Modify: `test/e2e/operator_e2e_test.go` (`TestOperatorWebServer`)
- Modify: `test/e2e/audit_e2e_test.go` (add `runLastInput`)

**Interfaces:**
- Consumes:
  - Tasks 2–5: the plugin's create-and-follow flow and its output lines:
    - `Discovery command <hash>`, `Reason: …`, `Approved discovery command …`;
    - `Plan <hash>`, `Approved plan <hash>`;
    - `Intent <ns>/web is Ready.`, `ManagedResource web-0`, `outputs: Secret web-outputs`.
  - M3b: `agent.Encode`, `agent.Turn`, `agent.KindQuestions`, `agent.KindCommand`, `agent.KindPlan`.
  - Existing e2e helpers: `kubectlLast`, `kubectl`, `waitFor`, `intentEntryTypes`, `containsInOrder`, `verifyAuditChain`, `secretValue`.
- Produces: `func runLastInput(t *testing.T, timeout time.Duration, input string, args ...string) (string, error)`.

- [ ] **Step 1: Add the stdin-driven runner**

In `test/e2e/audit_e2e_test.go`, add after `runLast` (and add the `context` and `strings` imports if missing):

```go
// runLastInput runs kubectl last as the current kubeconfig user, typing input
// at its prompts, and returns its combined output.
func runLastInput(t *testing.T, timeout time.Duration, input string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectlLast, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
```

- [ ] **Step 2: Script a question, a discovery command and the plan**

In `TestOperatorWebServer`, replace

```go
	answer, err := agent.Encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{
		Summary: "nginx Deployment and Service web in " + ns, Steps: steps, Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal([]string{answer})
	if err != nil {
		t.Fatal(err)
	}
```

with

```go
	// The scripted model asks one question, asks for one discovery command, then plans (M3 spec §8).
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespace", ns, "-o", "name"}},
		Providers: k8s, Timeout: &metav1.Duration{Duration: time.Minute}}
	turns := []agent.Turn{
		{Kind: agent.KindQuestions, Questions: []lastv1alpha1.Question{
			{ID: "replicas", Text: "How many nginx replicas?", Options: []string{"1", "2"}}}},
		{Kind: agent.KindCommand, Command: &look, Reason: "Check that namespace " + ns + " exists"},
		{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{Summary: "nginx Deployment and Service web in " + ns,
			Steps: steps, Resources: resources}},
	}
	answers := make([]string, 0, len(turns))
	for _, turn := range turns {
		a, err := agent.Encode(turn)
		if err != nil {
			t.Fatal(err)
		}
		answers = append(answers, a)
	}
	script, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
```

- [ ] **Step 3: Replace the hand-made Intent and approval with the user's session**

Replace everything from the comment `// The Intent, as a user writes it.` through the end of the `waitFor(t, 10*time.Minute, "the Intent to become Ready", …)` call with:

```go
	// The user's session: kubectl last creates the Intent and follows it, and the
	// user answers the question and approves the discovery command and the plan.
	out, err := runLastInput(t, 15*time.Minute, "1\ny\ny\n",
		"Run an nginx web server named web in namespace "+ns, "-n", ns, "--name", "web")
	if err != nil {
		t.Fatalf("kubectl last: %v\n%s", err, out)
	}
	for _, want := range []string{"How many nginx replicas?", "Discovery command sha256:",
		"Reason: Check that namespace " + ns + " exists", "Approved discovery command sha256:",
		"Plan " + wantHash, "Approved plan " + wantHash, "Intent " + ns + "/web is Ready.",
		"ManagedResource web-0", "outputs: Secret web-outputs"} {
		if !strings.Contains(out, want) {
			t.Errorf("kubectl last output lacks %q:\n%s", want, out)
		}
	}
	key := client.ObjectKey{Namespace: ns, Name: "web"}
	var intent lastv1alpha1.Intent
	if err := crc.Get(ctx, key, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.Status.Phase != lastv1alpha1.PhaseReady || intent.Status.Plan == nil || intent.Status.Plan.Hash != wantHash {
		t.Fatalf("intent status = %+v, want Ready with plan %s", intent.Status, wantHash)
	}
	if want := []lastv1alpha1.Answer{{ID: "replicas", Value: "1"}}; !slices.Equal(intent.Spec.Answers, want) {
		t.Fatalf("spec.answers = %+v, want %+v", intent.Spec.Answers, want)
	}

	// Both decisions are the user's own: the admission policy requires approvedBy to be the creator.
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var approvals lastv1alpha1.IntentApprovalList
	if err := crc.List(ctx, &approvals, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	stages := map[lastv1alpha1.Stage]int{}
	for _, a := range approvals.Items {
		s := a.Spec
		if s.Decision != lastv1alpha1.DecisionApprove || s.ApprovedBy != review.Status.UserInfo.Username ||
			(s.Stage == lastv1alpha1.StageCreate && s.SubjectHash != wantHash) {
			t.Errorf("approval %s = %+v", a.Name, s)
		}
		stages[s.Stage]++
	}
	if stages[lastv1alpha1.StageDiscovery] != 1 || stages[lastv1alpha1.StageCreate] != 1 {
		t.Fatalf("approvals by stage = %v, want one Discovery and one Create", stages)
	}
```

If the `state` closure or another variable is no longer used, remove it. Keep `meta`, which the ManagedResource check still uses. Update the doc comment of `TestOperatorWebServer` to:

```go
// TestOperatorWebServer is M2's done-criterion (spec §20), driven as M3 runs it
// (M3 spec §8): through kubectl last, a scripted model asks a question, runs a
// discovery command, and plans a web server that goes Ready in a sandbox, with
// a verified audit chain.
```

- [ ] **Step 4: Expect the question and the Discovery phase in the audit chain**

Replace the `want := []string{…}` list before `containsInOrder` with:

```go
	want := []string{
		audit.TypeLLMRequest, audit.TypeLLMResponse, // the question
		audit.TypeLLMRequest, audit.TypeLLMResponse, // the discovery command
		audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandStart, audit.TypeCommandResult, audit.TypePhaseEnd,
		audit.TypeLLMRequest, audit.TypeLLMResponse, // the plan
		audit.TypeApprovalObserved, audit.TypePhaseBegin,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeOutputWritten, audit.TypePhaseEnd,
		audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult,
	}
```

- [ ] **Step 5: Verify**

Run: `go vet -tags e2e ./test/e2e/`
Expected: no output.

Run: `make lint`
Expected: `0 issues.`

If Docker is running and a kind cluster can be used, also run `KIND_CLUSTER=last-e2e-m2a hack/e2e.sh`. Expected: `ok … test/e2e`. If Docker is not running, do not start it: say so in the report. The controller runs the e2e once Docker is available.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/operator_e2e_test.go test/e2e/audit_e2e_test.go
git commit -m "test(e2e): drive the web-server Intent through kubectl last with a question and a discovery command"
```

---
### Task 7: The real-model demo, the README, and the spec amendments

This task adds `hack/demo.sh`, which runs M3's done-criterion against Claude on kind:
1. It installs the chart.
2. It puts `ANTHROPIC_API_KEY` into a gateway Secret, through stdin only.
3. It creates a Provider that can edit one namespace.
4. It points the operator at an `anthropic` LLMProvider.
5. It runs `kubectl last "run an nginx web server serving a hello page"` interactively.
6. It verifies the audit chain.

The demo is opt-in, never runs in CI, and spends API credit.

The kind install that `hack/e2e.sh` does moves into `hack/kind-install.sh`, so both scripts share it.

The task also brings the README up to date. It amends M3 spec §5, for what M3c decided, and §6, adding the missing row for a refused RunCommand. And it records the follow-up items this plan closes.

**Files:**
- Create: `hack/kind-install.sh`, `hack/demo.sh`
- Modify: `hack/e2e.sh`, `Makefile`, `README.md`
- Modify: `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`
- Modify: `docs/superpowers/followups/2026-10-13-m3a-followups.md`, `docs/superpowers/followups/2026-10-15-m3b-followups.md`

**Interfaces:**
- Consumes:
  - Tasks 2–5: the plugin's commands and flags;
  - Task 1: the gateway wording;
  - the chart's names: the namespaces `last-gateway` and `last-audit`; the `last-audit-config` and `last-audit-pubkey` ConfigMaps; the `last-audit-reader` Secret; `svc/last-rustfs`; the `OperatorConfig` named `default`.
- Produces: `hack/kind-install.sh` (env: `KIND_CLUSTER`, `IMAGE_TAG`, `IMAGE_REGISTRY`), `hack/demo.sh`, and `make demo`.

- [ ] **Step 1: Share the kind install**

Create `hack/kind-install.sh` from the install part of `hack/e2e.sh`. That is everything from the line `CLUSTER="${KIND_CLUSTER:-last-e2e}"` through the two `kubectl rollout status` lines, unchanged. Give it this header:

```bash
#!/usr/bin/env bash
# Build the images and install the chart on a kind cluster, creating the
# cluster if needed. Used by hack/e2e.sh and hack/demo.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
```

Then replace `hack/e2e.sh` with:

```bash
#!/usr/bin/env bash
# Install on kind (hack/kind-install.sh) and run the e2e suite.
set -euo pipefail
cd "$(dirname "$0")/.."

hack/kind-install.sh
go test -tags e2e ./test/e2e/ -count=1 -v -timeout 30m
```

Run: `chmod +x hack/kind-install.sh && bash -n hack/kind-install.sh && bash -n hack/e2e.sh`
Expected: no output.

- [ ] **Step 2: Write the demo**

Create `hack/demo.sh` and make it executable (`chmod +x`):

```bash
#!/usr/bin/env bash
# M3's done-criterion against a real model (M3 spec §1, §8): install on kind with
# an anthropic LLMProvider, then create an Intent with kubectl last, which asks
# you its questions and for each approval. Opt-in and never run in CI: it spends
# API credit. The key comes from ANTHROPIC_API_KEY and goes only into a Secret
# in last-gateway, through stdin, never on a command line.
#
# Record a transcript with: hack/demo.sh 2>&1 | tee demo.log
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "Set ANTHROPIC_API_KEY to run the demo." >&2
  exit 1
fi
export KIND_CLUSTER="${KIND_CLUSTER:-last-demo}"
MODEL="${DEMO_MODEL:-claude-opus-5-5}"
NS="${DEMO_NAMESPACE:-demo}"
REQUEST="${DEMO_REQUEST:-run an nginx web server serving a hello page}"
PROVIDER=demo-kubernetes
LLM=demo-anthropic

hack/kind-install.sh

# The model's API key, under the key name the gateway reads.
printf '%s' "$ANTHROPIC_API_KEY" | kubectl create secret generic "$LLM" -n last-gateway \
  --from-file=apiKey=/dev/stdin --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# A namespace for the demo, a Provider that can edit only it, and the model.
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: $NS}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: $PROVIDER, namespace: last-gateway}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: $PROVIDER-edit, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit}
subjects: [{kind: ServiceAccount, name: $PROVIDER, namespace: last-gateway}]
---
apiVersion: last.moogie-labs.com/v1alpha1
kind: Provider
metadata: {name: $PROVIDER}
spec:
  type: kubernetes
  description: "Edit access to namespace $NS on this cluster"
  kubernetes: {serviceAccountRef: {name: $PROVIDER}}
---
apiVersion: last.moogie-labs.com/v1alpha1
kind: LLMProvider
metadata: {name: $LLM}
spec:
  type: anthropic
  model: $MODEL
  secretRef: {name: $LLM}
EOF

# Point the operator at the model, and let Intents use the Provider.
kubectl apply --server-side --force-conflicts --field-manager=last-demo -f - <<EOF
apiVersion: last.moogie-labs.com/v1alpha1
kind: OperatorConfig
metadata: {name: default}
spec:
  llmProviderRef: {name: $LLM}
  defaults:
    providers: [$PROVIDER]
EOF

go build -o bin/kubectl-last ./cmd/kubectl-last
echo
echo "Creating an Intent in namespace $NS from: $REQUEST"
bin/kubectl-last "$REQUEST" -n "$NS"

# Verify the WORM audit chain the run wrote.
echo
echo "Verifying the audit chain"
PUB="$(mktemp)"
kubectl port-forward -n last-audit svc/last-rustfs 19000:9000 >/dev/null 2>&1 &
PF=$!
trap 'kill "$PF" 2>/dev/null || true; rm -f "$PUB"' EXIT
kubectl get cm -n last-gateway last-audit-pubkey -o jsonpath='{.data.ed25519\.pub}' > "$PUB"
echo >> "$PUB"
for _ in $(seq 30); do
  if curl -s -o /dev/null http://127.0.0.1:19000; then break; fi
  sleep 1
done
LAST_AUDIT_ENDPOINT=http://127.0.0.1:19000 \
LAST_AUDIT_CHAIN_ID="$(kubectl get cm -n last-gateway last-audit-config -o jsonpath='{.data.chainId}')" \
AWS_ACCESS_KEY_ID="$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.accessKey}' | base64 --decode)" \
AWS_SECRET_ACCESS_KEY="$(kubectl get secret -n last-audit last-audit-reader -o jsonpath='{.data.secretKey}' | base64 --decode)" \
  bin/kubectl-last audit verify --pubkey-file "$PUB"
```

In `Makefile`, add after the `test-e2e` target:

```make
.PHONY: demo
demo: ## Run M3's done-criterion on kind against Claude (needs ANTHROPIC_API_KEY; never run in CI; spends API credit).
	./hack/demo.sh
```

Run: `bash -n hack/demo.sh`
Expected: no output.

If `shellcheck` is installed, also run `shellcheck hack/demo.sh hack/kind-install.sh hack/e2e.sh` and fix what it reports. Do NOT run `hack/demo.sh`: it needs an API key and spends credit, so the user runs it.

- [ ] **Step 3: Bring the README up to date**

In `README.md`:

1. Replace the **Status** paragraph with:

```markdown
**Status:** M3 (real LLMs). The operator plans with Anthropic Claude or an
OpenAI-compatible model, asks clarifying questions, runs approved discovery
commands, and executes approved plans in sandbox pods. Every action goes
through the gateway, which records it in a write-once audit log. Image builds,
day-2 changes, allow rules and rule suggestions arrive in M4–M7.
```

2. In **What works today**, add before the `kubectl last audit` line:

```markdown
- `kubectl last "<request>"`, which creates an Intent and follows it: it asks you
  the model's questions, shows each discovery command and the plan, and asks for
  your approval. Also `kubectl last answer | plan | approve | deny NAME`.
```

3. Add a section after **Building the CLI**:

````markdown
## Using `kubectl last`

```bash
kubectl last "run an nginx web server serving a hello page" -n team-x
```

The plugin creates an Intent and follows it, every two seconds:

- **Questions.** Each clarifying question is shown as a numbered menu or a
  free-text prompt.
- **Discovery commands and the plan.** Each is shown with its commands, files
  (by path and size), Providers and egress, then the plugin asks
  `Approve? [y/N/d]`:
  - `y` approves;
  - `d` denies, after asking for a reason, which the model reads;
  - `N` or Enter leaves it pending.
- **Ready.** At Ready it lists the ManagedResources and output Secrets.

Ctrl-C stops following; the Intent carries on. Pick it up again with:

```bash
kubectl last plan NAME                  # what the Intent is waiting for (read-only)
kubectl last answer NAME                # answer its open questions
kubectl last approve NAME [--hash H]    # approve exactly what plan shows
kubectl last deny NAME --reason R
```

Every decision is an `IntentApproval` that you create under your own identity,
for the hash you were shown.

## Demo against Claude (opt-in)

```bash
export ANTHROPIC_API_KEY=...            # spends API credit
make demo                               # kind + chart + an anthropic LLMProvider, then kubectl last
```

The key goes only into a Secret in `last-gateway`. The demo never runs in CI.
````

- [ ] **Step 4: Amend the M3 spec and the follow-ups**

In `docs/superpowers/specs/2026-10-05-m3-real-llms-design.md`:

**§5:**

1. Replace `  - Then follows it with a watch:` with `  - Then follows it, reading the Intent every 2 seconds:`.
2. At the end of the **AwaitingDiscoveryApproval** sub-bullet, append ` Each command and plan is put to the user once; one left pending is decided later with \`approve\` or \`deny\`.`
3. After the `Ctrl-C stops following; …` sub-bullet, add these sub-bullets at the same level:

```markdown
  - A request with no space in it is refused as a mistyped command, so `kubectl last aprove web` never creates an Intent.
  - If input ends at a prompt, the CLI stops and names the commands to continue with.
  - Model-written text (summaries, reasons, commands, file paths, questions) is shown with control characters and bidirectional overrides escaped, so nothing can hide or reorder what an approver reads.
```

4. In the **How approvals are made** list, add:

```markdown
- `approve` and `deny` take `--hash H` and decide only if the Intent still asks about H, for scripts that showed the subject earlier.
```

**§6:** in the Errors table, after the row for a refused Discovery phase, add:

```markdown
| Discovery command refused while it runs (approval withdrawn, policy narrowed) | The gateway ends the phase as failed, and the model is told "The command did not run: …". |
```

In `docs/superpowers/followups/2026-10-13-m3a-followups.md`, replace the `- **Discovery refusal wording.**` item with:

```markdown
- **Discovery refusal wording.** Done in M3c: BeginPhase and RunCommand name "this discovery command" for stage Discovery, and a replayed discovery subject says it already ran.
```

In `docs/superpowers/followups/2026-10-15-m3b-followups.md`, replace the `- **A RunCommand refusal is missing from the M3 spec §6 table.**` item with:

```markdown
- **A RunCommand refusal is missing from the M3 spec §6 table.** Done in M3c.
```

- [ ] **Step 5: Verify and commit**

Run: `make lint && make test`
Expected: `0 issues.` and PASS. This task changes no Go code, so this confirms nothing else moved.

```bash
git add hack/kind-install.sh hack/e2e.sh hack/demo.sh Makefile README.md docs/superpowers/specs/2026-10-05-m3-real-llms-design.md \
  docs/superpowers/followups/2026-10-13-m3a-followups.md docs/superpowers/followups/2026-10-15-m3b-followups.md
git commit -m "feat(demo): add the opt-in real-model demo; document kubectl last; amend the M3 spec for M3c"
```

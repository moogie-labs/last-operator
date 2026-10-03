# M2a: Gateway RPC + Sandbox Execution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the gateway the two capabilities the rest of M2 builds on:
- an authenticated gRPC endpoint that only the operator can call;
- an execution engine that runs a command or a batch of probes in a locked-down sandbox pod, with injected credentials, per-run egress, a priority concurrency cap, and fail-closed audit entries for every step.

**Architecture:** 
- **Sandbox runner.** Each sandbox pod runs a small `sandbox-runner` binary. It executes the command(s) described in a mounted spec, and prints one JSON result envelope that the gateway reads from the pod log.
- **Executor.** `internal/sandbox` contains pure builders (pod, spec Secret, credentials, egress NetworkPolicy), a priority scheduler, and an `Executor`. For each run, the Executor:
  1. writes `command.start`;
  2. issues credentials, writing `credential.issued` before each one;
  3. grants egress, writing `egress.granted` before it;
  4. runs the pod and collects the result;
  5. cleans up, writing `egress.revoked`;
  6. writes `command.result` through `audit.Writer.Do`.

  Probe runs use the compact `probe.start`/`probe.result` pair (spec §10.2). CEL assertions are evaluated in the gateway.
- **gRPC endpoint.** In this milestone the gateway exposes gRPC with a TokenReview-based caller check and a single `Ping` RPC. The command RPCs are added in M2b, together with the authorization and plan enforcement that must guard them. So no network-reachable path can run a command without enforcement.

**Tech Stack:** Go 1.27, client-go (fake clientset for unit tests), controller-runtime client (Provider CRs), google.golang.org/grpc + protobuf (generated with buf), github.com/google/cel-go, Debian bookworm-slim sandbox image, kind for e2e.

**Spec:** `docs/superpowers/specs/2026-09-26-last-operator-design.md`. Read §4.2 (gateway, including the transport/auth paragraph), §8.1 (command format), §10 (probes, scheduling, compact audit), §11.1 (sandbox pods), §13 (credentials), and §14.2 (entry format).

**Follow-ups doc:** `docs/superpowers/followups/2026-10-03-foundation-followups.md`. This plan resolves the "outcome payload numbers" item for the payloads it creates (Global Constraints).

## Design decisions in this plan (review these)

1. **No command-running RPC in M2a.** `RunCommand`/`RunProbes` gRPC methods arrive in M2b with phase, approval, and plan enforcement. M2a's executor is exercised by unit tests and by an e2e test that drives it in-process against the kind cluster.
2. **Egress destinations are IP/CIDR only** (`10.1.2.3:443`, `10.0.0.0/8:443`). Host-name egress returns `ErrHostEgressUnsupported`; spec §11.1 says it needs Cilium, which is out of scope for M2. **DNS is not allowed** from sandboxes: kubeconfigs address the API server by ClusterIP with `tls-server-name: kubernetes.default.svc`. Opening DNS would give a prompt-injected script an exfiltration channel.
3. **At most one `kubernetes` Provider per command.** kubectl merges multiple kubeconfigs ambiguously.
4. **Provider `files` paths must be under `/work/`.** They are mounted as single files into the sandbox's writable `/work`.
5. **`image.install` is rejected** with `ErrImageBuildsUnsupported` until M4 (spec §11.2). Every run uses the configured base image.
6. **Audit payloads written by this plan contain only strings, integers below 2^53, booleans, arrays and objects.** Command stdout and stderr are stored as strings, and parsed JSON is never put in an audit payload. This avoids the RFC 8785 number rule (spec §14.2) rejecting an outcome after the action has already run.
7. **The base image is referenced by tag** in the e2e and dev setups. The digest pin required by `OperatorConfig.spec.sandbox.baseImage` (CRD-validated) is enforced when M2b wires the executor to `OperatorConfig`.

## Global Constraints

- Go module `github.com/moogie-labs/last-operator`; API group `last.moogie-labs.com/v1alpha1`.
- Namespaces: operator `last-system` (Helm release namespace), gateway `last-gateway`, audit `last-audit`, sandboxes `last-sandbox`.
- Operator identity accepted by the gateway: `system:serviceaccount:<release namespace>:last-operator`. Token audience: `last-gateway`.
- gRPC listens on `:9090` (`LAST_GRPC_ADDR`); HTTP health and metrics stay on `:8080`.
- Sandbox pods:
  - non-root UID/GID 65532, `readOnlyRootFilesystem: true`, all capabilities dropped, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, `automountServiceAccountToken: false`;
  - writable `/work` (emptyDir, 1Gi limit) and `/tmp` (emptyDir, 64Mi limit); `HOME=/work`;
  - `restartPolicy: Never`.
- The runner caps each of stdout and stderr at **1 MiB** (1048576 bytes) per command, and records truncation flags.
- Default command timeout is **5m**. A pod's `activeDeadlineSeconds` is the sum of its commands' timeouts plus 60. The gateway waits that long plus 2m for scheduling and image pull.
- Every sandbox object carries these labels:
  - `app.kubernetes.io/managed-by: last-gateway`;
  - `last.moogie-labs.com/run: <runID>`;
  - `app.kubernetes.io/name: last-sandbox` (pods only).
- Audit entry types and payloads follow spec §14.2. Each action is preceded by its audit entry (write-before-act); the exception is `egress.revoked`, which records a capability being removed.
- Credential **values** never appear in audit payloads or logs.
- Tests use the standard `testing` package. Integration tests use build tag `integration`, e2e tests use `e2e`.
- Commit messages are conventional style. **No `Co-authored-by` or other attribution trailers.** Never commit `.superpowers/`.
- Kubebuilder-generated files (`zz_generated.*`, `config/crd/bases`, `PROJECT`) are regenerated, never hand-edited.

## Review Focus

These are the failure modes most likely to hurt a real user that the spec implies but doesn't spell out. Each one has a pinning test in the task named.

1. **Command output that isn't valid UTF-8, or exceeds 1 MiB.** The envelope and the audit entry must still be valid. Output is truncated with flags set, not dropped. (Task 1: runner tests.)
2. **A sandbox pod that never starts** (bad image, unschedulable). The run must end with a recorded `command.result` error after the deadline, with every sandbox object deleted, not hang. (Task 7: executor timeout test.)
3. **A gateway crash mid-run leaves credential Secrets and allow-egress policies behind.** The reaper must delete objects older than the threshold. (Task 9: reaper test.)
4. **TokenReview is unavailable.** The gateway must refuse the call (`Unavailable`), never let it through. A token for the wrong audience or the wrong ServiceAccount is rejected. (Task 10: auth tests.)
5. **A credential value leaking into the audit log through a payload.** No audit entry may contain a Secret's value. (Task 4 and Task 7 tests; Task 12 e2e checks it on a real cluster.)

## File Map

| Path | Responsibility |
|---|---|
| `internal/sandbox/runnerapi/types.go` | Spec/Envelope types shared by runner and gateway |
| `cmd/sandbox-runner/main.go`, `internal/sandbox/runner/run.go` | The in-pod runner |
| `images/sandbox-base/Dockerfile` | Sandbox base image (tools + runner) |
| `internal/sandbox/egress.go` | Egress parsing and per-run NetworkPolicy builder |
| `internal/sandbox/credentials.go` | Provider → per-run credential Secret, env, and mounts |
| `internal/sandbox/pod.go` | Spec Secret and Pod builders |
| `internal/sandbox/scheduler.go` | Priority concurrency limiter |
| `internal/sandbox/executor.go` | Run orchestration with audit, waiting, log parsing, cleanup |
| `internal/sandbox/probes.go`, `internal/probe/assert.go` | Probe runs and CEL assertions |
| `internal/sandbox/reaper.go` | Orphaned sandbox object cleanup |
| `proto/last/gateway/v1/gateway.proto`, `buf.yaml`, `buf.gen.yaml`, `internal/gatewayapi/gatewayv1/*` | gRPC API and generated code |
| `internal/gateway/auth.go`, `internal/gateway/rpc.go` | TokenReview caller auth and the `Ping` service |
| `charts/last-operator/templates/{sandbox,operator,gateway}.yaml` | Sandbox namespace and default-deny; operator ServiceAccount; gateway gRPC port, NetworkPolicy, TokenReview RBAC |
| `test/e2e/sandbox_e2e_test.go`, `test/e2e/grpc_e2e_test.go` | Real-cluster sandbox and gRPC auth tests |

---
### Task 1: Sandbox runner and its result contract

**Files:**
- Create: `internal/sandbox/runnerapi/types.go`, `internal/sandbox/runnerapi/types_test.go`, `internal/sandbox/runner/run.go`, `internal/sandbox/runner/run_test.go`, `cmd/sandbox-runner/main.go`

**Interfaces:**
- Produces (package `runnerapi`): constants `SpecPath = "/last/spec/spec.json"`, `ResultPrefix = "LAST-RESULT "`, `Version = 1`, `MaxOutput = 1 << 20`; types `Spec{Version int; Commands []Command}`, `Command{ID string; Argv []string; Script string; Files map[string]string; TimeoutSeconds int64}`, `Result{ID string; ExitCode int; Stdout, Stderr string; StdoutTruncated, StderrTruncated bool; Error string}`, `Envelope{Version int; Results []Result}`; `func ParseEnvelope(log []byte) (Envelope, error)`; `var ErrNoResult`.
- Produces (package `runner`): `Options{MaxOutput int; WorkDir string; Shell string}`; `func Run(ctx context.Context, spec runnerapi.Spec, opts Options) runnerapi.Envelope`.
- JSON field names (exact): `version, commands, id, argv, script, files, timeoutSeconds, results, exitCode, stdout, stderr, stdoutTruncated, stderrTruncated, error`.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/runnerapi/types_test.go`:
```go
package runnerapi_test

import (
	"errors"
	"testing"

	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func TestParseEnvelopeFindsResultLine(t *testing.T) {
	log := []byte("some noise\n" + runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"a","exitCode":0,"stdout":"hi","stderr":"","stdoutTruncated":false,"stderrTruncated":false}]}` + "\ntrailing noise\n")
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Results) != 1 || env.Results[0].ID != "a" || env.Results[0].Stdout != "hi" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestParseEnvelopeMissingResult(t *testing.T) {
	if _, err := runnerapi.ParseEnvelope([]byte("no result here\n")); !errors.Is(err, runnerapi.ErrNoResult) {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
}

func TestParseEnvelopeRejectsUnknownVersion(t *testing.T) {
	log := []byte(runnerapi.ResultPrefix + `{"version":2,"results":[]}` + "\n")
	if _, err := runnerapi.ParseEnvelope(log); err == nil {
		t.Fatal("accepted envelope version 2")
	}
}

func TestParseEnvelopeHandlesLargeLine(t *testing.T) {
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = 'a'
	}
	log := []byte(runnerapi.ResultPrefix + `{"version":1,"results":[{"id":"x","exitCode":0,"stdout":"` + string(big) + `","stderr":""}]}` + "\n")
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil || len(env.Results[0].Stdout) != len(big) {
		t.Fatalf("large envelope: err=%v", err)
	}
}
```

`internal/sandbox/runner/run_test.go`:
```go
package runner_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/moogie-labs/last-operator/internal/sandbox/runner"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func run(t *testing.T, maxOutput int, cmds ...runnerapi.Command) (runnerapi.Envelope, string) {
	t.Helper()
	work := t.TempDir()
	for i := range cmds {
		files := map[string]string{}
		for p, c := range cmds[i].Files {
			files[strings.ReplaceAll(p, "$WORK", work)] = c
		}
		cmds[i].Files = files
		for j, a := range cmds[i].Argv {
			cmds[i].Argv[j] = strings.ReplaceAll(a, "$WORK", work)
		}
		if cmds[i].TimeoutSeconds == 0 {
			cmds[i].TimeoutSeconds = 30
		}
	}
	env := runner.Run(context.Background(), runnerapi.Spec{Version: runnerapi.Version, Commands: cmds},
		runner.Options{MaxOutput: maxOutput, WorkDir: work, Shell: "/bin/sh"})
	return env, work
}

func TestRunArgvCapturesOutputAndExitCode(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "a", Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}})
	r := env.Results[0]
	if r.ID != "a" || r.ExitCode != 3 || r.Stdout != "out\n" || r.Stderr != "err\n" || r.Error != "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunScriptUsesShell(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "s", Script: "printf hello | tr a-z A-Z"})
	if r := env.Results[0]; r.ExitCode != 0 || r.Stdout != "HELLO" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunWritesFilesUnderWork(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{
		ID: "f", Files: map[string]string{"$WORK/a/b.txt": "content"}, Argv: []string{"cat", "$WORK/a/b.txt"},
	})
	if r := env.Results[0]; r.ExitCode != 0 || r.Stdout != "content" {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunRejectsFilesOutsideWork(t *testing.T) {
	for _, p := range []string{"/etc/last-escape", "$WORK/../escape", "relative/path"} {
		env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "x", Files: map[string]string{p: "x"}, Argv: []string{"true"}})
		if r := env.Results[0]; r.ExitCode != -1 || !strings.Contains(r.Error, "outside") {
			t.Errorf("path %q: result = %+v", p, r)
		}
	}
}

func TestRunTruncatesOutputAndFlagsIt(t *testing.T) {
	env, _ := run(t, 10, runnerapi.Command{ID: "t", Script: "head -c 100 /dev/zero | tr '\\0' a; head -c 100 /dev/zero | tr '\\0' b >&2"})
	r := env.Results[0]
	if len(r.Stdout) != 10 || !r.StdoutTruncated || len(r.Stderr) != 10 || !r.StderrTruncated {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunInvalidUTF8StaysValidJSON(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "u", Script: `printf '\377\376ok'`})
	r := env.Results[0]
	if !utf8.ValidString(r.Stdout) || !strings.HasSuffix(r.Stdout, "ok") {
		t.Fatalf("stdout %q not valid UTF-8 with original suffix", r.Stdout)
	}
	b, err := json.Marshal(env)
	if err != nil || !json.Valid(b) {
		t.Fatalf("envelope not valid JSON: %v", err)
	}
}

func TestRunEnforcesTimeout(t *testing.T) {
	start := time.Now()
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "slow", Script: "sleep 30", TimeoutSeconds: 1})
	r := env.Results[0]
	if r.ExitCode != -1 || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("result = %+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

func TestRunContinuesAfterAFailedCommand(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput,
		runnerapi.Command{ID: "bad", Argv: []string{"/nonexistent/binary"}},
		runnerapi.Command{ID: "good", Argv: []string{"echo", "ok"}},
	)
	if len(env.Results) != 2 || env.Results[0].ExitCode != -1 || env.Results[0].Error == "" || env.Results[1].Stdout != "ok\n" {
		t.Fatalf("results = %+v", env.Results)
	}
}

func TestRunRejectsCommandWithoutArgvOrScript(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "empty"})
	if r := env.Results[0]; r.ExitCode != -1 || r.Error == "" {
		t.Fatalf("result = %+v", r)
	}
}

```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/... -count=1`
Expected: compile errors (`package .../runnerapi is not in std` / undefined symbols).

- [ ] **Step 3: Implement the contract**

`internal/sandbox/runnerapi/types.go`:
```go
// Package runnerapi is the contract between the gateway and the in-pod sandbox runner.
package runnerapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// SpecPath is where the gateway mounts the run spec inside the sandbox pod.
	SpecPath = "/last/spec/spec.json"
	// ResultPrefix marks the single log line that carries the result envelope.
	ResultPrefix = "LAST-RESULT "
	// Version is the spec/envelope format version.
	Version = 1
	// MaxOutput caps each of stdout and stderr per command (spec §11.1).
	MaxOutput = 1 << 20
)

// ErrNoResult means the pod log contained no result envelope.
var ErrNoResult = errors.New("runnerapi: sandbox produced no result envelope")

// Spec is what the runner executes, in order.
type Spec struct {
	Version  int       `json:"version"`
	Commands []Command `json:"commands"`
}

// Command is one execution. Exactly one of Argv or Script is set.
type Command struct {
	ID             string            `json:"id"`
	Argv           []string          `json:"argv,omitempty"`
	Script         string            `json:"script,omitempty"`
	Files          map[string]string `json:"files,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds"`
}

// Result is one command's outcome. ExitCode is -1 when the command could not run
// to completion (bad spec, start failure, timeout); Error then says why.
type Result struct {
	ID              string `json:"id"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error,omitempty"`
}

// Envelope is the runner's single output line.
type Envelope struct {
	Version int      `json:"version"`
	Results []Result `json:"results"`
}

// ParseEnvelope returns the last result envelope found in a pod log.
func ParseEnvelope(log []byte) (Envelope, error) {
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	var line []byte
	for sc.Scan() {
		if bytes.HasPrefix(sc.Bytes(), []byte(ResultPrefix)) {
			line = bytes.Clone(sc.Bytes()[len(ResultPrefix):])
		}
	}
	if err := sc.Err(); err != nil {
		return Envelope{}, fmt.Errorf("runnerapi: read log: %w", err)
	}
	if line == nil {
		return Envelope{}, ErrNoResult
	}
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Envelope{}, fmt.Errorf("runnerapi: parse envelope: %w", err)
	}
	if env.Version != Version {
		return Envelope{}, fmt.Errorf("runnerapi: unsupported envelope version %d", env.Version)
	}
	return env, nil
}
```

`internal/sandbox/runner/run.go`:
```go
// Package runner executes a sandbox spec inside the sandbox pod.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Options configures a run.
type Options struct {
	MaxOutput int    // cap per stream, bytes
	WorkDir   string // the only directory files may be written under
	Shell     string // used for Script commands
}

// Run executes every command in order. A failing command never stops later ones.
func Run(ctx context.Context, spec runnerapi.Spec, opts Options) runnerapi.Envelope {
	env := runnerapi.Envelope{Version: runnerapi.Version, Results: make([]runnerapi.Result, 0, len(spec.Commands))}
	for _, c := range spec.Commands {
		env.Results = append(env.Results, runOne(ctx, c, opts))
	}
	return env
}

func runOne(ctx context.Context, c runnerapi.Command, opts Options) runnerapi.Result {
	res := runnerapi.Result{ID: c.ID}
	fail := func(format string, args ...any) runnerapi.Result {
		res.ExitCode = -1
		res.Error = fmt.Sprintf(format, args...)
		return res
	}
	if (len(c.Argv) == 0) == (c.Script == "") {
		return fail("exactly one of argv or script must be set")
	}
	if err := writeFiles(c.Files, opts.WorkDir); err != nil {
		return fail("%v", err)
	}
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if len(c.Argv) > 0 {
		cmd = exec.CommandContext(cctx, c.Argv[0], c.Argv[1:]...)
	} else {
		cmd = exec.CommandContext(cctx, opts.Shell, "-c", c.Script)
	}
	cmd.Dir = opts.WorkDir
	cmd.WaitDelay = 2 * time.Second // don't hang on grandchildren holding the pipes
	stdout := &cappedBuffer{limit: opts.MaxOutput}
	stderr := &cappedBuffer{limit: opts.MaxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	res.Stdout, res.StdoutTruncated = stdout.text(), stdout.truncated
	res.Stderr, res.StderrTruncated = stderr.text(), stderr.truncated
	var exitErr *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return fail("timed out after %s", timeout)
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return fail("could not run command: %v", err)
	}
	return res
}

// writeFiles writes each file, refusing any path that resolves outside workDir.
func writeFiles(files map[string]string, workDir string) error {
	root := filepath.Clean(workDir) + string(filepath.Separator)
	for p, content := range files {
		clean := filepath.Clean(p)
		if !filepath.IsAbs(p) || !strings.HasPrefix(clean, root) {
			return fmt.Errorf("file path %q is outside %s", p, workDir)
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
			return fmt.Errorf("create directory for %q: %w", p, err)
		}
		if err := os.WriteFile(clean, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write %q: %w", p, err)
		}
	}
	return nil
}

// cappedBuffer keeps the first limit bytes and records whether more arrived.
type cappedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if room <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// text returns the captured bytes as valid UTF-8 (invalid sequences become U+FFFD).
func (b *cappedBuffer) text() string { return strings.ToValidUTF8(string(b.buf), "�") }
```

`cmd/sandbox-runner/main.go`:
```go
// Command sandbox-runner is the entrypoint of every sandbox pod. It runs the
// mounted spec and prints a single result envelope line for the gateway.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/moogie-labs/last-operator/internal/sandbox/runner"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func main() {
	env, code := execute()
	out, err := json.Marshal(env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-runner: encode envelope:", err)
		os.Exit(2)
	}
	fmt.Printf("%s%s\n", runnerapi.ResultPrefix, out)
	os.Exit(code)
}

func execute() (runnerapi.Envelope, int) {
	failed := func(err error) (runnerapi.Envelope, int) {
		return runnerapi.Envelope{Version: runnerapi.Version, Results: []runnerapi.Result{{ExitCode: -1, Error: err.Error()}}}, 2
	}
	raw, err := os.ReadFile(runnerapi.SpecPath)
	if err != nil {
		return failed(fmt.Errorf("read spec: %w", err))
	}
	var spec runnerapi.Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return failed(fmt.Errorf("parse spec: %w", err))
	}
	if spec.Version != runnerapi.Version {
		return failed(fmt.Errorf("unsupported spec version %d", spec.Version))
	}
	return runner.Run(context.Background(), spec, runner.Options{
		MaxOutput: runnerapi.MaxOutput, WorkDir: "/work", Shell: "/bin/sh",
	}), 0
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/sandbox/... -count=1 -v && go build ./cmd/sandbox-runner`
Expected: PASS (13 tests). The build succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox cmd/sandbox-runner
git commit -m "feat(sandbox): add the in-pod runner and its result envelope"
```

### Task 2: Sandbox base image

**Files:**
- Create: `images/sandbox-base/Dockerfile`, `hack/sandbox-smoke.sh`
- Modify: `Makefile` (add `image-sandbox-base`, make `images` depend on it)

**Interfaces:**
- Consumes: `cmd/sandbox-runner` (Task 1).
- Produces: image `$(IMAGE_REGISTRY)/last-sandbox-base:$(IMAGE_TAG)` with entrypoint `/last/sandbox-runner`, user 65532, `HOME=/work`, and tools `sh, curl, kubectl, jq, git, python3`. Task 12 loads it into kind.

- [ ] **Step 1: Write the smoke test (it fails until the image exists)**

`hack/sandbox-smoke.sh`:
```bash
#!/usr/bin/env bash
# Run the sandbox image the way the gateway will: read-only root, tmpfs /work and /tmp,
# spec mounted at /last/spec. Prints the envelope and checks it.
set -euo pipefail
IMAGE="${1:?usage: sandbox-smoke.sh <image>}"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
cat > "$dir/spec.json" <<'JSON'
{"version":1,"commands":[
 {"id":"tools","script":"id -u; kubectl version --client >/dev/null && jq --version >/dev/null && git --version >/dev/null && python3 --version >/dev/null && curl --version >/dev/null && echo tools-ok","timeoutSeconds":30},
 {"id":"files","files":{"/work/hello.txt":"hi"},"argv":["cat","/work/hello.txt"],"timeoutSeconds":30},
 {"id":"rootfs","script":"touch /etc/x 2>/dev/null && echo writable || echo readonly","timeoutSeconds":30}
]}
JSON
out="$(docker run --rm --read-only --tmpfs /work:uid=65532,gid=65532 --tmpfs /tmp \
  -v "$dir:/last/spec:ro" --cap-drop ALL --security-opt no-new-privileges "$IMAGE")"
echo "$out"
line="$(printf '%s\n' "$out" | grep '^LAST-RESULT ' | sed 's/^LAST-RESULT //')"
echo "$line" | jq -e '.results[0].stdout == "65532\ntools-ok\n"' >/dev/null
echo "$line" | jq -e '.results[1].stdout == "hi"' >/dev/null
echo "$line" | jq -e '.results[2].stdout == "readonly\n"' >/dev/null
echo "sandbox smoke test passed"
```
Run: `chmod +x hack/sandbox-smoke.sh && ./hack/sandbox-smoke.sh ghcr.io/moogie-labs/last-sandbox-base:dev`
Expected: FAIL (`Unable to find image` / pull error).

- [ ] **Step 2: Write the Dockerfile**

`images/sandbox-base/Dockerfile`:
```dockerfile
# Sandbox base image: common CLIs plus the last-operator sandbox runner (spec §11.1).
FROM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/sandbox-runner/ cmd/sandbox-runner/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/sandbox-runner ./cmd/sandbox-runner

FROM debian:bookworm-slim
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.36.1
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq git python3 \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && curl -fsSLo /tmp/kubectl.sha256 "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" \
 && echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/kubectl \
 && rm /tmp/kubectl.sha256
COPY --from=builder /out/sandbox-runner /last/sandbox-runner
USER 65532:65532
WORKDIR /work
ENV HOME=/work
ENTRYPOINT ["/last/sandbox-runner"]
```

Append to `Makefile`, after the existing `images` target:
```make
.PHONY: image-sandbox-base
image-sandbox-base: ## Build the sandbox base image.
	$(CONTAINER_TOOL) build -f images/sandbox-base/Dockerfile -t $(IMAGE_REGISTRY)/last-sandbox-base:$(IMAGE_TAG) .
```
Then edit the existing `images:` target line so it reads `images: image-sandbox-base ## Build container images for all components.`

- [ ] **Step 3: Build and smoke-test (Docker must be running)**

Run: `make image-sandbox-base IMAGE_TAG=dev && ./hack/sandbox-smoke.sh ghcr.io/moogie-labs/last-sandbox-base:dev`
Expected: the envelope prints, followed by `sandbox smoke test passed`.
If `kubectl` checksum verification fails, check that `KUBECTL_VERSION` exists at dl.k8s.io and use the newest available `v1.36.x`. Record the version you used in the report.

- [ ] **Step 4: Commit**

```bash
git add images/sandbox-base hack/sandbox-smoke.sh Makefile
git commit -m "feat(sandbox): add the sandbox base image with the runner and common CLIs"
```

### Task 3: Labels and per-run egress policy

**Files:**
- Create: `internal/sandbox/labels.go`, `internal/sandbox/egress.go`, `internal/sandbox/egress_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (package `sandbox`):
  - Constants `LabelManagedBy = "app.kubernetes.io/managed-by"`, `ManagedByValue = "last-gateway"`, `LabelRun = "last.moogie-labs.com/run"`, `LabelName = "app.kubernetes.io/name"`, `NameValue = "last-sandbox"`.
  - `func RunLabels(runID string) map[string]string` (managed-by + run).
  - `type EgressRule struct{ CIDR string; Port int32 }` with `String() string` (e.g. `"10.1.2.3/32:443"`).
  - `var ErrHostEgressUnsupported`.
  - `func ParseEgress(s string) (EgressRule, error)`.
  - `func EgressPolicy(namespace, runID string, rules []EgressRule) *networkingv1.NetworkPolicy` (nil when there are no rules).
  - `func APIServerEgress(ctx context.Context, kube kubernetes.Interface) (clusterIP string, rules []EgressRule, err error)`.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/egress_test.go`:
```go
package sandbox_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func TestParseEgress(t *testing.T) {
	ok := map[string]string{
		"10.1.2.3:443":     "10.1.2.3/32:443",
		"10.0.0.0/8:443":   "10.0.0.0/8:443",
		"10.1.2.3/8:80":    "10.0.0.0/8:80",
		"[fd00::1]:443":    "fd00::1/128:443",
		"[fd00::/8]:6443":  "fd00::/8:6443",
	}
	for in, want := range ok {
		r, err := sandbox.ParseEgress(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseEgress(%q) = %v, %v; want %s", in, r, err, want)
		}
	}
	for _, in := range []string{"example.com:443", "*.amazonaws.com:443"} {
		if _, err := sandbox.ParseEgress(in); !errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			t.Errorf("ParseEgress(%q) err = %v, want ErrHostEgressUnsupported", in, err)
		}
	}
	for _, in := range []string{"10.1.2.3", "10.1.2.3:0", "10.1.2.3:70000", "10.1.2.3:http", ""} {
		if _, err := sandbox.ParseEgress(in); err == nil || errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			t.Errorf("ParseEgress(%q) err = %v, want a format error", in, err)
		}
	}
}

func TestEgressPolicySelectsOnlyTheRun(t *testing.T) {
	rules := []sandbox.EgressRule{{CIDR: "10.1.2.3/32", Port: 443}, {CIDR: "10.0.0.0/8", Port: 80}}
	np := sandbox.EgressPolicy("last-sandbox", "run1", rules)
	if np.Namespace != "last-sandbox" || np.Spec.PodSelector.MatchLabels[sandbox.LabelRun] != "run1" || len(np.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("selector = %+v", np.Spec.PodSelector)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policyTypes = %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[0].To[0].IPBlock.CIDR != "10.1.2.3/32" ||
		*np.Spec.Egress[0].Ports[0].Protocol != corev1.ProtocolTCP || np.Spec.Egress[0].Ports[0].Port.IntVal != 443 {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	if np.Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue || np.Labels[sandbox.LabelRun] != "run1" {
		t.Fatalf("labels = %v", np.Labels)
	}
	if sandbox.EgressPolicy("last-sandbox", "run1", nil) != nil {
		t.Fatal("expected no policy for an empty rule set")
	}
}

func TestAPIServerEgressIncludesClusterIPAndEndpoints(t *testing.T) {
	kube := fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1", Ports: []corev1.ServicePort{{Name: "https", Port: 443}}}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
			Ports:      []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To(int32(6443))}},
		},
	)
	ip, rules, err := sandbox.APIServerEgress(context.Background(), kube)
	if err != nil || ip != "10.96.0.1" {
		t.Fatalf("ip=%s err=%v", ip, err)
	}
	got := map[string]bool{}
	for _, r := range rules {
		got[r.String()] = true
	}
	if !got["10.96.0.1/32:443"] || !got["172.18.0.2/32:6443"] || len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run 'Egress|APIServer'`
Expected: compile errors (`undefined: sandbox.ParseEgress`).

- [ ] **Step 3: Implement**

`internal/sandbox/labels.go`:
```go
// Package sandbox runs commands in locked-down pods on behalf of the gateway (spec §11.1).
package sandbox

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "last-gateway"
	LabelRun       = "last.moogie-labs.com/run"
	LabelName      = "app.kubernetes.io/name"
	NameValue      = "last-sandbox"
)

// RunLabels are carried by every object a run creates, so cleanup and the reaper can find them.
func RunLabels(runID string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedByValue, LabelRun: runID}
}
```

`internal/sandbox/egress.go`:
```go
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// ErrHostEgressUnsupported: host-name egress needs an FQDN-aware CNI (spec §11.1).
var ErrHostEgressUnsupported = errors.New("sandbox: host-name egress is not supported (it needs Cilium); use an IP or CIDR destination")

// EgressRule allows TCP to one CIDR and port.
type EgressRule struct {
	CIDR string
	Port int32
}

func (r EgressRule) String() string { return fmt.Sprintf("%s:%d", r.CIDR, r.Port) }

// ParseEgress parses "IP:port", "CIDR:port", "[IPv6]:port" or "[IPv6/len]:port".
func ParseEgress(s string) (EgressRule, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return EgressRule{}, fmt.Errorf("sandbox: egress %q: %w", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return EgressRule{}, fmt.Errorf("sandbox: egress %q: port must be 1-65535", s)
	}
	var cidr string
	if _, network, cerr := net.ParseCIDR(host); cerr == nil {
		cidr = network.String()
	} else if ip := net.ParseIP(host); ip != nil {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		cidr = fmt.Sprintf("%s/%d", ip.String(), bits)
	} else {
		return EgressRule{}, fmt.Errorf("%w: %q", ErrHostEgressUnsupported, s)
	}
	return EgressRule{CIDR: cidr, Port: int32(port)}, nil
}

// EgressPolicy allows the run's pod to reach exactly the given destinations. The
// namespace's default-deny policy blocks everything else. Nil means "nothing to allow".
func EgressPolicy(namespace, runID string, rules []EgressRule) *networkingv1.NetworkPolicy {
	if len(rules) == 0 {
		return nil
	}
	tcp := corev1.ProtocolTCP
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID, Namespace: namespace, Labels: RunLabels(runID)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelRun: runID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	for _, r := range rules {
		port := intstr.FromInt32(r.Port)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	return np
}

// APIServerEgress returns the in-cluster API server's ClusterIP and egress rules for
// both the ClusterIP and its endpoint addresses: depending on the CNI, NetworkPolicy
// is evaluated before or after service DNAT, so both must be allowed.
func APIServerEgress(ctx context.Context, kube kubernetes.Interface) (string, []EgressRule, error) {
	svc, err := kube.CoreV1().Services(metav1.NamespaceDefault).Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: read kubernetes service: %w", err)
	}
	seen := map[string]bool{}
	var rules []EgressRule
	add := func(ip string, port int32) error {
		r, err := ParseEgress(net.JoinHostPort(ip, strconv.Itoa(int(port))))
		if err != nil {
			return err
		}
		if !seen[r.String()] {
			seen[r.String()] = true
			rules = append(rules, r)
		}
		return nil
	}
	for _, p := range svc.Spec.Ports {
		if err := add(svc.Spec.ClusterIP, p.Port); err != nil {
			return "", nil, err
		}
	}
	slices, err := kube.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=kubernetes",
	})
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: list kubernetes endpoints: %w", err)
	}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			for _, addr := range ep.Addresses {
				for _, p := range s.Ports {
					if p.Port != nil {
						if err := add(addr, *p.Port); err != nil {
							return "", nil, err
						}
					}
				}
			}
		}
	}
	return svc.Spec.ClusterIP, rules, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/sandbox/ -count=1 -v -run 'Egress|APIServer'`
Expected: PASS. If `k8s.io/utils/ptr` isn't a direct dependency yet, `go mod tidy` adds it. It is already in the module graph via client-go.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox go.mod go.sum
git commit -m "feat(sandbox): parse egress destinations and build per-run egress policies"
```

### Task 4: Provider credentials

**Files:**
- Create: `internal/sandbox/credentials.go`, `internal/sandbox/credentials_test.go`

**Interfaces:**
- Consumes: `RunLabels` (Task 3); `lastv1alpha1.Provider`, `ProviderKubernetes`, `ProviderEnvSecret` (api/v1alpha1).
- Produces:
  - `type ProviderGetter func(ctx context.Context, name string) (*lastv1alpha1.Provider, error)`.
  - `type IssuedCredential struct{ Provider, Type string; Env, Files []string; ServiceAccount, TTL string }`, with JSON fields `provider, type, env, files, serviceAccount, ttl`. This is the audit payload, so it never holds values.
  - `type CredentialPlan struct{ Provider *lastv1alpha1.Provider; Issued IssuedCredential }`.
  - `func PlanCredentials(ctx context.Context, get ProviderGetter, providerNamespace string, names []string) ([]CredentialPlan, error)`, which reads Provider CRs only and validates.
  - `type CredentialConfig struct{ SandboxNamespace, ProviderNamespace, APIServerHost string }`.
  - `type Materialized struct{ Secret *corev1.Secret; Env []corev1.EnvVar; Mounts []corev1.VolumeMount; Volumes []corev1.Volume; NeedsAPIServer bool }`.
  - `func Materialize(ctx context.Context, kube kubernetes.Interface, cfg CredentialConfig, runID string, plans []CredentialPlan) (Materialized, error)`, which reads provider Secrets and mints tokens but creates nothing in the sandbox namespace.
  - Errors: `ErrMultipleKubernetesProviders`, `ErrUnsupportedProvider`.
  - Constants: `CredsMountPath = "/last/creds"`, `KubeCAMountPath = "/last/kube-ca"`, `DefaultTokenTTL = 10 * time.Minute`.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/credentials_test.go`:
```go
package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const secretValue = "s3cr3t-value"

func providers(ps ...*lastv1alpha1.Provider) sandbox.ProviderGetter {
	m := map[string]*lastv1alpha1.Provider{}
	for _, p := range ps {
		m[p.Name] = p
	}
	return func(_ context.Context, name string) (*lastv1alpha1.Provider, error) {
		if p, ok := m[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("provider %q not found", name)
	}
}

func envProvider() *lastv1alpha1.Provider {
	return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "demo"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "demo-creds"},
		Env:   map[string]string{"DEMO_TOKEN": "token"},
		Files: map[string]string{"config": "/work/.demo/config"},
	}}
}

func k8sProvider(name string) *lastv1alpha1.Provider {
	return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "deployer"}},
	}}
}

var credCfg = sandbox.CredentialConfig{SandboxNamespace: "last-sandbox", ProviderNamespace: "last-gateway", APIServerHost: "10.96.0.1"}

func tokenClient(token string, gotTTL *int64) *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if gotTTL != nil && tr.Spec.ExpirationSeconds != nil {
			*gotTTL = *tr.Spec.ExpirationSeconds
		}
		tr.Status.Token = token
		return true, tr, nil
	})
	return kube
}

func TestEnvSecretProviderMaterializesWithoutLeakingValues(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-creds", Namespace: "last-gateway"},
		Data: map[string][]byte{"token": []byte(secretValue), "config": []byte("cfg-" + secretValue)}})
	plans, err := sandbox.PlanCredentials(ctx, providers(envProvider()), "last-gateway", []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandbox.Materialize(ctx, kube, credCfg, "run1", plans)
	if err != nil {
		t.Fatal(err)
	}
	if m.Secret == nil || m.Secret.Namespace != "last-sandbox" || m.Secret.Labels[sandbox.LabelRun] != "run1" {
		t.Fatalf("secret = %+v", m.Secret)
	}
	if string(m.Secret.Data["demo.env.DEMO_TOKEN"]) != secretValue {
		t.Fatalf("secret data = %v", m.Secret.Data)
	}
	if len(m.Env) != 1 || m.Env[0].Name != "DEMO_TOKEN" || m.Env[0].ValueFrom.SecretKeyRef.Key != "demo.env.DEMO_TOKEN" {
		t.Fatalf("env = %+v", m.Env)
	}
	foundFile := false
	for _, mt := range m.Mounts {
		if mt.MountPath == "/work/.demo/config" && mt.SubPath == "demo.file.config" && mt.ReadOnly {
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("mounts = %+v", m.Mounts)
	}
	issued, _ := json.Marshal(plans[0].Issued)
	if strings.Contains(string(issued), secretValue) || !strings.Contains(string(issued), "DEMO_TOKEN") {
		t.Fatalf("issued description = %s", issued)
	}
}

func TestEnvSecretMissingKeyNamesKeyNotValue(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-creds", Namespace: "last-gateway"},
		Data: map[string][]byte{"token": []byte(secretValue)}})
	plans, _ := sandbox.PlanCredentials(ctx, providers(envProvider()), "last-gateway", []string{"demo"})
	_, err := sandbox.Materialize(ctx, kube, credCfg, "run1", plans)
	if err == nil || !strings.Contains(err.Error(), `"config"`) || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanRejectsFilePathOutsideWork(t *testing.T) {
	p := envProvider()
	p.Spec.Files = map[string]string{"config": "/etc/demo"}
	if _, err := sandbox.PlanCredentials(context.Background(), providers(p), "last-gateway", []string{"demo"}); err == nil {
		t.Fatal("accepted a file path outside /work")
	}
}

func TestKubernetesProviderMintsTokenAndKubeconfig(t *testing.T) {
	ctx := context.Background()
	var ttl int64
	kube := tokenClient("tok-"+secretValue, &ttl)
	plans, err := sandbox.PlanCredentials(ctx, providers(k8sProvider("cluster")), "last-gateway", []string{"cluster"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandbox.Materialize(ctx, kube, credCfg, "run1", plans)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != int64(sandbox.DefaultTokenTTL.Seconds()) || !m.NeedsAPIServer {
		t.Fatalf("ttl=%d needsAPIServer=%v", ttl, m.NeedsAPIServer)
	}
	kc := string(m.Secret.Data["cluster.kubeconfig"])
	for _, want := range []string{"server: https://10.96.0.1:443", "tls-server-name: kubernetes.default.svc",
		"certificate-authority: /last/kube-ca/ca.crt", "tokenFile: /last/creds/cluster.token"} {
		if !strings.Contains(kc, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, kc)
		}
	}
	if string(m.Secret.Data["cluster.token"]) != "tok-"+secretValue {
		t.Fatal("token not stored in the run secret")
	}
	hasEnv := false
	for _, e := range m.Env {
		if e.Name == "KUBECONFIG" && e.Value == "/last/creds/cluster.kubeconfig" {
			hasEnv = true
		}
	}
	if !hasEnv {
		t.Fatalf("env = %+v", m.Env)
	}
	issued, _ := json.Marshal(plans[0].Issued)
	if strings.Contains(string(issued), secretValue) || !strings.Contains(string(issued), `"serviceAccount":"last-gateway/deployer"`) {
		t.Fatalf("issued = %s", issued)
	}
}

func TestAtMostOneKubernetesProvider(t *testing.T) {
	_, err := sandbox.PlanCredentials(context.Background(), providers(k8sProvider("a"), k8sProvider("b")), "last-gateway", []string{"a", "b"})
	if !errors.Is(err, sandbox.ErrMultipleKubernetesProviders) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownProviderAndDuplicateEnv(t *testing.T) {
	ctx := context.Background()
	if _, err := sandbox.PlanCredentials(ctx, providers(), "last-gateway", []string{"missing"}); err == nil {
		t.Fatal("accepted an unknown provider")
	}
	a, b := envProvider(), envProvider()
	b.Name = "demo2"
	b.Spec.Files = nil
	if _, err := sandbox.PlanCredentials(ctx, providers(a, b), "last-gateway", []string{"demo", "demo2"}); err == nil || !strings.Contains(err.Error(), "DEMO_TOKEN") {
		t.Fatalf("duplicate env var err = %v", err)
	}
}

func TestNoProvidersMaterializesNothing(t *testing.T) {
	m, err := sandbox.Materialize(context.Background(), fake.NewClientset(), credCfg, "run1", nil)
	if err != nil || m.Secret != nil || len(m.Env) != 0 || len(m.Volumes) != 0 {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run 'Provider|Credential|Materializes|Plan'`
Expected: compile errors (`undefined: sandbox.PlanCredentials`).

- [ ] **Step 3: Implement**

`internal/sandbox/credentials.go`:
```go
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	CredsMountPath  = "/last/creds"
	KubeCAMountPath = "/last/kube-ca"
	DefaultTokenTTL = 10 * time.Minute
	credsVolume     = "creds"
	kubeCAVolume    = "kube-ca"
	kubeRootCA      = "kube-root-ca.crt" // ConfigMap present in every namespace
)

var (
	ErrMultipleKubernetesProviders = errors.New("sandbox: at most one kubernetes provider per command")
	ErrUnsupportedProvider         = errors.New("sandbox: unsupported provider type")
)

// ProviderGetter reads a cluster-scoped Provider by name.
type ProviderGetter func(ctx context.Context, name string) (*lastv1alpha1.Provider, error)

// IssuedCredential describes an issued credential for the audit log. It never holds values.
type IssuedCredential struct {
	Provider       string   `json:"provider"`
	Type           string   `json:"type"`
	Env            []string `json:"env,omitempty"`
	Files          []string `json:"files,omitempty"`
	ServiceAccount string   `json:"serviceAccount,omitempty"`
	TTL            string   `json:"ttl,omitempty"`
}

// CredentialPlan is a validated provider plus its audit description.
type CredentialPlan struct {
	Provider *lastv1alpha1.Provider
	Issued   IssuedCredential
}

// CredentialConfig locates the namespaces and the API server.
type CredentialConfig struct {
	SandboxNamespace  string
	ProviderNamespace string
	APIServerHost     string // the kubernetes Service ClusterIP
}

// Materialized is what a run's pod needs to present its credentials.
type Materialized struct {
	Secret         *corev1.Secret // nil when the run has no providers
	Env            []corev1.EnvVar
	Mounts         []corev1.VolumeMount
	Volumes        []corev1.Volume
	NeedsAPIServer bool
}

// PlanCredentials resolves and validates providers without touching any secret,
// so the caller can audit the issuance before it happens.
func PlanCredentials(ctx context.Context, get ProviderGetter, providerNamespace string, names []string) ([]CredentialPlan, error) {
	plans := make([]CredentialPlan, 0, len(names))
	envSeen := map[string]string{}
	kubernetesCount := 0
	for _, name := range names {
		p, err := get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("sandbox: provider %q: %w", name, err)
		}
		issued := IssuedCredential{Provider: name, Type: string(p.Spec.Type)}
		switch p.Spec.Type {
		case lastv1alpha1.ProviderEnvSecret:
			if p.Spec.SecretRef == nil {
				return nil, fmt.Errorf("sandbox: provider %q has no secretRef", name)
			}
			for _, v := range sortedKeys(p.Spec.Env) {
				if other, dup := envSeen[v]; dup {
					return nil, fmt.Errorf("sandbox: env var %s is set by both provider %q and %q", v, other, name)
				}
				envSeen[v] = name
				issued.Env = append(issued.Env, v)
			}
			for _, key := range sortedKeys(p.Spec.Files) {
				path := p.Spec.Files[key]
				if !underWork(path) {
					return nil, fmt.Errorf("sandbox: provider %q file path %q must be under /work/", name, path)
				}
				issued.Files = append(issued.Files, path)
			}
		case lastv1alpha1.ProviderKubernetes:
			kubernetesCount++
			if kubernetesCount > 1 {
				return nil, ErrMultipleKubernetesProviders
			}
			if p.Spec.Kubernetes == nil {
				return nil, fmt.Errorf("sandbox: provider %q has no kubernetes section", name)
			}
			issued.ServiceAccount = providerNamespace + "/" + p.Spec.Kubernetes.ServiceAccountRef.Name
			issued.TTL = tokenTTL(p).String()
		default:
			return nil, fmt.Errorf("%w: %q (provider %q)", ErrUnsupportedProvider, p.Spec.Type, name)
		}
		plans = append(plans, CredentialPlan{Provider: p, Issued: issued})
	}
	return plans, nil
}

// Materialize reads provider secrets and mints tokens into a per-run Secret
// (not yet created) plus the env, mounts and volumes that expose it.
func Materialize(ctx context.Context, kube kubernetes.Interface, cfg CredentialConfig, runID string, plans []CredentialPlan) (Materialized, error) {
	var m Materialized
	if len(plans) == 0 {
		return m, nil
	}
	secretName := "last-run-" + runID + "-creds"
	data := map[string][]byte{}
	for _, plan := range plans {
		p := plan.Provider
		switch p.Spec.Type {
		case lastv1alpha1.ProviderEnvSecret:
			src, err := kube.CoreV1().Secrets(cfg.ProviderNamespace).Get(ctx, p.Spec.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q secret: %w", p.Name, err)
			}
			for _, envVar := range sortedKeys(p.Spec.Env) {
				val, err := secretKey(src, p.Name, p.Spec.Env[envVar])
				if err != nil {
					return Materialized{}, err
				}
				key := p.Name + ".env." + envVar
				data[key] = val
				m.Env = append(m.Env, corev1.EnvVar{Name: envVar, ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: key},
				}})
			}
			for _, srcKey := range sortedKeys(p.Spec.Files) {
				val, err := secretKey(src, p.Name, srcKey)
				if err != nil {
					return Materialized{}, err
				}
				key := p.Name + ".file." + srcKey
				data[key] = val
				m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: credsVolume, MountPath: filepath.Clean(p.Spec.Files[srcKey]), SubPath: key, ReadOnly: true})
			}
		case lastv1alpha1.ProviderKubernetes:
			sa := p.Spec.Kubernetes.ServiceAccountRef.Name
			secs := int64(tokenTTL(p).Seconds())
			tr, err := kube.CoreV1().ServiceAccounts(cfg.ProviderNamespace).CreateToken(ctx, sa,
				&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &secs}}, metav1.CreateOptions{})
			if err != nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q token for %s/%s: %w", p.Name, cfg.ProviderNamespace, sa, err)
			}
			data[p.Name+".token"] = []byte(tr.Status.Token)
			data[p.Name+".kubeconfig"] = []byte(kubeconfig(cfg.APIServerHost, CredsMountPath+"/"+p.Name+".token"))
			m.Env = append(m.Env, corev1.EnvVar{Name: "KUBECONFIG", Value: CredsMountPath + "/" + p.Name + ".kubeconfig"})
			m.NeedsAPIServer = true
		}
	}
	m.Secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cfg.SandboxNamespace, Labels: RunLabels(runID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	m.Volumes = append(m.Volumes, corev1.Volume{Name: credsVolume, VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: ptrInt32(0o440)},
	}})
	m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: credsVolume, MountPath: CredsMountPath, ReadOnly: true})
	if m.NeedsAPIServer {
		m.Volumes = append(m.Volumes, corev1.Volume{Name: kubeCAVolume, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: kubeRootCA}},
		}})
		m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: kubeCAVolume, MountPath: KubeCAMountPath, ReadOnly: true})
	}
	return m, nil
}

func kubeconfig(apiServerHost, tokenFile string) string {
	return strings.Join([]string{
		"apiVersion: v1",
		"kind: Config",
		"clusters:",
		"- name: in-cluster",
		"  cluster:",
		"    server: https://" + net.JoinHostPort(apiServerHost, "443"),
		"    certificate-authority: " + KubeCAMountPath + "/ca.crt",
		"    tls-server-name: kubernetes.default.svc",
		"users:",
		"- name: provider",
		"  user:",
		"    tokenFile: " + tokenFile,
		"contexts:",
		"- name: provider",
		"  context:",
		"    cluster: in-cluster",
		"    user: provider",
		"current-context: provider",
		"",
	}, "\n")
}

func secretKey(s *corev1.Secret, provider, key string) ([]byte, error) {
	v, ok := s.Data[key]
	if !ok {
		return nil, fmt.Errorf("sandbox: provider %q secret %s/%s has no key %q", provider, s.Namespace, s.Name, key)
	}
	return v, nil
}

func tokenTTL(p *lastv1alpha1.Provider) time.Duration {
	if p.Spec.Kubernetes != nil && p.Spec.Kubernetes.TokenTTL != nil && p.Spec.Kubernetes.TokenTTL.Duration >= DefaultTokenTTL {
		return p.Spec.Kubernetes.TokenTTL.Duration
	}
	return DefaultTokenTTL // the API server's minimum token lifetime
}

func underWork(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(path) && strings.HasPrefix(clean, "/work/")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func ptrInt32(v int32) *int32 { return &v }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/sandbox/ -count=1 -v -run 'Provider|Credential|Materializes|Plan|Kubernetes|EnvSecret|Unknown|NoProviders'`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox
git commit -m "feat(sandbox): materialize env-secret and kubernetes provider credentials"
```

### Task 5: Run spec, spec Secret, and sandbox Pod builders

**Files:**
- Create: `internal/sandbox/pod.go`, `internal/sandbox/pod_test.go`

**Interfaces:**
- Consumes: `runnerapi.Spec/Command/Version/SpecPath` (Task 1); `RunLabels`, label constants (Task 3); `Materialized` (Task 4); `lastv1alpha1.Command` (api/v1alpha1).
- Produces:
  - `var ErrImageBuildsUnsupported`; `const DefaultCommandTimeout = 5 * time.Minute`.
  - `func RunnerSpec(cmds []lastv1alpha1.Command) (runnerapi.Spec, time.Duration, error)`. It returns the spec and the sum of the commands' timeouts.
  - `func SpecSecret(namespace, runID string, spec runnerapi.Spec) (*corev1.Secret, error)`. The Secret is named `last-run-<runID>-spec` and holds the key `spec.json`.
  - `type PodConfig struct{ Namespace, Image string; ImagePullPolicy corev1.PullPolicy; Resources corev1.ResourceRequirements }`.
  - `func SandboxPod(cfg PodConfig, runID string, creds Materialized, activeDeadline time.Duration) *corev1.Pod`. The pod is named `last-run-<runID>` with a single container named `runner`.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/pod_test.go`:
```go
package sandbox_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func TestRunnerSpecConvertsCommands(t *testing.T) {
	spec, total, err := sandbox.RunnerSpec([]lastv1alpha1.Command{
		{ID: "a", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods"}}, Timeout: &metav1.Duration{Duration: time.Minute}},
		{Run: lastv1alpha1.CommandRun{Script: "echo hi"}, Files: map[string]string{"/work/x": "y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != runnerapi.Version || len(spec.Commands) != 2 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Commands[0].ID != "a" || spec.Commands[0].TimeoutSeconds != 60 || spec.Commands[0].Argv[0] != "kubectl" {
		t.Fatalf("cmd0 = %+v", spec.Commands[0])
	}
	if spec.Commands[1].ID != "cmd-1" || spec.Commands[1].TimeoutSeconds != 300 || spec.Commands[1].Files["/work/x"] != "y" {
		t.Fatalf("cmd1 = %+v", spec.Commands[1])
	}
	if total != 6*time.Minute {
		t.Fatalf("total timeout = %s", total)
	}
}

func TestRunnerSpecRejectsImageBuildsAndBadCommands(t *testing.T) {
	_, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{{Run: lastv1alpha1.CommandRun{Argv: []string{"aws"}},
		Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "awscli"}}}}}})
	if !errors.Is(err, sandbox.ErrImageBuildsUnsupported) {
		t.Fatalf("image err = %v", err)
	}
	for _, c := range []lastv1alpha1.Command{
		{Run: lastv1alpha1.CommandRun{}},
		{Run: lastv1alpha1.CommandRun{Argv: []string{"a"}, Script: "b"}},
		{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}, Files: map[string]string{"/etc/x": "y"}},
	} {
		if _, _, err := sandbox.RunnerSpec([]lastv1alpha1.Command{c}); err == nil {
			t.Errorf("accepted invalid command %+v", c)
		}
	}
	if _, _, err := sandbox.RunnerSpec(nil); err == nil {
		t.Error("accepted an empty command list")
	}
}

func TestSpecSecretHoldsTheSpec(t *testing.T) {
	spec := runnerapi.Spec{Version: 1, Commands: []runnerapi.Command{{ID: "a", Argv: []string{"true"}, TimeoutSeconds: 5}}}
	s, err := sandbox.SpecSecret("last-sandbox", "run1", spec)
	if err != nil {
		t.Fatal(err)
	}
	var back runnerapi.Spec
	if err := json.Unmarshal(s.Data["spec.json"], &back); err != nil || back.Commands[0].ID != "a" {
		t.Fatalf("spec.json = %s (%v)", s.Data["spec.json"], err)
	}
	if s.Name != "last-run-run1-spec" || s.Namespace != "last-sandbox" || s.Labels[sandbox.LabelRun] != "run1" {
		t.Fatalf("secret meta = %+v", s.ObjectMeta)
	}
}

func TestSandboxPodIsLockedDown(t *testing.T) {
	creds := sandbox.Materialized{Env: []corev1.EnvVar{{Name: "DEMO", Value: "x"}}}
	pod := sandbox.SandboxPod(sandbox.PodConfig{Namespace: "last-sandbox", Image: "img:tag", ImagePullPolicy: corev1.PullIfNotPresent},
		"run1", creds, 7*time.Minute)
	ps := pod.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever || ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken ||
		ps.EnableServiceLinks == nil || *ps.EnableServiceLinks || *ps.ActiveDeadlineSeconds != 420 {
		t.Fatalf("pod spec = %+v", ps)
	}
	if !*ps.SecurityContext.RunAsNonRoot || *ps.SecurityContext.RunAsUser != 65532 || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security = %+v", ps.SecurityContext)
	}
	c := ps.Containers[0]
	if c.Name != "runner" || c.Image != "img:tag" || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container = %+v", c)
	}
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = true
	}
	if !mounts["/work"] || !mounts["/tmp"] || !mounts["/last/spec"] {
		t.Fatalf("mounts = %+v", c.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/work" || env["DEMO"] != "x" {
		t.Fatalf("env = %+v", c.Env)
	}
	if pod.Labels[sandbox.LabelRun] != "run1" || pod.Labels[sandbox.LabelName] != sandbox.NameValue || pod.Name != "last-run-run1" {
		t.Fatalf("meta = %+v", pod.ObjectMeta)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run 'RunnerSpec|SpecSecret|SandboxPod'`
Expected: compile errors (`undefined: sandbox.RunnerSpec`).

- [ ] **Step 3: Implement**

`internal/sandbox/pod.go`:
```go
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	DefaultCommandTimeout = 5 * time.Minute
	sandboxUID            = int64(65532)
	specVolume            = "spec"
	workVolume            = "work"
	tmpVolume             = "tmp"
)

// ErrImageBuildsUnsupported: image.install needs automatic image builds (spec §11.2, M4).
var ErrImageBuildsUnsupported = errors.New("sandbox: image.install is not supported until image builds (M4)")

// RunnerSpec converts API commands into the runner's spec and returns the summed timeout.
func RunnerSpec(cmds []lastv1alpha1.Command) (runnerapi.Spec, time.Duration, error) {
	if len(cmds) == 0 {
		return runnerapi.Spec{}, 0, errors.New("sandbox: no commands to run")
	}
	spec := runnerapi.Spec{Version: runnerapi.Version}
	var total time.Duration
	for i, c := range cmds {
		if c.Image != nil {
			return runnerapi.Spec{}, 0, ErrImageBuildsUnsupported
		}
		if (len(c.Run.Argv) == 0) == (c.Run.Script == "") {
			return runnerapi.Spec{}, 0, fmt.Errorf("sandbox: command %d: exactly one of argv or script must be set", i)
		}
		for p := range c.Files {
			if !underWork(p) {
				return runnerapi.Spec{}, 0, fmt.Errorf("sandbox: command %d: file path %q must be under /work/", i, p)
			}
		}
		timeout := DefaultCommandTimeout
		if c.Timeout != nil && c.Timeout.Duration > 0 {
			timeout = c.Timeout.Duration
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("cmd-%d", i)
		}
		spec.Commands = append(spec.Commands, runnerapi.Command{
			ID: id, Argv: c.Run.Argv, Script: c.Run.Script, Files: c.Files,
			TimeoutSeconds: int64((timeout + time.Second - 1) / time.Second), // round up; never 0
		})
		total += timeout
	}
	return spec, total, nil
}

// SpecSecret carries the run spec. It is a Secret because command files may hold
// sensitive content (e.g. manifests with embedded config).
func SpecSecret(namespace, runID string, spec runnerapi.Spec) (*corev1.Secret, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode spec: %w", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID + "-spec", Namespace: namespace, Labels: RunLabels(runID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"spec.json": raw},
	}, nil
}

// PodConfig is the per-installation sandbox pod configuration.
type PodConfig struct {
	Namespace       string
	Image           string
	ImagePullPolicy corev1.PullPolicy
	Resources       corev1.ResourceRequirements
}

// SandboxPod builds the locked-down pod for a run (spec §11.1).
func SandboxPod(cfg PodConfig, runID string, creds Materialized, activeDeadline time.Duration) *corev1.Pod {
	labels := RunLabels(runID)
	labels[LabelName] = NameValue
	no, yes := false, true
	uid := sandboxUID
	deadline := int64(activeDeadline / time.Second)
	workLimit := resource.MustParse("1Gi")
	tmpLimit := resource.MustParse("64Mi")

	mounts := []corev1.VolumeMount{
		{Name: workVolume, MountPath: "/work"},
		{Name: tmpVolume, MountPath: "/tmp"},
		{Name: specVolume, MountPath: "/last/spec", ReadOnly: true},
	}
	mounts = append(mounts, creds.Mounts...)
	volumes := []corev1.Volume{
		{Name: workVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workLimit}}},
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}},
		{Name: specVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: "last-run-" + runID + "-spec", DefaultMode: ptrInt32(0o440),
		}}},
	}
	volumes = append(volumes, creds.Volumes...)
	env := append([]corev1.EnvVar{{Name: "HOME", Value: "/work"}}, creds.Env...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "last-run-" + runID, Namespace: cfg.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ActiveDeadlineSeconds:        &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "runner",
				Image:           cfg.Image,
				ImagePullPolicy: cfg.ImagePullPolicy,
				Env:             env,
				VolumeMounts:    mounts,
				Resources:       cfg.Resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					RunAsNonRoot:             &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: volumes,
		},
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/sandbox/ -count=1 -v -run 'RunnerSpec|SpecSecret|SandboxPod'`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox
git commit -m "feat(sandbox): build run specs, spec secrets and locked-down sandbox pods"
```

### Task 6: Priority scheduler

**Files:**
- Create: `internal/sandbox/scheduler.go`, `internal/sandbox/scheduler_test.go`

**Interfaces:**
- Produces:
  - `type Priority int` with constants, highest priority first: `PriorityExecution` (plan execution and teardown), `PriorityDiscovery`, `PriorityBuild` (image builds for probes, M4), `PriorityProbe`.
  - `func NewScheduler(max int) *Scheduler`. A `max` below 1 becomes 1.
  - `func (s *Scheduler) Acquire(ctx context.Context, p Priority) (release func(), err error)`. `release` is idempotent.
  - `func (s *Scheduler) Waiting(p Priority) int`.
- Semantics (spec §10.1):
  - The cap applies cluster-wide, to sandbox pods and build pods together.
  - Waiters are served strictly by priority class, and in FIFO order within a class.
  - A new caller never jumps ahead of queued waiters.
  - A cancelled waiter leaves the queue and never leaks a slot.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/scheduler_test.go`:
```go
package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSchedulerEnforcesTheCap(t *testing.T) {
	s := sandbox.NewScheduler(2)
	ctx := context.Background()
	r1, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	r2, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	got := make(chan struct{})
	go func() {
		r3, err := s.Acquire(ctx, sandbox.PriorityProbe)
		if err == nil {
			defer r3()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("third acquire succeeded above the cap")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	<-got
	r2()
}

func TestSchedulerServesHigherPriorityFirst(t *testing.T) {
	s := sandbox.NewScheduler(1)
	ctx := context.Background()
	hold, _ := s.Acquire(ctx, sandbox.PriorityExecution)
	order := make(chan sandbox.Priority, 2)
	var wg sync.WaitGroup
	start := func(p sandbox.Priority) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Acquire(ctx, p)
			if err != nil {
				return
			}
			order <- p
			r()
		}()
	}
	start(sandbox.PriorityProbe)
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == 1 })
	start(sandbox.PriorityExecution)
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityExecution) == 1 })
	hold()
	wg.Wait()
	if first := <-order; first != sandbox.PriorityExecution {
		t.Fatalf("first served = %d, want execution", first)
	}
}

func TestSchedulerIsFIFOWithinAClass(t *testing.T) {
	s := sandbox.NewScheduler(1)
	ctx := context.Background()
	hold, _ := s.Acquire(ctx, sandbox.PriorityProbe)
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _ := s.Acquire(ctx, sandbox.PriorityProbe)
			order <- i
			r()
		}(i)
		waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == i+1 })
	}
	hold()
	wg.Wait()
	for want := 0; want < 3; want++ {
		if got := <-order; got != want {
			t.Fatalf("served %d, want %d", got, want)
		}
	}
}

func TestSchedulerCancelledWaiterLeavesNoLeak(t *testing.T) {
	s := sandbox.NewScheduler(1)
	hold, _ := s.Acquire(context.Background(), sandbox.PriorityExecution)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, sandbox.PriorityProbe)
		errc <- err
	}()
	waitFor(t, func() bool { return s.Waiting(sandbox.PriorityProbe) == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s.Waiting(sandbox.PriorityProbe) != 0 {
		t.Fatal("cancelled waiter still queued")
	}
	hold()
	hold() // idempotent
	r, err := s.Acquire(context.Background(), sandbox.PriorityProbe)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	r()
}

func TestSchedulerUnderRace(t *testing.T) {
	s := sandbox.NewScheduler(3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	active, peak := 0, 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Acquire(context.Background(), sandbox.Priority(i%4))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			r()
		}(i)
	}
	wg.Wait()
	if peak > 3 {
		t.Fatalf("peak concurrency %d above cap 3", peak)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run Scheduler`
Expected: compile errors (`undefined: sandbox.NewScheduler`).

- [ ] **Step 3: Implement**

`internal/sandbox/scheduler.go`:
```go
package sandbox

import (
	"context"
	"sync"
)

// Priority orders queued sandbox work (spec §10.1); lower values are served first.
type Priority int

const (
	PriorityExecution Priority = iota // plan execution and teardown
	PriorityDiscovery
	PriorityBuild // image builds for probes (M4)
	PriorityProbe
	numPriorities
)

type waiter struct {
	ready   chan struct{}
	granted bool
}

// Scheduler caps concurrent sandbox and build pods cluster-wide.
type Scheduler struct {
	mu     sync.Mutex
	max    int
	inUse  int
	queues [numPriorities][]*waiter
}

func NewScheduler(max int) *Scheduler {
	if max < 1 {
		max = 1
	}
	return &Scheduler{max: max}
}

// Acquire blocks until a slot is free for p or ctx ends. The returned release is idempotent.
func (s *Scheduler) Acquire(ctx context.Context, p Priority) (func(), error) {
	if p < 0 || p >= numPriorities {
		p = PriorityProbe
	}
	s.mu.Lock()
	if s.inUse < s.max && s.queuedLocked() == 0 {
		s.inUse++
		s.mu.Unlock()
		return s.releaser(), nil
	}
	w := &waiter{ready: make(chan struct{})}
	s.queues[p] = append(s.queues[p], w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		return s.releaser(), nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if w.granted { // granted concurrently with cancellation: hand the slot on
			s.inUse--
			s.dispatchLocked()
		} else {
			s.removeLocked(p, w)
		}
		return nil, ctx.Err()
	}
}

// Waiting reports how many callers are queued at priority p.
func (s *Scheduler) Waiting(p Priority) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queues[p])
}

func (s *Scheduler) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inUse--
			s.dispatchLocked()
		})
	}
}

func (s *Scheduler) dispatchLocked() {
	for s.inUse < s.max {
		w := s.popLocked()
		if w == nil {
			return
		}
		w.granted = true
		s.inUse++
		close(w.ready)
	}
}

func (s *Scheduler) popLocked() *waiter {
	for p := range s.queues {
		if q := s.queues[p]; len(q) > 0 {
			s.queues[p] = q[1:]
			return q[0]
		}
	}
	return nil
}

func (s *Scheduler) removeLocked(p Priority, w *waiter) {
	q := s.queues[p]
	for i, x := range q {
		if x == w {
			s.queues[p] = append(q[:i], q[i+1:]...)
			return
		}
	}
}

func (s *Scheduler) queuedLocked() int {
	n := 0
	for _, q := range s.queues {
		n += len(q)
	}
	return n
}
```

- [ ] **Step 4: Run the tests (with the race detector)**

Run: `go test -race ./internal/sandbox/ -count=1 -v -run Scheduler`
Expected: PASS (5 tests), no races.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox
git commit -m "feat(sandbox): add the priority concurrency scheduler"
```

### Task 7: Executor — one audited command run

**Files:**
- Create: `internal/sandbox/executor.go`, `internal/sandbox/executor_test.go`

**Interfaces:**
- Consumes:
  - `audit.Writer` methods `Append(ctx, Draft) (Entry, error)` and `Do(ctx, before Draft, action func(context.Context) (any, error), after func(before Entry, result any, actionErr error) Draft) error`;
  - `audit.Open(ctx, sink, signer, WriterConfig, func(StartInfo) any)`, `audit.ErrAuditUnavailable`, and the `audit.Type*` constants;
  - Tasks 1 and 3–6 (`runnerapi`, `ParseEgress`, `EgressPolicy`, `APIServerEgress`, `PlanCredentials`, `Materialize`, `RunnerSpec`, `SpecSecret`, `SandboxPod`, `Scheduler`).
- Produces:
  - `type Auditor interface{ Append(...); Do(...) }`, which `*audit.Writer` satisfies.
  - `type LogReader func(ctx context.Context, namespace, pod string) ([]byte, error)` and `func KubeLogReader(kube kubernetes.Interface) LogReader`.
  - `type ExecutorConfig struct{ Pod PodConfig; ProviderNamespace string; PollInterval, DeadlineOverhead, StartupGrace, CleanupTimeout time.Duration; NewRunID func() string }`. Defaults are 1s, 60s, 2m, 30s, and 10 random hex characters.
  - `func NewExecutor(kube kubernetes.Interface, providers ProviderGetter, auditor Auditor, sched *Scheduler, logs LogReader, cfg ExecutorConfig) *Executor`.
  - `type RunRequest struct{ Intent, Stage, SubjectHash string; AuthorizedBy *audit.Authorization; Command lastv1alpha1.Command; Priority Priority }`.
  - `type RunResult struct{ RunID string; Result runnerapi.Result; CleanupError string }`.
  - `func (e *Executor) Run(ctx context.Context, req RunRequest) (RunResult, error)`.
  - Unexported helpers that Tasks 8 and 9 reuse:
    - `prepare(ctx, cmds []lastv1alpha1.Command, providers, egress []string) (prepared, error)`;
    - `launch(ctx, runID string, p prepared, hooks launchHooks) ([]runnerapi.Result, error)`;
    - `cleanup(ctx, runID string, onRevoked func(context.Context) error) error`;
    - `deleteRun(ctx, selector string) (policiesDeleted int, err error)`;
    - `ruleStrings`.
  - `prepared` holds `spec`, `total`, `rules`, `plans` and `credCfg`. `launchHooks` has two fields, `beforeCredential` and `beforeEgress`; when they are nil, those audit entries are skipped (compact audit).
- Audit sequence for one run (all `Actor: "gateway"`, all carrying the request's Intent, Stage, SubjectHash and AuthorizedBy):
  1. `command.start` `{runId, image, command, providers, egress}`;
  2. one `credential.issued` `{runId, provider, type, env, files, serviceAccount, ttl}` per provider, written before the credential is read or minted;
  3. `egress.granted` `{runId, rules}`, written before the NetworkPolicy is created (only when there are rules);
  4. `egress.revoked` `{runId}`, written after the policy is deleted;
  5. `command.result` `{runId, startSeq, exitCode, stdout, stderr, stdoutTruncated, stderrTruncated, error, cleanupError}`.
- `Run` returns a non-nil error only when the sandbox infrastructure failed or the audit log was unavailable. A command that exits non-zero is reported in `RunResult.Result.ExitCode` with a nil error.

- [ ] **Step 1: Write the failing tests**

`internal/sandbox/executor_test.go`:
```go
package sandbox_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const execChain = "exec-test"

func newAudit(t *testing.T) (*audit.Writer, *audittest.MemSink) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	sink := audittest.NewMemSink()
	w, err := audit.Open(context.Background(), sink, audit.NewSigner(key),
		audit.WriterConfig{ChainID: execChain, Retention: time.Hour}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return w, sink
}

func entries(t *testing.T, sink *audittest.MemSink) []audit.Entry {
	t.Helper()
	ctx := context.Background()
	objs, err := sink.List(ctx, audit.EntryPrefix(execChain), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Entry
	for _, o := range objs {
		body, err := sink.Get(ctx, o.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		var e audit.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func entryTypes(es []audit.Entry) []string {
	var ts []string
	for _, e := range es {
		if e.Type != audit.TypeGatewayStart && e.Type != audit.TypeCheckpoint {
			ts = append(ts, e.Type)
		}
	}
	return ts
}

// fakeCluster records created resource kinds and gives created pods the given phase.
func fakeCluster(phase corev1.PodPhase, objs ...runtime.Object) (*fake.Clientset, *[]string) {
	kube := fake.NewClientset(objs...)
	created := &[]string{}
	kube.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*created = append(*created, a.GetResource().Resource)
		if pod, ok := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = phase
		}
		return false, nil, nil
	})
	return kube, created
}

func envelopeLogs(results ...runnerapi.Result) sandbox.LogReader {
	return func(context.Context, string, string) ([]byte, error) {
		b, _ := json.Marshal(runnerapi.Envelope{Version: runnerapi.Version, Results: results})
		return append([]byte(runnerapi.ResultPrefix), b...), nil
	}
}

func demoSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-creds", Namespace: "last-gateway"},
		Data: map[string][]byte{"token": []byte(secretValue), "config": []byte("cfg-" + secretValue)}}
}

func apiServerObjects() []runtime.Object {
	return []runtime.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1", Ports: []corev1.ServicePort{{Name: "https", Port: 443}}}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
			Ports:      []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To(int32(6443))}},
		},
	}
}

func newExec(kube *fake.Clientset, aud sandbox.Auditor, logs sandbox.LogReader) *sandbox.Executor {
	return sandbox.NewExecutor(kube, providers(envProvider(), k8sProvider("cluster")), aud, sandbox.NewScheduler(2), logs,
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: "last-sandbox", Image: "img:test"},
			ProviderNamespace: "last-gateway",
			PollInterval:      time.Millisecond,
			DeadlineOverhead:  time.Millisecond,
			StartupGrace:      50 * time.Millisecond,
			CleanupTimeout:    time.Second,
			NewRunID:          func() string { return "run1" },
		})
}

func assertCleaned(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	ctx := context.Background()
	pods, _ := kube.CoreV1().Pods("last-sandbox").List(ctx, metav1.ListOptions{})
	secrets, _ := kube.CoreV1().Secrets("last-sandbox").List(ctx, metav1.ListOptions{})
	nps, _ := kube.NetworkingV1().NetworkPolicies("last-sandbox").List(ctx, metav1.ListOptions{})
	if len(pods.Items)+len(secrets.Items)+len(nps.Items) != 0 {
		t.Fatalf("leftovers: %d pods, %d secrets, %d policies", len(pods.Items), len(secrets.Items), len(nps.Items))
	}
}

func cmd(providers []string, egress []string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: "c1", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", "ok"}},
		Providers: providers, Egress: egress, Timeout: &metav1.Duration{Duration: 10 * time.Millisecond}}
}

func TestRunSucceedsAndAuditsEveryStepInOrder(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", ExitCode: 0, Stdout: "ok\n"})).Run(context.Background(),
		sandbox.RunRequest{Intent: "team-x/web", Stage: "Create", Command: cmd([]string{"demo"}, []string{"10.1.2.3:443"})})
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != "run1" || res.Result.Stdout != "ok\n" || res.Result.ExitCode != 0 || res.CleanupError != "" {
		t.Fatalf("result = %+v", res)
	}
	es := entries(t, sink)
	want := []string{audit.TypeCommandStart, audit.TypeCredentialIssued, audit.TypeEgressGranted, audit.TypeEgressRevoked, audit.TypeCommandResult}
	if got := entryTypes(es); !slices.Equal(got, want) {
		t.Fatalf("audit types = %v, want %v", got, want)
	}
	for _, e := range es {
		if strings.Contains(string(e.Payload), secretValue) {
			t.Fatalf("entry %s leaks a credential value: %s", e.Type, e.Payload)
		}
		if e.Type != audit.TypeGatewayStart && (e.Intent != "team-x/web" || e.Stage != "Create") {
			t.Fatalf("entry %s lacks intent/stage", e.Type)
		}
	}
	last := es[len(es)-1]
	if !strings.Contains(string(last.Payload), `"stdout":"ok\n"`) || !strings.Contains(string(last.Payload), `"startSeq":1`) {
		t.Fatalf("command.result payload = %s", last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunReportsNonZeroExitWithoutError(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", ExitCode: 3, Stderr: "boom"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd(nil, nil)})
	if err != nil || res.Result.ExitCode != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	es := entries(t, sink)
	if !strings.Contains(string(es[len(es)-1].Payload), `"exitCode":3`) {
		t.Fatalf("result payload = %s", es[len(es)-1].Payload)
	}
	assertCleaned(t, kube)
}

func TestRunTimesOutWhenThePodNeverFinishes(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	start := time.Now()
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Command: cmd(nil, nil)})
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	es := entries(t, sink)
	if last := es[len(es)-1]; last.Type != audit.TypeCommandResult || !strings.Contains(string(last.Payload), "did not finish") {
		t.Fatalf("last entry = %s %s", last.Type, last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunWithoutAnEnvelopeFails(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodFailed)
	aud, _ := newAudit(t)
	garbage := func(context.Context, string, string) ([]byte, error) { return []byte("segfault\n"), nil }
	_, err := newExec(kube, aud, garbage).Run(context.Background(), sandbox.RunRequest{Command: cmd(nil, nil)})
	if err == nil || !strings.Contains(err.Error(), "no result envelope") {
		t.Fatalf("err = %v", err)
	}
	assertCleaned(t, kube)
}

func TestRunFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd([]string{"demo"}, nil)})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if len(*created) != 0 {
		t.Fatalf("created %v although the audit log was down", *created)
	}
}

func TestRunWithKubernetesProviderAllowsTheAPIServer(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd([]string{"cluster"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var granted string
	for _, e := range entries(t, sink) {
		if strings.Contains(string(e.Payload), secretValue) {
			t.Fatalf("entry %s leaks the token", e.Type)
		}
		if e.Type == audit.TypeEgressGranted {
			granted = string(e.Payload)
		}
	}
	if !strings.Contains(granted, "10.96.0.1/32:443") || !strings.Contains(granted, "172.18.0.2/32:6443") {
		t.Fatalf("egress.granted = %s", granted)
	}
	assertCleaned(t, kube)
}

func TestRunRejectsImageBuildsBeforeAuditing(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	c := cmd(nil, nil)
	c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "x"}}}}
	if _, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Command: c}); !errors.Is(err, sandbox.ErrImageBuildsUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
		t.Fatal("rejected command still produced audit entries or objects")
	}
}
```
The token reactor in `TestRunWithKubernetesProviderAllowsTheAPIServer` is prepended after `fakeCluster`'s create-recording reactor, so it runs first and handles token requests itself.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/sandbox/ -count=1 -run 'TestRun'`
Expected: compile errors (`undefined: sandbox.NewExecutor`).

- [ ] **Step 3: Implement**

`internal/sandbox/executor.go`:
```go
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Auditor is the part of *audit.Writer the executor needs.
type Auditor interface {
	Append(ctx context.Context, d audit.Draft) (audit.Entry, error)
	Do(ctx context.Context, before audit.Draft, action func(context.Context) (any, error),
		after func(before audit.Entry, result any, actionErr error) audit.Draft) error
}

// LogReader returns the runner container's log.
type LogReader func(ctx context.Context, namespace, pod string) ([]byte, error)

// KubeLogReader reads pod logs through the API server.
func KubeLogReader(kube kubernetes.Interface) LogReader {
	return func(ctx context.Context, namespace, pod string) ([]byte, error) {
		return kube.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "runner"}).DoRaw(ctx)
	}
}

// ExecutorConfig configures sandbox runs.
type ExecutorConfig struct {
	Pod               PodConfig
	ProviderNamespace string
	PollInterval      time.Duration // default 1s
	DeadlineOverhead  time.Duration // added to the summed command timeouts for activeDeadlineSeconds; default 60s
	StartupGrace      time.Duration // extra wait for scheduling and image pull; default 2m
	CleanupTimeout    time.Duration // default 30s
	NewRunID          func() string // default: 10 random hex characters
}

func (c ExecutorConfig) withDefaults() ExecutorConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.DeadlineOverhead <= 0 {
		c.DeadlineOverhead = 60 * time.Second
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = 2 * time.Minute
	}
	if c.CleanupTimeout <= 0 {
		c.CleanupTimeout = 30 * time.Second
	}
	if c.NewRunID == nil {
		c.NewRunID = randomRunID
	}
	return c
}

// Executor runs commands in sandbox pods with fail-closed audit (spec §8.3, §11.1).
type Executor struct {
	kube      kubernetes.Interface
	providers ProviderGetter
	auditor   Auditor
	sched     *Scheduler
	logs      LogReader
	cfg       ExecutorConfig
}

func NewExecutor(kube kubernetes.Interface, providers ProviderGetter, auditor Auditor, sched *Scheduler, logs LogReader, cfg ExecutorConfig) *Executor {
	return &Executor{kube: kube, providers: providers, auditor: auditor, sched: sched, logs: logs, cfg: cfg.withDefaults()}
}

// RunRequest is one authorized command execution.
type RunRequest struct {
	Intent       string
	Stage        string
	SubjectHash  string
	AuthorizedBy *audit.Authorization
	Command      lastv1alpha1.Command
	Priority     Priority
}

// RunResult is the command's outcome. Result.ExitCode is -1 when it could not run.
type RunResult struct {
	RunID        string
	Result       runnerapi.Result
	CleanupError string
}

type commandStart struct {
	RunID     string               `json:"runId"`
	Image     string               `json:"image"`
	Command   lastv1alpha1.Command `json:"command"`
	Providers []string             `json:"providers,omitempty"`
	Egress    []string             `json:"egress,omitempty"`
}

type commandResult struct {
	RunID           string `json:"runId"`
	StartSeq        uint64 `json:"startSeq"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error,omitempty"`
	CleanupError    string `json:"cleanupError,omitempty"`
}

type credentialIssued struct {
	RunID string `json:"runId"`
	IssuedCredential
}

type egressGranted struct {
	RunID string   `json:"runId"`
	Rules []string `json:"rules"`
}

type runRef struct {
	RunID string `json:"runId"`
}

// prepared is a validated run: nothing has been audited or created yet.
type prepared struct {
	spec    runnerapi.Spec
	total   time.Duration
	rules   []EgressRule // includes the API server when a kubernetes provider is used
	plans   []CredentialPlan
	credCfg CredentialConfig
}

// launchHooks let the caller audit each credential issuance and the egress grant
// before it happens. Nil hooks skip per-step entries (compact probe audit).
type launchHooks struct {
	beforeCredential func(context.Context, CredentialPlan) error
	beforeEgress     func(context.Context, []EgressRule) error
}

// Run executes one command. Validation failures return before anything is audited
// or created; once command.start is durable, every path ends with command.result.
func (e *Executor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	p, err := e.prepare(ctx, []lastv1alpha1.Command{req.Command}, req.Command.Providers, req.Command.Egress)
	if err != nil {
		return RunResult{}, err
	}
	release, err := e.sched.Acquire(ctx, req.Priority)
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	runID := e.cfg.NewRunID()
	out := RunResult{RunID: runID, Result: runnerapi.Result{ID: req.Command.ID, ExitCode: -1}}
	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: "gateway", Intent: req.Intent, Stage: req.Stage,
			SubjectHash: req.SubjectHash, AuthorizedBy: req.AuthorizedBy, Payload: payload}
	}
	hooks := launchHooks{
		beforeCredential: func(ctx context.Context, cp CredentialPlan) error {
			_, err := e.auditor.Append(ctx, draft(audit.TypeCredentialIssued, credentialIssued{RunID: runID, IssuedCredential: cp.Issued}))
			return err
		},
		beforeEgress: func(ctx context.Context, rules []EgressRule) error {
			_, err := e.auditor.Append(ctx, draft(audit.TypeEgressGranted, egressGranted{RunID: runID, Rules: ruleStrings(rules)}))
			return err
		},
	}
	err = e.auditor.Do(ctx,
		draft(audit.TypeCommandStart, commandStart{RunID: runID, Image: e.cfg.Pod.Image, Command: req.Command,
			Providers: req.Command.Providers, Egress: ruleStrings(p.rules)}),
		func(ctx context.Context) (any, error) {
			results, runErr := e.launch(ctx, runID, p, hooks)
			if runErr == nil && len(results) != 1 {
				runErr = fmt.Errorf("sandbox: expected 1 result, got %d", len(results))
			}
			if runErr == nil {
				out.Result = results[0]
			}
			onRevoked := func(ctx context.Context) error {
				_, err := e.auditor.Append(ctx, draft(audit.TypeEgressRevoked, runRef{RunID: runID}))
				return err
			}
			if cerr := e.cleanup(ctx, runID, onRevoked); cerr != nil {
				out.CleanupError = cerr.Error()
			}
			return nil, runErr
		},
		func(start audit.Entry, _ any, actionErr error) audit.Draft {
			r := commandResult{RunID: runID, StartSeq: start.Seq, ExitCode: out.Result.ExitCode,
				Stdout: out.Result.Stdout, Stderr: out.Result.Stderr,
				StdoutTruncated: out.Result.StdoutTruncated, StderrTruncated: out.Result.StderrTruncated,
				Error: out.Result.Error, CleanupError: out.CleanupError}
			if actionErr != nil {
				r.ExitCode, r.Error = -1, actionErr.Error()
			}
			return draft(audit.TypeCommandResult, r)
		})
	return out, err
}

// prepare validates commands, egress and providers and resolves the API server
// for kubernetes providers. It only reads; nothing is created.
func (e *Executor) prepare(ctx context.Context, cmds []lastv1alpha1.Command, providers, egress []string) (prepared, error) {
	spec, total, err := RunnerSpec(cmds)
	if err != nil {
		return prepared{}, err
	}
	rules, err := parseRules(egress)
	if err != nil {
		return prepared{}, err
	}
	plans, err := PlanCredentials(ctx, e.providers, e.cfg.ProviderNamespace, providers)
	if err != nil {
		return prepared{}, err
	}
	credCfg := CredentialConfig{SandboxNamespace: e.cfg.Pod.Namespace, ProviderNamespace: e.cfg.ProviderNamespace}
	if needsAPIServer(plans) {
		host, apiRules, err := APIServerEgress(ctx, e.kube)
		if err != nil {
			return prepared{}, err
		}
		credCfg.APIServerHost = host
		rules = append(rules, apiRules...)
	}
	return prepared{spec: spec, total: total, rules: rules, plans: plans, credCfg: credCfg}, nil
}

// launch issues credentials, grants egress, runs the pod and returns the runner's results.
func (e *Executor) launch(ctx context.Context, runID string, p prepared, hooks launchHooks) ([]runnerapi.Result, error) {
	ns := e.cfg.Pod.Namespace
	for _, cp := range p.plans { // record each issuance before the credential is read or minted
		if hooks.beforeCredential != nil {
			if err := hooks.beforeCredential(ctx, cp); err != nil {
				return nil, err
			}
		}
	}
	creds, err := Materialize(ctx, e.kube, p.credCfg, runID, p.plans)
	if err != nil {
		return nil, err
	}
	if creds.Secret != nil {
		if _, err := e.kube.CoreV1().Secrets(ns).Create(ctx, creds.Secret, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("sandbox: create credential secret: %w", err)
		}
	}
	if np := EgressPolicy(ns, runID, p.rules); np != nil {
		if hooks.beforeEgress != nil {
			if err := hooks.beforeEgress(ctx, p.rules); err != nil {
				return nil, err
			}
		}
		if _, err := e.kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("sandbox: create egress policy: %w", err)
		}
	}
	specSecret, err := SpecSecret(ns, runID, p.spec)
	if err != nil {
		return nil, err
	}
	if _, err := e.kube.CoreV1().Secrets(ns).Create(ctx, specSecret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: create spec secret: %w", err)
	}
	deadline := p.total + e.cfg.DeadlineOverhead
	pod := SandboxPod(e.cfg.Pod, runID, creds, deadline)
	if _, err := e.kube.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: create pod: %w", err)
	}
	return e.await(ctx, pod.Name, deadline+e.cfg.StartupGrace)
}

// await polls the pod until it finishes or limit passes, then parses its envelope.
func (e *Executor) await(ctx context.Context, name string, limit time.Duration) ([]runnerapi.Result, error) {
	ns := e.cfg.Pod.Namespace
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	var last *corev1.Pod
	for {
		pod, err := e.kube.CoreV1().Pods(ns).Get(waitCtx, name, metav1.GetOptions{})
		if err == nil {
			last = pod
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				return e.collect(ctx, pod)
			}
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("sandbox: pod %s did not finish within %s (%s)", name, limit, describePod(last))
		case <-ticker.C:
		}
	}
}

func (e *Executor) collect(ctx context.Context, pod *corev1.Pod) ([]runnerapi.Result, error) {
	log, err := e.logs(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read pod %s log (%s): %w", pod.Name, describePod(pod), err)
	}
	env, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		return nil, fmt.Errorf("sandbox: pod %s finished (%s) but %w", pod.Name, describePod(pod), err)
	}
	return env.Results, nil
}

// cleanup deletes every object of the run, ignoring the caller's cancellation so
// credentials and egress grants never outlive a cancelled request. onRevoked (may
// be nil) records the revocation once a policy was actually deleted.
func (e *Executor) cleanup(ctx context.Context, runID string, onRevoked func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.CleanupTimeout)
	defer cancel()
	policies, err := e.deleteRun(cctx, LabelRun+"="+runID)
	if policies > 0 && onRevoked != nil {
		err = errors.Join(err, onRevoked(cctx))
	}
	return err
}

// deleteRun deletes the pods, secrets and network policies matching selector in the
// sandbox namespace and reports how many network policies it deleted.
func (e *Executor) deleteRun(ctx context.Context, selector string) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: selector}
	zero := int64(0)
	var errs []error
	ignore := func(err error) error {
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if pods, err := e.kube.CoreV1().Pods(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range pods.Items {
			errs = append(errs, ignore(e.kube.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})))
		}
	}
	if secrets, err := e.kube.CoreV1().Secrets(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, s := range secrets.Items {
			errs = append(errs, ignore(e.kube.CoreV1().Secrets(ns).Delete(ctx, s.Name, metav1.DeleteOptions{})))
		}
	}
	deleted := 0
	if nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts); err != nil {
		errs = append(errs, err)
	} else {
		for _, np := range nps.Items {
			if err := ignore(e.kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, np.Name, metav1.DeleteOptions{})); err != nil {
				errs = append(errs, err)
			} else {
				deleted++
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return deleted, fmt.Errorf("sandbox: cleanup %s: %w", selector, err)
	}
	return deleted, nil
}

func describePod(p *corev1.Pod) string {
	if p == nil {
		return "pod not found"
	}
	desc := "phase " + string(p.Status.Phase)
	if p.Status.Reason != "" {
		desc += ", reason " + p.Status.Reason
	}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Terminated != nil:
			desc += fmt.Sprintf(", container %s terminated: %s (exit %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		case cs.State.Waiting != nil:
			desc += fmt.Sprintf(", container %s waiting: %s", cs.Name, cs.State.Waiting.Reason)
		}
	}
	return desc
}

func parseRules(egress []string) ([]EgressRule, error) {
	rules := make([]EgressRule, 0, len(egress))
	for _, s := range egress {
		r, err := ParseEgress(s)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func ruleStrings(rules []EgressRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.String())
	}
	return out
}

func needsAPIServer(plans []CredentialPlan) bool {
	for _, p := range plans {
		if p.Provider.Spec.Type == lastv1alpha1.ProviderKubernetes {
			return true
		}
	}
	return false
}

func randomRunID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 4: Run the tests (with the race detector)**

Run: `go test -race ./internal/sandbox/... -count=1 -v`
Expected: PASS for all sandbox tests, including the 7 new `TestRun*` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox
git commit -m "feat(sandbox): run audited commands in sandbox pods with cleanup"
```

### Task 8: Probe runs with CEL assertions and compact audit

**Files:**
- Create: `internal/probe/assert.go`, `internal/probe/assert_test.go`, `internal/sandbox/probes.go`, `internal/sandbox/probes_test.go`

**Interfaces:**
- Consumes: from Task 7, `Executor.prepare/launch/cleanup`, `launchHooks{}` (nil hooks give the compact audit form), `ruleStrings`, `Auditor.Do`, and the `audit.TypeProbeStart/TypeProbeResult` constants. From the Task 7 tests: `fakeCluster`, `newAudit`, `entries`, `entryTypes`, `envelopeLogs`, `newExec`, `assertCleaned`.
- Produces:
  - Package `probe`: `type Result struct{ ExitCode int; Stdout, Stderr string }` and `func Evaluate(expr string, r Result) (bool, error)`. Inside the expression, `result.exitCode` is an int and `result.stdout`/`result.stderr` are strings. `result.json` is present only when stdout is a JSON document; integral JSON numbers become ints.
  - Package `sandbox`:
    - `type ProbeRunRequest struct{ Intent string; Probes []lastv1alpha1.Probe }`;
    - `type ProbeOutcome struct{ ID string; Passed bool; Result runnerapi.Result; Error string }`;
    - `func (e *Executor) RunProbes(ctx context.Context, req ProbeRunRequest) ([]ProbeOutcome, error)`. It returns one outcome per probe, in input order. The error is non-nil only for infrastructure or audit failures; a failed assertion is `Passed: false` with no error.
- Grouping (spec §10): probes with the same set of providers and egress run in a single pod. Each pod gets one `probe.start` entry, written before anything is issued or created. It lists the probes, providers, egress, and credential descriptions. Each pod also gets one `probe.result` entry with per-probe outcomes.

- [ ] **Step 1: Write the failing tests**

`internal/probe/assert_test.go`:
```go
package probe_test

import (
	"testing"

	"github.com/moogie-labs/last-operator/internal/probe"
)

func TestEvaluate(t *testing.T) {
	deploy := `{"spec":{"replicas":2},"status":{"readyReplicas":2},"ratio":0.75,"items":[1,2]}`
	cases := []struct {
		name, expr string
		r          probe.Result
		want       bool
		wantErr    bool
	}{
		{"exit ok", "result.exitCode == 0", probe.Result{}, true, false},
		{"exit failed", "result.exitCode == 0", probe.Result{ExitCode: 1}, false, false},
		{"json ints", "result.json.spec.replicas == 2 && result.json.status.readyReplicas == 2", probe.Result{Stdout: deploy}, true, false},
		{"json mismatch", "result.json.spec.replicas == 3", probe.Result{Stdout: deploy}, false, false},
		{"json float", "result.json.ratio > 0.5", probe.Result{Stdout: deploy}, true, false},
		{"json list", "size(result.json.items) == 2", probe.Result{Stdout: deploy}, true, false},
		{"stdout", `result.stdout.contains("ready")`, probe.Result{Stdout: "pod ready\n"}, true, false},
		{"stderr", `result.stderr == ""`, probe.Result{Stderr: "warn"}, false, false},
		{"no json", "result.json.x == 1", probe.Result{Stdout: "not json"}, false, true},
		{"not bool", "result.exitCode + 1", probe.Result{}, false, true},
		{"syntax", "result.exitCode ==", probe.Result{}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := probe.Evaluate(c.expr, c.r)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("Evaluate(%q) = %v, %v; want %v, err=%v", c.expr, got, err, c.want, c.wantErr)
			}
		})
	}
}
```

`internal/sandbox/probes_test.go`:
```go
package sandbox_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func probeOf(id, assert string, egress ...string) lastv1alpha1.Probe {
	return lastv1alpha1.Probe{ID: id, Assert: assert, Command: lastv1alpha1.Command{
		Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deploy", "-o", "json"}}, Egress: egress,
		Timeout: &metav1.Duration{Duration: 10 * time.Millisecond}}}
}

func count(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

func TestRunProbesGroupsByProvidersAndEgressWithCompactAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	logs := envelopeLogs(
		runnerapi.Result{ID: "replicas", Stdout: `{"spec":{"replicas":2}}`},
		runnerapi.Result{ID: "ready", Stdout: `{"status":{"readyReplicas":1}}`},
		runnerapi.Result{ID: "remote", Stdout: "ok"},
	)
	outs, err := newExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Intent: "team-x/web", Probes: []lastv1alpha1.Probe{
		probeOf("replicas", "result.json.spec.replicas == 2"),
		probeOf("ready", "result.json.status.readyReplicas == 2"),
		probeOf("remote", `result.stdout == "ok"`, "10.1.2.3:443"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 || !outs[0].Passed || outs[1].Passed || outs[1].Error != "" || !outs[2].Passed {
		t.Fatalf("outcomes = %+v", outs)
	}
	if n := count(*created, "pods"); n != 2 {
		t.Fatalf("created %d pods, want 2 (one per provider/egress group)", n)
	}
	want := []string{audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult}
	if got := entryTypes(entries(t, sink)); !slices.Equal(got, want) {
		t.Fatalf("audit types = %v, want %v (no per-step entries for probes)", got, want)
	}
	assertCleaned(t, kube)
}

func TestRunProbesBadAssertionIsAFailedProbeNotAnError(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "p", Stdout: "not json"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("p", "result.json.x == 1")}})
	if err != nil || outs[0].Passed || outs[0].Error == "" {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
}

func TestRunProbesInfrastructureFailureFailsEveryProbeInTheGroup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("b", "true")}})
	if err == nil || len(outs) != 2 || outs[0].Passed || outs[1].Passed || !strings.Contains(outs[1].Error, "did not finish") {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	es := entries(t, sink)
	if last := es[len(es)-1]; last.Type != audit.TypeProbeResult || !strings.Contains(string(last.Payload), "did not finish") {
		t.Fatalf("last entry = %s %s", last.Type, last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunProbesRejectsDuplicateIDs(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("a", "true")}})
	if err == nil || len(*created) != 0 {
		t.Fatalf("err=%v created=%v", err, *created)
	}
}

func TestRunProbesFailsClosedWithoutAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "a"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true")}})
	if !errors.Is(err, audit.ErrAuditUnavailable) || count(*created, "pods") != 0 {
		t.Fatalf("err=%v created=%v", err, *created)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go get github.com/google/cel-go@latest && go test ./internal/probe/ ./internal/sandbox/ -count=1 -run 'Evaluate|Probes'`
Expected: compile errors (`undefined: probe.Evaluate`, `undefined: sandbox.ProbeRunRequest`).

- [ ] **Step 3: Implement**

`internal/probe/assert.go`:
```go
// Package probe evaluates probe assertions (spec §10).
package probe

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/cel-go/cel"
)

// Result is what an assertion sees as `result`.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

var celEnv = mustEnv()

func mustEnv() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("result", cel.MapType(cel.StringType, cel.DynType)),
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		panic(err)
	}
	return env
}

// Evaluate runs a CEL assertion against a probe result. `result` has exitCode (int),
// stdout and stderr (strings) and, when stdout is a JSON document, json.
func Evaluate(expr string, r Result) (bool, error) {
	ast, iss := celEnv.Compile(expr)
	if iss.Err() != nil {
		return false, fmt.Errorf("probe: compile %q: %w", expr, iss.Err())
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return false, fmt.Errorf("probe: assertion %q must be a bool expression, not %s", expr, t)
	}
	prg, err := celEnv.Program(ast, cel.CostLimit(1_000_000))
	if err != nil {
		return false, fmt.Errorf("probe: program %q: %w", expr, err)
	}
	vars := map[string]any{"exitCode": int64(r.ExitCode), "stdout": r.Stdout, "stderr": r.Stderr}
	var doc any
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err == nil {
		vars["json"] = normalize(doc)
	}
	out, _, err := prg.Eval(map[string]any{"result": vars})
	if err != nil {
		return false, fmt.Errorf("probe: evaluate %q: %w", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("probe: assertion %q returned %T, not bool", expr, out.Value())
	}
	return b, nil
}

// normalize turns integral JSON numbers into int64 so `== 2` behaves as written.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
		return t
	default:
		return v
	}
}
```

`internal/sandbox/probes.go`:
```go
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/probe"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// ProbeRunRequest is a set of probes for one Intent.
type ProbeRunRequest struct {
	Intent string
	Probes []lastv1alpha1.Probe
}

// ProbeOutcome is one probe's result. Passed is false whenever Error is set.
type ProbeOutcome struct {
	ID     string
	Passed bool
	Result runnerapi.Result
	Error  string
}

type probeRef struct {
	ID      string               `json:"id"`
	Command lastv1alpha1.Command `json:"command"`
	Assert  string               `json:"assert"`
}

type probeStart struct {
	RunID       string             `json:"runId"`
	Image       string             `json:"image"`
	Probes      []probeRef         `json:"probes"`
	Providers   []string           `json:"providers,omitempty"`
	Egress      []string           `json:"egress,omitempty"`
	Credentials []IssuedCredential `json:"credentials,omitempty"`
}

type probeResultEntry struct {
	ID              string `json:"id"`
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Passed          bool   `json:"passed"`
	Error           string `json:"error,omitempty"`
}

type probeResultPayload struct {
	RunID        string             `json:"runId"`
	StartSeq     uint64             `json:"startSeq"`
	Results      []probeResultEntry `json:"results"`
	Error        string             `json:"error,omitempty"`
	CleanupError string             `json:"cleanupError,omitempty"`
}

// RunProbes runs probes grouped by identical providers and egress, one pod per group,
// each recorded as a single probe.start/probe.result pair (spec §10.2).
func (e *Executor) RunProbes(ctx context.Context, req ProbeRunRequest) ([]ProbeOutcome, error) {
	seen := map[string]bool{}
	for _, p := range req.Probes {
		if seen[p.ID] {
			return nil, fmt.Errorf("sandbox: duplicate probe id %q", p.ID)
		}
		seen[p.ID] = true
	}
	byID := map[string]ProbeOutcome{}
	var errs []error
	for _, group := range groupProbes(req.Probes) {
		outs, err := e.runProbeGroup(ctx, req.Intent, group)
		for _, o := range outs {
			byID[o.ID] = o
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	outcomes := make([]ProbeOutcome, 0, len(req.Probes))
	for _, p := range req.Probes {
		outcomes = append(outcomes, byID[p.ID])
	}
	return outcomes, errors.Join(errs...)
}

// groupProbes groups probes by their sorted providers and egress, keeping first-seen order.
func groupProbes(probes []lastv1alpha1.Probe) [][]lastv1alpha1.Probe {
	var keys []string
	groups := map[string][]lastv1alpha1.Probe{}
	for _, p := range probes {
		providers := slices.Clone(p.Command.Providers)
		egress := slices.Clone(p.Command.Egress)
		slices.Sort(providers)
		slices.Sort(egress)
		key := strings.Join(providers, ",") + "|" + strings.Join(egress, ",")
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], p)
	}
	out := make([][]lastv1alpha1.Probe, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

func (e *Executor) runProbeGroup(ctx context.Context, intent string, probes []lastv1alpha1.Probe) ([]ProbeOutcome, error) {
	outcomes := make([]ProbeOutcome, len(probes))
	failAll := func(err error) ([]ProbeOutcome, error) {
		for i, p := range probes {
			if outcomes[i].ID == "" {
				outcomes[i] = ProbeOutcome{ID: p.ID, Result: runnerapi.Result{ID: p.ID, ExitCode: -1}, Error: err.Error()}
			}
		}
		return outcomes, err
	}
	cmds := make([]lastv1alpha1.Command, len(probes))
	refs := make([]probeRef, len(probes))
	for i, p := range probes {
		c := p.Command
		c.ID = p.ID
		cmds[i] = c
		refs[i] = probeRef{ID: p.ID, Command: p.Command, Assert: p.Assert}
	}
	prep, err := e.prepare(ctx, cmds, probes[0].Command.Providers, probes[0].Command.Egress)
	if err != nil {
		return failAll(err)
	}
	release, err := e.sched.Acquire(ctx, PriorityProbe)
	if err != nil {
		return failAll(err)
	}
	defer release()

	runID := e.cfg.NewRunID()
	issued := make([]IssuedCredential, 0, len(prep.plans))
	for _, cp := range prep.plans {
		issued = append(issued, cp.Issued)
	}
	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: "gateway", Intent: intent, Payload: payload}
	}
	var cleanupErr string
	err = e.auditor.Do(ctx,
		draft(audit.TypeProbeStart, probeStart{RunID: runID, Image: e.cfg.Pod.Image, Probes: refs,
			Providers: probes[0].Command.Providers, Egress: ruleStrings(prep.rules), Credentials: issued}),
		func(ctx context.Context) (any, error) {
			results, runErr := e.launch(ctx, runID, prep, launchHooks{})
			if cerr := e.cleanup(ctx, runID, nil); cerr != nil {
				cleanupErr = cerr.Error()
			}
			if runErr != nil {
				return nil, runErr
			}
			byID := map[string]runnerapi.Result{}
			for _, r := range results {
				byID[r.ID] = r
			}
			for i, p := range probes {
				r, ok := byID[p.ID]
				o := ProbeOutcome{ID: p.ID, Result: r}
				switch {
				case !ok:
					o.Result = runnerapi.Result{ID: p.ID, ExitCode: -1}
					o.Error = "sandbox: no result for probe"
				case r.Error != "":
					o.Error = r.Error
				default:
					passed, aerr := probe.Evaluate(p.Assert, probe.Result{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr})
					o.Passed = passed && aerr == nil
					if aerr != nil {
						o.Error = aerr.Error()
					}
				}
				outcomes[i] = o
			}
			return nil, nil
		},
		func(start audit.Entry, _ any, actionErr error) audit.Draft {
			p := probeResultPayload{RunID: runID, StartSeq: start.Seq, CleanupError: cleanupErr}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			for _, o := range outcomes {
				if o.ID == "" {
					continue
				}
				p.Results = append(p.Results, probeResultEntry{ID: o.ID, ExitCode: o.Result.ExitCode,
					Stdout: o.Result.Stdout, Stderr: o.Result.Stderr, StdoutTruncated: o.Result.StdoutTruncated,
					StderrTruncated: o.Result.StderrTruncated, Passed: o.Passed, Error: o.Error})
			}
			return draft(audit.TypeProbeResult, p)
		})
	if err != nil {
		return failAll(err)
	}
	return outcomes, nil
}
```

- [ ] **Step 4: Run the tests (with the race detector)**

Run: `go test -race ./internal/probe/ ./internal/sandbox/ -count=1 -v`
Expected: PASS. That covers the 11 `TestEvaluate` subtests and 5 new probe-run tests, and all earlier sandbox tests still pass.
If `cel.CrossTypeNumericComparisons` or `(*types.Type).IsExactType` doesn't exist in the cel-go version fetched, use that version's equivalent and record the substitution. The tests define the required behavior.

- [ ] **Step 5: Commit**

```bash
git add internal/probe internal/sandbox go.mod go.sum
git commit -m "feat(sandbox): run probes with CEL assertions and compact audit"
```

### Task 9: Orphan reaper

**Files:**
- Create: `internal/sandbox/reaper.go`, `internal/sandbox/reaper_test.go`

**Interfaces:**
- Consumes: `Executor.cleanup`, `LabelManagedBy`/`ManagedByValue`/`LabelRun` (Tasks 3 and 7), `audit.TypeEgressRevoked`.
- Produces: `func (e *Executor) ReapOrphans(ctx context.Context, olderThan time.Duration, now time.Time) (int, error)`. It reaps sandbox runs whose objects were created more than `olderThan` before `now`, and returns how many runs it reaped. Each reaped run that held an egress policy gets an `egress.revoked` entry `{runId, reason: "reaped"}`. M2b calls `ReapOrphans(ctx, 0, time.Now())` when the gateway starts leading. At that point every existing sandbox object is an orphan, because only the leader runs sandboxes.

- [ ] **Step 1: Write the failing test**

`internal/sandbox/reaper_test.go`:
```go
package sandbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func runMeta(name, runID string, created time.Time) metav1.ObjectMeta {
	labels := sandbox.RunLabels(runID)
	return metav1.ObjectMeta{Name: name, Namespace: "last-sandbox", Labels: labels, CreationTimestamp: metav1.NewTime(created)}
}

func TestReapOrphansDeletesOnlyOldRunObjects(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	objs := []runtime.Object{
		&corev1.Pod{ObjectMeta: runMeta("last-run-old", "old", old)},
		&corev1.Secret{ObjectMeta: runMeta("last-run-old-creds", "old", old)},
		&networkingv1.NetworkPolicy{ObjectMeta: runMeta("last-run-old", "old", old)},
		&corev1.Pod{ObjectMeta: runMeta("last-run-new", "new", recent)},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "last-sandbox", CreationTimestamp: metav1.NewTime(old)}},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, sink := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
	ctx := context.Background()
	if _, err := kube.CoreV1().Pods("last-sandbox").Get(ctx, "last-run-old", metav1.GetOptions{}); err == nil {
		t.Fatal("old pod survived")
	}
	if _, err := kube.CoreV1().Secrets("last-sandbox").Get(ctx, "last-run-old-creds", metav1.GetOptions{}); err == nil {
		t.Fatal("old credential secret survived")
	}
	if _, err := kube.CoreV1().Pods("last-sandbox").Get(ctx, "last-run-new", metav1.GetOptions{}); err != nil {
		t.Fatal("recent pod was reaped")
	}
	if _, err := kube.CoreV1().Secrets("last-sandbox").Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled secret was reaped")
	}
	var revoked []audit.Entry
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeEgressRevoked {
			revoked = append(revoked, e)
		}
	}
	if len(revoked) != 1 || !strings.Contains(string(revoked[0].Payload), `"runId":"old"`) || !strings.Contains(string(revoked[0].Payload), `"reason":"reaped"`) {
		t.Fatalf("revoked entries = %+v", revoked)
	}
}

func TestReapOrphansWithZeroAgeReapsEverything(t *testing.T) {
	now := time.Now()
	kube, _ := fakeCluster(corev1.PodSucceeded, &corev1.Pod{ObjectMeta: runMeta("last-run-x", "x", now)})
	aud, _ := newAudit(t)
	if n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), 0, now.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/sandbox/ -count=1 -run Reap`
Expected: compile error (`e.ReapOrphans undefined`).

- [ ] **Step 3: Implement**

`internal/sandbox/reaper.go`:
```go
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type reapedRef struct {
	RunID  string `json:"runId"`
	Reason string `json:"reason"`
}

// ReapOrphans deletes sandbox runs whose objects were created more than olderThan
// before now: leftovers of a gateway that stopped mid-run. Credentials and egress
// grants must not outlive their run (spec §11.1).
func (e *Executor) ReapOrphans(ctx context.Context, olderThan time.Duration, now time.Time) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}
	cutoff := now.Add(-olderThan)
	runs := map[string]bool{}
	note := func(meta metav1.ObjectMeta) {
		if id := meta.Labels[LabelRun]; id != "" && !meta.CreationTimestamp.Time.After(cutoff) {
			runs[id] = true
		}
	}
	pods, err := e.kube.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list pods: %w", err)
	}
	for _, p := range pods.Items {
		note(p.ObjectMeta)
	}
	secrets, err := e.kube.CoreV1().Secrets(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		note(s.ObjectMeta)
	}
	nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list network policies: %w", err)
	}
	for _, np := range nps.Items {
		note(np.ObjectMeta)
	}

	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var errs []error
	for _, id := range ids {
		onRevoked := func(ctx context.Context) error {
			_, err := e.auditor.Append(ctx, audit.Draft{Type: audit.TypeEgressRevoked, Actor: "gateway",
				Payload: reapedRef{RunID: id, Reason: "reaped"}})
			return err
		}
		errs = append(errs, e.cleanup(ctx, id, onRevoked))
	}
	return len(ids), errors.Join(errs...)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/sandbox/ -count=1 -v -run Reap`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox
git commit -m "feat(sandbox): reap orphaned sandbox runs and revoke their egress"
```

### Task 10: gRPC API with TokenReview caller authentication

**Files:**
- Create: `proto/last/gateway/v1/gateway.proto`, `buf.yaml`, `buf.gen.yaml`, `internal/gatewayapi/gatewayv1/` (generated), `internal/gateway/auth.go`, `internal/gateway/auth_test.go`, `internal/gateway/rpc.go`, `internal/gateway/rpc_test.go`
- Modify: `Makefile` (buf and plugin tools, `proto` target), `internal/gateway/config.go`, `internal/gateway/config_test.go`, `internal/gateway/server.go` (`Run` starts the gRPC server)

**Interfaces:**
- Consumes: `Gateway` (fields `cfg`, `kube`, `writer`), `Version`, and the test helper `testGateway(t, sink) *Gateway` from `internal/gateway/server_test.go`.
- Produces:
  - Proto `last.gateway.v1.GatewayService` with `Ping(PingRequest) returns (PingResponse)`. `PingResponse` has the fields `caller`, `version`, `chain_id`, `audit_ready`. The Go package is `github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1`.
  - Package `gateway`:
    - `const GRPCAudience = "last-gateway"`;
    - `func NewAuthenticator(kube kubernetes.Interface, audience, allowed string) *Authenticator`;
    - `(*Authenticator).Authenticate(ctx, token string) (string, error)`;
    - `(*Authenticator).UnaryInterceptor() grpc.UnaryServerInterceptor`;
    - `func CallerFrom(ctx context.Context) string`;
    - `(*Gateway).GRPCServer(auth *Authenticator) *grpc.Server`.
  - `Config` fields:
    - `GRPCAddr` (`LAST_GRPC_ADDR`, default `:9090`);
    - `OperatorIdentity` (`LAST_OPERATOR_IDENTITY`, **required**; the chart sets it to `system:serviceaccount:<release ns>:last-operator`).
- Error contract (gRPC status codes):
  - a missing or malformed bearer token → `Unauthenticated`;
  - a token that is not authenticated, or has the wrong audience → `Unauthenticated`;
  - an authenticated token for any other identity → `PermissionDenied`;
  - a failed TokenReview call → `Unavailable` (fail closed).
- Only successful reviews are cached, for 30s.

- [ ] **Step 1: Add the code generator tooling**

Append to `Makefile`, next to the other tool definitions (it uses kubebuilder's existing `go-install-tool` helper and `LOCALBIN`):
```make
BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC ?= $(LOCALBIN)/protoc-gen-go-grpc
BUF_VERSION ?= latest
PROTOC_GEN_GO_VERSION ?= latest
PROTOC_GEN_GO_GRPC_VERSION ?= latest

.PHONY: buf
buf: $(BUF) ## Download buf locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))

.PHONY: protoc-gen-go
protoc-gen-go: $(PROTOC_GEN_GO) ## Download protoc-gen-go locally if necessary.
$(PROTOC_GEN_GO): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(PROTOC_GEN_GO_GRPC) ## Download protoc-gen-go-grpc locally if necessary.
$(PROTOC_GEN_GO_GRPC): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))

.PHONY: proto
proto: buf protoc-gen-go protoc-gen-go-grpc ## Lint the protobuf API and regenerate Go code.
	$(BUF) lint
	PATH="$(LOCALBIN):$$PATH" $(BUF) generate
```
Run `make buf protoc-gen-go protoc-gen-go-grpc`. Then replace the three `latest` values with the versions that were installed, which `ls bin/` shows as `<tool>-<version>`. Pinning them keeps code generation reproducible. Record the versions in the report.

`buf.yaml`:
```yaml
version: v2
modules:
  - path: proto
lint:
  use:
    - STANDARD
breaking:
  use:
    - FILE
```

`buf.gen.yaml`:
```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: internal/gatewayapi
    opt: module=github.com/moogie-labs/last-operator/internal/gatewayapi
  - local: protoc-gen-go-grpc
    out: internal/gatewayapi
    opt: module=github.com/moogie-labs/last-operator/internal/gatewayapi
```

`proto/last/gateway/v1/gateway.proto`:
```proto
syntax = "proto3";

package last.gateway.v1;

option go_package = "github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1;gatewayv1";

// GatewayService is the trusted gateway's API. Only the operator's ServiceAccount
// may call it (spec §4.2). Command-running methods arrive with enforcement in M2b.
service GatewayService {
  // Ping reports the authenticated caller and whether the audit log is writable.
  rpc Ping(PingRequest) returns (PingResponse);
}

// PingRequest has no fields.
message PingRequest {}

// PingResponse describes the caller and the gateway.
message PingResponse {
  // The caller's authenticated Kubernetes username.
  string caller = 1;
  // The gateway build version.
  string version = 2;
  // The audit chain this gateway writes.
  string chain_id = 3;
  // Whether this gateway currently holds a healthy audit writer.
  bool audit_ready = 4;
}
```

Run: `go get google.golang.org/grpc@latest google.golang.org/protobuf@latest && make proto && ls internal/gatewayapi/gatewayv1`
Expected: `buf lint` passes, and `gateway.pb.go` and `gateway_grpc.pb.go` exist.

- [ ] **Step 2: Write the failing tests**

`internal/gateway/auth_test.go`:
```go
package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const operatorID = "system:serviceaccount:last-system:last-operator"

// reviewingClient answers TokenReviews by token value and counts the calls.
func reviewingClient(calls *int) *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*calls++
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		switch tr.Spec.Token {
		case "good":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{GRPCAudience}}
		case "other":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "system:serviceaccount:last-system:default"}, Audiences: []string{GRPCAudience}}
		case "wrong-audience":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{"https://kubernetes.default.svc"}}
		case "boom":
			return true, nil, errors.New("apiserver unavailable")
		default:
			tr.Status = authnv1.TokenReviewStatus{Authenticated: false, Error: "invalid token"}
		}
		return true, tr, nil
	})
	return kube
}

func codeOf(err error) codes.Code { return status.Code(err) }

func TestAuthenticateAcceptsTheOperatorAndCaches(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	for range 2 {
		user, err := a.Authenticate(context.Background(), "good")
		if err != nil || user != operatorID {
			t.Fatalf("user=%q err=%v", user, err)
		}
	}
	if calls != 1 {
		t.Fatalf("TokenReview calls = %d, want 1 (cached)", calls)
	}
	now = now.Add(31 * time.Second)
	if _, err := a.Authenticate(context.Background(), "good"); err != nil || calls != 2 {
		t.Fatalf("after expiry: err=%v calls=%d", err, calls)
	}
}

func TestAuthenticateRejectsAndNeverCachesFailures(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	cases := map[string]codes.Code{
		"":               codes.Unauthenticated,
		"bad":            codes.Unauthenticated,
		"wrong-audience": codes.Unauthenticated,
		"other":          codes.PermissionDenied,
		"boom":           codes.Unavailable,
	}
	for token, want := range cases {
		for range 2 {
			if _, err := a.Authenticate(context.Background(), token); codeOf(err) != want {
				t.Fatalf("token %q: code %v, want %v (err %v)", token, codeOf(err), want, err)
			}
		}
	}
	if calls != 8 { // 4 non-empty tokens × 2 attempts, nothing cached
		t.Fatalf("TokenReview calls = %d, want 8", calls)
	}
}
```

`internal/gateway/rpc_test.go`:
```go
package gateway

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func pingClient(t *testing.T) gatewayv1.GatewayServiceClient {
	t.Helper()
	calls := 0
	g := testGateway(t, audittest.NewMemSink())
	srv := g.GRPCServer(NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gatewayv1.NewGatewayServiceClient(conn)
}

func TestPingReturnsTheAuthenticatedCaller(t *testing.T) {
	c := pingClient(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer good")
	resp, err := c.Ping(ctx, &gatewayv1.PingRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCaller() != operatorID || resp.GetChainId() != "chain-1" || resp.GetVersion() != Version || resp.GetAuditReady() {
		t.Fatalf("resp = %+v (audit_ready must be false: no writer yet)", resp)
	}
}

func TestPingRequiresABearerToken(t *testing.T) {
	c := pingClient(t)
	for _, md := range [][]string{nil, {"authorization", "good"}, {"authorization", "Basic good"}} {
		ctx := context.Background()
		if md != nil {
			ctx = metadata.AppendToOutgoingContext(ctx, md...)
		}
		if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("metadata %v: code %v", md, codeOf(err))
		}
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer other")
	if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.PermissionDenied {
		t.Fatalf("other SA: code %v", codeOf(err))
	}
}
```

Add to `internal/gateway/config_test.go`. Put `"LAST_OPERATOR_IDENTITY": "system:serviceaccount:last-system:last-operator"` into `baseEnv()`, and add `"LAST_OPERATOR_IDENTITY"` to the list in `TestConfigFromEnvRequiresChainAndBucket`. Then add:
```go
func TestConfigFromEnvGRPCDefaults(t *testing.T) {
	env := baseEnv()
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":9090" || cfg.OperatorIdentity != "system:serviceaccount:last-system:last-operator" {
		t.Fatalf("cfg = %+v", cfg)
	}
}
```

Run: `go test ./internal/gateway/ -count=1`
Expected: compile errors (`undefined: NewAuthenticator`, `g.GRPCServer undefined`, `cfg.GRPCAddr undefined`).

- [ ] **Step 3: Implement**

`internal/gateway/auth.go`:
```go
package gateway

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GRPCAudience is the audience the operator's projected token must carry.
const GRPCAudience = "last-gateway"

// Authenticator admits only the operator's ServiceAccount (spec §4.2).
type Authenticator struct {
	kube     kubernetes.Interface
	audience string
	allowed  string
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]time.Time // hash of an accepted token → cache expiry
}

func NewAuthenticator(kube kubernetes.Interface, audience, allowed string) *Authenticator {
	return &Authenticator{kube: kube, audience: audience, allowed: allowed, ttl: 30 * time.Second,
		now: time.Now, cache: map[[32]byte]time.Time{}}
}

// Authenticate validates token with the TokenReview API and returns the caller.
// Failures are never cached; an unreachable API server fails closed.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if exp, ok := a.cache[key]; ok && now.Before(exp) {
		a.mu.Unlock()
		return a.allowed, nil
	}
	a.mu.Unlock()

	res, err := a.kube.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", status.Error(codes.Unavailable, "token review failed")
	}
	switch {
	case !res.Status.Authenticated:
		return "", status.Error(codes.Unauthenticated, "token not authenticated")
	case !slices.Contains(res.Status.Audiences, a.audience):
		return "", status.Error(codes.Unauthenticated, "token audience is not "+a.audience)
	case res.Status.User.Username != a.allowed:
		return "", status.Errorf(codes.PermissionDenied, "caller %s is not allowed", res.Status.User.Username)
	}
	a.mu.Lock()
	for k, exp := range a.cache {
		if !now.Before(exp) {
			delete(a.cache, k)
		}
	}
	a.cache[key] = now.Add(a.ttl)
	a.mu.Unlock()
	return a.allowed, nil
}

type callerKey struct{}

// CallerFrom returns the authenticated caller stored by the interceptor.
func CallerFrom(ctx context.Context) string {
	s, _ := ctx.Value(callerKey{}).(string)
	return s
}

// UnaryInterceptor authenticates every call from its "authorization: Bearer" metadata.
func (a *Authenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		user, err := a.Authenticate(ctx, bearerToken(ctx))
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, callerKey{}, user), req)
	}
}

func bearerToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
	}
	return ""
}
```

`internal/gateway/rpc.go`:
```go
package gateway

import (
	"context"

	"google.golang.org/grpc"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

type rpcServer struct {
	gatewayv1.UnimplementedGatewayServiceServer
	g *Gateway
}

func (s *rpcServer) Ping(ctx context.Context, _ *gatewayv1.PingRequest) (*gatewayv1.PingResponse, error) {
	w := s.g.writer.Load()
	return &gatewayv1.PingResponse{
		Caller: CallerFrom(ctx), Version: Version, ChainId: s.g.cfg.ChainID, AuditReady: w != nil && !w.Broken(),
	}, nil
}

// GRPCServer builds the gateway's gRPC server; every call is authenticated.
func (g *Gateway) GRPCServer(auth *Authenticator) *grpc.Server {
	srv := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryInterceptor()))
	gatewayv1.RegisterGatewayServiceServer(srv, &rpcServer{g: g})
	return srv
}
```

In `internal/gateway/config.go`:
- add fields `GRPCAddr string` and `OperatorIdentity string` to `Config`;
- in `ConfigFromEnv`, set `GRPCAddr: or("LAST_GRPC_ADDR", ":9090")` and `OperatorIdentity: getenv("LAST_OPERATOR_IDENTITY")`;
- add `"LAST_OPERATOR_IDENTITY": cfg.OperatorIdentity` to the required-variables map.

In `internal/gateway/server.go` `Run`, after the HTTP server goroutine and its deferred shutdown, add:
```go
	lis, err := net.Listen("tcp", g.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("gateway: grpc listen: %w", err)
	}
	grpcSrv := g.GRPCServer(NewAuthenticator(g.kube, GRPCAudience, g.cfg.OperatorIdentity))
	go func() {
		if err := grpcSrv.Serve(lis); err != nil {
			cancel(fmt.Errorf("gateway: grpc server: %w", err))
		}
	}()
	defer func() {
		done := make(chan struct{})
		go func() { grpcSrv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			grpcSrv.Stop()
		}
	}()
```
Add `"net"` to its imports. `testGateway` in `server_test.go` doesn't set `GRPCAddr`, so `Run` isn't exercised by unit tests; e2e covers it.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/gateway/ -count=1 -v && go build ./cmd/gateway && go vet ./...`
Expected: PASS for all gateway tests, including 2 auth, 2 RPC and 1 new config test. The build and vet are clean.

- [ ] **Step 5: Commit**

```bash
git add Makefile buf.yaml buf.gen.yaml proto internal/gatewayapi internal/gateway go.mod go.sum
git commit -m "feat(gateway): serve an authenticated gRPC API with a Ping RPC"
```

### Task 11: Chart — sandbox namespace, operator identity, gateway gRPC

**Files:**
- Modify: `charts/last-operator/values.yaml`, `charts/last-operator/templates/namespaces.yaml`, `charts/last-operator/templates/gateway.yaml`, `Makefile` (`helm-lint` render checks)
- Create: `charts/last-operator/templates/sandbox.yaml`, `charts/last-operator/templates/operator.yaml`

**Interfaces:**
- Consumes: gateway env `LAST_GRPC_ADDR` and `LAST_OPERATOR_IDENTITY` (Task 10).
- Produces (what Task 12 and M2b rely on):
  - Namespace `last-sandbox` with NetworkPolicy `last-sandbox-default-deny`, which denies all ingress and egress for every pod.
  - ServiceAccount `last-operator` in the release namespace.
  - Service `last-gateway` exposing port `grpc` 9090 alongside `http` 8080.
  - NetworkPolicy `last-gateway` admitting:
    - port 9090 only from the release namespace;
    - port 8080 from anywhere (health checks and metrics).
  - ClusterRole and ClusterRoleBinding `last-gateway-tokenreview`, granting `create` on `tokenreviews` to the gateway ServiceAccount.
- Sandbox *execution* RBAC (pods, secrets, networkpolicies in `last-sandbox`, provider secrets, token minting) is **not** added here. It arrives in M2b, when the gateway starts running sandboxes. The Task 12 executor test runs with the kind admin kubeconfig.

- [ ] **Step 1: Write the render checks (they fail first)**

Append to the `helm-lint` recipe in `Makefile`, after the existing `helm template` lines:
```make
	helm template last charts/last-operator -n last-system | grep -q 'name: last-sandbox-default-deny'
	helm template last charts/last-operator -n last-system | grep -q 'value: "system:serviceaccount:last-system:last-operator"'
	helm template last charts/last-operator -n last-system | grep -q 'containerPort: 9090'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \["tokenreviews"\]'
```
Run: `make helm-lint`
Expected: FAIL on the first new grep.

- [ ] **Step 2: Implement the templates**

In `charts/last-operator/values.yaml`, add `sandbox: last-sandbox` under `namespaces:` (after `audit:`).

In `charts/last-operator/templates/namespaces.yaml`, inside the existing `{{- if .Values.namespaces.create }}` block and after the `last-audit` Namespace, add:
```yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Values.namespaces.sandbox }}
  labels: {{- include "last.labels" . | nindent 4 }}
```

`charts/last-operator/templates/sandbox.yaml`:
```yaml
# Sandbox pods get no network access unless the gateway grants it per run (spec §11.1).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: last-sandbox-default-deny
  namespace: {{ .Values.namespaces.sandbox }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
```

`charts/last-operator/templates/operator.yaml`:
```yaml
# The operator's identity. The gateway accepts gRPC calls only from this
# ServiceAccount's projected token (audience last-gateway; spec §4.2).
apiVersion: v1
kind: ServiceAccount
metadata:
  name: last-operator
  namespace: {{ .Release.Namespace }}
  labels: {{- include "last.labels" . | nindent 4 }}
```

In `charts/last-operator/templates/gateway.yaml`:
1. In the gateway container `env:` list, add:
   ```yaml
            - {name: LAST_GRPC_ADDR, value: ":9090"}
            - {name: LAST_OPERATOR_IDENTITY, value: {{ printf "system:serviceaccount:%s:last-operator" .Release.Namespace | quote }}}
   ```
2. In the container `ports:` list, add `- {name: grpc, containerPort: 9090}`.
3. In the `last-gateway` Service `ports:` list, add `- {name: grpc, port: 9090, targetPort: grpc}`.
4. Append these documents to the end of the file:
```yaml
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: last-gateway
  namespace: {{ .Values.namespaces.gateway }}
  labels: {{- include "last.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: last-gateway
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: {{ .Release.Namespace }}
      ports:
        - {port: 9090, protocol: TCP}
    - ports:
        - {port: 8080, protocol: TCP}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: last-gateway-tokenreview
  labels: {{- include "last.labels" . | nindent 4 }}
rules:
  - apiGroups: ["authentication.k8s.io"]
    resources: ["tokenreviews"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: last-gateway-tokenreview
  labels: {{- include "last.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: last-gateway-tokenreview
subjects:
  - kind: ServiceAccount
    name: last-gateway
    namespace: {{ .Values.namespaces.gateway }}
```

- [ ] **Step 3: Run the render checks**

Run: `make helm-lint`
Expected: lint passes, all renders succeed, and every new grep matches.
Also check that `namespaces.create=false` still renders without any Namespace: `helm template last charts/last-operator -n last-system --set namespaces.create=false | grep -c 'kind: Namespace'` prints `0`.

- [ ] **Step 4: Commit**

```bash
git add charts Makefile
git commit -m "feat(chart): add the sandbox namespace, operator identity and gateway gRPC endpoint"
```

### Task 12: End-to-end — sandbox execution and gRPC auth on kind

**Files:**
- Create: `test/e2e/sandbox_e2e_test.go`, `test/e2e/grpc_e2e_test.go`
- Modify: `test/e2e/audit_e2e_test.go` (generalize `portForward`), `hack/e2e.sh` (load the sandbox image)

**Interfaces:**
- Consumes:
  - from Tasks 3–9: `sandbox.NewExecutor`, `ExecutorConfig`, `PodConfig`, `NewScheduler`, `KubeLogReader`, `RunRequest`, `ProbeRunRequest`, `PriorityExecution`, `LabelManagedBy`, `ManagedByValue`;
  - from the audit package: `audit.Open`, `audit.Verify`, `audittest.NewMemSink`;
  - from Task 10: `gatewayv1`;
  - from Task 11: chart objects;
  - the image `ghcr.io/moogie-labs/last-sandbox-base:e2e` (Task 2).
- Produces: `portForward(t, namespace, target, remotePort string) string`, which returns `127.0.0.1:<port>`. The existing audit test is updated to use it.

**Prerequisites:**
- Docker must be running.
- If the old kind cluster `last-e2e` still exists, its audit store holds a pre-v1-format chain and the gateway will correctly refuse it. Run this task with `KIND_CLUSTER=last-e2e-m2a`; `hack/e2e.sh` honours it.
- Don't delete clusters you didn't create. Leave that decision to the user.

- [ ] **Step 1: Generalize the port-forward helper and load the sandbox image**

In `test/e2e/audit_e2e_test.go`:
- change `func portForward(t *testing.T) string` to `func portForward(t *testing.T, namespace, target, remotePort string) string`;
- build the command as `exec.Command("kubectl", "port-forward", "-n", namespace, target, ":"+remotePort)`;
- return `"127.0.0.1:" + port` (host and port only, no scheme);
- update its two call sites to `"http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000")`.

In `hack/e2e.sh`:
- keep the existing `make images ... COMPONENTS="gateway audit-init"` line as it is. Since Task 2, `images` depends on `image-sandbox-base`, so the same command also builds `$REGISTRY/last-sandbox-base:$TAG`;
- extend the `kind load` loop to `for c in gateway audit-init sandbox-base; do`, so the sandbox image is available in the cluster.

- [ ] **Step 2: Write the sandbox e2e test**

`test/e2e/sandbox_e2e_test.go`:
```go
//go:build e2e

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	targetNS        = "last-e2e-target"
	e2eSecretValue  = "e2e-secret-value-9f3a"
	sandboxImageE2E = "ghcr.io/moogie-labs/last-sandbox-base:e2e"
)

type sandboxEnv struct {
	kube     kubernetes.Interface
	ctrl     client.Client
	exec     *sandbox.Executor
	sink     *audittest.MemSink
	signer   *audit.Signer
	targetIP string
}

func setupSandbox(t *testing.T) *sandboxEnv {
	t.Helper()
	ctx := context.Background()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = lastv1alpha1.AddToScheme(scheme)
	ctrl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	create := func(obj client.Object) {
		t.Helper()
		if err := ctrl.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
		t.Cleanup(func() { _ = ctrl.Delete(context.Background(), obj) })
	}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNS}})
	create(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: targetNS, Labels: map[string]string{"app": "target"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27-alpine",
			Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}},
	})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: "last-gateway"}})
	create(&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: targetNS},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: targetNS},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "e2e-reader"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "e2e-reader", Namespace: "last-gateway"}}})
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "e2e-demo", Namespace: "last-gateway"},
		Data: map[string][]byte{"token": []byte(e2eSecretValue)}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "e2e-cluster"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "e2e-reader"}}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "e2e-demo"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "e2e-demo"}, Env: map[string]string{"DEMO_TOKEN": "token"}}})

	var targetIP string
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := kube.CoreV1().Pods(targetNS).Get(ctx, "target", metav1.GetOptions{})
		if err != nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			return false, nil
		}
		targetIP = p.Status.PodIP
		return true, nil
	}); err != nil {
		t.Fatalf("target pod not running: %v", err)
	}

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.NewSigner(key)
	sink := audittest.NewMemSink()
	w, err := audit.Open(ctx, sink, signer, audit.WriterConfig{ChainID: "e2e-sandbox", Retention: time.Hour},
		func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	getProvider := func(ctx context.Context, name string) (*lastv1alpha1.Provider, error) {
		var p lastv1alpha1.Provider
		return &p, ctrl.Get(ctx, client.ObjectKey{Name: name}, &p)
	}
	exec := sandbox.NewExecutor(kube, getProvider, w, sandbox.NewScheduler(4), sandbox.KubeLogReader(kube), sandbox.ExecutorConfig{
		Pod: sandbox.PodConfig{Namespace: "last-sandbox", Image: sandboxImageE2E, ImagePullPolicy: corev1.PullIfNotPresent,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}},
		ProviderNamespace: "last-gateway",
	})
	return &sandboxEnv{kube: kube, ctrl: ctrl, exec: exec, sink: sink, signer: signer, targetIP: targetIP}
}

func (s *sandboxEnv) run(t *testing.T, c lastv1alpha1.Command) sandbox.RunResult {
	t.Helper()
	c.Timeout = &metav1.Duration{Duration: time.Minute}
	res, err := s.exec.Run(context.Background(), sandbox.RunRequest{Intent: "e2e/sandbox", Stage: "Create",
		Command: c, Priority: sandbox.PriorityExecution})
	if err != nil {
		t.Fatalf("run %v: %v", c.Run, err)
	}
	return res
}

func TestSandboxExecution(t *testing.T) {
	s := setupSandbox(t)
	curl := "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://" + s.targetIP + "/"

	t.Run("egress is denied by default", func(t *testing.T) {
		if r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: curl}}); r.Result.ExitCode == 0 {
			t.Fatalf("curl succeeded without an egress grant: %+v (does the CNI enforce NetworkPolicy?)", r.Result)
		}
	})
	t.Run("a grant opens exactly that destination", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: curl}, Egress: []string{s.targetIP + ":80"}})
		if r.Result.ExitCode != 0 || r.Result.Stdout != "200" {
			t.Fatalf("result = %+v", r.Result)
		}
	})
	t.Run("pod is non-root, read-only and has no service account token", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "id -u; touch /etc/x 2>/dev/null && echo writable || echo readonly; " +
			"test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo has-token || echo no-token"}})
		if r.Result.Stdout != "65532\nreadonly\nno-token\n" {
			t.Fatalf("stdout = %q", r.Result.Stdout)
		}
	})
	t.Run("kubernetes provider reaches the API server with its own RBAC", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "name"}},
			Providers: []string{"e2e-cluster"}})
		if r.Result.ExitCode != 0 || !strings.Contains(r.Result.Stdout, "pod/target") {
			t.Fatalf("result = %+v", r.Result)
		}
		denied := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "secrets", "-n", targetNS}},
			Providers: []string{"e2e-cluster"}})
		if denied.Result.ExitCode == 0 || !strings.Contains(denied.Result.Stderr, "forbidden") {
			t.Fatalf("provider exceeded its RBAC: %+v", denied.Result)
		}
	})
	t.Run("env-secret provider injects the value without auditing it", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: `test "$DEMO_TOKEN" = "` + e2eSecretValue + `" && echo ok`},
			Providers: []string{"e2e-demo"}})
		if r.Result.Stdout != "ok\n" {
			t.Fatalf("result = %+v", r.Result)
		}
	})
	t.Run("probes evaluate assertions in one audited run", func(t *testing.T) {
		probes := []lastv1alpha1.Probe{
			{ID: "listed", Assert: `result.exitCode == 0 && result.json.items.exists(p, p.metadata.name == "target")`,
				Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "json"}},
					Providers: []string{"e2e-cluster"}, Timeout: &metav1.Duration{Duration: time.Minute}}},
			{ID: "wrong", Assert: `size(result.json.items) == 5`,
				Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "json"}},
					Providers: []string{"e2e-cluster"}, Timeout: &metav1.Duration{Duration: time.Minute}}},
		}
		outs, err := s.exec.RunProbes(context.Background(), sandbox.ProbeRunRequest{Intent: "e2e/sandbox", Probes: probes})
		if err != nil || !outs[0].Passed || outs[1].Passed {
			t.Fatalf("outs=%+v err=%v", outs, err)
		}
	})
	t.Run("nothing is left behind and the audit chain verifies", func(t *testing.T) {
		ctx := context.Background()
		sel := metav1.ListOptions{LabelSelector: sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue}
		pods, _ := s.kube.CoreV1().Pods("last-sandbox").List(ctx, sel)
		secrets, _ := s.kube.CoreV1().Secrets("last-sandbox").List(ctx, sel)
		nps, _ := s.kube.NetworkingV1().NetworkPolicies("last-sandbox").List(ctx, sel)
		if len(pods.Items)+len(secrets.Items)+len(nps.Items) != 0 {
			t.Fatalf("leftovers: %d pods, %d secrets, %d policies", len(pods.Items), len(secrets.Items), len(nps.Items))
		}
		rep, err := audit.Verify(ctx, s.sink, s.signer.Public(), "e2e-sandbox", audit.VerifyOptions{})
		if err != nil || !rep.OK() {
			t.Fatalf("verify: %v %+v", err, rep.Problems)
		}
		objs, _ := s.sink.List(ctx, audit.EntryPrefix("e2e-sandbox"), "")
		for _, o := range objs {
			body, _ := s.sink.Get(ctx, o.Key, "")
			if strings.Contains(string(body), e2eSecretValue) {
				t.Fatalf("audit entry %s contains the provider secret value", o.Key)
			}
		}
	})
}
```

- [ ] **Step 3: Write the gRPC e2e test**

`test/e2e/grpc_e2e_test.go`:
```go
//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func TestGatewayGRPCAuth(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	addr := portForward(t, "last-gateway", "svc/last-gateway", "9090")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := gatewayv1.NewGatewayServiceClient(conn)
	ping := func(token string) (*gatewayv1.PingResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		return client.Ping(ctx, &gatewayv1.PingRequest{})
	}
	operatorToken := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--audience", "last-gateway", "--duration", "10m")

	t.Run("operator token is accepted", func(t *testing.T) {
		resp, err := ping(operatorToken)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCaller() != "system:serviceaccount:last-system:last-operator" || !resp.GetAuditReady() {
			t.Fatalf("resp = %+v", resp)
		}
	})
	t.Run("other callers are refused", func(t *testing.T) {
		otherSA := kubectl(t, "create", "token", "default", "-n", "last-system", "--audience", "last-gateway", "--duration", "10m")
		wrongAudience := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--duration", "10m")
		cases := map[string]codes.Code{
			"":                       codes.Unauthenticated,
			"not-a-token":            codes.Unauthenticated,
			wrongAudience:            codes.Unauthenticated,
			otherSA:                  codes.PermissionDenied,
		}
		for token, want := range cases {
			if _, err := ping(token); status.Code(err) != want {
				t.Errorf("token %.12q…: code %v, want %v (%v)", token, status.Code(err), want, err)
			}
		}
	})
}
```

- [ ] **Step 4: Run the e2e suite (Docker must be running)**

Run: `KIND_CLUSTER=last-e2e-m2a make test-e2e`
(Use the default cluster name instead if `kind get clusters` doesn't list a stale `last-e2e`.)
Expected:
- `TestAuditChain` passes as before.
- `TestSandboxExecution` passes all 7 subtests.
- `TestGatewayGRPCAuth` passes both subtests.

Stop-and-report conditions:
- **"egress is denied by default" fails.** The CNI isn't enforcing NetworkPolicy. Record `kubectl get pods -n kube-system` and the kind version, then report DONE_WITH_CONCERNS. Do not weaken the test.
- **The kubernetes-provider subtest can't reach the API server.** Inspect the `egress.granted` rules in the MemSink and the actual endpoint IPs (`kubectl get endpointslices -n default`) before changing any code.

- [ ] **Step 5: Run every suite once more and commit**

Run: `make lint test helm-lint test-integration`
Expected: all pass.

```bash
git add test/e2e hack/e2e.sh
git commit -m "test(e2e): verify sandbox isolation, credentials, probes and gRPC auth on kind"
```

---

## Spec coverage (M2a)

| Spec requirement | Task |
|---|---|
| §4.2 gRPC transport and TokenReview caller auth; NetworkPolicy to last-system | 10, 11, 12 |
| §8.1 command format (argv/script, files under /work, providers, egress, timeout) | 1, 5, 7 |
| §10 probes: command + CEL assertion, grouped by provider set | 8, 12 |
| §10.1 concurrency cap, priority queue (execution > discovery > builds > probes) | 6 |
| §10.2 compact probe audit (probe.start / probe.result) | 8 |
| §11.1 sandbox pods: base image, no SA token, non-root, read-only rootfs, /work, limits, activeDeadline | 2, 5, 12 |
| §11.1 credentials per run, deleted at end; credential.issued without values | 4, 7, 12 |
| §11.1 default-deny egress; per-run NetworkPolicy; egress.granted/revoked | 3, 7, 11, 12 |
| §11.1 output capture, 1 MiB truncation with flags | 1, 7 |
| §13 Provider credentials held only by the gateway | 4 (gateway-side RBAC arrives in M2b) |
| §14.1 write-before-act, fail-closed | 7, 8 |

**Deferred to M2b:**
- the `BeginPhase/RunCommand/RunProbes/EndPhase/WriteOutputSecret` RPCs, with approval, rule and plan enforcement;
- wiring the executor into the leading gateway, including `ReapOrphans` on lead;
- sandbox-execution RBAC for the gateway;
- `OperatorConfig`-driven sandbox settings, including the digest-pinned base image;
- `QueryAudit`;
- the operator itself.

package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/moogie-labs/last-operator/internal/sandbox/runner"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	shellBin = "/bin/sh"
	trueCmd  = "true"
	badID    = "bad"
)

func run(t *testing.T, maxOutput int, cmds ...runnerapi.Command) (runnerapi.Envelope, string) { //nolint:unparam
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
		if cmds[i].Script != "" {
			cmds[i].Script = strings.ReplaceAll(cmds[i].Script, "$WORK", work)
		}
		if cmds[i].TimeoutSeconds == 0 {
			cmds[i].TimeoutSeconds = 30
		}
	}
	env := runner.Run(context.Background(), runnerapi.Spec{Version: runnerapi.Version, Commands: cmds},
		runner.Options{MaxOutput: maxOutput, WorkDir: work, Shell: shellBin})
	return env, work
}

func TestRunArgvCapturesOutputAndExitCode(t *testing.T) {
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "a", Argv: []string{shellBin, "-c", "echo out; echo err >&2; exit 3"}})
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
		env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{ID: "x", Files: map[string]string{p: "x"}, Argv: []string{trueCmd}})
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
		runnerapi.Command{ID: badID, Argv: []string{"/nonexistent/binary"}},
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

func TestRunPrefixCollisionRejected(t *testing.T) {
	// A file path like /tmp/foo-evil/x should be rejected when workDir is /tmp/foo
	tempBase := t.TempDir()
	workPath := filepath.Join(tempBase, "work")
	if err := os.Mkdir(workPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	evilPath := filepath.Join(tempBase, "work-evil", "x")

	env := runner.Run(context.Background(),
		runnerapi.Spec{
			Version: runnerapi.Version,
			Commands: []runnerapi.Command{
				{
					ID:    "p",
					Files: map[string]string{evilPath: badID},
					Argv:  []string{trueCmd},
				},
			},
		},
		runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: workPath, Shell: shellBin},
	)
	r := env.Results[0]
	if r.ExitCode != -1 || !strings.Contains(r.Error, "outside") {
		t.Fatalf("prefix collision not rejected: result = %+v", r)
	}
}

func TestRunSymlinkEscapeRejected(t *testing.T) {
	work := t.TempDir()
	evilDir := t.TempDir()

	// Create symlink from work/lnk to evilDir
	lnkPath := filepath.Join(work, "lnk")
	if err := os.Symlink(evilDir, lnkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Try to write through symlink
	env := runner.Run(context.Background(),
		runnerapi.Spec{
			Version: runnerapi.Version,
			Commands: []runnerapi.Command{
				{
					ID: "e",
					Files: map[string]string{
						filepath.Join(work, "lnk", "escape.txt"): badID,
					},
					Argv: []string{trueCmd},
				},
			},
		},
		runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: work, Shell: shellBin},
	)

	r := env.Results[0]
	if r.ExitCode != -1 || r.Error == "" {
		t.Fatalf("symlink escape not rejected: result = %+v", r)
	}

	// Verify evilDir stayed empty (write was truly blocked)
	entries, err := os.ReadDir(evilDir)
	if err != nil {
		t.Fatalf("readdir evilDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("evilDir not empty, write escaped: %v", entries)
	}
}

func TestRunTimeoutWithGrandchildPipe(t *testing.T) {
	start := time.Now()
	work := t.TempDir()

	env := runner.Run(context.Background(),
		runnerapi.Spec{
			Version: runnerapi.Version,
			Commands: []runnerapi.Command{
				{
					ID:             "g",
					Script:         "sleep 30 & echo $! > " + filepath.Join(work, "bg.pid") + "; wait",
					TimeoutSeconds: 1,
				},
			},
		},
		runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: work, Shell: shellBin},
	)
	r := env.Results[0]
	if r.ExitCode != -1 || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("result = %+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}

	// Check that the background process was killed
	pidFile := filepath.Join(work, "bg.pid")
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}

	// Poll for up to 2 seconds to ensure process is gone
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break // Process is dead
		}
		if err != nil {
			t.Fatalf("syscall.Kill check: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("background process still alive after 2s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunSuccessWithBackgroundChild(t *testing.T) {
	start := time.Now()
	env, _ := run(t, runnerapi.MaxOutput, runnerapi.Command{
		ID:             "b",
		Script:         "(sleep 20 &); echo done",
		TimeoutSeconds: 30,
	})
	r := env.Results[0]
	if r.ExitCode != 0 || r.Stdout != "done\n" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Error, "background processes") {
		t.Fatalf("error does not mention background processes: %q", r.Error)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("command took %s", time.Since(start))
	}
}

var errDeny = errors.New("dropped") // scripted outcome: the dial blocks until its deadline

// scriptedDial plays outcomes per address in order, repeating the last one. A nil
// outcome connects, errDeny drops (blocks until the dial deadline), anything else
// fails immediately.
type scriptedDial struct {
	mu       sync.Mutex
	outcomes map[string][]error
	calls    map[string]int
	conns    []*fakeConn
}

type fakeConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *fakeConn) Close() error { c.closed.Store(true); return nil }

func newDial(outcomes map[string][]error) *scriptedDial {
	return &scriptedDial{outcomes: outcomes, calls: map[string]int{}}
}

func (d *scriptedDial) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	seq := d.outcomes[addr]
	i := min(d.calls[addr], len(seq)-1)
	d.calls[addr]++
	out := seq[i]
	var c *fakeConn
	if out == nil {
		c = &fakeConn{}
		d.conns = append(d.conns, c)
	}
	d.mu.Unlock()
	switch {
	case out == nil:
		return c, nil
	case errors.Is(out, errDeny):
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, out
}

func (d *scriptedDial) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		n += c
	}
	return n
}

const canaryA, canaryB = "10.0.0.1:8080", "10.0.0.2:10250"

func repeat(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}

func gateRun(t *testing.T, canaries []string, d *scriptedDial, mod ...func(*runner.Options)) (runnerapi.Envelope, string) {
	t.Helper()
	work := t.TempDir()
	marker := filepath.Join(work, "ran")
	opts := runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: work, Shell: shellBin, Dial: d.dial,
		EgressGateTimeout: 500 * time.Millisecond, EgressRetryInterval: time.Millisecond,
		EgressDialTimeout: 5 * time.Millisecond, EgressStableFor: 20 * time.Millisecond}
	for _, m := range mod {
		m(&opts)
	}
	env := runner.Run(context.Background(), runnerapi.Spec{Version: runnerapi.Version, EgressCanaries: canaries,
		Commands: []runnerapi.Command{{ID: "a", Script: "touch " + marker, TimeoutSeconds: 10}, {ID: "b", Argv: []string{trueCmd}, TimeoutSeconds: 10}}}, opts)
	return env, marker
}

func ran(marker string) bool { _, err := os.Stat(marker); return err == nil }

func TestEgressGateFailsWhenCanaryStaysReachable(t *testing.T) {
	d := newDial(map[string][]error{canaryA: {nil}})
	env, marker := gateRun(t, []string{canaryA}, d)
	if ran(marker) {
		t.Fatal("a command ran although egress was not confirmed denied")
	}
	if len(env.Results) != 2 {
		t.Fatalf("results = %+v", env.Results)
	}
	for i, r := range env.Results {
		if r.ID != []string{"a", "b"}[i] || r.ExitCode != -1 ||
			!strings.Contains(r.Error, "egress policy not enforced: canary 10.0.0.1:8080 still reachable after 500ms") {
			t.Fatalf("result %d = %+v", i, r)
		}
	}
	for _, c := range d.conns {
		if !c.closed.Load() {
			t.Fatal("a canary connection was left open")
		}
	}
}

func TestEgressGatePassesAfterAStableDeniedWindow(t *testing.T) {
	d := newDial(map[string][]error{canaryA: append(repeat(nil, 3), errDeny)})
	env, marker := gateRun(t, []string{canaryA}, d)
	if !ran(marker) || env.Results[0].ExitCode != 0 || env.Results[1].ExitCode != 0 {
		t.Fatalf("results = %+v", env.Results)
	}
	if d.total() < 6 { // 3 reachable rounds, then at least 3 denied rounds
		t.Fatalf("gate passed after only %d dials", d.total())
	}
}

func TestEgressGateFailsWhenOneOfTwoCanariesIsReachable(t *testing.T) {
	d := newDial(map[string][]error{canaryA: {errDeny}, canaryB: {nil}})
	env, marker := gateRun(t, []string{canaryA, canaryB}, d)
	if ran(marker) || env.Results[0].ExitCode != -1 || !strings.Contains(env.Results[0].Error, "canary "+canaryB+" still reachable") {
		t.Fatalf("results = %+v", env.Results)
	}
}

func TestEgressGatePassesWhenAllCanariesAreDenied(t *testing.T) {
	d := newDial(map[string][]error{canaryA: {errDeny}, canaryB: {errDeny}})
	env, marker := gateRun(t, []string{canaryA, canaryB}, d)
	if !ran(marker) || env.Results[0].ExitCode != 0 {
		t.Fatalf("results = %+v", env.Results)
	}
}

func TestEgressGateRestartsTheWindowAfterAReachableRound(t *testing.T) {
	// denied x2, reachable, then denied: the two early denials must not count.
	d := newDial(map[string][]error{canaryA: append([]error{errDeny, errDeny, nil}, errDeny)})
	env, marker := gateRun(t, []string{canaryA}, d)
	if !ran(marker) || env.Results[0].ExitCode != 0 {
		t.Fatalf("results = %+v", env.Results)
	}
	if d.total() < 6 { // 2 + 1 + a full new streak of 3
		t.Fatalf("streak was not reset: %d dials", d.total())
	}
}

func TestEgressGateEnforcesTheStabilityWindow(t *testing.T) {
	// Everything is denied, but the window is longer than the gate allows.
	d := newDial(map[string][]error{canaryA: {errDeny}})
	env, marker := gateRun(t, []string{canaryA}, d, func(o *runner.Options) {
		o.EgressStableFor, o.EgressGateTimeout = time.Hour, 100*time.Millisecond
	})
	if ran(marker) || !strings.Contains(env.Results[0].Error, "not confirmed denied") {
		t.Fatalf("results = %+v", env.Results)
	}
	start := time.Now()
	d = newDial(map[string][]error{canaryA: {errDeny}})
	env, marker = gateRun(t, []string{canaryA}, d, func(o *runner.Options) { o.EgressStableFor = 80 * time.Millisecond })
	if !ran(marker) || time.Since(start) < 80*time.Millisecond || env.Results[0].ExitCode != 0 {
		t.Fatalf("passed after %s: %+v", time.Since(start), env.Results)
	}
}

func TestEgressGateFailsWhenDeniedIsNeverObserved(t *testing.T) {
	d := newDial(map[string][]error{canaryA: {syscall.ECONNREFUSED}})
	env, marker := gateRun(t, []string{canaryA}, d)
	if r := env.Results[0]; ran(marker) || r.ExitCode != -1 || !strings.Contains(r.Error, "not confirmed denied") || strings.Contains(r.Error, "still reachable") {
		t.Fatalf("result = %+v", r)
	}
}

func TestEgressGateIgnoresTimeoutErrorsThatAreNotTheDialDeadline(t *testing.T) {
	// A net.Error that says Timeout() (EAGAIN, DNS) is not a dropped packet.
	d := newDial(map[string][]error{canaryA: {&net.DNSError{IsTimeout: true}}})
	env, marker := gateRun(t, []string{canaryA}, d)
	if ran(marker) || env.Results[0].ExitCode != -1 {
		t.Fatalf("results = %+v", env.Results)
	}
}

func TestEgressGateFailsClosedOnNonLiteralCanaries(t *testing.T) {
	for _, bad := range []string{"gateway.last-gateway.svc:8080", "10.0.0.1", "10.0.0.1:0", ""} {
		d := newDial(map[string][]error{bad: {errDeny}, canaryA: {errDeny}})
		env, marker := gateRun(t, []string{canaryA, bad}, d)
		if ran(marker) || !strings.Contains(env.Results[0].Error, "invalid egress canary") {
			t.Fatalf("canary %q: results = %+v", bad, env.Results)
		}
		if d.total() != 0 {
			t.Fatalf("canary %q: dialed %d times", bad, d.total())
		}
	}
}

func TestEgressGateIsSkippedWithoutCanaries(t *testing.T) {
	d := newDial(map[string][]error{canaryA: {nil}})
	env, marker := gateRun(t, nil, d)
	if !ran(marker) || d.total() != 0 || env.Results[0].ExitCode != 0 {
		t.Fatalf("results = %+v", env.Results)
	}
}

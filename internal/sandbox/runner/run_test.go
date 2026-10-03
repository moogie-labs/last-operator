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

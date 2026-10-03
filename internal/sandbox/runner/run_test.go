package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: workPath, Shell: "/bin/sh"},
	)
	r := env.Results[0]
	if r.ExitCode != -1 || !strings.Contains(r.Error, "outside") {
		t.Fatalf("prefix collision not rejected: result = %+v", r)
	}
}

func TestRunSymlinkEscapeRejected(t *testing.T) {
	// Create a symlink and then try to write through it in the same run
	evilDir := t.TempDir()
	env, _ := run(t, runnerapi.MaxOutput,
		runnerapi.Command{
			ID:     "s",
			Script: "ln -s " + evilDir + " $WORK/lnk",
		},
		runnerapi.Command{
			ID: "e",
			Files: map[string]string{
				"$WORK/lnk/escape.txt": badID,
			},
			Argv: []string{trueCmd},
		},
	)

	// First command should succeed (creating symlink)
	if env.Results[0].ExitCode != 0 {
		t.Fatalf("symlink creation failed: %+v", env.Results[0])
	}

	// Second command should fail (trying to write through symlink)
	r := env.Results[1]
	if r.ExitCode != -1 || r.Error == "" {
		t.Fatalf("symlink escape not rejected: result = %+v", r)
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
		runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: work, Shell: "/bin/sh"},
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

// Package runner executes a sandbox spec inside the sandbox pod.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Options configures a run.
type Options struct {
	MaxOutput int    // cap per stream, bytes
	WorkDir   string // the only directory files may be written under
	Shell     string // used for Script commands

	// Egress gate (see Spec.EgressCanary). Zero values select the defaults.
	EgressGateTimeout   time.Duration // bounds the whole gate; default 30s
	EgressDialTimeout   time.Duration // per canary dial; default 500ms
	EgressRetryInterval time.Duration // between rounds; default 200ms
	EgressStableFor     time.Duration // denied rounds must span this long; default 2s
	// Dial opens the canary connection; default net.Dialer.DialContext.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Run executes every command in order. A failing command never stops later ones.
func Run(ctx context.Context, spec runnerapi.Spec, opts Options) runnerapi.Envelope {
	env := runnerapi.Envelope{Version: runnerapi.Version, Results: make([]runnerapi.Result, 0, len(spec.Commands))}
	if len(spec.EgressCanaries) > 0 {
		if msg := egressGate(ctx, spec.EgressCanaries, opts); msg != "" {
			for _, c := range spec.Commands {
				env.Results = append(env.Results, runnerapi.Result{ID: c.ID, ExitCode: -1, Error: msg})
			}
			return env
		}
	}
	for _, c := range spec.Commands {
		env.Results = append(env.Results, runOne(ctx, c, opts))
	}
	return env
}

// egressGate blocks until every canary dial times out in each of a run of
// consecutive rounds that spans at least EgressStableFor and has at least
// minDeniedRounds rounds. NetworkPolicy denies by dropping packets, so only a dial
// whose own deadline expired counts as denied. Anything else (connect, refused,
// unreachable, DNS) means "not yet enforced" and resets the streak. CNIs apply a
// new pod's policy late and may police some destinations before others, so several
// canaries and a stability window guard against a single early verdict.
// It returns "" when egress is confirmed denied, else the failure message. Nothing is
// read from a canary connection.
func egressGate(ctx context.Context, canaries []string, opts Options) string {
	gateTimeout, dialTimeout, retry, stable := opts.EgressGateTimeout, opts.EgressDialTimeout, opts.EgressRetryInterval, opts.EgressStableFor
	if gateTimeout <= 0 {
		gateTimeout = 30 * time.Second
	}
	if dialTimeout <= 0 {
		dialTimeout = 500 * time.Millisecond
	}
	if retry <= 0 {
		retry = 200 * time.Millisecond
	}
	if stable <= 0 {
		stable = 2 * time.Second
	}
	for _, c := range canaries {
		if ap, err := netip.ParseAddrPort(c); err != nil || ap.Port() == 0 {
			return fmt.Sprintf("egress policy not enforced: invalid egress canary %q", c)
		}
	}
	dial := opts.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	gctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	var streak int
	var streakStart time.Time
	bad, badReachable := canaries[0], false
	for {
		denied := make([]bool, len(canaries))
		reachable := make([]bool, len(canaries))
		var wg sync.WaitGroup
		for i, c := range canaries {
			wg.Go(func() {
				dctx, dcancel := context.WithTimeout(gctx, dialTimeout)
				conn, err := dial(dctx, "tcp", c)
				// Checked before cancel: only the dial's own deadline is a denial.
				denied[i] = err != nil && gctx.Err() == nil && errors.Is(dctx.Err(), context.DeadlineExceeded)
				dcancel()
				if err == nil {
					_ = conn.Close()
					reachable[i] = true
				}
			})
		}
		wg.Wait()
		all := true
		for i := range canaries {
			if !denied[i] {
				all = false
				if gctx.Err() == nil || reachable[i] {
					bad, badReachable = canaries[i], reachable[i]
				}
			}
		}
		if all {
			if streak == 0 {
				streakStart = time.Now()
			}
			streak++
			if streak >= minDeniedRounds && time.Since(streakStart) >= stable {
				return ""
			}
		} else {
			streak = 0
		}
		select {
		case <-gctx.Done():
			if badReachable {
				return fmt.Sprintf("egress policy not enforced: canary %s still reachable after %s", bad, gateTimeout)
			}
			return fmt.Sprintf("egress policy not enforced: canary %s not confirmed denied after %s", bad, gateTimeout)
		case <-time.After(retry):
		}
	}
}

// minDeniedRounds is the fewest consecutive all-denied rounds the gate accepts.
const minDeniedRounds = 3

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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	stdout := &cappedBuffer{limit: opts.MaxOutput}
	stderr := &cappedBuffer{limit: opts.MaxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	res.Stdout, res.StdoutTruncated = stdout.text(), stdout.truncated
	res.Stderr, res.StderrTruncated = stderr.text(), stderr.truncated

	// Clean up process group in all cases (ignore ESRCH if already gone)
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var exitErr *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return fail("timed out after %s", timeout)
	case errors.Is(err, exec.ErrWaitDelay):
		// Command exited but background processes held output open
		if cmd.ProcessState != nil {
			res.ExitCode = cmd.ProcessState.ExitCode()
		} else {
			res.ExitCode = -1
		}
		res.Error = "background processes held output open and were killed"
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
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return fmt.Errorf("open root %s: %w", workDir, err)
	}
	defer func() {
		_ = root.Close()
	}()

	cleanRoot := filepath.Clean(workDir) + string(filepath.Separator)
	for p, content := range files {
		clean := filepath.Clean(p)
		if !filepath.IsAbs(p) || !strings.HasPrefix(clean, cleanRoot) {
			return fmt.Errorf("file path %q is outside %s", p, workDir)
		}
		rel, err := filepath.Rel(workDir, clean)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", p, err)
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
			return fmt.Errorf("create directory for %q: %w", p, err)
		}
		if err := root.WriteFile(rel, []byte(content), 0o600); err != nil {
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

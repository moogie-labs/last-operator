package registrygc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminPassword = "s3cret-admin-password"
	gcPath        = "/gc"
)

type fixture struct {
	cfg   Config
	repos string
	mu    sync.Mutex
	calls [][]string
}

func newFixture(t *testing.T, withRepos bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := bcrypt.GenerateFromPassword([]byte("pusher-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(dir, "htpasswd")
	content := "pusher:" + string(pusher) + "\nadmin:" + string(hash) + "\n"
	if err := os.WriteFile(htpasswd, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fixture{repos: filepath.Join(dir, "data", "docker", "registry", "v2", "repositories")}
	if withRepos {
		if err := os.MkdirAll(filepath.Join(f.repos, "last", "images"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg = Config{HtpasswdFile: htpasswd, ConfigFile: "/etc/distribution/config.yml", DataDir: filepath.Join(dir, "data")}
	return f
}

func (f *fixture) run(fn func(ctx context.Context, name string, args ...string) ([]byte, error)) {
	f.cfg.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		f.mu.Lock()
		f.calls = append(f.calls, append([]string{name}, args...))
		f.mu.Unlock()
		return fn(ctx, name, args...)
	}
}

func postAdmin(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, gcPath, nil)
	req.SetBasicAuth(adminUser, adminPassword)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGCRunsGarbageCollectAndReportsBytesFreed(t *testing.T) {
	f := newFixture(t, true)
	victim := filepath.Join(f.repos, "last", "images", "blob")
	if err := os.WriteFile(victim, make([]byte, 10000), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("deleted blob\n"), os.Remove(victim)
	})
	rec := postAdmin(Handler(f.cfg))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		BytesFreed int64  `json:"bytesFreed"`
		Log        string `json:"log"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.BytesFreed < 10000 || !strings.Contains(out.Log, "deleted blob") {
		t.Fatalf("unexpected response %+v", out)
	}
	want := []string{"registry", "garbage-collect", "/etc/distribution/config.yml"}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestGCOnAnEmptyRegistryFreesNothing(t *testing.T) {
	f := newFixture(t, false)
	f.run(func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
	rec := postAdmin(Handler(f.cfg))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["bytesFreed"] != float64(0) {
		t.Fatalf("response %v", out)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Run must not be called, got %v", f.calls)
	}
}

func TestGCRejects(t *testing.T) {
	f := newFixture(t, true)
	f.run(func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
	h := Handler(f.cfg)
	tests := []struct {
		name   string
		method string
		path   string
		user   string
		pass   string
		want   int
	}{
		{"no auth", http.MethodPost, gcPath, "", "", http.StatusUnauthorized},
		{"wrong password", http.MethodPost, gcPath, adminUser, "nope", http.StatusUnauthorized},
		{"pusher", http.MethodPost, gcPath, "pusher", "pusher-pass", http.StatusUnauthorized},
		{"pusher password as admin", http.MethodPost, gcPath, adminUser, "pusher-pass", http.StatusUnauthorized},
		{"GET", http.MethodGet, gcPath, adminUser, adminPassword, http.StatusMethodNotAllowed},
		{"other path", http.MethodPost, "/other", adminUser, adminPassword, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.user != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if len(f.calls) != 0 {
		t.Fatalf("Run must not be called, got %v", f.calls)
	}
}

func TestGCRefusesAConcurrentRun(t *testing.T) {
	f := newFixture(t, true)
	started := make(chan struct{})
	release := make(chan struct{})
	f.run(func(context.Context, string, ...string) ([]byte, error) {
		close(started)
		<-release
		return nil, nil
	})
	h := Handler(f.cfg)
	done := make(chan int)
	go func() { done <- postAdmin(h).Code }()
	<-started
	if code := postAdmin(h).Code; code != http.StatusConflict {
		t.Fatalf("second run status %d, want 409", code)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first run status %d", code)
	}
}

func TestGCReportsAFailedRun(t *testing.T) {
	f := newFixture(t, true)
	f.run(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("boom: something broke"), errors.New("exit status 1")
	})
	rec := postAdmin(Handler(f.cfg))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Log   string `json:"log"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Log, "boom") {
		t.Fatalf("log = %q", out.Log)
	}
	if strings.Contains(rec.Body.String(), adminPassword) {
		t.Fatal("response leaks the password")
	}
}

func TestNeverLogsCredentials(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFixture(t, true)
	f.run(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exit status 1") })
	h := Handler(f.cfg)
	postAdmin(h)
	bad := httptest.NewRequest(http.MethodPost, gcPath, nil)
	bad.SetBasicAuth(adminUser, "wrong-password-value")
	h.ServeHTTP(httptest.NewRecorder(), bad)
	basic := base64.StdEncoding.EncodeToString([]byte(adminUser + ":" + adminPassword))
	for _, secret := range []string{adminPassword, "wrong-password-value", basic, "Authorization"} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, buf.String())
		}
	}
	if buf.Len() == 0 {
		t.Fatal("expected some log output")
	}
}

func TestAuthFailsClosedOnBadHtpasswd(t *testing.T) {
	good, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"garbled":            "not a valid line\n",
		"empty":              "",
		"non-bcrypt hash":    "admin:{SHA}5en6G6MezRroT3XKqkdPOmY/BfQ=\n",
		"only another user":  "pusher:" + string(good) + "\n",
		"admin without hash": "admin\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			f.run(func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
			if err := os.WriteFile(f.cfg.HtpasswdFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if code := postAdmin(Handler(f.cfg)).Code; code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", code)
			}
			if len(f.calls) != 0 {
				t.Fatalf("Run must not be called, got %v", f.calls)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		f := newFixture(t, true)
		f.cfg.HtpasswdFile = filepath.Join(t.TempDir(), "absent")
		if code := postAdmin(Handler(f.cfg)).Code; code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", code)
		}
	})
}

func TestLockIsReleasedAfterFailureAndAfterConflict(t *testing.T) {
	f := newFixture(t, true)
	var n atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	f.run(func(context.Context, string, ...string) ([]byte, error) {
		switch n.Add(1) {
		case 1:
			return nil, errors.New("exit status 1")
		case 2:
			close(started)
			<-release
		}
		return nil, nil
	})
	h := Handler(f.cfg)
	if code := postAdmin(h).Code; code != http.StatusInternalServerError {
		t.Fatalf("first run status %d, want 500", code)
	}
	done := make(chan int)
	go func() { done <- postAdmin(h).Code }()
	<-started
	if code := postAdmin(h).Code; code != http.StatusConflict {
		t.Fatalf("status %d, want 409", code)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("second run status %d", code)
	}
	if code := postAdmin(h).Code; code != http.StatusOK {
		t.Fatalf("run after a conflict status %d, want 200", code)
	}
}

// A client that disconnects (the gateway stepping down, or giving up after its
// timeout) cancels the run, so it never sweeps while builds resume. The lock is
// held until the process has ended, however long it takes to exit.
func TestADisconnectCancelsTheRun(t *testing.T) {
	f := newFixture(t, true)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	exited := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	f.run(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		if first.Swap(false) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-exited // the killed process takes a while to exit
			return nil, ctx.Err()
		}
		return nil, nil
	})
	h := Handler(f.cfg)
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, gcPath, nil)
	req.SetBasicAuth(adminUser, adminPassword)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("a disconnect did not cancel the run")
	}
	if code := postAdmin(h).Code; code != http.StatusConflict {
		t.Fatalf("status %d, want 409 until the cancelled process has exited", code)
	}
	close(exited)
	<-done
	if code := postAdmin(h).Code; code != http.StatusOK {
		t.Fatalf("status %d after the cancelled run ended", code)
	}
}

func TestTailKeepsTheLast64KiB(t *testing.T) {
	in := []byte(strings.Repeat("a", 100000) + "END")
	got := tail(in)
	if len(got) != maxLog || !strings.HasSuffix(got, "END") {
		t.Fatalf("len %d", len(got))
	}
}

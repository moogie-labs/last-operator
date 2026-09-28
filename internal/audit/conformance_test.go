package audit_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// fakeS3 is a minimal in-memory S3 server that implements just enough of the REST
// API surface RunConformance exercises (PutObject, GetObject, DeleteObject,
// PutObjectRetention, DeleteBucket) to prove the discrimination rules in R1/R2: a
// store that doesn't actually enforce WORM must never make a check report Passed:
// true, even though it answers every request with a plausible-looking 2xx/4xx/5xx.
type fakeS3 struct {
	mu sync.Mutex

	// broken, when true, makes every request after the first succeed with a 500
	// InternalError -- simulating a store that fails for the wrong reason instead
	// of genuinely refusing the WORM-violating operation.
	broken    bool
	firstDone bool

	versions map[string][]byte // "key#versionID" -> body
	latest   map[string]string // key -> latest versionID
	seq      int
}

func newFakeS3(broken bool) *fakeS3 {
	return &fakeS3{broken: broken, versions: map[string][]byte{}, latest: map[string]string{}}
}

func (f *fakeS3) nextVersion() string {
	f.seq++
	return fmt.Sprintf("v%d", f.seq)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>test</RequestId></Error>`, code, code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// The very first request is the conformance probe's own setup PutObject; it
	// must succeed even in the "broken" scenario, or RunConformance never gets
	// past setup.
	isSetup := !f.firstDone
	f.firstDone = true

	// Path is /<bucket>/<key...> (path-style); DeleteBucket has no key remainder.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	var key string
	if len(parts) == 2 {
		key = parts[1]
	}

	if f.broken && !isSetup {
		writeS3Error(w, http.StatusInternalServerError, "InternalError")
		return
	}

	switch {
	case r.Method == http.MethodPut && r.URL.Query().Has("retention"):
		// PutObjectRetention: the leaky store just accepts it.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPut && key != "":
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.latest[key]; exists {
				writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
		}
		vid := f.nextVersion()
		f.versions[key+"#"+vid] = body
		f.latest[key] = vid
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("x-amz-version-id", vid)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodGet && key != "":
		vid := r.URL.Query().Get("versionId")
		if vid == "" {
			vid = f.latest[key]
		}
		body, ok := f.versions[key+"#"+vid]
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case r.Method == http.MethodDelete && key != "":
		vid := r.URL.Query().Get("versionId")
		if vid != "" {
			// Leaky: actually deletes the "locked" version instead of refusing.
			delete(f.versions, key+"#"+vid)
		}
		// An unversioned delete would add a delete marker on a real store; the
		// fake leaves existing versions untouched either way.
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodDelete && key == "":
		// DeleteBucket.
		w.WriteHeader(http.StatusNoContent)

	default:
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest")
	}
}

// noRetryClient builds an S3 client against srv the same way production code does
// (audit.NewS3Client, path-style, static creds), then disables retries so the
// "broken" 500 scenario doesn't sit through the SDK's default retry/backoff.
func noRetryClient(t *testing.T, srv *httptest.Server) *s3.Client {
	t.Helper()
	base, err := audit.NewS3Client(context.Background(), audit.S3Config{
		Endpoint: srv.URL, PathStyle: true, AccessKey: testValue, SecretKey: testValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3.New(base.Options(), func(o *s3.Options) { o.RetryMaxAttempts = 1 })
}

func resultsByName(results []audit.CheckResult) map[string]audit.CheckResult {
	m := make(map[string]audit.CheckResult, len(results))
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestConformanceDoesNotFalsePassOnLeakyStore(t *testing.T) {
	fake := newFakeS3(false)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	client := noRetryClient(t, srv)

	results, err := audit.RunConformance(context.Background(), client, "test-bucket", time.Hour)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	by := resultsByName(results)
	for _, name := range []string{"delete-version-refused", "shorten-retention-refused", "switch-to-governance-refused"} {
		r, ok := by[name]
		if !ok {
			t.Fatalf("missing result %q", name)
		}
		if r.Passed {
			t.Errorf("%s: Passed = true on a store that allowed it, want false; detail=%q", name, r.Detail)
		}
	}
}

func TestConformanceDoesNotFalsePassOnBrokenStore(t *testing.T) {
	fake := newFakeS3(true)
	srv := httptest.NewServer(fake)
	defer srv.Close()
	client := noRetryClient(t, srv)

	results, err := audit.RunConformance(context.Background(), client, "test-bucket", time.Hour)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(results) != 7 {
		t.Fatalf("got %d results, want 7", len(results))
	}
	for _, r := range results {
		if strings.Contains(r.Name, "refused") && r.Passed {
			t.Errorf("%s: Passed = true against a store returning 500 for everything; detail=%q", r.Name, r.Detail)
		}
	}
	by := resultsByName(results)
	for _, name := range []string{"plain-overwrite-preserves-original", "delete-marker-preserves-original"} {
		r, ok := by[name]
		if !ok {
			t.Fatalf("missing result %q", name)
		}
		if r.Passed {
			t.Errorf("%s: Passed = true, want false when the mutation itself failed with a non-AccessDenied error", name)
		}
		if !strings.HasPrefix(r.Detail, "inconclusive") {
			t.Errorf("%s: Detail = %q, want it to start with %q", name, r.Detail, "inconclusive")
		}
	}
}

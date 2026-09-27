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

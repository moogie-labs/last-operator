//go:build integration

package audit_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

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

// TestIntegrationS3SinkWORMSemantics proves against real RustFS that (a) a
// stored object carries a COMPLIANCE retention the store itself reports back,
// (b) the store refuses to delete a locked version, and (c) List/Get expose
// extra versions and delete markers the way Verify depends on.
func TestIntegrationS3SinkWORMSemantics(t *testing.T) {
	srv := rustfstest.Start(t)
	client, sink, bucket := lockedSink(t, srv)

	key := audit.EntryKey(chainID, 0)
	original := []byte(`{"a":1}`)
	until := time.Now().Add(time.Hour)
	if err := sink.Put(bg, key, original, until); err != nil {
		t.Fatalf("put: %v", err)
	}

	objs, err := sink.List(bg, key, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) != 1 || len(objs[0].VersionIDs) != 1 {
		t.Fatalf("list after first put = %+v", objs)
	}
	originalVersion := objs[0].VersionIDs[0]

	// (a) the store reports the retention the sink asked for.
	retOut, err := client.GetObjectRetention(bg, &s3.GetObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(originalVersion),
	})
	if err != nil {
		t.Fatalf("GetObjectRetention: %v", err)
	}
	if retOut.Retention == nil {
		t.Fatal("GetObjectRetention returned no retention")
	}
	if retOut.Retention.Mode != types.ObjectLockRetentionModeCompliance {
		t.Fatalf("retention mode = %q, want %q", retOut.Retention.Mode, types.ObjectLockRetentionModeCompliance)
	}
	retainUntil := aws.ToTime(retOut.Retention.RetainUntilDate)
	if retainUntil.Before(until.Add(-time.Second)) {
		t.Fatalf("retain-until %s is before the requested %s (allowing 1s rounding)", retainUntil, until)
	}

	// (b) the store must refuse to delete the locked version.
	_, err = client.DeleteObject(bg, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(originalVersion),
	})
	if err == nil {
		t.Fatal("DeleteObject on a locked version succeeded, want it to be refused")
	}
	if !audit.IsAccessDenied(err) {
		t.Fatalf("DeleteObject on a locked version = %v, want an access-denied error (audit.IsAccessDenied)", err)
	}

	// (c) an unconditional raw put creates a second version, and an
	// unversioned delete adds a delete marker rather than removing anything.
	replacement := []byte(`{"a":2}`)
	sum := md5.Sum(replacement)
	if _, err := client.PutObject(bg, &s3.PutObjectInput{
		Bucket:                    aws.String(bucket),
		Key:                       aws.String(key),
		Body:                      bytes.NewReader(replacement),
		ContentLength:             aws.Int64(int64(len(replacement))),
		ContentMD5:                aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		ContentType:               aws.String("application/json"),
		ObjectLockMode:            types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: aws.Time(until.UTC()),
	}); err != nil {
		t.Fatalf("raw put of a second version: %v", err)
	}
	if _, err := client.DeleteObject(bg, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	}); err != nil {
		t.Fatalf("unversioned delete (should add a delete marker): %v", err)
	}

	objs, err = sink.List(bg, key, "")
	if err != nil {
		t.Fatalf("list after replace+delete: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("list after replace+delete = %+v, want exactly one key", objs)
	}
	o := objs[0]
	if len(o.VersionIDs) != 2 {
		t.Fatalf("versions = %+v, want exactly 2", o.VersionIDs)
	}
	if o.VersionIDs[0] != originalVersion {
		t.Fatalf("versions[0] = %s, want the original version %s first (oldest first)", o.VersionIDs[0], originalVersion)
	}
	if o.DeleteMarkers != 1 {
		t.Fatalf("delete markers = %d, want 1", o.DeleteMarkers)
	}

	gotOriginal, err := sink.Get(bg, key, "")
	if err != nil {
		t.Fatalf("get original (version \"\"): %v", err)
	}
	if !bytes.Equal(gotOriginal, original) {
		t.Fatalf("get(original) = %q, want %q", gotOriginal, original)
	}

	gotReplacement, err := sink.Get(bg, key, o.VersionIDs[1])
	if err != nil {
		t.Fatalf("get replacement version: %v", err)
	}
	if !bytes.Equal(gotReplacement, replacement) {
		t.Fatalf("get(replacement) = %q, want %q", gotReplacement, replacement)
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

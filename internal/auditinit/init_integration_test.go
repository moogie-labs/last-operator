//go:build integration

package auditinit_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/auditinit"
	"github.com/moogie-labs/last-operator/internal/testutil/rustfstest"
)

func expectDenied(t *testing.T, what string, err error) {
	t.Helper()
	if !audit.IsAccessDenied(err) {
		t.Fatalf("%s: want AccessDenied, got %v", what, err)
	}
}

func TestIntegrationInitProvisionsLeastPrivilegeUsers(t *testing.T) {
	ctx := context.Background()
	srv := rustfstest.Start(t)
	cfg := auditinit.Config{
		Endpoint: srv.Endpoint, Bucket: "last-audit", RetentionDays: 1,
		RootAccessKey: rustfstest.RootAccessKey, RootSecretKey: rustfstest.RootSecretKey,
		WriterAccessKey: "writer", WriterSecretKey: "writer-secret-0123456789",
		ReaderAccessKey: "reader", ReaderSecretKey: "reader-secret-0123456789",
		ReadyTimeout: time.Minute,
	}
	if err := auditinit.Run(ctx, cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := auditinit.Run(ctx, cfg); err != nil {
		t.Fatalf("second run (must be idempotent): %v", err)
	}

	writer := srv.Client(t, cfg.WriterAccessKey, cfg.WriterSecretKey)
	wsink := audit.NewS3Sink(writer, cfg.Bucket)
	key := audit.EntryKey("c", 0)
	if err := wsink.Put(ctx, key, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writer put: %v", err)
	}
	objs, err := wsink.List(ctx, audit.EntryPrefix("c"), "")
	if err != nil || len(objs) != 1 {
		t.Fatalf("writer list: %+v %v", objs, err)
	}
	if _, err := wsink.LockConfig(ctx); err != nil {
		t.Fatalf("writer lock config: %v", err)
	}
	_, err = writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key), VersionId: aws.String(objs[0].VersionIDs[0])})
	expectDenied(t, "writer delete version", err)
	_, err = writer.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key)})
	expectDenied(t, "writer delete (marker)", err)

	reader := srv.Client(t, cfg.ReaderAccessKey, cfg.ReaderSecretKey)
	rsink := audit.NewS3Sink(reader, cfg.Bucket)
	if _, err := rsink.Get(ctx, key, ""); err != nil {
		t.Fatalf("reader get: %v", err)
	}
	expectDenied(t, "reader put", rsink.Put(ctx, audit.EntryKey("c", 1), []byte(`{}`), time.Now().Add(time.Hour)))
}

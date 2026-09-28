//go:build integration

package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/testutil/rustfstest"
)

func TestIntegrationConformancePassesOnLockedBucketWithRootCredentials(t *testing.T) {
	srv := rustfstest.Start(t)
	client, _, bucket := lockedSink(t, srv)
	results, err := audit.RunConformance(context.Background(), client, bucket, 24*time.Hour)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(results) != 7 {
		t.Fatalf("got %d results, want 7", len(results))
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s FAILED: %s", r.Name, r.Detail)
		}
	}
}

func TestIntegrationConformanceRejectsBucketWithoutLock(t *testing.T) {
	srv := rustfstest.Start(t)
	client := srv.RootClient(t)
	bucket := randomBucket(t)
	if _, err := client.CreateBucket(bg, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	_, _ = client.PutBucketVersioning(bg, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	results, err := audit.RunConformance(context.Background(), client, bucket, time.Hour)
	if err == nil {
		for _, r := range results {
			if !r.Passed {
				return // a failed check is also an acceptable outcome
			}
		}
		t.Fatal("conformance passed on a bucket without Object Lock")
	}
}

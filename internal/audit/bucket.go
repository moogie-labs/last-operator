package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// EnsureLockedBucket creates bucket with Object Lock enabled if it is missing, and
// sets a compliance-mode default retention of at least retentionDays. It is idempotent.
func EnsureLockedBucket(ctx context.Context, client *s3.Client, bucket string, retentionDays int32) error {
	if retentionDays <= 0 {
		return errors.New("audit: retentionDays must be positive")
	}
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                     aws.String(bucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	})
	if err != nil && !hasCode(err, 0, "BucketAlreadyOwnedByYou", "BucketAlreadyExists") {
		return fmt.Errorf("audit: create bucket %s: %w", bucket, err)
	}
	lc, err := NewS3Sink(client, bucket).LockConfig(ctx)
	if err != nil {
		return err
	}
	if !lc.Enabled {
		return fmt.Errorf("audit: bucket %s exists without Object Lock; use a new bucket", bucket)
	}
	want := time.Duration(retentionDays) * 24 * time.Hour
	if CheckLockConfig(lc, want) == nil {
		return nil
	}
	_, err = client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String(bucket),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{
				Mode: types.ObjectLockRetentionModeCompliance,
				Days: aws.Int32(retentionDays),
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("audit: set lock config on %s: %w", bucket, err)
	}
	return nil
}

package audit

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// CheckResult is one WORM conformance check.
type CheckResult struct {
	Name   string
	Passed bool
	Detail string
}

// RunConformance checks whether bucket really enforces compliance-mode Object Lock
// (spec §14.4). It writes one probe object under conformance/ that stays locked for
// retention. Run it with each set of credentials you want to evaluate.
func RunConformance(ctx context.Context, client *s3.Client, bucket string, retention time.Duration) ([]CheckResult, error) {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	key := fmt.Sprintf("conformance/%d-%s.json", time.Now().UnixNano(), hex.EncodeToString(suffix))
	original := []byte(`{"conformance":true}`)
	retainUntil := time.Now().Add(retention).UTC()

	put, err := client.PutObject(ctx, putInput(bucket, key, original, true, retainUntil))
	if err != nil {
		return nil, fmt.Errorf("conformance setup: write probe object: %w", err)
	}
	versionID := aws.ToString(put.VersionId)
	if versionID == "" {
		return nil, errors.New("conformance setup: store returned no version ID; versioning/Object Lock is not enabled")
	}

	var results []CheckResult
	refused := func(name string, err error) {
		detail := "allowed (should have been refused)"
		if err != nil {
			detail = "refused: " + errorCode(err)
		}
		results = append(results, CheckResult{Name: name, Passed: err != nil, Detail: detail})
	}
	intact := func(name string) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
		if err != nil {
			results = append(results, CheckResult{Name: name, Detail: "original version unreadable: " + errorCode(err)})
			return
		}
		body, _ := io.ReadAll(out.Body)
		out.Body.Close()
		ok := bytes.Equal(body, original)
		detail := "original version intact"
		if !ok {
			detail = "original version changed"
		}
		results = append(results, CheckResult{Name: name, Passed: ok, Detail: detail})
	}

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	refused("delete-version-refused", err)

	_, err = client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), true, retainUntil))
	refused("conditional-overwrite-refused", err)

	_, _ = client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), false, retainUntil))
	intact("plain-overwrite-preserves-original")

	_, err = client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(time.Now().Add(time.Minute).UTC())},
	})
	refused("shorten-retention-refused", err)

	_, err = client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(retainUntil)},
	})
	refused("switch-to-governance-refused", err)

	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	intact("delete-marker-preserves-original")

	_, err = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	refused("delete-bucket-refused", err)

	return results, nil
}

func putInput(bucket, key string, body []byte, ifNoneMatch bool, retainUntil time.Time) *s3.PutObjectInput {
	sum := md5.Sum(body)
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		ContentLength:             aws.Int64(int64(len(body))),
		ContentMD5:                aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		ObjectLockMode:            types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: aws.Time(retainUntil),
	}
	if ifNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	return in
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return err.Error()
}

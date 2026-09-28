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

// refusalCodes maps each "-refused" check to the S3 API error codes that count as a
// genuine refusal, matched on smithy.APIError.ErrorCode() only (never HTTP status,
// so a 403 from bad credentials such as SignatureDoesNotMatch/InvalidAccessKeyId can
// never masquerade as a WORM refusal). Probed live against RustFS 1.0.0 with root
// credentials (see task-10-report.md, "Fix round 1" section) before being finalized;
// every refusal RustFS returned is already covered by these lists.
var refusalCodes = map[string][]string{
	"delete-version-refused":        {"AccessDenied"},
	"shorten-retention-refused":     {"AccessDenied"},
	"switch-to-governance-refused":  {"AccessDenied"},
	"conditional-overwrite-refused": {"PreconditionFailed", "ConditionalRequestConflict"},
	"delete-bucket-refused":         {"BucketNotEmpty", "AccessDenied"},
}

// RunConformance checks whether bucket really enforces compliance-mode Object Lock
// (spec §14.4). It writes one probe object under conformance/ that stays locked for
// retention. Run it with each set of credentials you want to evaluate.
//
// Every "-refused" check only passes when the store's error carries one of that
// check's allowed API error codes (refusalCodes); any other error, or none at all,
// is not treated as a pass. This exists specifically so a broken or overly permissive
// store cannot produce a false PASS: a store that fails closed for the wrong reason
// (e.g. a 500) or doesn't fail at all is reported honestly as inconclusive or failed,
// never as a working WORM guarantee.
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

	// refused records a "-refused" check. It only passes when err's API error code
	// is in that check's allowlist (refusalCodes) -- any other error, or a nil
	// error, is not a pass.
	refused := func(name string, err error) {
		if err == nil {
			results = append(results, CheckResult{Name: name, Passed: false, Detail: "allowed (should have been refused)"})
			return
		}
		code := errorCode(err)
		for _, allowed := range refusalCodes[name] {
			if code == allowed {
				results = append(results, CheckResult{Name: name, Passed: true, Detail: "refused: " + code})
				return
			}
		}
		results = append(results, CheckResult{Name: name, Passed: false, Detail: "inconclusive: " + code})
	}

	// intact records a check that a mutation must not have altered the original
	// version. mutationErr is the error (if any) from the mutation itself: any
	// error other than AccessDenied makes the check's outcome inconclusive rather
	// than checking bytes that may not reflect what actually happened.
	intact := func(name string, mutationErr error) {
		if mutationErr != nil {
			if code := errorCode(mutationErr); code != "AccessDenied" {
				results = append(results, CheckResult{Name: name, Passed: false, Detail: "inconclusive: mutation failed: " + code})
				return
			}
		}
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
		if err != nil {
			results = append(results, CheckResult{Name: name, Detail: "original version unreadable: " + errorCode(err)})
			return
		}
		body, _ := io.ReadAll(out.Body)
		out.Body.Close()
		ok := bytes.Equal(body, original)
		var detail string
		switch {
		case !ok:
			detail = "original version changed"
		case mutationErr != nil:
			detail = "mutation refused (AccessDenied); original intact"
		default:
			detail = "original version intact"
		}
		results = append(results, CheckResult{Name: name, Passed: ok, Detail: detail})
	}

	_, err = client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), true, retainUntil))
	refused("conditional-overwrite-refused", err)

	_, mutErr := client.PutObject(ctx, putInput(bucket, key, []byte(`{"tampered":true}`), false, retainUntil))
	intact("plain-overwrite-preserves-original", mutErr)

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

	_, mutErr = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	intact("delete-marker-preserves-original", mutErr)

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
	refused("delete-version-refused", err)

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

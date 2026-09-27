package audit

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config locates an S3-compatible audit bucket.
type S3Config struct {
	Endpoint  string // empty = AWS endpoint resolution
	Region    string // default us-east-1
	Bucket    string
	AccessKey string // empty = AWS default credential chain
	SecretKey string
	PathStyle bool // required for RustFS
}

// NewS3Client builds a client for S3-compatible stores. It sends checksums only
// when an operation requires them, since many stores reject aws-chunked trailers.
func NewS3Client(ctx context.Context, cfg S3Config) (*s3.Client, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if cfg.AccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("audit: load aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}), nil
}

// S3Sink is a Sink backed by an Object Lock bucket.
type S3Sink struct {
	client *s3.Client
	bucket string
}

func NewS3Sink(client *s3.Client, bucket string) *S3Sink {
	return &S3Sink{client: client, bucket: bucket}
}

func (s *S3Sink) Put(ctx context.Context, key string, body []byte, retainUntil time.Time) error {
	sum := md5.Sum(body)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:                    aws.String(s.bucket),
		Key:                       aws.String(key),
		Body:                      bytes.NewReader(body),
		ContentLength:             aws.Int64(int64(len(body))),
		ContentMD5:                aws.String(base64.StdEncoding.EncodeToString(sum[:])),
		ContentType:               aws.String("application/json"),
		IfNoneMatch:               aws.String("*"),
		ObjectLockMode:            types.ObjectLockModeCompliance,
		ObjectLockRetainUntilDate: aws.Time(retainUntil.UTC()),
	})
	if err != nil {
		if hasCode(err, http.StatusPreconditionFailed, "PreconditionFailed", "ConditionalRequestConflict") {
			return ErrExists
		}
		return fmt.Errorf("audit: put %s: %w", key, err)
	}
	return nil
}

func (s *S3Sink) Get(ctx context.Context, key, versionID string) ([]byte, error) {
	if versionID == "" {
		objs, err := s.List(ctx, key, "")
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if o.Key == key && len(o.VersionIDs) > 0 {
				versionID = o.VersionIDs[0]
			}
		}
		if versionID == "" {
			return nil, ErrNotFound
		}
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), VersionId: aws.String(versionID),
	})
	if err != nil {
		if hasCode(err, http.StatusNotFound, "NoSuchKey", "NoSuchVersion", "NotFound") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("audit: get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// List uses ListObjectVersions so extra versions and delete markers are visible.
// S3 lists each key's versions newest first; we reverse them to oldest first.
func (s *S3Sink) List(ctx context.Context, prefix, startAfter string) ([]ObjectInfo, error) {
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)}
	if startAfter != "" {
		in.KeyMarker = aws.String(startAfter)
	}
	byKey := map[string]*ObjectInfo{}
	info := func(k string) *ObjectInfo {
		if byKey[k] == nil {
			byKey[k] = &ObjectInfo{Key: k}
		}
		return byKey[k]
	}
	for {
		page, err := s.client.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("audit: list %s: %w", prefix, err)
		}
		for _, v := range page.Versions {
			if k := aws.ToString(v.Key); k > startAfter {
				o := info(k)
				o.VersionIDs = append(o.VersionIDs, aws.ToString(v.VersionId))
			}
		}
		for _, d := range page.DeleteMarkers {
			if k := aws.ToString(d.Key); k > startAfter {
				info(k).DeleteMarkers++
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	out := make([]ObjectInfo, 0, len(byKey))
	for _, o := range byKey {
		for i, j := 0, len(o.VersionIDs)-1; i < j; i, j = i+1, j-1 {
			o.VersionIDs[i], o.VersionIDs[j] = o.VersionIDs[j], o.VersionIDs[i]
		}
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *S3Sink) LockConfig(ctx context.Context) (LockConfig, error) {
	out, err := s.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		if hasCode(err, 0, "ObjectLockConfigurationNotFoundError") {
			return LockConfig{}, nil
		}
		return LockConfig{}, fmt.Errorf("audit: get lock config: %w", err)
	}
	var lc LockConfig
	olc := out.ObjectLockConfiguration
	if olc == nil {
		return lc, nil
	}
	lc.Enabled = olc.ObjectLockEnabled == types.ObjectLockEnabledEnabled
	if olc.Rule != nil && olc.Rule.DefaultRetention != nil {
		dr := olc.Rule.DefaultRetention
		lc.Mode = string(dr.Mode)
		lc.DefaultRetention = time.Duration(aws.ToInt32(dr.Days))*24*time.Hour +
			time.Duration(aws.ToInt32(dr.Years))*365*24*time.Hour
	}
	return lc, nil
}

// IsAccessDenied reports whether err is an authorization refusal from the store.
func IsAccessDenied(err error) bool { return hasCode(err, http.StatusForbidden, "AccessDenied") }

// hasCode matches an S3 error by API error code or, if status > 0, HTTP status.
func hasCode(err error, status int, codes ...string) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		for _, c := range codes {
			if apiErr.ErrorCode() == c {
				return true
			}
		}
	}
	var re *awshttp.ResponseError
	return status > 0 && errors.As(err, &re) && re.HTTPStatusCode() == status
}

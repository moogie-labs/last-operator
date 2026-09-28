// Package auditinit provisions the bundled RustFS audit store: an Object Lock
// bucket plus least-privilege writer and reader users (spec §14.4).
package auditinit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/rustfsadmin"
)

const (
	WriterPolicy = "last-audit-writer"
	ReaderPolicy = "last-audit-reader"
)

type Config struct {
	Endpoint                         string
	Bucket                           string
	RetentionDays                    int32
	RootAccessKey, RootSecretKey     string
	WriterAccessKey, WriterSecretKey string
	ReaderAccessKey, ReaderSecretKey string
	ReadyTimeout                     time.Duration
}

func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{ReadyTimeout: 5 * time.Minute, RetentionDays: 365}
	required := map[string]*string{
		"LAST_AUDIT_ENDPOINT":          &cfg.Endpoint,
		"LAST_AUDIT_BUCKET":            &cfg.Bucket,
		"RUSTFS_ACCESS_KEY":            &cfg.RootAccessKey,
		"RUSTFS_SECRET_KEY":            &cfg.RootSecretKey,
		"LAST_AUDIT_WRITER_ACCESS_KEY": &cfg.WriterAccessKey,
		"LAST_AUDIT_WRITER_SECRET_KEY": &cfg.WriterSecretKey,
		"LAST_AUDIT_READER_ACCESS_KEY": &cfg.ReaderAccessKey,
		"LAST_AUDIT_READER_SECRET_KEY": &cfg.ReaderSecretKey,
	}
	for name, dst := range required {
		if *dst = getenv(name); *dst == "" {
			return Config{}, fmt.Errorf("auditinit: %s is required", name)
		}
	}
	if v := getenv("LAST_AUDIT_RETENTION_DAYS"); v != "" {
		days, err := strconv.ParseInt(v, 10, 32)
		if err != nil || days < 1 {
			return Config{}, fmt.Errorf("auditinit: LAST_AUDIT_RETENTION_DAYS=%q is not a positive integer", v)
		}
		cfg.RetentionDays = int32(days)
	}
	return cfg, nil
}

func policy(bucket string, actions []string) []byte {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":   "Allow",
			"Action":   actions,
			"Resource": []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"},
		}},
	}
	b, _ := json.Marshal(doc)
	return b
}

// WriterPolicyDocument lets the gateway append and read, never delete or relax locks.
// PutObjectRetention is needed to set retention on each write; compliance mode
// still refuses any shortening.
func WriterPolicyDocument(bucket string) []byte {
	return policy(bucket, []string{
		"s3:PutObject", "s3:PutObjectRetention", "s3:GetObject", "s3:GetObjectVersion",
		"s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketObjectLockConfiguration",
	})
}

// ReaderPolicyDocument is for `kubectl last audit`.
func ReaderPolicyDocument(bucket string) []byte {
	return policy(bucket, []string{
		"s3:GetObject", "s3:GetObjectVersion", "s3:ListBucket", "s3:ListBucketVersions",
		"s3:GetBucketObjectLockConfiguration",
	})
}

// Run is idempotent: safe to execute on every install and upgrade.
func Run(ctx context.Context, cfg Config) error {
	if err := waitReady(ctx, cfg.Endpoint, cfg.ReadyTimeout); err != nil {
		return err
	}
	client, err := audit.NewS3Client(ctx, audit.S3Config{
		Endpoint: cfg.Endpoint, AccessKey: cfg.RootAccessKey, SecretKey: cfg.RootSecretKey, PathStyle: true,
	})
	if err != nil {
		return err
	}
	if err := audit.EnsureLockedBucket(ctx, client, cfg.Bucket, cfg.RetentionDays); err != nil {
		return err
	}
	admin := rustfsadmin.New(cfg.Endpoint, cfg.RootAccessKey, cfg.RootSecretKey)
	users := []struct {
		policy, ak, sk string
		doc            []byte
	}{
		{WriterPolicy, cfg.WriterAccessKey, cfg.WriterSecretKey, WriterPolicyDocument(cfg.Bucket)},
		{ReaderPolicy, cfg.ReaderAccessKey, cfg.ReaderSecretKey, ReaderPolicyDocument(cfg.Bucket)},
	}
	for _, u := range users {
		if err := admin.AddCannedPolicy(ctx, u.policy, u.doc); err != nil {
			return fmt.Errorf("auditinit: policy %s: %w", u.policy, err)
		}
		if err := admin.AddUser(ctx, u.ak, u.sk); err != nil {
			return fmt.Errorf("auditinit: user %s: %w", u.ak, err)
		}
		if err := admin.SetUserPolicy(ctx, u.policy, u.ak); err != nil {
			return fmt.Errorf("auditinit: attach %s to %s: %w", u.policy, u.ak, err)
		}
	}
	return nil
}

func waitReady(ctx context.Context, endpoint string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health/ready", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("auditinit: %s not ready after %s", endpoint, timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

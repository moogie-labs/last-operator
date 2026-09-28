package auditinit_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moogie-labs/last-operator/internal/auditinit"
)

type policyDoc struct {
	Statement []struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	} `json:"Statement"`
}

func actions(t *testing.T, doc []byte) []string {
	t.Helper()
	var p policyDoc
	if err := json.Unmarshal(doc, &p); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	var out []string
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			t.Fatalf("unexpected effect %q", s.Effect)
		}
		out = append(out, s.Action...)
	}
	slices.Sort(out)
	return out
}

func TestWriterPolicyIsLeastPrivilege(t *testing.T) {
	got := actions(t, auditinit.WriterPolicyDocument("last-audit"))
	want := []string{
		"s3:GetBucketObjectLockConfiguration", "s3:GetObject", "s3:GetObjectVersion",
		"s3:ListBucket", "s3:ListBucketVersions", "s3:PutObject", "s3:PutObjectRetention",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("writer actions = %v, want %v", got, want)
	}
}

func TestReaderPolicyCannotWrite(t *testing.T) {
	for _, a := range actions(t, auditinit.ReaderPolicyDocument("last-audit")) {
		if strings.HasPrefix(a, "s3:Put") || strings.HasPrefix(a, "s3:Delete") {
			t.Fatalf("reader policy grants %s", a)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"LAST_AUDIT_ENDPOINT": "http://last-rustfs.last-audit.svc:9000", "LAST_AUDIT_BUCKET": "last-audit",
		"RUSTFS_ACCESS_KEY": "root", "RUSTFS_SECRET_KEY": "rootsecret",
		"LAST_AUDIT_WRITER_ACCESS_KEY": "w", "LAST_AUDIT_WRITER_SECRET_KEY": "wsecret",
		"LAST_AUDIT_READER_ACCESS_KEY": "r", "LAST_AUDIT_READER_SECRET_KEY": "rsecret",
	}
	cfg, err := auditinit.ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetentionDays != 365 || cfg.ReadyTimeout == 0 || cfg.WriterAccessKey != "w" {
		t.Fatalf("cfg = %+v", cfg)
	}
	delete(env, "LAST_AUDIT_WRITER_SECRET_KEY")
	if _, err := auditinit.ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "LAST_AUDIT_WRITER_SECRET_KEY") {
		t.Fatalf("missing var error = %v", err)
	}
}

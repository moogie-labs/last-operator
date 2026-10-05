package gateway

import (
	"maps"
	"strings"
	"testing"
	"time"
)

const envCanaries = "LAST_EGRESS_CANARIES"

func baseEnv() map[string]string {
	return map[string]string{
		"POD_NAMESPACE": ns, "POD_NAME": testPodName,
		"LAST_AUDIT_ENDPOINT": "http://last-rustfs.last-audit.svc:9000",
		"LAST_AUDIT_BUCKET":   "last-audit", "LAST_AUDIT_CHAIN_ID": testChainID,
		"LAST_OPERATOR_IDENTITY": operatorID,
		envCanaries:              testCanary,
	}
}

// envWith merges overrides onto the shared base env and returns a getenv func.
func envWith(overrides map[string]string) func(string) string {
	env := baseEnv()
	maps.Copy(env, overrides)
	return func(k string) string { return env[k] }
}

func TestConfigFromEnvDefaults(t *testing.T) {
	env := baseEnv()
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention != 8760*time.Hour || !cfg.Audit.PathStyle || cfg.Audit.Region != "us-east-1" ||
		cfg.LeaseName != "last-gateway-audit-writer" || cfg.HTTPAddr != ":8080" ||
		cfg.SigningKeySecret != "last-audit-signing" || cfg.PublicKeyConfigMap != "last-audit-pubkey" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigFromEnvRequiresChainAndBucket(t *testing.T) {
	for _, missing := range []string{"LAST_AUDIT_CHAIN_ID", "LAST_AUDIT_BUCKET", "POD_NAMESPACE", "POD_NAME", "LAST_OPERATOR_IDENTITY"} {
		env := baseEnv()
		delete(env, missing)
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: err = %v", missing, err)
		}
	}
}

func TestConfigFromEnvRejectsBadValues(t *testing.T) {
	env := baseEnv()
	env["LAST_AUDIT_RETENTION"] = "forever"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("accepted retention=forever")
	}
	env = baseEnv()
	env["LAST_AUDIT_PATH_STYLE"] = "maybe"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("accepted path style=maybe")
	}
}

func TestConfigFromEnvAllowNewChain(t *testing.T) {
	env := baseEnv()
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.AllowNewChain {
		t.Fatalf("default AllowNewChain = %v (err=%v), want false", cfg.AllowNewChain, err)
	}
	env["LAST_AUDIT_ALLOW_NEW_CHAIN"] = "true"
	if cfg, err = ConfigFromEnv(func(k string) string { return env[k] }); err != nil || !cfg.AllowNewChain {
		t.Fatalf("AllowNewChain = %v (err=%v), want true", cfg.AllowNewChain, err)
	}
	env["LAST_AUDIT_ALLOW_NEW_CHAIN"] = "maybe"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("accepted LAST_AUDIT_ALLOW_NEW_CHAIN=maybe")
	}
}

func TestConfigFromEnvGRPCDefaults(t *testing.T) {
	env := baseEnv()
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":9090" || cfg.OperatorIdentity != operatorID {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigFromEnvTreatsBlankAsMissingAndReportsFirstInFixedOrder(t *testing.T) {
	env := baseEnv()
	for k := range env {
		env[k] = "  \t"
	}
	for range 20 {
		_, err := ConfigFromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
			t.Fatalf("err = %v, want deterministic POD_NAMESPACE", err)
		}
	}
	env = baseEnv()
	env["LAST_OPERATOR_IDENTITY"] = "   "
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "LAST_OPERATOR_IDENTITY") {
		t.Fatalf("whitespace-only identity: err = %v", err)
	}
}

func TestConfigFromEnvTrimsIdentityAndHonorsGRPCAddr(t *testing.T) {
	env := baseEnv()
	env["LAST_OPERATOR_IDENTITY"] = " " + operatorID + "\n"
	env["LAST_GRPC_ADDR"] = ":7070"
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OperatorIdentity != operatorID || cfg.GRPCAddr != ":7070" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigFromEnvSandboxSettings(t *testing.T) {
	cfg, err := ConfigFromEnv(envWith(map[string]string{
		envCanaries: " 10.96.0.10:8080 , service/last-gateway/last-gateway:8080,node:10250 ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxNamespace != testSandboxNS || cfg.DrainTimeout != 5*time.Minute {
		t.Fatalf("defaults = %q %v", cfg.SandboxNamespace, cfg.DrainTimeout)
	}
	want := []string{"10.96.0.10:8080", "service/last-gateway/last-gateway:8080", "node:10250"}
	if strings.Join(cfg.EgressCanaries, ",") != strings.Join(want, ",") {
		t.Fatalf("canaries = %q", cfg.EgressCanaries)
	}
	for _, bad := range []string{"", "  ", "example.com:80", "service/x:80", "node:0", "10.0.0.1:99999", "pod/a/b:1",
		"127.0.0.1:80", "0.0.0.0:80", "[fe80::1%eth0]:80", "[::1]:80", "224.0.0.1:80"} {
		if _, err := ConfigFromEnv(envWith(map[string]string{envCanaries: bad})); err == nil {
			t.Errorf("LAST_EGRESS_CANARIES=%q was accepted", bad)
		}
	}
}

func TestConfigFromEnvReadsTheWriterSecretName(t *testing.T) {
	cfg, err := ConfigFromEnv(envWith(map[string]string{"LAST_AUDIT_WRITER_SECRET": " my-writer "}))
	if err != nil || cfg.AuditWriterSecret != "my-writer" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	cfg, err = ConfigFromEnv(envWith(nil))
	if err != nil || cfg.AuditWriterSecret != "" {
		t.Fatalf("default cfg = %+v, %v", cfg, err)
	}
}

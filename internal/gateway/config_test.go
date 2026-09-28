package gateway

import (
	"strings"
	"testing"
	"time"
)

func baseEnv() map[string]string {
	return map[string]string{
		"POD_NAMESPACE": "last-gateway", "POD_NAME": "gw-0",
		"LAST_AUDIT_ENDPOINT": "http://last-rustfs.last-audit.svc:9000",
		"LAST_AUDIT_BUCKET":   "last-audit", "LAST_AUDIT_CHAIN_ID": "chain-1",
	}
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
	for _, missing := range []string{"LAST_AUDIT_CHAIN_ID", "LAST_AUDIT_BUCKET", "POD_NAMESPACE", "POD_NAME"} {
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

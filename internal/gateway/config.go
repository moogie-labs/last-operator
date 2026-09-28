// Package gateway is the trusted component that holds credentials and writes the audit log.
package gateway

import (
	"fmt"
	"strconv"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type Config struct {
	Namespace          string
	PodName            string
	Audit              audit.S3Config
	ChainID            string
	Retention          time.Duration
	SigningKeySecret   string
	PublicKeyConfigMap string
	LeaseName          string
	HTTPAddr           string
	// AllowNewChain (LAST_AUDIT_ALLOW_NEW_CHAIN) permits starting a new chain in a
	// bucket that already holds other chains.
	AllowNewChain bool
}

func ConfigFromEnv(getenv func(string) string) (Config, error) {
	or := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	cfg := Config{
		Namespace: getenv("POD_NAMESPACE"),
		PodName:   getenv("POD_NAME"),
		ChainID:   getenv("LAST_AUDIT_CHAIN_ID"),
		Audit: audit.S3Config{
			Endpoint:  getenv("LAST_AUDIT_ENDPOINT"),
			Region:    or("LAST_AUDIT_REGION", "us-east-1"),
			Bucket:    getenv("LAST_AUDIT_BUCKET"),
			AccessKey: getenv("LAST_AUDIT_ACCESS_KEY"),
			SecretKey: getenv("LAST_AUDIT_SECRET_KEY"),
		},
		SigningKeySecret:   "last-audit-signing",
		PublicKeyConfigMap: "last-audit-pubkey",
		LeaseName:          "last-gateway-audit-writer",
		HTTPAddr:           or("LAST_HTTP_ADDR", ":8080"),
	}
	for name, v := range map[string]string{
		"POD_NAMESPACE": cfg.Namespace, "POD_NAME": cfg.PodName,
		"LAST_AUDIT_CHAIN_ID": cfg.ChainID, "LAST_AUDIT_BUCKET": cfg.Audit.Bucket,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("gateway: %s is required", name)
		}
	}
	pathStyle, err := strconv.ParseBool(or("LAST_AUDIT_PATH_STYLE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("gateway: LAST_AUDIT_PATH_STYLE: %w", err)
	}
	cfg.Audit.PathStyle = pathStyle
	cfg.AllowNewChain, err = strconv.ParseBool(or("LAST_AUDIT_ALLOW_NEW_CHAIN", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("gateway: LAST_AUDIT_ALLOW_NEW_CHAIN: %w", err)
	}
	cfg.Retention, err = time.ParseDuration(or("LAST_AUDIT_RETENTION", "8760h"))
	if err != nil || cfg.Retention <= 0 {
		return Config{}, fmt.Errorf("gateway: LAST_AUDIT_RETENTION must be a positive duration: %v", err)
	}
	return cfg, nil
}

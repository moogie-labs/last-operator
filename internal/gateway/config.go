// Package gateway is the trusted component that holds credentials and writes the audit log.
package gateway

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
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
	// GRPCAddr (LAST_GRPC_ADDR) is the gRPC listen address.
	GRPCAddr string
	// OperatorIdentity (LAST_OPERATOR_IDENTITY) is the only caller the gRPC API admits.
	OperatorIdentity string
	// AllowNewChain (LAST_AUDIT_ALLOW_NEW_CHAIN) permits starting a new chain in a
	// bucket that already holds other chains.
	AllowNewChain bool
	// SandboxNamespace (LAST_SANDBOX_NAMESPACE, default last-sandbox) holds sandbox pods.
	SandboxNamespace string
	// SandboxImage (LAST_SANDBOX_IMAGE) is used only when OperatorConfig
	// spec.sandbox.baseImage is unset; production pins the image by digest there.
	SandboxImage string
	// EgressCanaries (LAST_EGRESS_CANARIES, required) are comma-separated canary
	// specs: "IP:port", "service/<namespace>/<name>:<port>" (the Service's
	// ClusterIP), or "node:<port>" (a node InternalIP). Every sandbox waits until
	// all of them are observed denied before running anything (spec §11.1).
	EgressCanaries []string
	// DrainTimeout (LAST_DRAIN_TIMEOUT, default 5m) bounds the wait for in-flight
	// RPCs when this replica stops leading.
	DrainTimeout time.Duration
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
		GRPCAddr:           or("LAST_GRPC_ADDR", ":9090"),
		OperatorIdentity:   strings.TrimSpace(getenv("LAST_OPERATOR_IDENTITY")),
		SandboxNamespace:   or("LAST_SANDBOX_NAMESPACE", "last-sandbox"),
		SandboxImage:       strings.TrimSpace(getenv("LAST_SANDBOX_IMAGE")),
	}
	// A fixed-order slice keeps the reported variable deterministic.
	for _, req := range []struct{ name, value string }{
		{"POD_NAMESPACE", cfg.Namespace}, {"POD_NAME", cfg.PodName},
		{"LAST_AUDIT_CHAIN_ID", cfg.ChainID}, {"LAST_AUDIT_BUCKET", cfg.Audit.Bucket},
		{"LAST_OPERATOR_IDENTITY", cfg.OperatorIdentity},
	} {
		if strings.TrimSpace(req.value) == "" {
			return Config{}, fmt.Errorf("gateway: %s is required", req.name)
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
	cfg.DrainTimeout, err = time.ParseDuration(or("LAST_DRAIN_TIMEOUT", "5m"))
	if err != nil || cfg.DrainTimeout <= 0 {
		return Config{}, fmt.Errorf("gateway: LAST_DRAIN_TIMEOUT must be a positive duration: %v", err)
	}
	for spec := range strings.SplitSeq(getenv("LAST_EGRESS_CANARIES"), ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		if err := validateCanarySpec(spec); err != nil {
			return Config{}, fmt.Errorf("gateway: LAST_EGRESS_CANARIES: %w", err)
		}
		cfg.EgressCanaries = append(cfg.EgressCanaries, spec)
	}
	if len(cfg.EgressCanaries) == 0 {
		return Config{}, errors.New("gateway: LAST_EGRESS_CANARIES is required (the sandbox egress gate needs at least one canary)")
	}
	return cfg, nil
}

// validateCanarySpec accepts "IP:port", "service/<ns>/<name>:<port>", or "node:<port>".
func validateCanarySpec(spec string) error {
	head, portStr, ok := strings.Cut(spec, ":")
	if strings.Count(spec, ":") > 1 || strings.HasPrefix(spec, "[") {
		head, portStr, ok = "", "", false
		if ap, err := netip.ParseAddrPort(spec); err == nil && ap.Port() != 0 {
			return checkCanaryAddr(ap.Addr())
		}
	}
	if !ok {
		return fmt.Errorf("canary %q: want IP:port, service/<ns>/<name>:<port>, or node:<port>", spec)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("canary %q: bad port", spec)
	}
	switch {
	case head == "node":
		return nil
	case strings.HasPrefix(head, "service/"):
		if parts := strings.Split(head, "/"); len(parts) == 3 && parts[1] != "" && parts[2] != "" {
			return nil
		}
		return fmt.Errorf("canary %q: want service/<namespace>/<name>:<port>", spec)
	default:
		addr, err := netip.ParseAddr(head)
		if err != nil {
			return fmt.Errorf("canary %q: %q is not an IP address", spec, head)
		}
		return checkCanaryAddr(addr)
	}
}

// checkCanaryAddr refuses addresses a network policy can never block (loopback,
// unspecified, link-local, multicast, zoned): a canary there would be "denied"
// trivially and prove nothing about egress.
func checkCanaryAddr(a netip.Addr) error {
	if a.Zone() != "" || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return fmt.Errorf("canary address %q is loopback, unspecified, link-local, multicast or zoned; network policy cannot block it", a)
	}
	return nil
}

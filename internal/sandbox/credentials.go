package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	CredsMountPath  = "/last/creds"
	KubeCAMountPath = "/last/kube-ca"
	DefaultTokenTTL = 10 * time.Minute
	tokenTTLSlack   = time.Minute // tolerated shortfall between requested and granted token lifetime
	credsVolume     = "creds"
	kubeCAVolume    = "kube-ca"
	kubeRootCA      = "kube-root-ca.crt" // ConfigMap present in every namespace
)

var (
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reservedEnvNames = map[string]bool{
		"PATH": true, "HOME": true, "KUBECONFIG": true, "SHELL": true,
		"USER": true, "PWD": true, "TMPDIR": true,
	}
)

var (
	ErrMultipleKubernetesProviders = errors.New("sandbox: at most one kubernetes provider per command")
	ErrUnsupportedProvider         = errors.New("sandbox: unsupported provider type")
	ErrReservedEnv                 = errors.New("sandbox: reserved or invalid env var name")
	// ErrForbiddenProvider marks a Provider that would hand the sandbox the gateway's
	// identity or audit credentials.
	ErrForbiddenProvider = errors.New("sandbox: provider would expose gateway credentials")
)

// AnchorSuffix names the per-run Secret in the provider namespace that kubernetes
// tokens are bound to; deleting it revokes them (TokenRequest boundObjectRef).
const AnchorSuffix = "-anchor"

// ProviderGetter reads a cluster-scoped Provider by name.
type ProviderGetter func(ctx context.Context, name string) (*lastv1alpha1.Provider, error)

// IssuedCredential describes an issued credential for the audit log. It never holds values.
type IssuedCredential struct {
	Provider       string   `json:"provider"`
	Type           string   `json:"type"`
	Env            []string `json:"env,omitempty"`
	Files          []string `json:"files,omitempty"`
	ServiceAccount string   `json:"serviceAccount,omitempty"`
	TTL            string   `json:"ttl,omitempty"`
}

// CredentialPlan is a validated provider plus its audit description.
type CredentialPlan struct {
	Provider *lastv1alpha1.Provider
	Issued   IssuedCredential
	TokenTTL time.Duration // the effective token lifetime for kubernetes providers
}

// CredentialConfig locates the namespaces and the API server.
type CredentialConfig struct {
	SandboxNamespace  string
	ProviderNamespace string
	APIServerHost     string // the kubernetes Service ClusterIP
	// ExpiresAt stamps the run's expiry on the token anchor; zero means no expiry label.
	ExpiresAt time.Time
}

// Materialized is what a run's pod needs to present its credentials.
type Materialized struct {
	Secret         *corev1.Secret // nil when the run has no providers
	Env            []corev1.EnvVar
	Mounts         []corev1.VolumeMount
	Volumes        []corev1.Volume
	NeedsAPIServer bool
}

const (
	gatewayServiceAccount = "last-gateway"
	runSecretPrefix       = "last-run-"
)

// gatewaySecrets are the gateway's own Secrets, which no Provider may reference.
var gatewaySecrets = []string{"last-audit-signing", "last-audit-writer"}

func forbiddenSecret(name string, extra []string) bool {
	return slices.Contains(gatewaySecrets, name) || slices.Contains(extra, name) || strings.HasPrefix(name, runSecretPrefix)
}

// PlanCredentials resolves and validates providers without touching any secret,
// so the caller can audit the issuance before it happens. A provider that names
// the gateway's ServiceAccount or one of its audit or run Secrets (or any of
// forbiddenSecrets) is refused with ErrForbiddenProvider.
func PlanCredentials(ctx context.Context, get ProviderGetter, providerNamespace string, names []string, forbiddenSecrets ...string) ([]CredentialPlan, error) {
	plans := make([]CredentialPlan, 0, len(names))
	envSeen := map[string]string{}
	filePathsSeen := map[string]string{}
	kubernetesCount := 0
	for _, name := range names {
		p, err := get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("sandbox: provider %q: %w", name, err)
		}
		issued := IssuedCredential{Provider: name, Type: string(p.Spec.Type)}
		switch p.Spec.Type {
		case lastv1alpha1.ProviderEnvSecret:
			if p.Spec.SecretRef == nil {
				return nil, fmt.Errorf("sandbox: provider %q has no secretRef", name)
			}
			if forbiddenSecret(p.Spec.SecretRef.Name, forbiddenSecrets) {
				return nil, fmt.Errorf("%w: provider %q references Secret %q", ErrForbiddenProvider, name, p.Spec.SecretRef.Name)
			}
			for _, v := range sortedKeys(p.Spec.Env) {
				if err := isValidEnvName(v); err != nil {
					return nil, fmt.Errorf("sandbox: provider %q: %w (env var %q)", name, err, v)
				}
				if other, dup := envSeen[v]; dup {
					return nil, fmt.Errorf("sandbox: env var %s is set by both provider %q and %q", v, other, name)
				}
				envSeen[v] = name
				issued.Env = append(issued.Env, v)
			}
			for _, key := range sortedKeys(p.Spec.Files) {
				path := p.Spec.Files[key]
				if !underWork(path) {
					return nil, fmt.Errorf("sandbox: provider %q file path %q must be under /work/", name, path)
				}
				cleanPath := filepath.Clean(path)
				if other, dup := filePathsSeen[cleanPath]; dup {
					return nil, fmt.Errorf("sandbox: file path %q (from provider %q) conflicts with provider %q", cleanPath, name, other)
				}
				filePathsSeen[cleanPath] = name
				issued.Files = append(issued.Files, cleanPath)
			}
		case lastv1alpha1.ProviderKubernetes:
			kubernetesCount++
			if kubernetesCount > 1 {
				return nil, ErrMultipleKubernetesProviders
			}
			if p.Spec.Kubernetes == nil {
				return nil, fmt.Errorf("sandbox: provider %q has no kubernetes section", name)
			}
			if p.Spec.Kubernetes.ServiceAccountRef.Name == gatewayServiceAccount {
				return nil, fmt.Errorf("%w: provider %q references ServiceAccount %q", ErrForbiddenProvider, name, gatewayServiceAccount)
			}
			issued.ServiceAccount = providerNamespace + "/" + p.Spec.Kubernetes.ServiceAccountRef.Name
			issued.TTL = tokenTTL(p).String()
		default:
			return nil, fmt.Errorf("%w: %q (provider %q)", ErrUnsupportedProvider, p.Spec.Type, name)
		}
		plan := CredentialPlan{Provider: p, Issued: issued}
		if p.Spec.Type == lastv1alpha1.ProviderKubernetes {
			plan.TokenTTL = tokenTTL(p)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// RaiseTokenTTL adjusts the token lifetime for kubernetes providers to cover the pod's
// activeDeadline plus startup grace. The executor calls it before the credential.issued
// entry is written, so the audit entry records the effective lifetime.
func RaiseTokenTTL(plans []CredentialPlan, minTTL time.Duration) {
	// Round up to whole seconds
	var minTTLRounded time.Duration
	nanos := minTTL.Nanoseconds()
	secs := nanos / int64(time.Second)
	if nanos%int64(time.Second) != 0 {
		secs++
	}
	minTTLRounded = time.Duration(secs) * time.Second

	for i := range plans {
		if plans[i].Provider == nil {
			continue
		}
		if plans[i].Provider.Spec.Type == lastv1alpha1.ProviderKubernetes && plans[i].TokenTTL < minTTL {
			plans[i].TokenTTL = minTTLRounded
			plans[i].Issued.TTL = minTTLRounded.String()
		}
	}
}

// Materialize reads provider secrets and mints tokens into a per-run Secret
// (not yet created) plus the env, mounts and volumes that expose it. For a kubernetes
// provider it first creates one object: the token anchor Secret in the provider
// namespace, which every token is bound to. Callers must invoke it only after the
// credential.issued entry is durable. The anchor carries the run labels, so cleanup
// and the reaper remove it even when Materialize fails later.
func Materialize(ctx context.Context, kube kubernetes.Interface, cfg CredentialConfig, runID string, plans []CredentialPlan) (Materialized, error) {
	var m Materialized
	if len(plans) == 0 {
		return m, nil
	}
	secretName := "last-run-" + runID + "-creds"
	data := map[string][]byte{}
	var anchor *corev1.Secret
	for _, cp := range plans {
		if cp.Provider == nil {
			return Materialized{}, fmt.Errorf("sandbox: credential plan has nil provider")
		}
		p := cp.Provider
		switch p.Spec.Type {
		case lastv1alpha1.ProviderEnvSecret:
			if p.Spec.SecretRef == nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q has nil secretRef", p.Name)
			}
			src, err := kube.CoreV1().Secrets(cfg.ProviderNamespace).Get(ctx, p.Spec.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q secret: %w", p.Name, err)
			}
			for _, envVar := range sortedKeys(p.Spec.Env) {
				val, err := secretKey(src, p.Name, p.Spec.Env[envVar])
				if err != nil {
					return Materialized{}, err
				}
				key := p.Name + ".env." + envVar
				data[key] = val
				m.Env = append(m.Env, corev1.EnvVar{Name: envVar, ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: key},
				}})
			}
			for _, srcKey := range sortedKeys(p.Spec.Files) {
				val, err := secretKey(src, p.Name, srcKey)
				if err != nil {
					return Materialized{}, err
				}
				key := p.Name + ".file." + srcKey
				data[key] = val
				m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: credsVolume, MountPath: filepath.Clean(p.Spec.Files[srcKey]), SubPath: key, ReadOnly: true})
			}
		case lastv1alpha1.ProviderKubernetes:
			if p.Spec.Kubernetes == nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q has nil kubernetes section", p.Name)
			}
			sa := p.Spec.Kubernetes.ServiceAccountRef.Name
			ttl := cp.TokenTTL
			if ttl == 0 {
				ttl = tokenTTL(p)
			}
			if anchor == nil {
				meta := metav1.ObjectMeta{Name: "last-run-" + runID + AnchorSuffix, Namespace: cfg.ProviderNamespace, Labels: RunLabels(runID)}
				if !cfg.ExpiresAt.IsZero() {
					stampExpiry(&meta, cfg.ExpiresAt)
				}
				created, err := kube.CoreV1().Secrets(cfg.ProviderNamespace).Create(ctx,
					&corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque}, metav1.CreateOptions{})
				if err != nil {
					return Materialized{}, fmt.Errorf("sandbox: create token anchor: %w", err)
				}
				if created.UID == "" {
					return Materialized{}, fmt.Errorf("sandbox: token anchor %s has no uid", created.Name)
				}
				anchor = created
			}
			secs := int64(ttl.Seconds())
			requestedAt := time.Now()
			tr, err := kube.CoreV1().ServiceAccounts(cfg.ProviderNamespace).CreateToken(ctx, sa,
				&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &secs,
					BoundObjectRef: &authv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: anchor.Name, UID: anchor.UID}}}, metav1.CreateOptions{})
			if err != nil {
				return Materialized{}, fmt.Errorf("sandbox: provider %q token for %s/%s: %w", p.Name, cfg.ProviderNamespace, sa, err)
			}
			// The API server may cap the lifetime (e.g. --service-account-max-token-expiration);
			// a token that expires before the pod must not be used silently.
			if exp := tr.Status.ExpirationTimestamp.Time; !exp.IsZero() && exp.Before(requestedAt.Add(ttl-tokenTTLSlack)) {
				return Materialized{}, fmt.Errorf("sandbox: provider %q token for %s/%s was capped: requested %s, granted %s",
					p.Name, cfg.ProviderNamespace, sa, ttl, exp.Sub(requestedAt).Round(time.Second))
			}
			data[p.Name+".token"] = []byte(tr.Status.Token)
			data[p.Name+".kubeconfig"] = []byte(kubeconfig(cfg.APIServerHost, CredsMountPath+"/"+p.Name+".token"))
			m.Env = append(m.Env, corev1.EnvVar{Name: "KUBECONFIG", Value: CredsMountPath + "/" + p.Name + ".kubeconfig"})
			m.NeedsAPIServer = true
		default:
			return Materialized{}, fmt.Errorf("%w: %q (provider %q)", ErrUnsupportedProvider, p.Spec.Type, p.Name)
		}
	}
	m.Secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cfg.SandboxNamespace, Labels: RunLabels(runID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	m.Volumes = append(m.Volumes, corev1.Volume{Name: credsVolume, VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: ptrInt32(0o440)},
	}})
	m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: credsVolume, MountPath: CredsMountPath, ReadOnly: true})
	if m.NeedsAPIServer {
		m.Volumes = append(m.Volumes, corev1.Volume{Name: kubeCAVolume, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: kubeRootCA}},
		}})
		m.Mounts = append(m.Mounts, corev1.VolumeMount{Name: kubeCAVolume, MountPath: KubeCAMountPath, ReadOnly: true})
	}
	return m, nil
}

func kubeconfig(apiServerHost, tokenFile string) string {
	return strings.Join([]string{
		"apiVersion: v1",
		"kind: Config",
		"clusters:",
		"- name: in-cluster",
		"  cluster:",
		"    server: https://" + net.JoinHostPort(apiServerHost, "443"),
		"    certificate-authority: " + KubeCAMountPath + "/ca.crt",
		"    tls-server-name: kubernetes.default.svc",
		"users:",
		"- name: provider",
		"  user:",
		"    tokenFile: " + tokenFile,
		"contexts:",
		"- name: provider",
		"  context:",
		"    cluster: in-cluster",
		"    user: provider",
		"current-context: provider",
		"",
	}, "\n")
}

func secretKey(s *corev1.Secret, provider, key string) ([]byte, error) {
	v, ok := s.Data[key]
	if !ok {
		return nil, fmt.Errorf("sandbox: provider %q secret %s/%s has no key %q", provider, s.Namespace, s.Name, key)
	}
	return v, nil
}

func tokenTTL(p *lastv1alpha1.Provider) time.Duration {
	if p.Spec.Kubernetes != nil && p.Spec.Kubernetes.TokenTTL != nil && p.Spec.Kubernetes.TokenTTL.Duration >= DefaultTokenTTL {
		return p.Spec.Kubernetes.TokenTTL.Duration
	}
	return DefaultTokenTTL // the API server's minimum token lifetime
}

func underWork(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(path) && strings.HasPrefix(clean, "/work/")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func isValidEnvName(name string) error {
	if !envNamePattern.MatchString(name) {
		return ErrReservedEnv
	}
	if reservedEnvNames[name] {
		return ErrReservedEnv
	}
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "LAST_") {
		return ErrReservedEnv
	}
	return nil
}

func ptrInt32(v int32) *int32 {
	x := v
	return &x
}

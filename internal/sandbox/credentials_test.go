package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	secretValue   = "s3cr3t-value"
	demoProvider  = "demo"
	demoCreds     = "demo-creds"
	tokenKey      = "token"
	configKey     = "config"
	apiServerHost = "10.96.0.1"
	runID         = "run1"
	sandboxNS     = "last-sandbox"
	gatewayNS     = "last-gateway"
	clusterName   = "cluster"
)

func providers(ps ...*lastv1alpha1.Provider) sandbox.ProviderGetter {
	m := map[string]*lastv1alpha1.Provider{}
	for _, p := range ps {
		m[p.Name] = p
	}
	return func(_ context.Context, name string) (*lastv1alpha1.Provider, error) {
		if p, ok := m[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("provider %q not found", name)
	}
}

func envProvider() *lastv1alpha1.Provider {
	return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: demoProvider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: demoCreds},
		Env:   map[string]string{"DEMO_TOKEN": tokenKey},
		Files: map[string]string{configKey: "/work/.demo/" + configKey},
	}}
}

func k8sProvider(name string) *lastv1alpha1.Provider {
	return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "deployer"}},
	}}
}

var credCfg = sandbox.CredentialConfig{SandboxNamespace: sandboxNS, ProviderNamespace: gatewayNS, APIServerHost: apiServerHost}

func tokenClient(token string, gotTTL *int64) *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if gotTTL != nil && tr.Spec.ExpirationSeconds != nil {
			*gotTTL = *tr.Spec.ExpirationSeconds
		}
		tr.Status.Token = token
		return true, tr, nil
	})
	return kube
}

func TestEnvSecretProviderMaterializesWithoutLeakingValues(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: demoCreds, Namespace: gatewayNS},
		Data: map[string][]byte{tokenKey: []byte(secretValue), configKey: []byte("cfg-" + secretValue)}})
	plans, err := sandbox.PlanCredentials(ctx, providers(envProvider()), gatewayNS, []string{demoProvider})
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err != nil {
		t.Fatal(err)
	}
	if m.Secret == nil || m.Secret.Namespace != sandboxNS || m.Secret.Labels[sandbox.LabelRun] != runID {
		t.Fatalf("secret = %+v", m.Secret)
	}
	if string(m.Secret.Data[demoProvider+".env.DEMO_TOKEN"]) != secretValue {
		t.Fatalf("secret data = %v", m.Secret.Data)
	}
	if len(m.Env) != 1 || m.Env[0].Name != "DEMO_TOKEN" || m.Env[0].ValueFrom.SecretKeyRef.Key != demoProvider+".env.DEMO_TOKEN" {
		t.Fatalf("env = %+v", m.Env)
	}
	foundFile := false
	for _, mt := range m.Mounts {
		if mt.MountPath == "/work/.demo/"+configKey && mt.SubPath == demoProvider+".file."+configKey && mt.ReadOnly {
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("mounts = %+v", m.Mounts)
	}
	issued, _ := json.Marshal(plans[0].Issued)
	if strings.Contains(string(issued), secretValue) || !strings.Contains(string(issued), "DEMO_TOKEN") {
		t.Fatalf("issued description = %s", issued)
	}
}

func TestEnvSecretMissingKeyNamesKeyNotValue(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: demoCreds, Namespace: gatewayNS},
		Data: map[string][]byte{tokenKey: []byte(secretValue)}})
	plans, _ := sandbox.PlanCredentials(ctx, providers(envProvider()), gatewayNS, []string{demoProvider})
	_, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err == nil || !strings.Contains(err.Error(), `"`+configKey+`"`) || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanRejectsFilePathOutsideWork(t *testing.T) {
	p := envProvider()
	p.Spec.Files = map[string]string{configKey: "/etc/demo"}
	if _, err := sandbox.PlanCredentials(context.Background(), providers(p), gatewayNS, []string{demoProvider}); err == nil {
		t.Fatal("accepted a file path outside /work")
	}
}

func TestKubernetesProviderMintsTokenAndKubeconfig(t *testing.T) {
	ctx := context.Background()
	var ttl int64
	kube := tokenClient("tok-"+secretValue, &ttl)
	clusterName := "cluster"
	plans, err := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != int64(sandbox.DefaultTokenTTL.Seconds()) || !m.NeedsAPIServer {
		t.Fatalf("ttl=%d needsAPIServer=%v", ttl, m.NeedsAPIServer)
	}
	kc := string(m.Secret.Data[clusterName+".kubeconfig"])
	for _, want := range []string{"server: https://" + apiServerHost + ":443", "tls-server-name: kubernetes.default.svc",
		"certificate-authority: /last/kube-ca/ca.crt", "tokenFile: /last/creds/" + clusterName + ".token"} {
		if !strings.Contains(kc, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, kc)
		}
	}
	if string(m.Secret.Data[clusterName+".token"]) != "tok-"+secretValue {
		t.Fatal("token not stored in the run secret")
	}
	hasEnv := false
	for _, e := range m.Env {
		if e.Name == "KUBECONFIG" && e.Value == "/last/creds/"+clusterName+".kubeconfig" {
			hasEnv = true
		}
	}
	if !hasEnv {
		t.Fatalf("env = %+v", m.Env)
	}
	issued, _ := json.Marshal(plans[0].Issued)
	if strings.Contains(string(issued), secretValue) || !strings.Contains(string(issued), `"serviceAccount":"`+gatewayNS+`/deployer"`) {
		t.Fatalf("issued = %s", issued)
	}
}

func TestAtMostOneKubernetesProvider(t *testing.T) {
	_, err := sandbox.PlanCredentials(context.Background(), providers(k8sProvider("a"), k8sProvider("b")), gatewayNS, []string{"a", "b"})
	if !errors.Is(err, sandbox.ErrMultipleKubernetesProviders) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownProviderAndDuplicateEnv(t *testing.T) {
	ctx := context.Background()
	if _, err := sandbox.PlanCredentials(ctx, providers(), gatewayNS, []string{"missing"}); err == nil {
		t.Fatal("accepted an unknown provider")
	}
	a, b := envProvider(), envProvider()
	b.Name = "demo2"
	b.Spec.Files = nil
	if _, err := sandbox.PlanCredentials(ctx, providers(a, b), gatewayNS, []string{demoProvider, "demo2"}); err == nil || !strings.Contains(err.Error(), "DEMO_TOKEN") {
		t.Fatalf("duplicate env var err = %v", err)
	}
}

func TestNoProvidersMaterializesNothing(t *testing.T) {
	m, err := sandbox.Materialize(context.Background(), fake.NewClientset(), credCfg, runID, nil)
	if err != nil || m.Secret != nil || len(m.Env) != 0 || len(m.Volumes) != 0 {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

// R1: Reserved and invalid env names
func TestReservedAndInvalidEnvNames(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"PATH", false},
		{"KUBECONFIG", false},
		{"LD_PRELOAD", false},
		{"LAST_X", false},
		{"1BAD", false},
		{"A-B", false},
		{"VALID_NAME", true},
	}
	for _, test := range tests {
		p := envProvider()
		p.Spec.Env = map[string]string{test.name: tokenKey}
		_, err := sandbox.PlanCredentials(context.Background(), providers(p), gatewayNS, []string{demoProvider})
		if test.ok {
			if err != nil {
				t.Errorf("env %q: unexpected error: %v", test.name, err)
			}
		} else {
			if err == nil || !errors.Is(err, sandbox.ErrReservedEnv) {
				t.Errorf("env %q: expected ErrReservedEnv, got %v", test.name, err)
			}
		}
	}
}

// R2: Token lifetime
func TestRaiseTokenTTL(t *testing.T) {
	// Test: raises 10m to 17m and updates Issued.TTL
	p10m := k8sProvider("cluster10m")
	plans, _ := sandbox.PlanCredentials(context.Background(), providers(p10m), gatewayNS, []string{"cluster10m"})
	if plans[0].TokenTTL != 10*time.Minute {
		t.Fatalf("initial TokenTTL = %v, want 10m", plans[0].TokenTTL)
	}
	sandbox.RaiseTokenTTL(plans, 17*time.Minute)
	if plans[0].TokenTTL != 17*time.Minute {
		t.Fatalf("raised TokenTTL = %v, want 17m", plans[0].TokenTTL)
	}
	if plans[0].Issued.TTL != "17m0s" {
		t.Fatalf("Issued.TTL = %q, want 17m0s", plans[0].Issued.TTL)
	}

	// Test: leaves 30m unchanged
	p30m := k8sProvider("cluster30m")
	p30m.Spec.Kubernetes.TokenTTL = &metav1.Duration{Duration: 30 * time.Minute}
	plans, _ = sandbox.PlanCredentials(context.Background(), providers(p30m), gatewayNS, []string{"cluster30m"})
	if plans[0].TokenTTL != 30*time.Minute {
		t.Fatalf("initial TokenTTL = %v, want 30m", plans[0].TokenTTL)
	}
	sandbox.RaiseTokenTTL(plans, 17*time.Minute)
	if plans[0].TokenTTL != 30*time.Minute {
		t.Fatalf("raised TokenTTL = %v, want 30m unchanged", plans[0].TokenTTL)
	}

	// Test: ignores env-secret plans
	penv := envProvider()
	eplan := sandbox.CredentialPlan{Provider: penv, Issued: sandbox.IssuedCredential{Provider: "env"}}
	plans = []sandbox.CredentialPlan{eplan}
	originalTTL := eplan.TokenTTL
	sandbox.RaiseTokenTTL(plans, 17*time.Minute)
	if plans[0].TokenTTL != originalTTL {
		t.Fatalf("env-secret TokenTTL changed from %v to %v", originalTTL, plans[0].TokenTTL)
	}
}

func TestTokenTTLObservedByReactor(t *testing.T) {
	ctx := context.Background()
	var ttl int64
	kube := tokenClient("tok-test", &ttl)
	plans, _ := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	sandbox.RaiseTokenTTL(plans, 17*time.Minute)
	_, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != int64((17 * time.Minute).Seconds()) {
		t.Fatalf("reactor observed ExpirationSeconds = %d, want %d", ttl, int64((17 * time.Minute).Seconds()))
	}
}

// R3: Missing security tests
func TestUnknownProviderType(t *testing.T) {
	// A Provider with an unknown type returns ErrUnsupportedProvider
	p := &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "bad"}}
	p.Spec.Type = lastv1alpha1.ProviderType("unknown")
	_, err := sandbox.PlanCredentials(context.Background(), providers(p), gatewayNS, []string{"bad"})
	if !errors.Is(err, sandbox.ErrUnsupportedProvider) {
		t.Fatalf("err = %v, want ErrUnsupportedProvider", err)
	}
}

func TestTokenRequestValidation(t *testing.T) {
	// The TokenRequest reactor asserts namespace and ServiceAccount name
	ctx := context.Background()
	kube := fake.NewClientset()
	var gotNamespace string
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenKey {
			return false, nil, nil
		}
		gotNamespace = a.GetNamespace()
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = "test-token"
		return true, tr, nil
	})
	plans, _ := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	_, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err != nil {
		t.Fatal(err)
	}
	if gotNamespace != gatewayNS {
		t.Fatalf("TokenRequest namespace = %q, want %q", gotNamespace, gatewayNS)
	}
}

func TestFilePathRejections(t *testing.T) {
	tests := []struct {
		path string
	}{
		{"/work/../etc/x"},
		{"relative/x"},
		{"/work"},
	}
	for _, test := range tests {
		p := envProvider()
		p.Spec.Files = map[string]string{configKey: test.path}
		_, err := sandbox.PlanCredentials(context.Background(), providers(p), gatewayNS, []string{demoProvider})
		if err == nil {
			t.Errorf("path %q: expected error", test.path)
		}
	}
}

func TestKubernetesMountsAndVolumes(t *testing.T) {
	ctx := context.Background()
	kube := tokenClient("tok-test", nil)
	plans, _ := sandbox.PlanCredentials(ctx, providers(k8sProvider(clusterName)), gatewayNS, []string{clusterName})
	m, _ := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if !m.NeedsAPIServer {
		t.Fatal("NeedsAPIServer should be true")
	}
	// Check CA volume comes from ConfigMap kube-root-ca.crt
	found := false
	for _, vol := range m.Volumes {
		if vol.Name == "kube-ca" && vol.ConfigMap != nil && vol.ConfigMap.Name == "kube-root-ca.crt" {
			found = true
		}
	}
	if !found {
		t.Fatal("CA ConfigMap volume not found")
	}
	// Check mount is read-only
	found = false
	for _, mount := range m.Mounts {
		if mount.Name == "kube-ca" && mount.MountPath == sandbox.KubeCAMountPath && mount.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("CA mount not read-only at correct path")
	}
}

func TestTokenMintErrorPath(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, nil, fmt.Errorf("token creation failed")
	})
	plans, _ := sandbox.PlanCredentials(ctx, providers(k8sProvider("cluster")), gatewayNS, []string{"cluster"})
	_, err := sandbox.Materialize(ctx, kube, credCfg, runID, plans)
	if err == nil {
		t.Fatal("expected error")
	}
	errStr := err.Error()
	// Error must not contain token or secret values
	if strings.Contains(errStr, secretValue) {
		t.Fatal("error contains secret value")
	}
}

// R4: Duplicate file paths
func TestDuplicateFilePaths(t *testing.T) {
	p := envProvider()
	p.Spec.Files = map[string]string{
		"key1": "/work/file",
		"key2": "/work/./file", // Same cleaned path
	}
	_, err := sandbox.PlanCredentials(context.Background(), providers(p), gatewayNS, []string{demoProvider})
	if err == nil {
		t.Fatal("should reject duplicate file paths")
	}
}

// R5: Materialize defense in depth
func TestMaterializeWithBadPlan(t *testing.T) {
	// Plan with nil Provider
	badPlan := sandbox.CredentialPlan{Provider: nil}
	_, err := sandbox.Materialize(context.Background(), fake.NewClientset(), credCfg, runID, []sandbox.CredentialPlan{badPlan})
	if err == nil || !strings.Contains(err.Error(), "nil provider") {
		t.Fatalf("nil provider: err = %v", err)
	}

	// Env-secret plan with nil SecretRef
	p := envProvider()
	p.Spec.SecretRef = nil
	badPlan = sandbox.CredentialPlan{Provider: p, Issued: sandbox.IssuedCredential{}}
	_, err = sandbox.Materialize(context.Background(), fake.NewClientset(), credCfg, runID, []sandbox.CredentialPlan{badPlan})
	if err == nil || !strings.Contains(err.Error(), "nil secretRef") {
		t.Fatalf("nil secretRef: err = %v", err)
	}

	// Kubernetes plan with nil Kubernetes
	p = k8sProvider(clusterName)
	p.Spec.Kubernetes = nil
	badPlan = sandbox.CredentialPlan{Provider: p, Issued: sandbox.IssuedCredential{}}
	_, err = sandbox.Materialize(context.Background(), fake.NewClientset(), credCfg, runID, []sandbox.CredentialPlan{badPlan})
	if err == nil || !strings.Contains(err.Error(), "nil kubernetes") {
		t.Fatalf("nil kubernetes: err = %v", err)
	}
}

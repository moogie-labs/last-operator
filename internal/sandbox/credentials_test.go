package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

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

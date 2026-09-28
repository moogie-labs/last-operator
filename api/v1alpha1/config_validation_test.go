package v1alpha1_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const pinnedImage = "ghcr.io/moogie-labs/last-sandbox-base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestIntentPolicyAcceptsFullSpec(t *testing.T) {
	allow := true
	obj := &lastv1alpha1.IntentPolicy{ObjectMeta: meta(), Spec: lastv1alpha1.IntentPolicySpec{
		Modes:          &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeManual, Create: lastv1alpha1.ModeManual},
		AllowAutoModes: &lastv1alpha1.StageFlags{Reconcile: true},
		AllowScripts:   &allow,
		Providers:      []string{"cluster-team-x"},
		Egress:         &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}},
		Probes:         &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: 5 * time.Minute}, FastInterval: &metav1.Duration{Duration: time.Minute}},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestOperatorConfigMustBeNamedDefault(t *testing.T) {
	bad := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	expectInvalid(t, k8sClient.Create(ctx, bad), "OperatorConfig must be named 'default'")

	good := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: nameDefault}}
	if err := k8sClient.Create(ctx, good); err != nil {
		t.Fatalf("create default: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, good) })
}

func TestOperatorConfigRequiresDigestPinnedBaseImage(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: nameDefault}, Spec: lastv1alpha1.OperatorConfigSpec{
		Sandbox: &lastv1alpha1.SandboxConfig{BaseImage: "ghcr.io/moogie-labs/last-sandbox-base:latest"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.sandbox.baseImage")
}

func TestOperatorConfigExternalRegistryRequiresDetails(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: nameDefault}, Spec: lastv1alpha1.OperatorConfigSpec{
		Registry: &lastv1alpha1.RegistryConfig{Mode: lastv1alpha1.RegistryExternal},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "external is required when mode is external")
}

func TestOperatorConfigDefaults(t *testing.T) {
	obj := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: nameDefault}, Spec: lastv1alpha1.OperatorConfigSpec{
		Sandbox:  &lastv1alpha1.SandboxConfig{BaseImage: pinnedImage},
		Registry: &lastv1alpha1.RegistryConfig{},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	got := &lastv1alpha1.OperatorConfig{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Sandbox.MaxConcurrent != 20 {
		t.Errorf("maxConcurrent = %d, want 20", got.Spec.Sandbox.MaxConcurrent)
	}
	if got.Spec.Registry.Mode != lastv1alpha1.RegistryBundled {
		t.Errorf("registry mode = %q, want bundled", got.Spec.Registry.Mode)
	}
}

func TestProviderTypeRequiresMatchingFields(t *testing.T) {
	k8s := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{Type: lastv1alpha1.ProviderKubernetes}}
	expectInvalid(t, k8sClient.Create(ctx, k8s), "kubernetes is required when type is kubernetes")

	env := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{Type: lastv1alpha1.ProviderEnvSecret}}
	expectInvalid(t, k8sClient.Create(ctx, env), "secretRef is required when type is env-secret")

	ok := &lastv1alpha1.Provider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.ProviderSpec{
		Type:       lastv1alpha1.ProviderKubernetes,
		Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "team-x-deployer"}},
	}}
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })
}

func TestLLMProviderOpenAICompatibleRequiresEndpoint(t *testing.T) {
	obj := &lastv1alpha1.LLMProvider{ObjectMeta: clusterMeta(), Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMOpenAICompatible, Model: "llama", SecretRef: lastv1alpha1.LocalRef{Name: "key"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "endpoint is required for openai-compatible")
}

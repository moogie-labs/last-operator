package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var (
	k8sClient client.Client
	ctx       = context.Background()
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\nset KUBEBUILDER_ASSETS: export KUBEBUILDER_ASSETS=\"$(make -s envtest-assets)\"\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := lastv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func meta() metav1.ObjectMeta        { return metav1.ObjectMeta{GenerateName: "t-", Namespace: "default"} }
func clusterMeta() metav1.ObjectMeta { return metav1.ObjectMeta{GenerateName: "t-"} }

// expectInvalid asserts err is an API "Invalid" error whose message contains substr.
func expectInvalid(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error containing %q, got nil", substr)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got: %v", err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}

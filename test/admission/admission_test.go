// Package admission_test checks the chart's admission policies against a real API server.
package admission_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	operatorUser = "system:serviceaccount:last-system:last-operator"
	approvalsNS  = "approvals"
	approverName = "approver"
)

var (
	adminCfg *rest.Config
	scheme   = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = lastv1alpha1.AddToScheme(scheme)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\n"+
			"set KUBEBUILDER_ASSETS: export KUBEBUILDER_ASSETS=\"$(make -s envtest-assets)\"\n", err)
		os.Exit(1)
	}
	adminCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func clientAs(t *testing.T, user string, groups ...string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(adminCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: append(groups, "system:authenticated")}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func applyChartPolicy(t *testing.T, admin client.Client) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "last-operator", "files", "intentapproval-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatal(err)
		}
		if err := admin.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func grantApprovers(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: approverName}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"last.moogie-labs.com"}, Resources: []string{"intentapprovals"},
		Verbs: []string{"create", "get", "update", "patch"}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: approverName},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: approverName},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"},
			{Kind: rbacv1.ServiceAccountKind, Name: "last-operator", Namespace: "last-system"},
		}}
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: approvalsNS}}} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}

func approval(name, by string) *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: approvalsNS},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: "sha256:" + strings.Repeat("a", 64), Decision: lastv1alpha1.DecisionApprove, ApprovedBy: by}}
}

func TestIntentApprovalAdmission(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	grantApprovers(t, admin)
	applyChartPolicy(t, admin)
	alice := clientAs(t, "alice")
	operator := clientAs(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:last-system")

	// Admission policies take effect asynchronously; wait for enforcement.
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		err := alice.Create(ctx, approval(fmt.Sprintf("warmup-%d", i), "bob"))
		if err != nil && strings.Contains(err.Error(), "must be the requesting user") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy never took effect: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := alice.Create(ctx, approval("ok", "alice")); err != nil {
		t.Fatalf("alice approving as herself: %v", err)
	}
	err = alice.Create(ctx, approval("spoof", "bob"))
	if err == nil || !strings.Contains(err.Error(), "must be the requesting user") {
		t.Fatalf("spoofed approvedBy = %v", err)
	}
	err = operator.Create(ctx, approval("self", operatorUser))
	if err == nil || !strings.Contains(err.Error(), "operator may not") {
		t.Fatalf("operator approval = %v", err)
	}
	var got lastv1alpha1.IntentApproval
	if err := alice.Get(ctx, client.ObjectKey{Namespace: approvalsNS, Name: "ok"}, &got); err != nil {
		t.Fatal(err)
	}
	got.Labels = map[string]string{"tampered": "yes"}
	err = alice.Update(ctx, &got)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update = %v, want immutable", err)
	}
}

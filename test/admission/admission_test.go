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

	authv1 "k8s.io/api/authentication/v1"
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
	operatorSA   = "last-operator"
	systemNS     = "last-system"
	operatorUser = "system:serviceaccount:last-system:last-operator"
	approvalsNS  = "approvals"
	approverName = "approver"
	verbCreate   = "create"
	gatewayName  = "last-gateway" // the gateway namespace and its ServiceAccount

	gatewayNamespaceUser = "system:serviceaccount:last-gateway:sneaky"
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

func applyChartPolicy(t *testing.T, admin client.Client, file string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "last-operator", "files", file))
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
		if err := admin.Create(context.Background(), obj); client.IgnoreAlreadyExists(err) != nil {
			t.Fatal(err)
		}
	}
}

func grantApprovers(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: approverName}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"last.moogie-labs.com"}, Resources: []string{"intentapprovals"},
		Verbs: []string{verbCreate, verbGet, verbUpdate, verbPatch}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: approverName},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: approverName},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"},
			{Kind: rbacv1.ServiceAccountKind, Name: operatorSA, Namespace: systemNS},
			{Kind: rbacv1.ServiceAccountKind, Name: "sneaky", Namespace: gatewayName},
		}}
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: approvalsNS}}} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}

func approval(name, by string) *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: approvalsNS},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, IntentUID: "uid-web",
			Stage:       lastv1alpha1.StageCreate,
			SubjectHash: "sha256:" + strings.Repeat("a", 64), Decision: lastv1alpha1.DecisionApprove, ApprovedBy: by}}
}

func TestIntentApprovalAdmission(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	grantApprovers(t, admin)
	applyChartPolicy(t, admin, "intentapproval-policy.yaml")
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
	gwSA := clientAs(t, gatewayNamespaceUser, "system:serviceaccounts", "system:serviceaccounts:last-gateway")
	err = gwSA.Create(ctx, approval("gw-sa", gatewayNamespaceUser))
	if err == nil || !strings.Contains(err.Error(), "gateway namespace may not approve") {
		t.Fatalf("gateway-namespace ServiceAccount approval = %v", err)
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

func TestGatewayAuditGuard(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns = gatewayName
	const gwUser = "system:serviceaccount:last-gateway:last-gateway"
	const otherUser = "system:serviceaccount:last-gateway:other"
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "audit-guard-broad"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"},
		Verbs: []string{"get", verbCreate, "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{verbCreate}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "audit-guard-broad"},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: gatewayName, Namespace: ns},
			{Kind: rbacv1.ServiceAccountKind, Name: "other", Namespace: ns},
		}}
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{"k": "v"}}
	}
	cmap := func(name string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string]string{"k": "v"}}
	}
	for _, o := range []client.Object{secret("last-audit-signing"), secret("last-audit-writer"), cmap("last-audit-config"),
		secret("last-run-x-anchor"), cmap("last-audit-pubkey")} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	applyChartPolicy(t, admin, "gateway-audit-guard-policy.yaml")
	gw := clientAs(t, gwUser, "system:serviceaccounts", "system:serviceaccounts:last-gateway")
	other := clientAs(t, otherUser, "system:serviceaccounts", "system:serviceaccounts:last-gateway")

	// Admission policies take effect asynchronously; wait for enforcement.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := gw.Delete(ctx, secret("last-audit-signing"))
		if err != nil && strings.Contains(err.Error(), "may not change or delete") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy never took effect: %v", err)
		}
		if err == nil { // deleted before enforcement started; restore it
			if err := admin.Create(ctx, secret("last-audit-signing")); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	denied := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "may not change or delete") {
			t.Errorf("%s = %v, want a policy denial", what, err)
		}
	}
	guarded := []client.Object{secret("last-audit-signing"), secret("last-audit-writer"), cmap("last-audit-config")}
	for _, o := range guarded {
		denied("delete "+o.GetName(), gw.Delete(ctx, o))
		cur := o.DeepCopyObject().(client.Object)
		if err := admin.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
			t.Fatal(err)
		}
		cur.SetLabels(map[string]string{"tampered": "yes"})
		denied("update "+o.GetName(), gw.Update(ctx, cur))
	}

	if err := gw.Delete(ctx, secret("last-run-x-anchor")); err != nil {
		t.Errorf("deleting a run anchor: %v", err)
	}
	if err := gw.Create(ctx, secret("last-run-y-anchor")); err != nil {
		t.Errorf("creating a Secret: %v", err)
	}
	pub := &corev1.ConfigMap{}
	if err := gw.Get(ctx, client.ObjectKey{Namespace: ns, Name: "last-audit-pubkey"}, pub); err != nil {
		t.Fatal(err)
	}
	pub.Data["fingerprint"] = "updated"
	if err := gw.Update(ctx, pub); err != nil {
		t.Errorf("updating the public key ConfigMap: %v", err)
	}

	if err := other.Delete(ctx, secret("last-audit-writer")); err != nil {
		t.Errorf("another ServiceAccount deleting last-audit-writer: %v", err)
	}

	// The gateway may mint tokens for provider ServiceAccounts, never for its own identity.
	for _, name := range []string{gatewayName, "deployer"} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if err := admin.Create(ctx, sa); err != nil {
			t.Fatal(err)
		}
	}
	mint := func(c client.Client, name string) error {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		return c.SubResource("token").Create(ctx, sa, &authv1.TokenRequest{})
	}
	if err := mint(gw, gatewayName); err == nil || !strings.Contains(err.Error(), "may not mint") {
		t.Errorf("gateway minting its own token = %v, want a policy denial", err)
	}
	if err := mint(gw, "deployer"); err != nil {
		t.Errorf("gateway minting a token for another ServiceAccount: %v", err)
	}
	if err := mint(other, gatewayName); err != nil {
		t.Errorf("another ServiceAccount minting a last-gateway token: %v", err)
	}
}

// TestOperatorIntentPatches checks intent-operator-policy.yaml: the operator may
// add and remove its teardown finalizer on an Intent, and change nothing else.
func TestOperatorIntentPatches(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns, name, request = "intent-patches", "intent-patcher", "run web"
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{lastv1alpha1.GroupVersion.Group}, Resources: []string{"intents"},
		Verbs: []string{verbGet, verbPatch, verbUpdate}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "last-operator",
			Namespace: strings.Split(operatorUser, ":")[2]}}}
	intent := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: lastv1alpha1.IntentSpec{Request: request}}
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, intent} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	applyChartPolicy(t, admin, "intent-operator-policy.yaml")
	operator := clientAs(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:last-system")

	// patch applies change to a fresh copy of the Intent as the operator.
	patch := func(change func(*lastv1alpha1.Intent)) error {
		var cur lastv1alpha1.Intent
		if err := admin.Get(ctx, client.ObjectKeyFromObject(intent), &cur); err != nil {
			t.Fatal(err)
		}
		base := cur.DeepCopy()
		change(&cur)
		return operator.Patch(ctx, &cur, client.MergeFrom(base))
	}
	editSpec := func(in *lastv1alpha1.Intent) { in.Spec.Request = "run something else" }

	// Admission policies take effect asynchronously; wait for enforcement.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := patch(editSpec)
		if err != nil && strings.Contains(err.Error(), "may not change an Intent's spec") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy never took effect: %v", err)
		}
		if err == nil { // applied before enforcement started; restore it
			if err := patch(func(in *lastv1alpha1.Intent) { in.Spec.Request = request }); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := patch(func(in *lastv1alpha1.Intent) {
		in.Finalizers = append(in.Finalizers, lastv1alpha1.FinalizerTeardown)
	}); err != nil {
		t.Fatalf("adding the teardown finalizer: %v", err)
	}
	for _, c := range []struct {
		what, refusal string
		change        func(*lastv1alpha1.Intent)
	}{
		{"spec", "may not change an Intent's spec", editSpec},
		{"the retry annotation", "may not change an Intent's annotations", func(in *lastv1alpha1.Intent) {
			in.Annotations = map[string]string{lastv1alpha1.AnnotationRetry: "1"}
		}},
		{"a label", "may not change an Intent's labels", func(in *lastv1alpha1.Intent) {
			in.Labels = map[string]string{"edited": "by-the-operator"}
		}},
		{"another finalizer", "may change only the last.moogie-labs.com/teardown finalizer", func(in *lastv1alpha1.Intent) {
			in.Finalizers = append(in.Finalizers, "example.com/other")
		}},
		{"ownerReferences", "may not change an Intent's ownerReferences", func(in *lastv1alpha1.Intent) {
			in.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "uid-owner"}}
		}},
	} {
		if err := patch(c.change); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("changing %s = %v, want %q", c.what, err, c.refusal)
		}
	}
	if err := patch(func(in *lastv1alpha1.Intent) { in.Finalizers = nil }); err != nil {
		t.Fatalf("removing the teardown finalizer: %v", err)
	}
	var got lastv1alpha1.Intent
	if err := admin.Get(ctx, client.ObjectKeyFromObject(intent), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Request != request || len(got.Finalizers) != 0 || len(got.Annotations) != 0 {
		t.Fatalf("intent after the patches = %+v", got.ObjectMeta)
	}
}

// TestRuleWritesNeedAHuman checks that allow rules are written only by humans (spec
// §4.1): the operator and the gateway namespace's ServiceAccounts are refused on
// create and update of both kinds, and a plain user is allowed.
func TestRuleWritesNeedAHuman(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	// RBAC lets all three principals write, so only admission can refuse.
	const name = "rule-writer"
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{lastv1alpha1.GroupVersion.Group}, Resources: []string{"commandrules", "clustercommandrules"},
		Verbs: []string{verbCreate, verbGet, verbUpdate}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: name},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"},
			{Kind: rbacv1.ServiceAccountKind, Name: operatorSA, Namespace: systemNS},
			{Kind: rbacv1.ServiceAccountKind, Name: "sneaky", Namespace: gatewayName},
		}}
	const ns = "rules-ns"
	for _, o := range []client.Object{role, binding, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	applyChartPolicy(t, admin, "rules-policy.yaml")

	spec := lastv1alpha1.CommandRuleSpec{
		Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
		Providers: []string{provProd},
		Match:     lastv1alpha1.ArgvPattern{Argv: ec2Argv()}}
	newRule := func(kind, n string) client.Object {
		if kind == "ClusterCommandRule" {
			return &lastv1alpha1.ClusterCommandRule{ObjectMeta: metav1.ObjectMeta{Name: n}, Spec: spec}
		}
		return &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}, Spec: spec}
	}
	touch := func(o client.Object) {
		o.SetLabels(map[string]string{labelKey: valTrue})
	}

	alice := clientAs(t, "alice")
	operator := clientAs(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:last-system")
	gw := clientAs(t, gatewayNamespaceUser, "system:serviceaccounts", "system:serviceaccounts:last-gateway")
	refused := []struct {
		who, want string
		c         client.Client
	}{
		{"operator", "operator may not write allow rules", operator},
		{"gateway", "gateway namespace may not write allow rules", gw},
	}

	for _, kind := range []string{"CommandRule", "ClusterCommandRule"} {
		// Admission policies take effect asynchronously; wait for enforcement.
		deadline := time.Now().Add(30 * time.Second)
		for i := 0; ; i++ {
			err := operator.Create(ctx, newRule(kind, fmt.Sprintf("%s-warmup-%d", strings.ToLower(kind), i)))
			if err != nil && strings.Contains(err.Error(), "may not write allow rules") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the policy never took effect: %v", kind, err)
			}
			time.Sleep(200 * time.Millisecond)
		}

		base := strings.ToLower(kind)
		for _, r := range refused {
			err := r.c.Create(ctx, newRule(kind, base+"-"+r.who))
			if err == nil || !strings.Contains(err.Error(), r.want) {
				t.Errorf("%s create by %s = %v, want %q", kind, r.who, err, r.want)
			}
		}
		mine := newRule(kind, base+"-alice")
		if err := alice.Create(ctx, mine); err != nil {
			t.Fatalf("%s create by a plain user: %v", kind, err)
		}
		for _, r := range refused {
			cur := newRule(kind, base+"-alice")
			if err := admin.Get(ctx, client.ObjectKeyFromObject(mine), cur); err != nil {
				t.Fatal(err)
			}
			touch(cur)
			err := r.c.Update(ctx, cur)
			if err == nil || !strings.Contains(err.Error(), r.want) {
				t.Errorf("%s update by %s = %v, want %q", kind, r.who, err, r.want)
			}
		}
		cur := newRule(kind, base+"-alice")
		if err := admin.Get(ctx, client.ObjectKeyFromObject(mine), cur); err != nil {
			t.Fatal(err)
		}
		touch(cur)
		if err := alice.Update(ctx, cur); err != nil {
			t.Errorf("%s update by a plain user: %v", kind, err)
		}
	}
}

func ec2Argv() []string { return []string{awsBin, "ec2", ellipsis} }

const (
	userAlice  = "alice"
	userBob    = "bob"
	userSneaky = "sneaky"
	provProd   = "aws-prod"
	labelKey   = "touched"
	ellipsis   = "..."
	awsBin     = "aws"
	valTrue    = "true"
	mustBeUser = "must be the requesting user"
)

// waitForPolicy retries create until err mentions want, since admission policies take
// effect asynchronously.
func waitForPolicy(t *testing.T, what, want string, create func(i int) error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		err := create(i)
		if err != nil && strings.Contains(err.Error(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the policy never took effect: %v", what, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// expectErr checks err mentions want, or is nil when want is empty.
func expectErr(t *testing.T, what string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: %v, want allowed", what, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s = %v, want %q", what, err, want)
	}
}

func grantWriters(t *testing.T, admin client.Client, name, resource string, namespaces ...string) {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{lastv1alpha1.GroupVersion.Group}, Resources: []string{resource},
		Verbs: []string{verbCreate, verbGet, verbUpdate}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: name},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: userAlice},
			{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: userBob},
			{Kind: rbacv1.ServiceAccountKind, Name: operatorSA, Namespace: systemNS},
			{Kind: rbacv1.ServiceAccountKind, Name: userSneaky, Namespace: gatewayName},
		}}
	objs := make([]client.Object, 0, 2+len(namespaces))
	objs = append(objs, role, binding)
	for _, ns := range namespaces {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	for _, o := range objs {
		if err := admin.Create(ctx, o); client.IgnoreAlreadyExists(err) != nil {
			t.Fatal(err)
		}
	}
}

func suggestionIn(ns, n string,
	change func(*lastv1alpha1.CommandRuleSuggestionSpec)) *lastv1alpha1.CommandRuleSuggestion {
	s := &lastv1alpha1.CommandRuleSuggestion{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
		Spec: lastv1alpha1.CommandRuleSuggestionSpec{
			TargetScope: lastv1alpha1.RuleScopeNamespace, Trigger: lastv1alpha1.TriggerPlanTime, Rationale: "r",
			Proposed: lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
				Providers: []string{provProd}, Match: lastv1alpha1.ArgvPattern{Argv: ec2Argv()}}}}
	if change != nil {
		change(&s.Spec)
	}
	return s
}

// decidedAs makes a change that sets the decision to Denied, by the named user.
func decidedAs(by string) func(*lastv1alpha1.CommandRuleSuggestionSpec) {
	return func(s *lastv1alpha1.CommandRuleSuggestionSpec) {
		s.Decision, s.DecidedBy = lastv1alpha1.SuggestionDenied, by
	}
}

// TestSuggestionWritesFollowS1 checks the last-suggestions policy (M7 design S1).
func TestSuggestionWritesFollowS1(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "sugg-ns"
	grantWriters(t, admin, "suggestion-writer", "commandrulesuggestions", ns, systemNS)
	applyChartPolicy(t, admin, "suggestions-policy.yaml")

	alice := clientAs(t, userAlice)
	bob := clientAs(t, userBob)
	operator := clientAs(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:last-system")
	gw := clientAs(t, gatewayNamespaceUser, "system:serviceaccounts", "system:serviceaccounts:last-gateway")
	waitForPolicy(t, "suggestions", "gateway namespace may not write suggestions", func(i int) error {
		return gw.Create(ctx, suggestionIn(ns, fmt.Sprintf("warmup-%d", i), nil))
	})

	const onlyPending = "may only create Pending suggestions"
	expectErr(t, "operator creates Pending", operator.Create(ctx, suggestionIn(ns, "op-pending", nil)), "")
	expectErr(t, "operator creates Denied",
		operator.Create(ctx, suggestionIn(ns, "op-denied", decidedAs(operatorUser))), onlyPending)
	expectErr(t, "operator creates Denied as someone",
		operator.Create(ctx, suggestionIn(ns, "op-denied2", decidedAs(userAlice))), onlyPending)
	expectErr(t, "operator sets decidedBy", operator.Create(ctx, suggestionIn(ns, "op-by",
		func(s *lastv1alpha1.CommandRuleSuggestionSpec) { s.DecidedBy = userAlice })), onlyPending)
	expectErr(t, "operator sets proposed.source", operator.Create(ctx, suggestionIn(ns, "op-src",
		func(s *lastv1alpha1.CommandRuleSuggestionSpec) {
			s.Proposed.Source = &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginApproval, CreatedBy: userAlice}
		})), onlyPending)

	get := func(n string) *lastv1alpha1.CommandRuleSuggestion {
		cur := &lastv1alpha1.CommandRuleSuggestion{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: n}, cur); err != nil {
			t.Fatal(err)
		}
		return cur
	}
	cur := get("op-pending")
	cur.Labels = map[string]string{labelKey: valTrue}
	expectErr(t, "operator updates", operator.Update(ctx, cur), "operator may not update suggestions")
	expectErr(t, "gateway creates", gw.Create(ctx, suggestionIn(ns, "gw", nil)),
		"gateway namespace may not write suggestions")

	toCluster := func(s *lastv1alpha1.CommandRuleSuggestionSpec) { s.TargetScope = lastv1alpha1.RuleScopeCluster }
	expectErr(t, "Cluster target in a team namespace", operator.Create(ctx, suggestionIn(ns, "cluster-x", toCluster)),
		"must be in last-system")
	expectErr(t, "Cluster target in last-system",
		operator.Create(ctx, suggestionIn(systemNS, "cluster-ok", toCluster)), "")

	// A user decides: decidedBy must be the user.
	decide := func(n, by string, c client.Client) error {
		cur := get(n)
		cur.Spec.Decision, cur.Spec.DecidedBy = lastv1alpha1.SuggestionDenied, by
		return c.Update(ctx, cur)
	}
	expectErr(t, "user decides as someone else", decide("op-pending", userBob, alice), mustBeUser)
	expectErr(t, "user decides as themselves", decide("op-pending", userAlice, alice), "")
	expectErr(t, "user creates decided as themselves",
		alice.Create(ctx, suggestionIn(ns, "by-alice", decidedAs(userAlice))), "")
	expectErr(t, "user creates decided as someone else",
		alice.Create(ctx, suggestionIn(ns, "by-fake", decidedAs(userBob))), mustBeUser)

	// Another user changes a label on what alice decided.
	cur = get("op-pending")
	cur.Labels = map[string]string{labelKey: valTrue}
	expectErr(t, "label change on a suggestion another user decided", bob.Update(ctx, cur), "")
}

// TestRuleSourceNamesItsAuthor checks the S4 additions to the last-rules policy.
func TestRuleSourceNamesItsAuthor(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "source-ns"
	grantWriters(t, admin, "rule-source-writer", "commandrules", ns)
	applyChartPolicy(t, admin, "rules-policy.yaml")
	alice := clientAs(t, userAlice)

	newRule := func(n string, src *lastv1alpha1.RuleSource) *lastv1alpha1.CommandRule {
		return &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns},
			Spec: lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
				Providers: []string{provProd}, Match: lastv1alpha1.ArgvPattern{Argv: ec2Argv()},
				Source: src}}
	}
	source := func(from lastv1alpha1.RuleOrigin, by string) *lastv1alpha1.RuleSource {
		return &lastv1alpha1.RuleSource{CreatedFrom: from, CreatedBy: by}
	}
	approval := func(by string) *lastv1alpha1.RuleSource { return source(lastv1alpha1.RuleOriginApproval, by) }
	waitForPolicy(t, "rules", "spec.source.createdBy "+mustBeUser, func(i int) error {
		return alice.Create(ctx, newRule(fmt.Sprintf("warmup-%d", i), approval(userBob)))
	})

	const wrongAuthor = "createdBy " + mustBeUser
	expectErr(t, "create as someone else", alice.Create(ctx, newRule("other", approval(userBob))), wrongAuthor)
	expectErr(t, "create with no createdBy", alice.Create(ctx, newRule("anon", approval(""))), wrongAuthor)
	expectErr(t, "create as yourself", alice.Create(ctx, newRule("mine", approval(userAlice))), "")
	expectErr(t, "create without source", alice.Create(ctx, newRule("plain", nil)), "")
	expectErr(t, "suggestion without a ref",
		alice.Create(ctx, newRule("no-ref", source(lastv1alpha1.RuleOriginSuggestion, userAlice))),
		"suggestionRef is required")
	withRef := source(lastv1alpha1.RuleOriginSuggestion, userAlice)
	withRef.SuggestionRef = &lastv1alpha1.LocalRef{Name: "sugg-1"}
	expectErr(t, "suggestion with a ref", alice.Create(ctx, newRule("with-ref", withRef)), "")

	get := func(n string) *lastv1alpha1.CommandRule {
		r := &lastv1alpha1.CommandRule{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: n}, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	cur := get("mine")
	cur.Spec.Source.CreatedBy = userBob
	expectErr(t, "update changes source", alice.Update(ctx, cur), "spec.source cannot be changed")
	cur = get("mine")
	cur.Spec.Source = nil
	expectErr(t, "update removes source", alice.Update(ctx, cur), "spec.source cannot be changed")
	cur = get("plain")
	cur.Spec.Source = approval(userAlice)
	expectErr(t, "update adds source", alice.Update(ctx, cur), "spec.source cannot be changed")
	cur = get("mine")
	cur.Labels = map[string]string{labelKey: valTrue}
	expectErr(t, "update keeps source", alice.Update(ctx, cur), "")
}

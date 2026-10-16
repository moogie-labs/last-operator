package admission_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	verbGet         = "get"
	verbUpdate      = "update"
	verbPatch       = "patch"
	kindClusterRole = "ClusterRole"
)

// TestOperatorRBAC checks the chart's operator ClusterRole (spec §4.1, §17.1).
// The untrusted operator reads approvals and policy, writes only its own CRs,
// and touches nothing else. The role is bound to a fresh ServiceAccount, so
// grants made by other tests in this package cannot leak in.
func TestOperatorRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "last-operator", "files", "operator-clusterrole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	const sa = "rbac-check"
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "operator-rbac-check"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: "last-system"}}}
	for _, o := range []client.Object{&role, binding} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	kube := kubernetes.NewForConfigOrDie(adminCfg)
	can := func(a authzv1.ResourceAttributes) bool {
		t.Helper()
		review := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:last-system:" + sa,
			Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:last-system", "system:authenticated"},
			ResourceAttributes: &a}}
		res, err := kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Status.Allowed
	}
	attr := func(group, ns, resource, sub, verb string) authzv1.ResourceAttributes {
		return authzv1.ResourceAttributes{Group: group, Namespace: ns, Resource: resource, Subresource: sub, Verb: verb}
	}
	last := lastv1alpha1.GroupVersion.Group

	allowed := []authzv1.ResourceAttributes{
		attr(last, "", "intents", "", "watch"),
		attr(last, "team-x", "intents", "status", verbUpdate),
		attr(last, "team-x", "managedresources", "", "create"),
		attr(last, "team-x", "managedresources", "status", verbUpdate),
		attr(last, "", "intentapprovals", "", "list"),
		attr(last, "", "intentpolicies", "", "list"),
		attr(last, "", "operatorconfigs", "", "get"),
		attr(last, "", "providers", "", "list"),
		attr(last, "", "providers", "", "watch"),
		attr("events.k8s.io", "team-x", "events", "", "create"),
	}
	// The binding reaches the authorizer through an informer; wait until it applies.
	deadline := time.Now().Add(30 * time.Second)
	for !can(allowed[0]) {
		if time.Now().After(deadline) {
			t.Fatal("the operator role never took effect")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, a := range allowed {
		if !can(a) {
			t.Errorf("operator cannot %s %s/%s in %q", a.Verb, a.Resource, a.Subresource, a.Namespace)
		}
	}

	denied := make([]authzv1.ResourceAttributes, 0, 100)
	for _, ns := range []string{"", "team-x", "last-gateway", "last-sandbox"} {
		for _, verb := range []string{verbGet, "list", "watch", verbCreate, verbUpdate} {
			denied = append(denied, attr("", ns, "secrets", "", verb))
		}
	}
	denied = append(denied,
		attr("", "last-gateway", "configmaps", "", verbGet),
		attr("", "last-gateway", "configmaps", "", "list"),
		attr("", "last-sandbox", "pods", "", verbGet),
		attr("", "last-sandbox", "pods", "", "list"),
		attr("", "last-sandbox", "pods", "log", verbGet),
		attr("", "last-sandbox", "pods", "exec", "create"),
		attr("", "", "users", "", "impersonate"),
		attr("", "", "serviceaccounts", "", "impersonate"),
		attr("", "", "serviceaccounts", "token", "create"),
		attr("", "team-x", "serviceaccounts", "token", "create"),
		attr("", "last-gateway", "serviceaccounts", "token", "create"),
		attr("networking.k8s.io", "last-sandbox", "networkpolicies", "", "create"),
		attr(last, "", "intents", "", "create"),
		attr(last, "team-x", "intents", "", verbUpdate),
		attr(last, "team-x", "intents", "", "delete"),
		attr(last, "team-x", "managedresources", "", "delete"),
		attr(last, "team-x", "managedresources", "", "deletecollection"),
	)
	for _, verb := range []string{verbCreate, verbUpdate, verbPatch, "delete"} {
		denied = append(denied, attr(last, "team-x", "intentapprovals", "", verb))
		for _, r := range []string{"intentpolicies", "commandrules"} {
			denied = append(denied, attr(last, "team-x", r, "", verb))
		}
		for _, r := range []string{"operatorconfigs", "providers", "llmproviders", "clustercommandrules"} {
			denied = append(denied, attr(last, "", r, "", verb))
		}
		for _, r := range []string{"validatingadmissionpolicies", "validatingadmissionpolicybindings"} {
			denied = append(denied, attr("admissionregistration.k8s.io", "", r, "", verb))
		}
	}
	for _, verb := range []string{"create", verbUpdate, "escalate", "bind"} {
		for _, r := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
			denied = append(denied, attr("rbac.authorization.k8s.io", "", r, "", verb))
		}
	}
	for _, a := range denied {
		if can(a) {
			t.Errorf("operator can %s %s %s/%s in %q", a.Verb, a.Group, a.Resource, a.Subresource, a.Namespace)
		}
	}
}

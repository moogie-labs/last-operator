//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func TestGatewayLifecycle(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	pauseOperator(t)
	ctx := context.Background()
	// A unique suffix keeps this run's objects, and so its audit entries, distinct from any earlier run.
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	lifecycleNS := "e2e-lifecycle-" + suffix
	lifecycleSA := "e2e-lc-" + suffix
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = lastv1alpha1.AddToScheme(sch)
	crc, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	// Registered before anything is created; it tolerates objects that were never created.
	t.Cleanup(func() {
		bg := context.Background()
		for _, o := range []client.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lifecycleNS}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA, Namespace: "last-gateway"}},
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}},
			&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}},
			&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}},
		} {
			if err := crc.Delete(bg, o); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup %T: %v", o, err)
			}
		}
		for range 5 {
			var cur lastv1alpha1.OperatorConfig
			if err := crc.Get(bg, client.ObjectKey{Name: "default"}, &cur); err != nil || cur.Spec.Defaults == nil {
				return
			}
			cur.Spec.Defaults.Providers = slices.DeleteFunc(cur.Spec.Defaults.Providers, func(n string) bool { return n == lifecycleSA })
			if err := crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not remove %s from OperatorConfig defaults", lifecycleSA)
	})

	// A namespace, a Provider that can only read that namespace, and a policy allowing the Provider.
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: lifecycleNS}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA, Namespace: "last-gateway"}})
	create(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{lifecycleNS}, Verbs: []string{"get"}}}})
	create(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: lifecycleSA},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: lifecycleSA, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: lifecycleSA}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: lifecycleSA}}}})
	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		op = lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		create(&op)
		if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if op.Spec.Defaults == nil {
		op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
	}
	if !slices.Contains(op.Spec.Defaults.Providers, lifecycleSA) {
		op.Spec.Defaults.Providers = append(op.Spec.Defaults.Providers, lifecycleSA)
		if err := crc.Update(ctx, &op); err != nil {
			t.Fatal(err)
		}
	}

	// The Intent and its plan. The plan is written as an admin would; the operator arrives in M2c.
	getNS := lastv1alpha1.Command{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespace", lifecycleNS, "-o", "json"}},
		Providers: []string{lifecycleSA}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}}
	echo := lastv1alpha1.Command{ID: "s2", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", "${steps.s1.json.metadata.name}"}},
		Timeout: &metav1.Duration{Duration: time.Minute}}
	resource := lastv1alpha1.ManagedResourceSpec{Description: "the e2e namespace",
		Probes: []lastv1alpha1.Probe{{ID: "ns-exists", Command: lastv1alpha1.Command{Run: getNS.Run, Providers: getNS.Providers},
			Assert: `result.exitCode == 0 && result.json.metadata.name == "` + lifecycleNS + `"`}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"namespace": "${steps.s1.json.metadata.name}"}}
	steps := []lastv1alpha1.Command{getNS, echo}
	hash, err := plan.Hash(plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{resource}})
	if err != nil {
		t.Fatal(err)
	}
	intent := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: lifecycleNS},
		Spec: lastv1alpha1.IntentSpec{Request: "e2e lifecycle"}}
	create(intent)
	intent.Status = lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingApproval, Stage: lastv1alpha1.StageCreate,
		Plan: &lastv1alpha1.Plan{Hash: hash, Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{resource}}}
	if err := crc.Status().Update(ctx, intent); err != nil {
		t.Fatal(err)
	}

	// Approve as ourselves: the admission policy requires approvedBy to be the creator.
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	create(&lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: "web-create", Namespace: lifecycleNS},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, IntentUID: string(intent.UID), Stage: lastv1alpha1.StageCreate,
			SubjectHash: hash, Decision: lastv1alpha1.DecisionApprove, ApprovedBy: review.Status.UserInfo.Username}})

	// Call the deployed gateway as the operator.
	conn, err := grpc.NewClient(portForward(t, "last-gateway", "svc/last-gateway", "9090"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	gw := gatewayv1.NewGatewayServiceClient(conn)
	token := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--audience", "last-gateway", "--duration", "30m")
	call := func() (context.Context, context.CancelFunc) {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		return metadata.AppendToOutgoingContext(c, "authorization", "Bearer "+token), cancel
	}
	ref := &gatewayv1.IntentRef{Namespace: lifecycleNS, Name: "web"}

	c, cancel := call()
	if _, err := gw.BeginPhase(c, &gatewayv1.BeginPhaseRequest{Intent: ref, Stage: "Create", SubjectHash: hash}); err != nil {
		t.Fatalf("BeginPhase: %v", err)
	}
	cancel()
	results := plan.Results{}
	for _, s := range steps {
		cmd, err := plan.Resolve(s, results)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		c, cancel := call()
		resp, err := gw.RunCommand(c, &gatewayv1.RunCommandRequest{Intent: ref, SubjectHash: hash, StepId: s.ID, CommandJson: raw})
		cancel()
		if err != nil || resp.GetResult().GetExitCode() != 0 {
			t.Fatalf("step %s: %v %+v", s.ID, err, resp.GetResult())
		}
		results[s.ID] = runnerapi.Result{ID: s.ID, Stdout: resp.GetResult().GetStdout()}
	}
	if got := strings.TrimSpace(results["s2"].Stdout); got != lifecycleNS {
		t.Fatalf("s2 printed %q, want the resolved namespace name", got)
	}

	// The operator (M2c) creates ManagedResources; here the test does, owned by the Intent.
	var current lastv1alpha1.Intent
	if err := crc.Get(ctx, client.ObjectKeyFromObject(intent), &current); err != nil {
		t.Fatal(err)
	}
	mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "web-ns", Namespace: lifecycleNS,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: "web", UID: current.UID, Controller: ptr.To(true)}}},
		Spec: resource}
	mr.Spec.IntentRef = &lastv1alpha1.LocalRef{Name: "web"}
	create(mr)
	c, cancel = call()
	if _, err := gw.WriteOutputSecret(c, &gatewayv1.WriteOutputSecretRequest{Intent: ref, SubjectHash: hash, ManagedResource: "web-ns",
		Values: map[string]string{"namespace": "${steps.s1.json.metadata.name}"}}); err != nil {
		t.Fatalf("WriteOutputSecret: %v", err)
	}
	cancel()
	if v := secretValue(t, lifecycleNS, "web-outputs", "namespace"); v != lifecycleNS {
		t.Fatalf("output secret = %q", v)
	}
	c, cancel = call()
	if _, err := gw.EndPhase(c, &gatewayv1.EndPhaseRequest{Intent: ref, SubjectHash: hash, Succeeded: true}); err != nil {
		t.Fatalf("EndPhase: %v", err)
	}
	cancel()
	c, cancel = call()
	probes, err := gw.RunProbes(c, &gatewayv1.RunProbesRequest{Intent: ref})
	cancel()
	if err != nil || len(probes.GetOutcomes()) != 1 || !probes.GetOutcomes()[0].GetPassed() {
		t.Fatalf("RunProbes = %+v, %v", probes, err)
	}
	c, cancel = call()
	_, err = gw.BeginPhase(c, &gatewayv1.BeginPhaseRequest{Intent: ref, Stage: "Create", SubjectHash: hash})
	cancel()
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "already opened a phase") {
		t.Fatalf("re-running the executed plan = %v, want FailedPrecondition (already opened a phase)", err)
	}

	// Every step is in the real WORM chain, in order.
	types := intentEntryTypes(t, lifecycleNS+"/web")
	want := []string{audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeOutputWritten, audit.TypePhaseEnd,
		audit.TypeProbeStart, audit.TypeProbeResult, audit.TypePolicyDenied}
	if !containsInOrder(types, want) {
		t.Fatalf("chain entries for the intent = %v, want %v in order", types, want)
	}
}

// auditEntries reads the bundled audit store and returns every entry, in chain order,
// that belongs to intent ("" returns all of them).
func auditEntries(t *testing.T, intent string) []audit.Entry {
	t.Helper()
	ctx := context.Background()
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	s3c, err := audit.NewS3Client(ctx, audit.S3Config{
		Endpoint: "http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000"), Region: "us-east-1", Bucket: "last-audit",
		AccessKey: secretValue(t, "last-audit", "last-audit-reader", "accessKey"),
		SecretKey: secretValue(t, "last-audit", "last-audit-reader", "secretKey"), PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewS3Sink(s3c, "last-audit")
	objs, err := sink.List(ctx, audit.EntryPrefix(chain), "")
	if err != nil {
		t.Fatal(err)
	}
	var entries []audit.Entry
	for _, o := range objs {
		body, err := sink.Get(ctx, o.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		var e audit.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		if intent == "" || e.Intent == intent {
			entries = append(entries, e)
		}
	}
	return entries
}

// intentEntryTypes returns the types of the audit entries for intent.
func intentEntryTypes(t *testing.T, intent string) []string {
	t.Helper()
	var types []string
	for _, e := range auditEntries(t, intent) {
		types = append(types, e.Type)
	}
	return types
}

func containsInOrder(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// pauseOperator scales the operator to zero for the rest of the test, so a test
// that drives the gateway by hand owns its Intents.
func pauseOperator(t *testing.T) {
	t.Helper()
	kubectl(t, "scale", "-n", "last-system", "deploy/last-operator", "--replicas=0")
	kubectl(t, "wait", "-n", "last-system", "--for=delete", "pod", "-l", "app.kubernetes.io/name=last-operator", "--timeout=2m")
	t.Cleanup(func() {
		kubectl(t, "scale", "-n", "last-system", "deploy/last-operator", "--replicas=1")
		kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	})
}

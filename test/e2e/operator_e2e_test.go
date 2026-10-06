//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// nginxImage must match NGINX_IMAGE in hack/e2e.sh, which preloads it into kind.
const nginxImage = "nginx:1.27-alpine"

func e2eClients(t *testing.T) (client.Client, kubernetes.Interface) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = lastv1alpha1.AddToScheme(sch)
	crc, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	return crc, kubernetes.NewForConfigOrDie(cfg)
}

// waitFor polls cond every two seconds until it returns true or timeout passes.
// cond's string describes the current state for the failure message.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s; last state: %s", timeout, what, state)
		}
		time.Sleep(2 * time.Second)
	}
}

// verifyAuditChain runs `kubectl last audit verify` against the bundled store.
func verifyAuditChain(t *testing.T) {
	t.Helper()
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	pub := filepath.Join(t.TempDir(), "pub.pem")
	pubPEM := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-pubkey", "-o", `jsonpath={.data.ed25519\.pub}`)
	if err := os.WriteFile(pub, []byte(pubPEM+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runLast(env{
		"LAST_AUDIT_ENDPOINT": "http://" + portForward(t, "last-audit", "svc/last-rustfs", "9000"),
		"LAST_AUDIT_BUCKET":   "last-audit", "LAST_AUDIT_CHAIN_ID": chain,
		"AWS_ACCESS_KEY_ID":     secretValue(t, "last-audit", "last-audit-reader", "accessKey"),
		"AWS_SECRET_ACCESS_KEY": secretValue(t, "last-audit", "last-audit-reader", "secretKey"),
	}, "audit", "verify", "--pubkey-file", pub)
	if err != nil {
		t.Fatalf("audit verify: %v\n%s", err, out)
	}
}

// operatorCan asks the API server whether the operator's ServiceAccount may do
// something. Any answer other than a clean "yes" (exit 0) or "no" (exit 1) fails
// the test, so a broken kubectl never reads as a denial.
func operatorCan(t *testing.T, args ...string) bool {
	t.Helper()
	cmdArgs := append([]string{"auth", "can-i", "--as", "system:serviceaccount:last-system:last-operator"}, args...)
	out, err := exec.Command("kubectl", cmdArgs...).CombinedOutput()
	answer := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	switch {
	case err == nil && answer == "yes":
		return true
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && answer == "no":
		return false
	default:
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(cmdArgs, " "), err, out)
		return false
	}
}

// TestOperatorWebServer is M2's done-criterion (spec §20): a scripted-LLM web
// server Intent goes Pending → Ready via kubectl in a sandbox, with a verified
// audit chain.
func TestOperatorWebServer(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	ctx := context.Background()
	crc, kube := e2eClients(t)
	// A unique suffix keeps this run's objects, and so its audit entries, distinct from any earlier run.
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	ns, provider, llmName := "e2e-web-"+suffix, "e2e-web-"+suffix, "e2e-web-"+suffix
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	// OperatorConfig default: allow the Provider and use the scripted LLM; restored in cleanup.
	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		create(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	} else if err != nil {
		t.Fatal(err)
	}
	var prevLLM *lastv1alpha1.LocalRef
	t.Cleanup(func() {
		bg := context.Background()
		for _, o := range []client.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "last-gateway"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"}},
			&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: provider}},
			&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}},
		} {
			if err := crc.Delete(bg, o); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup %T: %v", o, err)
			}
		}
		for range 5 {
			var cur lastv1alpha1.OperatorConfig
			if err := crc.Get(bg, client.ObjectKey{Name: "default"}, &cur); err != nil {
				t.Errorf("cleanup: read OperatorConfig: %v", err)
				return
			}
			if cur.Spec.Defaults != nil {
				cur.Spec.Defaults.Providers = slices.DeleteFunc(cur.Spec.Defaults.Providers, func(n string) bool { return n == provider })
			}
			if cur.Spec.LLMProviderRef != nil && cur.Spec.LLMProviderRef.Name == llmName {
				cur.Spec.LLMProviderRef = prevLLM
			}
			if err := crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not restore OperatorConfig")
	})

	// The namespace, a Provider that can edit only that namespace, and a 1m probe interval there.
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "last-gateway"}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "provider-edit", Namespace: ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: provider, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: provider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: provider}}}})
	create(&lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "probes", Namespace: ns}, Spec: lastv1alpha1.IntentPolicySpec{
		Probes: &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: time.Minute}}}})

	// The plan the scripted LLM answers with.
	k8s := []string{provider}
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: nginx
          image: %[2]s
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 80}]
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: %[1]s}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 80}]
`, ns, nginxImage)
	steps := []lastv1alpha1.Command{
		{ID: "apply", Reason: "Create the nginx Deployment and its Service",
			Run:   lastv1alpha1.CommandRun{Argv: []string{"kubectl", "apply", "-f", "/work/web.yaml"}},
			Files: map[string]string{"/work/web.yaml": manifest}, Providers: k8s, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
		{ID: "rollout", Reason: "Wait until nginx is ready",
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "rollout", "status", "deployment/web", "-n", ns, "--timeout=3m"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: 4 * time.Minute}},
		{ID: "svc", Reason: "Read the Service",
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "service", "web", "-n", ns, "-o", "json"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: time.Minute}},
	}
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "nginx web server " + ns + "/web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","namespace":"` + ns + `","name":"web"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "web-ready",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deployment", "web", "-n", ns, "-o", "json"}}, Providers: k8s},
			Assert:  "result.exitCode == 0 && result.json.status.readyReplicas == 1"}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"clusterIP": "${steps.svc.json.spec.clusterIP}"},
	}}
	answer, err := json.Marshal(map[string]any{"summary": "nginx Deployment and Service web in " + ns, "steps": steps, "resources": resources})
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal([]string{string(answer)})
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"},
		Data: map[string]string{llm.ScriptKey(ns + "/web"): string(script)}})
	create(&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}, Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "scripted", Scripted: &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: llmName}}}})
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	prevLLM = op.Spec.LLMProviderRef
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: llmName}
	if op.Spec.Defaults == nil {
		op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
	}
	op.Spec.Defaults.Providers = append(op.Spec.Defaults.Providers, provider)
	if err := crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}

	// The Intent, as a user writes it.
	create(&lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: lastv1alpha1.IntentSpec{Request: "Run an nginx web server named web in namespace " + ns}})
	key := client.ObjectKey{Namespace: ns, Name: "web"}
	var intent lastv1alpha1.Intent
	state := func() string {
		c := meta.FindStatusCondition(intent.Status.Conditions, "Ready")
		if c == nil {
			return string(intent.Status.Phase)
		}
		return fmt.Sprintf("%s (%s: %s)", intent.Status.Phase, c.Reason, c.Message)
	}
	waitFor(t, 3*time.Minute, "the plan to await approval", func() (bool, string) {
		if err := crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		return intent.Status.Phase == lastv1alpha1.PhaseAwaitingApproval, state()
	})
	if intent.Status.Plan.Hash != wantHash {
		t.Fatalf("plan hash = %s, want %s (the scripted plan)", intent.Status.Plan.Hash, wantHash)
	}

	// Approve as ourselves: the admission policy requires approvedBy to be the creator.
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	create(&lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{Name: "web-create", Namespace: ns},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: "web"}, Stage: lastv1alpha1.StageCreate,
			SubjectHash: wantHash, Decision: lastv1alpha1.DecisionApprove, ApprovedBy: review.Status.UserInfo.Username}})
	waitFor(t, 10*time.Minute, "the Intent to become Ready", func() (bool, string) {
		if err := crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		if intent.Status.Phase == lastv1alpha1.PhaseFailed {
			t.Fatalf("the Intent failed: %s", state())
		}
		return intent.Status.Phase == lastv1alpha1.PhaseReady, state()
	})

	// The world matches the plan.
	if got := kubectl(t, "get", "deployment", "web", "-n", ns, "-o", "jsonpath={.status.readyReplicas}"); got != "1" {
		t.Fatalf("nginx readyReplicas = %q", got)
	}
	clusterIP := kubectl(t, "get", "service", "web", "-n", ns, "-o", "jsonpath={.spec.clusterIP}")
	if got := secretValue(t, ns, "web-outputs", "clusterIP"); got != clusterIP {
		t.Fatalf("output secret clusterIP = %q, want %q", got, clusterIP)
	}
	var mr lastv1alpha1.ManagedResource
	if err := crc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-0"}, &mr); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&mr, &intent) || !meta.IsStatusConditionTrue(mr.Status.Conditions, "InSync") || mr.Status.LastProbeTime == nil {
		t.Fatalf("managed resource = %+v", mr)
	}

	// The ManagedResource controller probes again on the 1m schedule.
	first := mr.Status.LastProbeTime.Time
	waitFor(t, 4*time.Minute, "a scheduled probe run", func() (bool, string) {
		if err := crc.Get(ctx, client.ObjectKeyFromObject(&mr), &mr); err != nil {
			return false, err.Error()
		}
		return mr.Status.LastProbeTime.After(first), mr.Status.LastProbeTime.String()
	})

	// The operator stayed within its RBAC: it cannot touch Secrets, sandbox pods, or approvals.
	// The positive control proves the checks below can answer "yes" at all.
	if !operatorCan(t, "get", "intents.last.moogie-labs.com", "-n", ns) {
		t.Fatalf("the operator cannot get Intents in %s; the RBAC checks below would be vacuous", ns)
	}
	for _, args := range [][]string{
		{"get", "secrets", "-n", ns},
		{"list", "pods", "-n", "last-sandbox"},
		{"create", "intentapprovals.last.moogie-labs.com", "-n", ns},
		{"delete", "managedresources.last.moogie-labs.com", "-n", ns},
	} {
		if operatorCan(t, args...) {
			t.Errorf("the operator can %v", args)
		}
	}

	// Every step is in the WORM chain, in order, and the chain verifies.
	types := intentEntryTypes(t, ns+"/web")
	want := []string{audit.TypeLLMRequest, audit.TypeLLMResponse, audit.TypeApprovalObserved, audit.TypePhaseBegin,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeOutputWritten, audit.TypePhaseEnd,
		audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult}
	if !containsInOrder(types, want) {
		t.Fatalf("chain entries for the intent = %v, want %v in order", types, want)
	}
	verifyAuditChain(t)
}

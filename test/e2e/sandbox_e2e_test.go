//go:build e2e

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	targetNS        = "last-e2e-target"
	e2eSecretValue  = "e2e-secret-value-9f3a"
	e2eFileValue    = "e2e-file-content-4c1d"
	e2eActor        = "system:serviceaccount:last-system:last-operator"
	sandboxImageE2E = "ghcr.io/moogie-labs/last-sandbox-base:e2e"
)

type sandboxEnv struct {
	kube     kubernetes.Interface
	ctrl     client.Client
	exec     *sandbox.Executor
	sink     *audittest.MemSink
	signer   *audit.Signer
	targetIP string
	canary   string
}

func setupSandbox(t *testing.T) *sandboxEnv {
	t.Helper()
	ctx := context.Background()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = lastv1alpha1.AddToScheme(scheme)
	ctrl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	create := func(obj client.Object) {
		t.Helper()
		if err := ctrl.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
		t.Cleanup(func() { _ = ctrl.Delete(context.Background(), obj) })
	}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNS}})
	create(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: targetNS, Labels: map[string]string{"app": "target"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27-alpine",
			Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}},
	})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: "last-gateway"}})
	create(&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: targetNS},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "e2e-reader", Namespace: targetNS},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "e2e-reader"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "e2e-reader", Namespace: "last-gateway"}}})
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "e2e-demo", Namespace: "last-gateway"},
		Data: map[string][]byte{"token": []byte(e2eSecretValue), "config": []byte(e2eFileValue)}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "e2e-cluster"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: "e2e-reader"}}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "e2e-demo"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "e2e-demo"}, Env: map[string]string{"DEMO_TOKEN": "token"}, Files: map[string]string{"config": "/work/.demo/config"}}})

	var targetIP string
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := kube.CoreV1().Pods(targetNS).Get(ctx, "target", metav1.GetOptions{})
		if err != nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			return false, nil
		}
		targetIP = p.Status.PodIP
		return true, nil
	}); err != nil {
		t.Fatalf("target pod not running: %v", err)
	}

	// The gateway Service's http port is admitted from anywhere by the chart, so it is
	// reachable if (and only if) a sandbox pod's egress is still open.
	gwSvc, err := kube.CoreV1().Services("last-gateway").Get(ctx, "last-gateway", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canary := gwSvc.Spec.ClusterIP + ":8080"

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.NewSigner(key)
	sink := audittest.NewMemSink()
	w, err := audit.Open(ctx, sink, signer, audit.WriterConfig{ChainID: "e2e-sandbox", Retention: time.Hour},
		func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	getProvider := func(ctx context.Context, name string) (*lastv1alpha1.Provider, error) {
		var p lastv1alpha1.Provider
		return &p, ctrl.Get(ctx, client.ObjectKey{Name: name}, &p)
	}
	exec := sandbox.NewExecutor(kube, getProvider, w, sandbox.NewScheduler(4), sandbox.KubeLogReader(kube), sandbox.ExecutorConfig{
		Pod: sandbox.PodConfig{Namespace: "last-sandbox", Image: sandboxImageE2E, ImagePullPolicy: corev1.PullIfNotPresent,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}},
		ProviderNamespace: "last-gateway", EgressCanaries: []string{canary},
	})
	return &sandboxEnv{kube: kube, ctrl: ctrl, exec: exec, sink: sink, signer: signer, targetIP: targetIP, canary: canary}
}

func (s *sandboxEnv) run(t *testing.T, c lastv1alpha1.Command) sandbox.RunResult {
	t.Helper()
	c.Timeout = &metav1.Duration{Duration: time.Minute}
	res, err := s.exec.Run(context.Background(), sandbox.RunRequest{Actor: e2eActor, Intent: "e2e/sandbox", Stage: "Create",
		Command: c, Priority: sandbox.PriorityExecution})
	if err != nil {
		t.Fatalf("run %v: %v", c.Run, err)
	}
	return res
}

func TestSandboxExecution(t *testing.T) {
	s := setupSandbox(t)
	curl := "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://" + s.targetIP + "/"

	t.Run("egress is denied by default", func(t *testing.T) {
		if r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: curl}}); r.Result.ExitCode == 0 {
			t.Fatalf("curl succeeded without an egress grant: %+v (does the CNI enforce NetworkPolicy?)", r.Result)
		}
	})
	t.Run("a grant that would open the egress canary is rejected", func(t *testing.T) {
		_, err := s.exec.Run(context.Background(), sandbox.RunRequest{Actor: e2eActor, Intent: "e2e/sandbox", Stage: "Create",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}, Egress: []string{s.canary}}})
		if err == nil || !strings.Contains(err.Error(), "egress grant would allow the egress canary "+s.canary) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a grant opens exactly that destination", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: curl}, Egress: []string{s.targetIP + ":80"}})
		if r.Result.ExitCode != 0 || r.Result.Stdout != "200" {
			t.Fatalf("result = %+v", r.Result)
		}
	})
	t.Run("pod is non-root, read-only and has no service account token", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "id -u; touch /etc/x 2>/dev/null && echo writable || echo readonly; " +
			"test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo has-token || echo no-token"}})
		if r.Result.Stdout != "65532\nreadonly\nno-token\n" {
			t.Fatalf("stdout = %q", r.Result.Stdout)
		}
	})
	t.Run("kubernetes provider reaches the API server with its own RBAC", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "name"}},
			Providers: []string{"e2e-cluster"}})
		if r.Result.ExitCode != 0 || !strings.Contains(r.Result.Stdout, "pod/target") {
			t.Fatalf("result = %+v", r.Result)
		}
		denied := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "secrets", "-n", targetNS}},
			Providers: []string{"e2e-cluster"}})
		if denied.Result.ExitCode == 0 || !strings.Contains(denied.Result.Stderr, "forbidden") {
			t.Fatalf("provider exceeded its RBAC: %+v", denied.Result)
		}
	})
	t.Run("env-secret provider injects the value, redacts it and does not audit it", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: `test -n "$DEMO_TOKEN" && echo "env-ok $DEMO_TOKEN"`},
			Providers: []string{"e2e-demo"}})
		if r.Result.ExitCode != 0 || !strings.Contains(r.Result.Stdout, "env-ok") ||
			!strings.Contains(r.Result.Stdout, "[REDACTED]") || strings.Contains(r.Result.Stdout, e2eSecretValue) {
			t.Fatalf("result = %+v", r.Result)
		}
	})
	t.Run("credential files are readable by the non-root user", func(t *testing.T) {
		r := s.run(t, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "id -u; cat /work/.demo/config; echo; cat /last/creds/e2e-cluster.kubeconfig"},
			Providers: []string{"e2e-demo", "e2e-cluster"}})
		if r.Result.ExitCode != 0 || r.Result.Stdout != "65532\n[REDACTED]\n[REDACTED]" {
			t.Fatalf("result = %+v", r.Result)
		}
		if strings.Contains(r.Result.Stdout, e2eFileValue) {
			t.Fatalf("file content leaked: %q", r.Result.Stdout)
		}
	})
	t.Run("probes evaluate assertions in one audited run", func(t *testing.T) {
		probes := []lastv1alpha1.Probe{
			{ID: "listed", Assert: `result.exitCode == 0 && result.json.items.exists(p, p.metadata.name == "target")`,
				Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "json"}},
					Providers: []string{"e2e-cluster"}, Timeout: &metav1.Duration{Duration: time.Minute}}},
			{ID: "wrong", Assert: `size(result.json.items) == 5`,
				Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods", "-n", targetNS, "-o", "json"}},
					Providers: []string{"e2e-cluster"}, Timeout: &metav1.Duration{Duration: time.Minute}}},
		}
		outs, err := s.exec.RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: e2eActor, Intent: "e2e/sandbox", Probes: probes})
		if err != nil || !outs[0].Passed || outs[1].Passed {
			t.Fatalf("outs=%+v err=%v", outs, err)
		}
	})
	t.Run("nothing is left behind and the audit chain verifies", func(t *testing.T) {
		ctx := context.Background()
		sel := metav1.ListOptions{LabelSelector: sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue}
		pods, _ := s.kube.CoreV1().Pods("last-sandbox").List(ctx, sel)
		secrets, _ := s.kube.CoreV1().Secrets("last-sandbox").List(ctx, sel)
		nps, _ := s.kube.NetworkingV1().NetworkPolicies("last-sandbox").List(ctx, sel)
		if len(pods.Items)+len(secrets.Items)+len(nps.Items) != 0 {
			t.Fatalf("leftovers: %d pods, %d secrets, %d policies", len(pods.Items), len(secrets.Items), len(nps.Items))
		}
		rep, err := audit.Verify(ctx, s.sink, s.signer.Public(), "e2e-sandbox", audit.VerifyOptions{})
		if err != nil || !rep.OK() {
			t.Fatalf("verify: %v %+v", err, rep.Problems)
		}
		objs, _ := s.sink.List(ctx, audit.EntryPrefix("e2e-sandbox"), "")
		for _, o := range objs {
			body, _ := s.sink.Get(ctx, o.Key, "")
			if strings.Contains(string(body), e2eSecretValue) {
				t.Fatalf("audit entry %s contains the provider secret value", o.Key)
			}
		}
	})
}

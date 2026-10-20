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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// TestDeleteIntentTearsDownWhatItCreated is M5a's done-criterion: an Intent that
// is Ready is deleted, its finalizer holds it while the scripted model orders
// the teardown and the user approves the Delete plan, and then the Deployment,
// Service and ConfigMap it created, and the Intent itself, are gone, with the
// audit chain verified.
func TestDeleteIntentTearsDownWhatItCreated(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	ctx := context.Background()
	crc, _ := e2eClients(t)
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	ns, provider, llmName := "e2e-del-"+suffix, "e2e-del-"+suffix, "e2e-del-"+suffix
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		create(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	} else if err != nil {
		t.Fatal(err)
	}
	var prevLLM *lastv1alpha1.LocalRef
	t.Cleanup(func() {
		bg := context.Background()
		releaseIntents(t, crc, ns)
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

	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "last-gateway"}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "provider-edit", Namespace: ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: provider, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: provider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: provider}}}})

	// The Create plan: a Deployment, a Service and a ConfigMap, each with a teardown command.
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
---
apiVersion: v1
kind: ConfigMap
metadata: {name: web-config, namespace: %[1]s}
data: {greeting: hello}
`, ns, nginxImage)
	steps := []lastv1alpha1.Command{
		{ID: "apply", Reason: "Create the nginx Deployment, its Service and a ConfigMap",
			Run:   lastv1alpha1.CommandRun{Argv: []string{"kubectl", "apply", "-f", "/work/web.yaml"}},
			Files: map[string]string{"/work/web.yaml": manifest}, Providers: k8s, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
		{ID: "rollout", Reason: "Wait until nginx is ready",
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "rollout", "status", "deployment/web", "-n", ns, "--timeout=3m"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: 4 * time.Minute}},
	}
	del := func(id, kind, name string) lastv1alpha1.Command {
		return lastv1alpha1.Command{ID: id, Reason: "Delete the " + kind,
			Run:       lastv1alpha1.CommandRun{Argv: []string{"kubectl", "delete", kind, name, "-n", ns, "--ignore-not-found"}},
			Providers: k8s, Timeout: &metav1.Duration{Duration: 2 * time.Minute}}
	}
	teardown := []lastv1alpha1.Command{del("rm-service", "service", "web"), del("rm-deployment", "deployment", "web"), del("rm-config", "configmap", "web-config")}
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "nginx web server " + ns + "/web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","namespace":"` + ns + `","name":"web"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "web-ready",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deployment", "web", "-n", ns, "-o", "json"}}, Providers: k8s},
			Assert:  "result.exitCode == 0 && result.json.status.readyReplicas == 1"}},
		Teardown: teardown,
	}}
	// The Delete plan orders the same teardown commands, the config first, with no resources.
	deleteSteps := []lastv1alpha1.Command{teardown[2], teardown[0], teardown[1]}

	encode := func(turns ...agent.Turn) string {
		t.Helper()
		answers := make([]string, 0, len(turns))
		for _, turn := range turns {
			a, err := agent.Encode(turn)
			if err != nil {
				t.Fatal(err)
			}
			answers = append(answers, a)
		}
		script, err := json.Marshal(answers)
		if err != nil {
			t.Fatal(err)
		}
		return string(script)
	}
	key := llm.ScriptKey(ns + "/web")
	createScript := encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{Summary: "nginx, its Service and a ConfigMap in " + ns,
		Steps: steps, Resources: resources}})
	deleteScript := encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{Summary: "Tear down web in " + ns, Steps: deleteSteps}})
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	wantDelete, err := plan.Hash(plan.Body{Steps: deleteSteps})
	if err != nil {
		t.Fatal(err)
	}
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"}, Data: map[string]string{key: createScript}})
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

	// Create: the user approves the plan on the follow prompt, and the Intent goes Ready.
	out, err := runLastInput(t, 15*time.Minute, "y\n",
		"Run an nginx web server named web in namespace "+ns, "-n", ns, "--name", "web")
	if err != nil {
		t.Fatalf("kubectl last: %v\n%s", err, out)
	}
	for _, want := range []string{"Plan " + wantCreate + " (stage Create)", "Intent " + ns + "/web is Ready."} {
		if !strings.Contains(out, want) {
			t.Errorf("kubectl last output lacks %q:\n%s", want, out)
		}
	}
	ikey := client.ObjectKey{Namespace: ns, Name: "web"}
	var intent lastv1alpha1.Intent
	if err := crc.Get(ctx, ikey, &intent); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(intent.Finalizers, "last.moogie-labs.com/teardown") {
		t.Fatalf("the Ready Intent's finalizers = %v, want the teardown finalizer", intent.Finalizers)
	}
	if got := kubectl(t, "get", "configmap", "web-config", "-n", ns, "-o", "jsonpath={.data.greeting}"); got != "hello" {
		t.Fatalf("web-config greeting = %q", got)
	}

	// The next conversation, the Delete session, starts from the first scripted answer again.
	var script corev1.ConfigMap
	if err := crc.Get(ctx, client.ObjectKey{Namespace: "last-gateway", Name: llmName}, &script); err != nil {
		t.Fatal(err)
	}
	script.Data[key] = deleteScript
	if err := crc.Update(ctx, &script); err != nil {
		t.Fatal(err)
	}

	// Delete: the finalizer holds the Intent while the Delete plan awaits approval.
	kubectl(t, "delete", "intent", "web", "-n", ns, "--wait=false")
	waitFor(t, 5*time.Minute, "the Delete plan to await approval", func() (bool, string) {
		if err := crc.Get(ctx, ikey, &intent); err != nil {
			return false, err.Error()
		}
		return intent.Status.Stage == lastv1alpha1.StageDelete && intent.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && intent.Status.Plan != nil,
			fmt.Sprintf("stage %q, phase %q", intent.Status.Stage, intent.Status.Phase)
	})
	if intent.DeletionTimestamp == nil || intent.Status.Plan.Hash != wantDelete || len(intent.Status.Plan.Resources) != 0 {
		t.Fatalf("intent = deletionTimestamp %v, plan %+v; want a deletion and the Delete plan %s", intent.DeletionTimestamp, intent.Status.Plan, wantDelete)
	}
	out, err = runLast(nil, "approve", "web", "-n", ns, "--hash", wantDelete)
	if err != nil {
		t.Fatalf("kubectl last approve: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Approved plan "+wantDelete) {
		t.Errorf("approve output:\n%s", out)
	}

	// Everything the Intent created, and the Intent, are gone.
	waitFor(t, 10*time.Minute, "the Intent to be gone", func() (bool, string) {
		err := crc.Get(ctx, ikey, &intent)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("phase %q, reason in %v", intent.Status.Phase, intent.Status.Conditions)
	})
	for _, o := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
		if err := crc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, o); !apierrors.IsNotFound(err) {
			t.Errorf("%T web after the teardown: %v", o, err)
		}
	}
	if err := crc.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap web-config after the teardown: %v", err)
	}
	waitFor(t, 2*time.Minute, "the ManagedResources to be garbage collected", func() (bool, string) {
		var mrs lastv1alpha1.ManagedResourceList
		if err := crc.List(ctx, &mrs, client.InNamespace(ns)); err != nil {
			return false, err.Error()
		}
		return len(mrs.Items) == 0, fmt.Sprintf("%d left", len(mrs.Items))
	})

	// The Delete phase is in the chain, after the Create phase, and the chain verifies.
	entries := auditEntries(t, ns+"/web")
	var types []string
	for _, e := range entries {
		types = append(types, e.Type)
	}
	want := []string{audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandResult, audit.TypePhaseEnd,
		audit.TypeLLMRequest, audit.TypeLLMResponse,
		audit.TypeApprovalObserved, audit.TypePhaseBegin,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypeCommandStart, audit.TypeCommandResult,
		audit.TypeCommandStart, audit.TypeCommandResult, audit.TypePhaseEnd}
	if !containsInOrder(types, want) {
		t.Fatalf("chain entries for the intent = %v, want %v in order", types, want)
	}
	// The newest phase.begin is the Delete phase, for the Delete plan, and its phase.end succeeded.
	var begin *audit.Entry
	for i := range entries {
		if entries[i].Type == audit.TypePhaseBegin {
			begin = &entries[i]
		}
	}
	if begin == nil || begin.Stage != string(lastv1alpha1.StageDelete) || begin.SubjectHash != wantDelete {
		t.Fatalf("newest phase.begin = %+v, want stage Delete for %s", begin, wantDelete)
	}
	var end *audit.Entry
	for i := range entries {
		var p struct {
			BeginSeq uint64 `json:"beginSeq"`
			Outcome  string `json:"outcome"`
		}
		if entries[i].Type == audit.TypePhaseEnd && json.Unmarshal(entries[i].Payload, &p) == nil && p.BeginSeq == begin.Seq {
			end = &entries[i]
			if p.Outcome != "succeeded" {
				t.Fatalf("the Delete phase.end (seq %d) has outcome %q, want succeeded", end.Seq, p.Outcome)
			}
		}
	}
	if end == nil {
		t.Fatalf("no phase.end for the Delete phase.begin at seq %d", begin.Seq)
	}
	verifyAuditChain(t)
}

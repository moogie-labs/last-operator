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

// day2 is the world of an M5b test: a namespace with a Provider that can edit
// only it, a 1m probe interval there, and a scripted model that is repointed
// per session. Cleanup restores the OperatorConfig as the M5a test does.
type day2 struct {
	t        *testing.T
	crc      client.Client
	ns       string
	provider string
	llmName  string
	k8s      []string
	key      string
}

func newDay2(t *testing.T, prefix string) *day2 {
	t.Helper()
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	ctx := context.Background()
	crc, _ := e2eClients(t)
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	name := prefix + suffix
	d := &day2{t: t, crc: crc, ns: name, provider: name, llmName: name, k8s: []string{name}, key: llm.ScriptKey(name + "/web")}
	create := d.create

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
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: d.ns}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: d.provider, Namespace: "last-gateway"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.llmName, Namespace: "last-gateway"}},
			&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: d.provider}},
			&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: d.llmName}},
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
				cur.Spec.Defaults.Providers = slices.DeleteFunc(cur.Spec.Defaults.Providers, func(n string) bool { return n == d.provider })
			}
			if cur.Spec.LLMProviderRef != nil && cur.Spec.LLMProviderRef.Name == d.llmName {
				cur.Spec.LLMProviderRef = prevLLM
			}
			if err := crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not restore OperatorConfig")
	})

	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: d.ns}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: d.provider, Namespace: "last-gateway"}})
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "provider-edit", Namespace: d.ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "edit"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: d.provider, Namespace: "last-gateway"}}})
	create(&lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: d.provider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderKubernetes, Kubernetes: &lastv1alpha1.KubernetesProvider{ServiceAccountRef: lastv1alpha1.LocalRef{Name: d.provider}}}})
	// The probe interval is the floor (1m), set by a policy that goes with the namespace.
	create(&lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "probes", Namespace: d.ns}, Spec: lastv1alpha1.IntentPolicySpec{
		Probes: &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: time.Minute}}}})
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.llmName, Namespace: "last-gateway"}, Data: map[string]string{}})
	create(&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: d.llmName}, Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "scripted", Scripted: &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: d.llmName}}}})
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	prevLLM = op.Spec.LLMProviderRef
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: d.llmName}
	if op.Spec.Defaults == nil {
		op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
	}
	op.Spec.Defaults.Providers = append(op.Spec.Defaults.Providers, d.provider)
	if err := crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *day2) create(o client.Object) {
	d.t.Helper()
	if err := d.crc.Create(context.Background(), o); err != nil {
		d.t.Fatalf("create %T: %v", o, err)
	}
}

// script points the scripted model's next conversation at the given plans, one
// answer each; a conversation starts from the first answer again.
func (d *day2) script(plans ...*lastv1alpha1.Plan) {
	d.t.Helper()
	answers := make([]string, 0, len(plans))
	for _, p := range plans {
		a, err := agent.Encode(agent.Turn{Kind: agent.KindPlan, Plan: p})
		if err != nil {
			d.t.Fatal(err)
		}
		answers = append(answers, a)
	}
	script, err := json.Marshal(answers)
	if err != nil {
		d.t.Fatal(err)
	}
	ctx := context.Background()
	var cm corev1.ConfigMap
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: "last-gateway", Name: d.llmName}, &cm); err != nil {
		d.t.Fatal(err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[d.key] = string(script)
	if err := d.crc.Update(ctx, &cm); err != nil {
		d.t.Fatal(err)
	}
}

// cmd is a kubectl step run as the test's Provider.
func (d *day2) cmd(id, reason string, timeout time.Duration, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Reason: reason, Run: lastv1alpha1.CommandRun{Argv: append([]string{"kubectl"}, argv...)},
		Providers: d.k8s, Timeout: &metav1.Duration{Duration: timeout}}
}

// webResource is the nginx Deployment web, with a probe that asserts readyReplicas >= 1
// and, if teardown, a teardown. A missing readyReplicas (zero replicas) fails the probe.
func (d *day2) webResource(teardown bool) lastv1alpha1.ManagedResourceSpec {
	r := lastv1alpha1.ManagedResourceSpec{
		Description: "nginx web server " + d.ns + "/web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","namespace":"` + d.ns + `","name":"web"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "web-ready",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deployment", "web", "-n", d.ns, "-o", "json"}}, Providers: d.k8s},
			Assert:  "result.exitCode == 0 && has(result.json.status.readyReplicas) && result.json.status.readyReplicas >= 1"}},
	}
	if teardown {
		r.Teardown = []lastv1alpha1.Command{d.cmd("rm-deployment", "Delete the Deployment", 2*time.Minute, "delete", "deployment", "web", "-n", d.ns, "--ignore-not-found")}
	}
	return r
}

func (d *day2) deploymentManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
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
`, d.ns, nginxImage)
}

func (d *day2) applyStep(manifest string) lastv1alpha1.Command {
	c := d.cmd("apply", "Create the resources", 2*time.Minute, "apply", "-f", "/work/web.yaml")
	c.Files = map[string]string{"/work/web.yaml": manifest}
	return c
}

func (d *day2) rolloutStep() lastv1alpha1.Command {
	return d.cmd("rollout", "Wait until nginx is ready", 4*time.Minute, "rollout", "status", "deployment/web", "-n", d.ns, "--timeout=3m")
}

// createReady creates the Intent, approves its Create plan on the follow prompt and returns it Ready.
func (d *day2) createReady(wantHash string) client.ObjectKey {
	d.t.Helper()
	out, err := runLastInput(d.t, 15*time.Minute, "y\n", "Run an nginx web server named web in namespace "+d.ns, "-n", d.ns, "--name", "web")
	if err != nil {
		d.t.Fatalf("kubectl last: %v\n%s", err, out)
	}
	for _, want := range []string{"Plan " + wantHash + " (stage Create)", "Intent " + d.ns + "/web is Ready."} {
		if !strings.Contains(out, want) {
			d.t.Errorf("kubectl last output lacks %q:\n%s", want, out)
		}
	}
	return client.ObjectKey{Namespace: d.ns, Name: "web"}
}

// awaiting waits until the Intent awaits approval of a plan for stage.
func (d *day2) awaiting(key client.ObjectKey, stage lastv1alpha1.Stage, timeout time.Duration) lastv1alpha1.Intent {
	d.t.Helper()
	var in lastv1alpha1.Intent
	waitFor(d.t, timeout, "the "+string(stage)+" plan to await approval", func() (bool, string) {
		if err := d.crc.Get(context.Background(), key, &in); err != nil {
			return false, err.Error()
		}
		return in.Status.Stage == stage && in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && in.Status.Plan != nil,
			fmt.Sprintf("stage %q, phase %q", in.Status.Stage, in.Status.Phase)
	})
	return in
}

// ready waits until the Intent is Ready again, after having left Ready.
func (d *day2) ready(key client.ObjectKey, timeout time.Duration) lastv1alpha1.Intent {
	d.t.Helper()
	var in lastv1alpha1.Intent
	waitFor(d.t, timeout, "the Intent to be Ready", func() (bool, string) {
		if err := d.crc.Get(context.Background(), key, &in); err != nil {
			return false, err.Error()
		}
		return in.Status.Phase == lastv1alpha1.PhaseReady, fmt.Sprintf("stage %q, phase %q, conditions %v", in.Status.Stage, in.Status.Phase, in.Status.Conditions)
	})
	return in
}

func (d *day2) approve(hash string) {
	d.t.Helper()
	out, err := runLast(nil, "approve", "web", "-n", d.ns, "--hash", hash)
	if err != nil {
		d.t.Fatalf("kubectl last approve: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Approved plan "+hash) {
		d.t.Errorf("approve output:\n%s", out)
	}
	d.t.Logf("kubectl last approve:\n%s", out)
}

// phaseOf returns the newest phase.begin of stage with subject, and its phase.end.
func phaseOf(t *testing.T, entries []audit.Entry, stage lastv1alpha1.Stage, subject string) (begin, end *audit.Entry, outcome string) {
	t.Helper()
	for i := range entries {
		if e := &entries[i]; e.Type == audit.TypePhaseBegin && e.Stage == string(stage) && e.SubjectHash == subject {
			begin = e
		}
	}
	if begin == nil {
		return nil, nil, ""
	}
	for i := range entries {
		var p struct {
			BeginSeq uint64 `json:"beginSeq"`
			Outcome  string `json:"outcome"`
		}
		if entries[i].Type == audit.TypePhaseEnd && json.Unmarshal(entries[i].Payload, &p) == nil && p.BeginSeq == begin.Seq {
			return begin, &entries[i], p.Outcome
		}
	}
	return begin, nil, ""
}

// TestEditPlansAnUpdate: editing the spec of a Ready Intent plans an Update. The
// Update keeps the Deployment, drops the ConfigMap and deletes it by its steps;
// once it succeeds the gateway deletes the dropped ManagedResource and audits it.
func TestEditPlansAnUpdate(t *testing.T) {
	d := newDay2(t, "e2e-upd-")
	ctx := context.Background()

	manifest := d.deploymentManifest() + fmt.Sprintf(`---
apiVersion: v1
kind: ConfigMap
metadata: {name: web-config, namespace: %s}
data: {greeting: hello}
`, d.ns)
	steps := []lastv1alpha1.Command{d.applyStep(manifest), d.rolloutStep()}
	config := lastv1alpha1.ManagedResourceSpec{
		Description: "web config " + d.ns + "/web-config",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"ConfigMap","namespace":"` + d.ns + `","name":"web-config"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "config-exists",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "configmap", "web-config", "-n", d.ns, "-o", "json"}}, Providers: d.k8s},
			Assert:  "result.exitCode == 0"}},
		Teardown: []lastv1alpha1.Command{d.cmd("rm-config", "Delete the ConfigMap", 2*time.Minute, "delete", "configmap", "web-config", "-n", d.ns, "--ignore-not-found")},
	}
	resources := []lastv1alpha1.ManagedResourceSpec{d.webResource(true), config}
	createPlan := &lastv1alpha1.Plan{Summary: "nginx and a ConfigMap in " + d.ns, Steps: steps, Resources: resources}

	// The Update keeps the Deployment as resource 0 and deletes the ConfigMap by its steps.
	updSteps := []lastv1alpha1.Command{d.cmd("rm-config", "Delete the ConfigMap the new request no longer wants", 2*time.Minute,
		"delete", "configmap", "web-config", "-n", d.ns, "--ignore-not-found")}
	updResources := []lastv1alpha1.ManagedResourceSpec{d.webResource(true)}
	updPlan := &lastv1alpha1.Plan{Summary: "Drop the ConfigMap in " + d.ns, Steps: updSteps, Resources: updResources}

	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	wantUpdate, err := plan.Hash(plan.Body{Steps: updSteps, Resources: updResources})
	if err != nil {
		t.Fatal(err)
	}
	d.script(createPlan)
	key := d.createReady(wantCreate)
	if got := kubectl(t, "get", "configmap", "web-config", "-n", d.ns, "-o", "jsonpath={.data.greeting}"); got != "hello" {
		t.Fatalf("web-config greeting = %q", got)
	}
	for _, name := range []string{"web-0", "web-1"} {
		if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: name}, &lastv1alpha1.ManagedResource{}); err != nil {
			t.Fatalf("ManagedResource %s: %v", name, err)
		}
	}

	// Edit the request: the next conversation is the Update session.
	d.script(updPlan)
	var intent lastv1alpha1.Intent
	if err := d.crc.Get(ctx, key, &intent); err != nil {
		t.Fatal(err)
	}
	intent.Spec.Request = "Run an nginx web server named web in namespace " + d.ns + ", without the ConfigMap"
	if err := d.crc.Update(ctx, &intent); err != nil {
		t.Fatal(err)
	}
	intent = d.awaiting(key, lastv1alpha1.StageUpdate, 5*time.Minute)
	if intent.Status.Plan.Hash != wantUpdate {
		t.Fatalf("the Update plan = %s, want %s", intent.Status.Plan.Hash, wantUpdate)
	}
	out, err := runLast(nil, "plan", "web", "-n", d.ns)
	if err != nil || !strings.Contains(out, "Plan "+wantUpdate+" (stage Update)") {
		t.Fatalf("kubectl last plan = %v:\n%s", err, out)
	}
	d.approve(wantUpdate)
	d.ready(key, 10*time.Minute)

	// The ConfigMap and its ManagedResource are gone, the Deployment and web-0 stay.
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap web-config after the Update: %v", err)
	}
	waitFor(t, 2*time.Minute, "ManagedResource web-1 to be gone", func() (bool, string) {
		err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web-1"}, &lastv1alpha1.ManagedResource{})
		return apierrors.IsNotFound(err), fmt.Sprint(err)
	})
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web-0"}, &lastv1alpha1.ManagedResource{}); err != nil {
		t.Errorf("ManagedResource web-0 after the Update: %v", err)
	}

	entries := auditEntries(t, d.ns+"/web")
	deleted := 0
	for _, e := range entries {
		if e.Type != audit.TypeManagedResourceDeleted {
			continue
		}
		deleted++
		if p := payloadOf(e); p["managedResource"] != "web-1" {
			t.Errorf("managedresource.deleted payload = %v, want web-1", p)
		}
	}
	if deleted != 1 {
		t.Errorf("%d managedresource.deleted entries, want exactly 1", deleted)
	}
	if begin, end, outcome := phaseOf(t, entries, lastv1alpha1.StageUpdate, wantUpdate); begin == nil || end == nil || outcome != "succeeded" {
		t.Errorf("Update phase = begin %v, end %v, outcome %q; want a succeeded phase for %s", begin, end, outcome, wantUpdate)
	}
	t.Logf("Update %s approved and succeeded; %d managedresource.deleted entry", wantUpdate, deleted)
	verifyAuditChain(t)
}

// TestDriftIsRemediated: a Ready Intent whose Deployment is scaled to zero
// drifts, the operator plans a Remediate whose resources are the executed
// plan's, and once the user approves it the Intent is Ready again with two
// replicas, the executed plan unchanged.
func TestDriftIsRemediated(t *testing.T) {
	d := newDay2(t, "e2e-rem-")
	steps := []lastv1alpha1.Command{d.applyStep(d.deploymentManifest()), d.rolloutStep()}
	resources := []lastv1alpha1.ManagedResourceSpec{d.webResource(true)}
	createPlan := &lastv1alpha1.Plan{Summary: "nginx in " + d.ns, Steps: steps, Resources: resources}
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	d.script(createPlan)
	key := d.createReady(wantCreate)

	// The model answers a Remediate with no resources (the operator copies the executed ones in).
	// The rollout step makes the repair finish before it is verified.
	remSteps := []lastv1alpha1.Command{
		d.cmd("scale", "Scale web back up", 2*time.Minute, "scale", "deployment/web", "--replicas=2", "-n", d.ns),
		d.rolloutStep(),
	}
	d.script(&lastv1alpha1.Plan{Summary: "Scale web back to 2 in " + d.ns, Steps: remSteps, Resources: []lastv1alpha1.ManagedResourceSpec{}})

	kubectl(t, "scale", "deployment/web", "--replicas=0", "-n", d.ns)
	intent := d.awaiting(key, lastv1alpha1.StageRemediate, 6*time.Minute)
	p := intent.Status.Plan
	if !plan.ValidNonce(p.Nonce) || p.Hash == wantCreate || len(p.Resources) != 1 {
		t.Fatalf("the Remediate plan = hash %s nonce %q resources %d; want its own hash, a nonce and the executed resource", p.Hash, p.Nonce, len(p.Resources))
	}
	remHash := p.Hash
	out, err := runLast(nil, "plan", "web", "-n", d.ns)
	if err != nil || !strings.Contains(out, "Plan "+remHash+" (stage Remediate)") {
		t.Fatalf("kubectl last plan = %v:\n%s", err, out)
	}
	d.approve(remHash)
	intent = d.ready(key, 10*time.Minute)
	if intent.Status.Plan == nil || intent.Status.Plan.Hash != remHash {
		t.Errorf("status.plan after the remediation = %+v, want %s", intent.Status.Plan, remHash)
	}
	waitFor(t, 3*time.Minute, "two ready replicas", func() (bool, string) {
		got := kubectl(t, "get", "deployment", "web", "-n", d.ns, "-o", "jsonpath={.status.readyReplicas}")
		return got == "2", "readyReplicas " + got
	})

	entries := auditEntries(t, d.ns+"/web")
	begin, end, outcome := phaseOf(t, entries, lastv1alpha1.StageRemediate, remHash)
	if begin == nil || end == nil || outcome != "succeeded" {
		t.Fatalf("Remediate phase = begin %v, end %v, outcome %q; want a succeeded phase for %s", begin, end, outcome, remHash)
	}
	// The executed plan is the Create plan, unchanged by the remediation.
	var executed string
	for _, e := range entries {
		if e.Type == audit.TypePhaseEnd && (e.Stage == string(lastv1alpha1.StageCreate) || e.Stage == string(lastv1alpha1.StageUpdate)) {
			executed = e.SubjectHash
		}
	}
	if executed != wantCreate {
		t.Errorf("the last Create or Update phase.end has subject %s, want the unchanged %s", executed, wantCreate)
	}
	t.Logf("Remediate %s approved and succeeded (phase.begin seq %d, phase.end seq %d); executed plan %s unchanged", remHash, begin.Seq, end.Seq, executed)
	verifyAuditChain(t)
}

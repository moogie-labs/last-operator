//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// scriptFor points the scripted model at answers for the Intent ns/name, leaving other keys alone.
func (d *day2) scriptFor(name string, plans ...*lastv1alpha1.Plan) {
	d.t.Helper()
	d.key = llm.ScriptKey(d.ns + "/" + name)
	d.script(plans...)
}

// suggestFor scripts the answer to the suggest call (M7 design S10) for the
// Intent ns/name: one pattern per argv in patterns, each for entry. A suggest
// conversation always takes the first answer.
func (d *day2) suggestFor(name, entry string, patterns ...[]string) {
	d.t.Helper()
	type suggestion struct {
		Entry     string   `json:"entry"`
		Argv      []string `json:"argv"`
		Rationale string   `json:"rationale"`
	}
	var reply struct {
		Suggestions []suggestion `json:"suggestions"`
	}
	for _, p := range patterns {
		reply.Suggestions = append(reply.Suggestions, suggestion{Entry: entry, Argv: p, Rationale: "Scripted for " + name})
	}
	answer, err := json.Marshal(reply)
	if err != nil {
		d.t.Fatal(err)
	}
	d.setScript(llm.SuggestScriptPrefix+llm.ScriptKey(d.ns+"/"+name), []string{string(answer)})
}

// nginxPlan is the plan for an nginx Deployment called name. Its argv put every
// value that differs between two such plans after the three literals that
// rules.Generalize keeps, or in a flag value, so one generalized rule covers both.
func (d *day2) nginxPlan(name string) (*lastv1alpha1.Plan, string) {
	d.t.Helper()
	steps := []lastv1alpha1.Command{
		d.cmd("create", "Create the nginx Deployment", 2*time.Minute,
			"create", "deployment", name, "--image="+nginxImage, "-n", d.ns),
		d.cmd("rollout", "Wait until nginx is ready", 4*time.Minute,
			"rollout", "status", "deployment/"+name, "-n", d.ns, "--timeout=3m"),
	}
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "nginx web server " + d.ns + "/" + name,
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","namespace":"` + d.ns + `","name":"` + name + `"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "web-ready",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "deployment", name, "-n", d.ns, "-o", "json"}}, Providers: d.k8s},
			Assert:  "result.exitCode == 0 && has(result.json.status.readyReplicas) && result.json.status.readyReplicas >= 1"}},
		Teardown: []lastv1alpha1.Command{d.cmd("rm-deployment", "Delete the Deployment", 2*time.Minute,
			"delete", "deployment", name, "-n", d.ns, "--ignore-not-found")},
	}}
	hash, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		d.t.Fatal(err)
	}
	return &lastv1alpha1.Plan{Summary: "nginx " + name + " in " + d.ns, Steps: steps, Resources: resources}, hash
}

func (d *day2) request(name string) {
	d.t.Helper()
	out, err := runLast(nil, "Run an nginx web server named "+name+" in namespace "+d.ns, "-n", d.ns, "--name", name, "--no-follow")
	if err != nil {
		d.t.Fatalf("kubectl last: %v\n%s", err, out)
	}
}

// TestAllowRulesLetASimilarIntentRun: approving Intent A with --always-allow=generalized
// creates rules, and Intent B, which differs from A only in names, then runs with no
// IntentApproval at all, authorized by those rules, for its steps, probe and teardown.
func TestAllowRulesLetASimilarIntentRun(t *testing.T) {
	d := newDay2(t, "e2e-rules-")
	ctx := context.Background()

	planA, hashA := d.nginxPlan("web-a")
	planB, hashB := d.nginxPlan("web-b")
	d.scriptFor("web-a", planA)
	d.scriptFor("web-b", planB)

	// A: a person approves, and the rules are made on the way.
	d.request("web-a")
	keyA := client.ObjectKey{Namespace: d.ns, Name: "web-a"}
	d.awaiting(keyA, lastv1alpha1.StageCreate, 5*time.Minute)
	out, err := runLast(nil, "approve", "web-a", "-n", d.ns, "--hash", hashA, "--always-allow=generalized")
	if err != nil {
		t.Fatalf("kubectl last approve --always-allow: %v\n%s", err, out)
	}
	t.Logf("kubectl last approve --always-allow:\n%s", out)
	d.ready(keyA, 10*time.Minute)

	var rules lastv1alpha1.CommandRuleList
	if err := d.crc.List(ctx, &rules, client.InNamespace(d.ns), client.MatchingLabels{"last.moogie-labs.com/intent": "web-a"}); err != nil {
		t.Fatal(err)
	}
	if len(rules.Items) == 0 {
		t.Fatalf("no CommandRule labelled intent=web-a in %s after --always-allow", d.ns)
	}
	for _, r := range rules.Items {
		t.Logf("rule %s: stages %v providers %v match %v", r.Name, r.Spec.Stages, r.Spec.Providers, r.Spec.Match.Argv)
	}

	// B: no one approves.
	d.request("web-b")
	keyB := client.ObjectKey{Namespace: d.ns, Name: "web-b"}
	var inB lastv1alpha1.Intent
	waitFor(t, 10*time.Minute, "Intent B to be Ready without approval", func() (bool, string) {
		if err := d.crc.Get(ctx, keyB, &inB); err != nil {
			return false, err.Error()
		}
		if inB.Status.Phase == lastv1alpha1.PhaseReady {
			return true, ""
		}
		unc := ""
		if inB.Status.Plan != nil {
			unc = fmt.Sprint(inB.Status.Plan.UncoveredSteps)
		}
		return false, fmt.Sprintf("stage %q, phase %q, uncovered steps %s", inB.Status.Stage, inB.Status.Phase, unc)
	})
	if inB.Status.Plan == nil || inB.Status.Plan.Hash != hashB {
		t.Errorf("Intent B plan = %+v, want hash %s", inB.Status.Plan, hashB)
	}
	var approvals lastv1alpha1.IntentApprovalList
	if err := d.crc.List(ctx, &approvals, client.InNamespace(d.ns)); err != nil {
		t.Fatal(err)
	}
	for _, a := range approvals.Items {
		if a.Spec.IntentRef.Name == "web-b" {
			t.Errorf("IntentApproval %s exists for Intent B", a.Name)
		}
	}

	entries := auditEntries(t, d.ns+"/web-b")
	begin := newestBegin(entries, lastv1alpha1.StageCreate)
	if begin == nil {
		t.Fatal("no Create phase.begin for Intent B")
	}
	if begin.AuthorizedBy == nil || begin.AuthorizedBy.Kind != "rule" {
		t.Errorf("phase.begin authorizedBy = %+v, want kind rule", begin.AuthorizedBy)
	}
	var beginPayload struct {
		Rules map[string]string `json:"rules"`
	}
	if err := json.Unmarshal(begin.Payload, &beginPayload); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"create", "rollout", "probe:web-ready", "teardown:0.0"} {
		if beginPayload.Rules[id] == "" {
			t.Errorf("phase.begin rules lacks %q: %v", id, beginPayload.Rules)
		}
	}
	distinct := map[string]bool{}
	for _, ref := range beginPayload.Rules {
		distinct[ref] = true
	}
	observed := map[string]bool{}
	var starts int
	for _, e := range entries {
		switch e.Type {
		case audit.TypeApprovalObserved:
			t.Errorf("approval.observed entry at seq %d for Intent B", e.Seq)
		case audit.TypeRuleObserved:
			var p struct {
				Scope, Namespace, Name string
			}
			_ = json.Unmarshal(e.Payload, &p)
			ref := fmt.Sprintf("%s/%s/%s", p.Scope, p.Namespace, p.Name)
			if observed[ref] {
				t.Errorf("rule.observed twice for %s", ref)
			}
			observed[ref] = true
		case audit.TypeCommandStart:
			starts++
		}
	}
	if len(observed) != len(distinct) {
		t.Errorf("rule.observed refs = %v, want one per distinct rule in %v", observed, distinct)
	}
	for ref := range distinct {
		if !observed[ref] {
			t.Errorf("no rule.observed for %s", ref)
		}
	}
	if starts == 0 {
		t.Error("no command.start entries for Intent B")
	}
	t.Logf("Intent B phase.begin seq %d authorizedBy %+v rules %v; observed %v", begin.Seq, *begin.AuthorizedBy, beginPayload.Rules, observed)
	verifyAuditChain(t)
}

// TestDeletingAPartialCreateTearsItDown: a Create whose first step made a ConfigMap
// and whose second failed leaves the Intent Failed. Deleting it plans a Delete that
// runs the teardown of that partial plan, so the ConfigMap is gone with the Intent.
func TestDeletingAPartialCreateTearsItDown(t *testing.T) {
	d := newDay2(t, "e2e-partial-")
	ctx := context.Background()

	steps := []lastv1alpha1.Command{
		d.cmd("make", "Create the ConfigMap", time.Minute, "create", "configmap", "partial-cm", "--from-literal=a=b", "-n", d.ns),
		d.cmd("fail", "Read a ConfigMap that is not there", time.Minute, "get", "configmap", "does-not-exist", "-n", d.ns),
	}
	teardown := d.cmd("rm-cm", "Delete the ConfigMap", time.Minute, "delete", "configmap", "partial-cm", "-n", d.ns, "--ignore-not-found")
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "ConfigMap " + d.ns + "/partial-cm",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"ConfigMap","namespace":"` + d.ns + `","name":"partial-cm"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "cm-exists",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "configmap", "partial-cm", "-n", d.ns, "-o", "json"}}, Providers: d.k8s},
			Assert:  "result.exitCode == 0"}},
		Teardown: []lastv1alpha1.Command{teardown},
	}}
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	// The model repeats its plan after the failure, as often as the operator will ask, so the Intent fails.
	answer, err := agent.Encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{Summary: "A ConfigMap, then a failing read", Steps: steps, Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal(slices.Repeat([]string{answer}, agent.MaxAttempts+1))
	if err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: "last-gateway", Name: d.llmName}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[d.key] = string(script)
	if err := d.crc.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}

	d.request("web")
	key := client.ObjectKey{Namespace: d.ns, Name: "web"}
	d.awaiting(key, lastv1alpha1.StageCreate, 5*time.Minute)
	d.approve(wantCreate)
	var in lastv1alpha1.Intent
	waitFor(t, 10*time.Minute, "the Intent to fail", func() (bool, string) {
		if err := d.crc.Get(ctx, key, &in); err != nil {
			return false, err.Error()
		}
		return in.Status.Phase == lastv1alpha1.PhaseFailed, fmt.Sprintf("stage %q, phase %q", in.Status.Stage, in.Status.Phase)
	})
	if got := kubectl(t, "get", "configmap", "partial-cm", "-n", d.ns, "-o", "jsonpath={.data.a}"); got != "b" {
		t.Fatalf("partial-cm data a = %q after the first step, want b", got)
	}

	// Delete: the Delete plan, ordered by the model, is the teardown of the partial Create.
	deleteSteps := []lastv1alpha1.Command{teardown}
	wantDelete, err := plan.Hash(plan.Body{Steps: deleteSteps})
	if err != nil {
		t.Fatal(err)
	}
	d.script(&lastv1alpha1.Plan{Summary: "Tear down partial-cm in " + d.ns, Steps: deleteSteps})
	kubectl(t, "delete", "intent", "web", "-n", d.ns, "--wait=false")
	in = d.awaiting(key, lastv1alpha1.StageDelete, 5*time.Minute)
	if in.Status.Plan.Hash != wantDelete {
		t.Fatalf("Delete plan hash = %s, want %s", in.Status.Plan.Hash, wantDelete)
	}
	if len(in.Status.Plan.Steps) != 1 || !slices.Equal(in.Status.Plan.Steps[0].Run.Argv, teardown.Run.Argv) {
		t.Fatalf("Delete plan steps = %+v, want the teardown of partial-cm", in.Status.Plan.Steps)
	}
	d.approve(wantDelete)

	waitFor(t, 10*time.Minute, "the Intent to be gone", func() (bool, string) {
		err := d.crc.Get(ctx, key, &in)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("stage %q, phase %q, conditions %v", in.Status.Stage, in.Status.Phase, in.Status.Conditions)
	})
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "partial-cm"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap partial-cm after the Delete: %v", err)
	}
	_, end, outcome := phaseOf(t, auditEntries(t, d.ns+"/web"), lastv1alpha1.StageDelete, wantDelete)
	if end == nil || outcome != "succeeded" {
		t.Errorf("Delete phase end %v, outcome %q; want succeeded", end, outcome)
	}
	verifyAuditChain(t)
}

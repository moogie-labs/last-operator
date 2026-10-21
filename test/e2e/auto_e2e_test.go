//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// newDay2Auto is newDay2 with Auto switched on for the test's namespace only.
// The namespace's policy (the one that also sets the 1m probe interval) sets
// every stage key to Auto and allows it, and OperatorConfig.spec.defaults
// allows Auto for every key (restored on cleanup). The defaults do not set
// modes, so a namespace whose policies never say Auto stays Manual: the
// Manual tests, in other namespaces, are unaffected by the wider allowance.
func newDay2Auto(t *testing.T, prefix string) *day2 {
	t.Helper()
	d := newDay2(t, prefix)
	ctx := context.Background()
	allow := &lastv1alpha1.StageFlags{Discovery: true, Create: true, Reconcile: true, Delete: true}

	var pol lastv1alpha1.IntentPolicy
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "probes"}, &pol); err != nil {
		t.Fatal(err)
	}
	pol.Spec.Modes = &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeAuto, Create: lastv1alpha1.ModeAuto,
		Reconcile: lastv1alpha1.ModeAuto, Delete: lastv1alpha1.ModeAuto}
	pol.Spec.AllowAutoModes = allow
	if err := d.crc.Update(ctx, &pol); err != nil {
		t.Fatal(err)
	}

	var op lastv1alpha1.OperatorConfig
	if err := d.crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	prev := (*lastv1alpha1.StageFlags)(nil)
	if op.Spec.Defaults != nil && op.Spec.Defaults.AllowAutoModes != nil {
		c := *op.Spec.Defaults.AllowAutoModes
		prev = &c
	}
	// Registered after newDay2's cleanup, so it runs before it.
	t.Cleanup(func() {
		bg := context.Background()
		for range 5 {
			var cur lastv1alpha1.OperatorConfig
			if err := d.crc.Get(bg, client.ObjectKey{Name: "default"}, &cur); err != nil {
				t.Errorf("cleanup: read OperatorConfig: %v", err)
				return
			}
			if cur.Spec.Defaults != nil {
				cur.Spec.Defaults.AllowAutoModes = prev
			}
			if err := d.crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not restore OperatorConfig allowAutoModes")
	})
	for range 5 {
		if op.Spec.Defaults == nil {
			op.Spec.Defaults = &lastv1alpha1.IntentPolicySpec{}
		}
		op.Spec.Defaults.AllowAutoModes = allow
		if err := d.crc.Update(ctx, &op); err == nil {
			return d
		}
		if err := d.crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("could not allow Auto in OperatorConfig.spec.defaults")
	return d
}

// createAuto creates the Intent with no input for kubectl last to read: it has
// nothing to ask, and returns once the Intent is Ready.
func (d *day2) createAuto(wantHash string) client.ObjectKey {
	d.t.Helper()
	out, err := runLastInput(d.t, 15*time.Minute, "", "Run an nginx web server named web in namespace "+d.ns, "-n", d.ns, "--name", "web")
	if err != nil {
		d.t.Fatalf("kubectl last: %v\n%s", err, out)
	}
	d.t.Logf("kubectl last (Auto):\n%s", out)
	for _, want := range []string{"Modes: ", "create=Auto", "Plan " + wantHash + " may run without approval (Auto)", "Intent " + d.ns + "/web is Ready."} {
		if !strings.Contains(out, want) {
			d.t.Errorf("kubectl last output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Approve:") {
		d.t.Errorf("kubectl last offered an approval:\n%s", out)
	}
	return client.ObjectKey{Namespace: d.ns, Name: "web"}
}

// noApprovals fails the test if the namespace holds an IntentApproval.
func (d *day2) noApprovals() {
	d.t.Helper()
	var list lastv1alpha1.IntentApprovalList
	if err := d.crc.List(context.Background(), &list, client.InNamespace(d.ns)); err != nil {
		d.t.Fatal(err)
	}
	if len(list.Items) != 0 {
		d.t.Errorf("%d IntentApprovals in %s, want none in Auto", len(list.Items), d.ns)
	}
}

// newestBegin returns the newest phase.begin of stage.
func newestBegin(entries []audit.Entry, stage lastv1alpha1.Stage) *audit.Entry {
	var begin *audit.Entry
	for i := range entries {
		if entries[i].Type == audit.TypePhaseBegin && entries[i].Stage == string(stage) {
			begin = &entries[i]
		}
	}
	return begin
}

// checkAutoAudit requires a succeeded phase of stage whose phase.begin is
// authorized by auto/key, and no approval.observed entry for the Intent.
func (d *day2) checkAutoAudit(entries []audit.Entry, stage lastv1alpha1.Stage, key string) {
	d.t.Helper()
	begin := newestBegin(entries, stage)
	if begin == nil {
		d.t.Fatalf("no %s phase.begin in the audit", stage)
	}
	if begin.AuthorizedBy == nil || begin.AuthorizedBy.Kind != "auto" || begin.AuthorizedBy.Name != key {
		d.t.Errorf("%s phase.begin authorizedBy = %+v, want auto/%s", stage, begin.AuthorizedBy, key)
	}
	if _, end, outcome := phaseOf(d.t, entries, stage, begin.SubjectHash); end == nil || outcome != "succeeded" {
		d.t.Errorf("%s phase for %s: end %v, outcome %q; want succeeded", stage, begin.SubjectHash, end, outcome)
	}
	for _, e := range entries {
		if e.Type == audit.TypeApprovalObserved {
			d.t.Errorf("approval.observed entry at seq %d for an Auto Intent", e.Seq)
		}
	}
	d.t.Logf("%s phase.begin seq %d subject %s authorizedBy %+v", stage, begin.Seq, begin.SubjectHash, *begin.AuthorizedBy)
}

// TestAutoEditPlansAndRunsAnUpdate: with Auto allowed in every layer, the Create
// runs with no IntentApproval, and editing the spec runs the Update the same
// way, authorized by auto/create.
func TestAutoEditPlansAndRunsAnUpdate(t *testing.T) {
	d := newDay2Auto(t, "e2e-auto-upd-")
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
	updSteps := []lastv1alpha1.Command{d.cmd("rm-config", "Delete the ConfigMap the new request no longer wants", 2*time.Minute,
		"delete", "configmap", "web-config", "-n", d.ns, "--ignore-not-found")}
	updResources := []lastv1alpha1.ManagedResourceSpec{d.webResource(true)}
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	wantUpdate, err := plan.Hash(plan.Body{Steps: updSteps, Resources: updResources})
	if err != nil {
		t.Fatal(err)
	}
	d.script(&lastv1alpha1.Plan{Summary: "nginx and a ConfigMap in " + d.ns, Steps: steps, Resources: resources})
	key := d.createAuto(wantCreate)
	d.noApprovals()

	d.script(&lastv1alpha1.Plan{Summary: "Drop the ConfigMap in " + d.ns, Steps: updSteps, Resources: updResources})
	var intent lastv1alpha1.Intent
	if err := d.crc.Get(ctx, key, &intent); err != nil {
		t.Fatal(err)
	}
	if m := intent.Status.EffectiveModes; m == nil || m.Create != lastv1alpha1.ModeAuto {
		t.Fatalf("status.effectiveModes = %+v, want create=Auto", m)
	}
	intent.Spec.Request = "Run an nginx web server named web in namespace " + d.ns + ", without the ConfigMap"
	if err := d.crc.Update(ctx, &intent); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Minute, "the Update to run to Ready without approval", func() (bool, string) {
		if err := d.crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		if intent.Status.Phase == lastv1alpha1.PhaseAwaitingApproval {
			t.Fatalf("the Update plan %s awaits approval in Auto", intent.Status.Plan.Hash)
		}
		return intent.Status.Stage == lastv1alpha1.StageUpdate && intent.Status.Phase == lastv1alpha1.PhaseReady &&
				intent.Status.Plan != nil && intent.Status.Plan.Hash == wantUpdate,
			fmt.Sprintf("stage %q, phase %q", intent.Status.Stage, intent.Status.Phase)
	})
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap web-config after the Update: %v", err)
	}
	d.noApprovals()
	d.checkAutoAudit(auditEntries(t, d.ns+"/web"), lastv1alpha1.StageUpdate, "create")
	verifyAuditChain(t)
}

// TestAutoDriftIsRemediated: the Create plan includes a scale to 2 and a
// rollout status, and the scripted Remediate repeats both exactly, so it is
// within the executed plan and runs with no approval once the Deployment is
// scaled to zero.
func TestAutoDriftIsRemediated(t *testing.T) {
	d := newDay2Auto(t, "e2e-auto-rem-")
	ctx := context.Background()
	scale := func() lastv1alpha1.Command {
		return d.cmd("scale", "Scale web to 2", 2*time.Minute, "scale", "deployment/web", "--replicas=2", "-n", d.ns)
	}
	steps := []lastv1alpha1.Command{d.applyStep(d.deploymentManifest()), scale(), d.rolloutStep()}
	resources := []lastv1alpha1.ManagedResourceSpec{d.webResource(true)}
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	d.script(&lastv1alpha1.Plan{Summary: "nginx in " + d.ns, Steps: steps, Resources: resources})
	key := d.createAuto(wantCreate)
	d.noApprovals()

	// The Remediate repeats the executed scale and rollout steps exactly.
	d.script(&lastv1alpha1.Plan{Summary: "Scale web back to 2 in " + d.ns, Steps: []lastv1alpha1.Command{scale(), d.rolloutStep()},
		Resources: []lastv1alpha1.ManagedResourceSpec{}})
	kubectl(t, "scale", "deployment/web", "--replicas=0", "-n", d.ns)

	var intent lastv1alpha1.Intent
	sawDrifted := false
	waitFor(t, 10*time.Minute, "the drift to be remediated without approval", func() (bool, string) {
		if err := d.crc.Get(ctx, key, &intent); err != nil {
			return false, err.Error()
		}
		switch intent.Status.Phase {
		case lastv1alpha1.PhaseDrifted:
			sawDrifted = true
		case lastv1alpha1.PhaseAwaitingApproval:
			t.Fatalf("the %s plan %s awaits approval in Auto", intent.Status.Stage, intent.Status.Plan.Hash)
		}
		return intent.Status.Stage == lastv1alpha1.StageRemediate && intent.Status.Phase == lastv1alpha1.PhaseReady,
			fmt.Sprintf("stage %q, phase %q", intent.Status.Stage, intent.Status.Phase)
	})
	t.Logf("the Intent was seen Drifted: %v; now Ready after a Remediate", sawDrifted)
	waitFor(t, 3*time.Minute, "two ready replicas", func() (bool, string) {
		got := kubectl(t, "get", "deployment", "web", "-n", d.ns, "-o", "jsonpath={.status.readyReplicas}")
		return got == "2", "readyReplicas " + got
	})
	d.noApprovals()
	d.checkAutoAudit(auditEntries(t, d.ns+"/web"), lastv1alpha1.StageRemediate, "reconcile")
	verifyAuditChain(t)
}

// TestAutoDeleteTearsDown: deleting the Intent runs a scripted Delete plan that
// is a permutation of the executed teardown, with no approval.
func TestAutoDeleteTearsDown(t *testing.T) {
	d := newDay2Auto(t, "e2e-auto-del-")
	ctx := context.Background()
	manifest := d.deploymentManifest() + fmt.Sprintf(`---
apiVersion: v1
kind: ConfigMap
metadata: {name: web-config, namespace: %s}
data: {greeting: hello}
`, d.ns)
	steps := []lastv1alpha1.Command{d.applyStep(manifest), d.rolloutStep()}
	web := d.webResource(true)
	web.Teardown = append(web.Teardown, d.cmd("rm-config", "Delete the ConfigMap", 2*time.Minute, "delete", "configmap", "web-config", "-n", d.ns, "--ignore-not-found"))
	resources := []lastv1alpha1.ManagedResourceSpec{web}
	wantCreate, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	d.script(&lastv1alpha1.Plan{Summary: "nginx and a ConfigMap in " + d.ns, Steps: steps, Resources: resources})
	key := d.createAuto(wantCreate)
	d.noApprovals()

	// The Delete plan orders the same teardown commands the other way round.
	d.script(&lastv1alpha1.Plan{Summary: "Tear down web in " + d.ns, Steps: []lastv1alpha1.Command{web.Teardown[1], web.Teardown[0]}})
	kubectl(t, "delete", "intent", "web", "-n", d.ns, "--wait=false")
	var intent lastv1alpha1.Intent
	waitFor(t, 10*time.Minute, "the Intent to be torn down without approval", func() (bool, string) {
		err := d.crc.Get(ctx, key, &intent)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		if intent.Status.Phase == lastv1alpha1.PhaseAwaitingApproval {
			t.Fatalf("the Delete plan %s awaits approval in Auto", intent.Status.Plan.Hash)
		}
		return false, fmt.Sprintf("phase %q", intent.Status.Phase)
	})
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("Deployment web after the teardown: %v", err)
	}
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: "web-config"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap web-config after the teardown: %v", err)
	}
	d.noApprovals()
	d.checkAutoAudit(auditEntries(t, d.ns+"/web"), lastv1alpha1.StageDelete, "delete")
	verifyAuditChain(t)
}

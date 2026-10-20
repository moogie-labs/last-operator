package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	nonce1 = "0123456789abcdef0123456789abcdef"
	nonce2 = "fedcba9876543210fedcba9876543210"
)

// executedResources are the resources of executedEnv's executed Create plan.
func executedResources() []lastv1alpha1.ManagedResourceSpec {
	res := probedResource()
	res.Teardown = teardownCommands()
	return []lastv1alpha1.ManagedResourceSpec{res}
}

func repairSteps() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{argvStep("fix", kubectlBin, "apply", "-f", "/work/web.yaml")}
}

func stageApproval(name string, stage lastv1alpha1.Stage, subject string) *lastv1alpha1.IntentApproval {
	a := approvalFor(name, subject, lastv1alpha1.DecisionApprove, approver)
	a.Spec.Stage = stage
	return a
}

// recordPlan records b as the stored Intent's plan in stage, as the operator
// does, and returns its subject.
func (e *testEnv) recordPlan(stage lastv1alpha1.Stage, b plan.Body) string {
	e.t.Helper()
	var in lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		e.t.Fatal(err)
	}
	h, err := plan.Hash(b)
	if err != nil {
		e.t.Fatal(err)
	}
	in.Status.Stage, in.Status.Phase = stage, lastv1alpha1.PhaseAwaitingApproval
	in.Status.Plan = &lastv1alpha1.Plan{Hash: h, Steps: b.Steps, Resources: b.Resources, Nonce: b.Nonce}
	if err := e.crd.Update(e.ctx, &in); err != nil {
		e.t.Fatal(err)
	}
	return h
}

// remediation records a Remediate plan of repairSteps with the executed
// resources and nonce, approves it, and returns its subject.
func (e *testEnv) remediation(name, nonce string) string {
	e.t.Helper()
	h := e.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce})
	e.approve(stageApproval(name, lastv1alpha1.StageRemediate, h))
	return h
}

// runPhase opens stage's phase for subject, runs steps, and ends it as succeeded.
func (e *testEnv) runPhase(stage lastv1alpha1.Stage, subject string, steps []lastv1alpha1.Command) {
	e.t.Helper()
	if _, err := e.beginStage(stage, subject); err != nil {
		e.t.Fatalf("BeginPhase(%s) = %v", stage, err)
	}
	for _, s := range steps {
		if _, err := e.run(subject, s); err != nil {
			e.t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if _, err := e.end(subject, true); err != nil {
		e.t.Fatalf("EndPhase(%s) = %v", stage, err)
	}
}

func (e *testEnv) executed() ExecutedPlan {
	e.t.Helper()
	rec, err := e.g.state.Load(e.ctx, envKey, envIntentUID)
	if err != nil || rec.Executed == nil {
		e.t.Fatalf("record = %+v, %v", rec, err)
	}
	return *rec.Executed
}

// refusedWith asserts a BeginPhase refusal: its code and reason, audited as
// policy.denied, and no phase.begin written since before.
func (e *testEnv) refusedWith(err error, code codes.Code, reason string, before int) {
	e.t.Helper()
	e.wantDenied(err, code)
	if !strings.Contains(err.Error(), reason) {
		e.t.Fatalf("BeginPhase = %v, want %q", err, reason)
	}
	for _, en := range e.entries()[before:] {
		if en.Type == audit.TypePhaseBegin {
			e.t.Fatal("a refused BeginPhase wrote phase.begin")
		}
	}
}

func TestUpdateNeedsAnExecutedPlan(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newProviderEnv(t, in, stageApproval("ok", lastv1alpha1.StageUpdate, in.Status.Plan.Hash))
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: twoSteps()})
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent has no executed plan to update", 0)
}

func TestASucceededUpdateReplacesTheExecutedPlan(t *testing.T) {
	env, in := executedEnv(t)
	before := env.executed()
	if before.Began == "" {
		t.Fatal("the executed Create plan has no Began")
	}
	// The Update manages a different resource with a single probe.
	res := probedResource()
	res.Description = "namespace, relabelled"
	res.Probes = res.Probes[:1]
	steps := []lastv1alpha1.Command{argvStep("u1", kubectlBin, "label", "ns", "team-x", "tier=web")}
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{res}})
	if h == in.Status.Plan.Hash {
		t.Fatal("the Update plan has the Create plan's subject")
	}
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	begun := time.Now().UTC().Truncate(time.Second)
	env.runPhase(lastv1alpha1.StageUpdate, h, steps)

	got := env.executed()
	if got.Stage != string(lastv1alpha1.StageUpdate) || got.PlanHash != h {
		t.Fatalf("executed = %s %s, want the Update %s", got.Stage, got.PlanHash, h)
	}
	if at, err := time.Parse(time.RFC3339, got.Began); err != nil || at.Before(begun) || at.After(time.Now()) {
		t.Fatalf("Began = %q, %v; want the Update's BeginPhase time", got.Began, err)
	}
	resp, err := env.probe()
	if err != nil || resp.GetPlanHash() != h || len(resp.GetOutcomes()) != 1 {
		t.Fatalf("RunProbes = %+v, %v; want the Update plan's one probe", resp, err)
	}
	// The Update ran once; its approval opens it no more.
	_, err = env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "runs only once", len(env.entries()))
}

func TestRemediateNeedsTheExecutedResources(t *testing.T) {
	// No executed plan: nothing to remediate.
	fresh, _ := newRunEnv(t, twoSteps(), probedResource())
	h := fresh.remediation("fix", nonce1)
	_, err := fresh.beginStage(lastv1alpha1.StageRemediate, h)
	fresh.refusedWith(err, codes.FailedPrecondition, "intent has no executed plan to remediate", 0)

	env, _ := executedEnv(t)
	for name, resources := range map[string][]lastv1alpha1.ManagedResourceSpec{
		"changed": {probedResource()}, // the teardown is gone
		"dropped": nil,
		"added":   append(executedResources(), lastv1alpha1.ManagedResourceSpec{Description: "extra"}),
	} {
		h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: resources, Nonce: nonce1})
		env.approve(stageApproval("fix-"+name, lastv1alpha1.StageRemediate, h))
		before := len(env.entries())
		_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
		env.refusedWith(err, codes.FailedPrecondition, "a Remediate plan must carry the executed plan's resources unchanged", before)
	}

	// The IntentRef the operator fills in is not a change.
	resources := executedResources()
	resources[0].IntentRef = &lastv1alpha1.LocalRef{Name: envIntent}
	h = env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: resources, Nonce: nonce2})
	env.approve(stageApproval("fix-ref", lastv1alpha1.StageRemediate, h))
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
		t.Fatalf("BeginPhase(Remediate with intentRef) = %v", err)
	}
}

func TestASucceededRemediateKeepsTheExecutedPlan(t *testing.T) {
	env, in := executedEnv(t)
	before := env.executed()
	h := env.remediation("fix", nonce1)
	env.runPhase(lastv1alpha1.StageRemediate, h, repairSteps())

	if got := env.executed(); got != before {
		t.Fatalf("executed = %+v, want the Create plan kept: %+v", got, before)
	}
	resp, err := env.probe()
	if err != nil || resp.GetPlanHash() != in.Status.Plan.Hash {
		t.Fatalf("RunProbes = %+v, %v; want the Create plan's probes", resp, err)
	}
}

func TestARemediateWritesNoOutputs(t *testing.T) {
	env, _ := executedEnv(t)
	h := env.remediation("fix", nonce1)
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, repairSteps()[0]); err != nil {
		t.Fatal(err)
	}
	_, err := env.g.writeOutputSecret(env.ctx, &gatewayv1.WriteOutputSecretRequest{Intent: ref(), SubjectHash: h,
		ManagedResource: envIntent + "-0", Values: map[string]string{"k": "${steps.fix.stdout}"}})
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "Remediate") {
		t.Fatalf("WriteOutputSecret = %v", err)
	}
}

func TestRemediateNeedsANonceAndOtherStagesRefuseOne(t *testing.T) {
	env, _ := executedEnv(t)
	for _, nonce := range []string{"", "not-hex", strings.ToUpper(nonce1), nonce1 + "00"} {
		h := env.remediation("fix-"+nonce, nonce)
		before := len(env.entries())
		_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
		env.refusedWith(err, codes.InvalidArgument, "nonce", before)
	}

	withNonce := plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce1}
	for _, stage := range []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate} {
		h := env.recordPlan(stage, withNonce)
		env.approve(stageApproval("nonce-"+strings.ToLower(string(stage)), stage, h))
		before := len(env.entries())
		_, err := env.beginStage(stage, h)
		env.refusedWith(err, codes.InvalidArgument, "nonce", before)
	}
	env.startDelete(teardownCommands())
	h := env.recordPlan(lastv1alpha1.StageDelete, plan.Body{Steps: teardownCommands(), Nonce: nonce1})
	env.approve(deleteApproval("nonce-delete", h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.refusedWith(err, codes.InvalidArgument, "nonce", before)
}

// The same drift twice gets two remediations with identical steps: each has its
// own subject, and the first approval never authorizes the second (Review Focus 5).
func TestTheSameRemediationTwiceNeedsTwoApprovals(t *testing.T) {
	env, _ := executedEnv(t)
	first := env.remediation("fix-1", nonce1)
	env.runPhase(lastv1alpha1.StageRemediate, first, repairSteps())

	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageRemediate, first)
	env.refusedWith(err, codes.FailedPrecondition, "runs only once", before)

	second := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce2})
	if second == first {
		t.Fatal("a new nonce kept the subject")
	}
	before = len(env.entries())
	_, err = env.beginStage(lastv1alpha1.StageRemediate, second)
	env.refusedWith(err, codes.FailedPrecondition, msgNoApproval, before)

	env.approve(stageApproval("fix-2", lastv1alpha1.StageRemediate, second))
	env.runPhase(lastv1alpha1.StageRemediate, second, repairSteps())
}

func TestUpdateAndRemediateRespectTheRecordLimit(t *testing.T) {
	env, _ := executedEnv(t)
	big := bigFilePlan(t, `"\`, MaxPlanBytes/4-200).Status.Plan.Steps // every byte is escaped twice in the record

	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: big})
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.InvalidArgument, "plan too large to record", before)

	// A Remediate is never recorded as the executed plan, so the same steps open.
	kept := env.executed()
	h = env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: big, Resources: executedResources(), Nonce: nonce1})
	env.approve(stageApproval("fix", lastv1alpha1.StageRemediate, h))
	env.runPhase(lastv1alpha1.StageRemediate, h, big)
	if got := env.executed(); got != kept {
		t.Fatalf("executed = %+v, want %+v", got, kept)
	}
}

func TestAnIntentBeingDeletedOpensNoUpdateOrRemediate(t *testing.T) {
	env, _ := executedEnv(t)
	env.startDelete(teardownCommands())
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: repairSteps(), Resources: executedResources()})
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent is being deleted", before)

	h = env.remediation("fix", nonce1)
	before = len(env.entries())
	_, err = env.beginStage(lastv1alpha1.StageRemediate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent is being deleted", before)
}

// controlledMR is a ManagedResource in the env's namespace controlled by the
// object with UID controller, or only owned by it when controls is false.
func controlledMR(name string, controller types.UID, controls bool) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNS, UID: types.UID("uid-" + name),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: "Intent",
				Name: envIntent, UID: controller, Controller: new(controls)}}},
		Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: envIntent}},
	}
}

// createMRs creates the env's ManagedResources: web-0 to web-<owned-1> controlled
// by the Intent, web-9 controlled by another object, and web-8 owned by the
// Intent without controlling it.
func (e *testEnv) createMRs(owned int) {
	e.t.Helper()
	mrs := make([]*lastv1alpha1.ManagedResource, 0, owned+2)
	mrs = append(mrs, controlledMR(lastv1alpha1.ManagedResourceName(envIntent, 9), "other-uid", true),
		controlledMR(lastv1alpha1.ManagedResourceName(envIntent, 8), envIntentUID, false))
	for i := range owned {
		mrs = append(mrs, controlledMR(lastv1alpha1.ManagedResourceName(envIntent, i), envIntentUID, true))
	}
	for _, mr := range mrs {
		if err := e.crd.Create(e.ctx, mr); err != nil {
			e.t.Fatal(err)
		}
	}
}

// mrExists reports whether the ManagedResource is still in the cluster.
func (e *testEnv) mrExists(name string) bool {
	e.t.Helper()
	err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: name}, &lastv1alpha1.ManagedResource{})
	if err != nil && !apierrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	return err == nil
}

// wantMRs asserts which of web-0 to web-9 remain.
func (e *testEnv) wantMRs(remaining ...int) {
	e.t.Helper()
	for i := range 10 {
		name := lastv1alpha1.ManagedResourceName(envIntent, i)
		if want := slices.Contains(remaining, i); e.mrExists(name) != want {
			e.t.Fatalf("ManagedResource %s exists = %t, want %t", name, !want, want)
		}
	}
}

// deletedMRs returns the managedresource.deleted payloads written for subject
// since entry from, failing on any that is not the Update's.
func (e *testEnv) deletedMRs(from int, subject string) []map[string]string {
	e.t.Helper()
	var out []map[string]string
	for _, en := range e.entries()[from:] {
		if en.Type != audit.TypeManagedResourceDeleted {
			continue
		}
		if en.Intent != envKey || en.Stage != string(lastv1alpha1.StageUpdate) || en.SubjectHash != subject ||
			en.Actor != testOperator || en.AuthorizedBy == nil {
			e.t.Fatalf("managedresource.deleted = %+v", en)
		}
		var p map[string]string
		if err := json.Unmarshal(en.Payload, &p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func deletedPayload(i int) map[string]string {
	name := lastv1alpha1.ManagedResourceName(envIntent, i)
	return map[string]string{"managedResource": name, "uid": "uid-" + name}
}

// countType counts the entries of type typ since entry from.
func (e *testEnv) countType(from int, typ string) int {
	e.t.Helper()
	n := 0
	for _, en := range e.entries()[from:] {
		if en.Type == typ {
			n++
		}
	}
	return n
}

// oneResourceUpdate records and approves an Update plan of one step and the
// executed plan's one resource, and returns its subject and steps.
func (e *testEnv) oneResourceUpdate() (string, []lastv1alpha1.Command) {
	e.t.Helper()
	steps := []lastv1alpha1.Command{argvStep("u1", kubectlBin, "delete", "deployment", "web-extra", "-n", envNS)}
	h := e.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: steps, Resources: executedResources()})
	e.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	return h, steps
}

// An Update that drops resources leaves no ManagedResource of the Intent behind
// beyond the new plan's, each deletion audited (ruling R1, Review Focus 3).
func TestASucceededUpdateDeletesStaleManagedResources(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(3)
	h, steps := env.oneResourceUpdate()
	before := len(env.entries())
	env.runPhase(lastv1alpha1.StageUpdate, h, steps)

	env.wantMRs(0, 8, 9)
	got := env.deletedMRs(before, h)
	if want := []map[string]string{deletedPayload(1), deletedPayload(2)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads = %v, want %v", got, want)
	}
	// Each deletion follows the phase.end that made the Update the executed plan.
	es := env.entries()[before:]
	end := slices.IndexFunc(es, func(en audit.Entry) bool { return en.Type == audit.TypePhaseEnd })
	first := slices.IndexFunc(es, func(en audit.Entry) bool { return en.Type == audit.TypeManagedResourceDeleted })
	if end < 0 || first < end {
		t.Fatalf("phase.end at %d, first managedresource.deleted at %d", end, first)
	}
	if env.g.phases.get(envKey) != nil {
		t.Fatal("the Update phase is still in memory")
	}
}

// Only a succeeded Update deletes: a failed one and a succeeded Remediate,
// which carries the executed resources unchanged, delete nothing.
func TestAFailedUpdateDeletesNothing(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(3)
	h, _ := env.oneResourceUpdate()
	before := len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatalf("EndPhase(Update, failed) = %v", err)
	}
	env.wantMRs(0, 1, 2, 8, 9)

	fix := env.remediation("fix", nonce1)
	env.runPhase(lastv1alpha1.StageRemediate, fix, repairSteps())
	env.wantMRs(0, 1, 2, 8, 9)
	if n := env.countType(before, audit.TypeManagedResourceDeleted); n != 0 {
		t.Fatalf("%d managedresource.deleted entries, want none", n)
	}
}

func TestStaleManagedResourceDeletionIsRetried(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(5)
	h, steps := env.oneResourceUpdate()
	var deletes atomic.Int32
	env.g.crd = interceptor.NewClient(env.crd.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*lastv1alpha1.ManagedResource); !ok {
				return c.Delete(ctx, obj, opts...)
			}
			switch n := deletes.Add(1); {
			case n == 2: // web-2, the first attempt
				return errors.New("api server unavailable")
			case obj.GetName() == lastv1alpha1.ManagedResourceName(envIntent, 3):
				// Deleted concurrently: the gateway's own delete finds it gone.
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				return apierrors.NewNotFound(lastv1alpha1.GroupVersion.WithResource("managedresources").GroupResource(), obj.GetName())
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())

	_, err := env.end(h, true)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("EndPhase with a failing delete = %v, want Unavailable", err)
	}
	if env.g.phases.get(envKey) == nil {
		t.Fatal("the Update phase left memory before its stale ManagedResources were deleted")
	}
	env.wantMRs(0, 2, 3, 4, 8, 9)
	if got := env.executed(); got.PlanHash != h {
		t.Fatalf("executed = %s, want the Update %s", got.PlanHash, h)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase retry = %v", err)
	}
	env.wantMRs(0, 8, 9)
	if n := env.countType(before, audit.TypePhaseEnd); n != 1 {
		t.Fatalf("%d phase.end entries, want 1", n)
	}
	got := env.deletedMRs(before, h)
	if want := []map[string]string{deletedPayload(1), deletedPayload(2), deletedPayload(4)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads = %v, want %v (web-3 was already gone)", got, want)
	}
	if env.g.phases.get(envKey) != nil {
		t.Fatal("the Update phase is still in memory")
	}
}

// A deletion whose audit entry could not be written is audited by the retry,
// once, and never deleted again.
func TestAStaleDeletionIsAuditedWhenItsEntryFailed(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	h, steps := env.oneResourceUpdate()
	env.g.crd = interceptor.NewClient(env.crd.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*lastv1alpha1.ManagedResource); ok {
				env.sink.SetFailPuts(true) // the deletion went through; its entry will not
			}
			return nil
		},
	})
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("EndPhase with the audit log down = %v, want Unavailable", err)
	}
	env.wantMRs(0, 8, 9)
	if n := env.countType(before, audit.TypeManagedResourceDeleted); n != 0 {
		t.Fatalf("%d managedresource.deleted entries, want none yet", n)
	}

	// The broken writer fails closed; give the same gateway a healthy one.
	env.sink.SetFailPuts(false)
	w, err := audit.Open(context.Background(), env.sink, env.signer,
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: sandbox.GatewayActor}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	env.g.writer.Store(w)
	env.g.crd = env.crd
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase retry = %v", err)
	}
	if got, want := env.deletedMRs(before, h), []map[string]string{deletedPayload(1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads = %v, want %v", got, want)
	}
	if n := env.countType(before, audit.TypePhaseEnd); n != 1 {
		t.Fatalf("%d phase.end entries, want 1", n)
	}
}

// sweptMRs returns the managedresource.deleted payloads written since entry
// from, failing on any not written by the gateway under the executed subject.
func (e *testEnv) sweptMRs(from int, stage lastv1alpha1.Stage, subject string) []map[string]string {
	e.t.Helper()
	var out []map[string]string
	for _, en := range e.entries()[from:] {
		if en.Type != audit.TypeManagedResourceDeleted {
			continue
		}
		if en.Intent != envKey || en.Stage != string(stage) || en.SubjectHash != subject || en.Actor != sandbox.GatewayActor {
			e.t.Fatalf("managedresource.deleted = %+v", en)
		}
		var p map[string]string
		if err := json.Unmarshal(en.Payload, &p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// The record sweep deletes, and audits, the ManagedResources the Intent controls
// beyond the executed plan's resources; any other ManagedResource stays.
func TestTheSweepDeletesManagedResourcesBeyondTheExecutedPlan(t *testing.T) {
	env, in := executedEnv(t)
	env.createMRs(2)
	before := len(env.entries())
	env.g.sweepRecords(env.ctx)

	env.wantMRs(0, 8, 9)
	got := env.sweptMRs(before, lastv1alpha1.StageCreate, in.Status.Plan.Hash)
	if want := []map[string]string{deletedPayload(1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads = %v, want %v", got, want)
	}
	if !env.recordExists() {
		t.Fatal("the sweep deleted the record of a live Intent")
	}
	// Nothing is left: the next sweep deletes and writes nothing.
	before = len(env.entries())
	env.g.sweepRecords(env.ctx)
	if n := len(env.entries()) - before; n != 0 {
		t.Fatalf("the second sweep wrote %d entries", n)
	}
}

// A restart between EndPhase's record save and its deletions loses the phase
// that would retry them; the new leader's sweep finishes them.
func TestTheSweepFinishesAnUpdateCleanupCutShortByARestart(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(3)
	h, steps := env.oneResourceUpdate()
	env.g.crd = interceptor.NewClient(env.crd.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == lastv1alpha1.ManagedResourceName(envIntent, 2) {
				return errors.New("api server unavailable")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("EndPhase with a failing delete = %v, want Unavailable", err)
	}
	env.wantMRs(0, 2, 8, 9)

	before := len(env.entries())
	env.g = env.newGateway()
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 8, 9)
	got := env.sweptMRs(before, lastv1alpha1.StageUpdate, h)
	if want := []map[string]string{deletedPayload(2)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads from the sweep = %v, want %v", got, want)
	}
}

// While a phase is open, in the record or only in memory, the sweep deletes
// nothing: its operator may have created the next plan's ManagedResources.
func TestTheSweepLeavesManagedResourcesWhileAPhaseIsOpen(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	fix := env.remediation("fix", nonce1)
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, fix); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 1, 8, 9)

	// The record no longer shows the phase, but this process still holds it.
	rec, err := env.g.state.Load(env.ctx, envKey, envIntentUID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Open = nil
	if err := env.g.saveRecord(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 1, 8, 9)
	if n := env.countType(before, audit.TypeManagedResourceDeleted); n != 0 {
		t.Fatalf("%d managedresource.deleted entries, want none", n)
	}
}

func TestTheSweepSkipsTheManagedResourcesOfABusyIntent(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	unlock := env.g.locks.lock(envKey)
	env.g.sweepRecords(env.ctx)
	unlock()
	env.wantMRs(0, 1, 8, 9)
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 8, 9)
}

// failMRDeletes makes the gateway's ManagedResource deletes fail while fail is
// set, and counts every ManagedResource delete it is asked for.
func (e *testEnv) failMRDeletes(fail *atomic.Bool) *atomic.Int32 {
	var calls atomic.Int32
	e.g.crd = interceptor.NewClient(e.crd.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*lastv1alpha1.ManagedResource); ok {
				calls.Add(1)
				if fail.Load() {
					return errors.New("api server unavailable")
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	return &calls
}

// An Update whose cleanup is still pending never blocks the next phase: once
// the operator has moved on (its EndPhase(failed) was refused as already
// succeeded), BeginPhase finishes the cleanup itself.
func TestABeginPhaseFinishesAnEndingUpdatesCleanup(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	h, steps := env.oneResourceUpdate()
	var failing atomic.Bool
	failing.Store(true)
	env.failMRDeletes(&failing)
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("EndPhase with a failing delete = %v, want Unavailable", err)
	}
	_, err := env.end(h, false) // the operator aborts the phase
	env.wantDenied(err, codes.FailedPrecondition)

	fix := env.remediation("fix", nonce1)
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, fix); status.Code(err) != codes.Unavailable {
		t.Fatalf("BeginPhase(Remediate) while the cleanup fails = %v, want Unavailable", err)
	}
	env.wantMRs(0, 1, 8, 9)
	p := env.g.phases.get(envKey)
	p.mu.Lock()
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, fix); status.Code(err) != codes.Aborted {
		t.Fatalf("BeginPhase(Remediate) while the Update is held = %v, want Aborted", err)
	}
	p.mu.Unlock()

	failing.Store(false)
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, fix); err != nil {
		t.Fatalf("BeginPhase(Remediate) once the delete works = %v", err)
	}
	env.wantMRs(0, 8, 9)
	if got, want := env.deletedMRs(before, h), []map[string]string{deletedPayload(1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("managedresource.deleted payloads = %v, want %v", got, want)
	}
	if n := env.countType(before, audit.TypePhaseEnd); n != 1 {
		t.Fatalf("%d phase.end entries, want the Update's one", n)
	}
}

// No ManagedResource is deleted while the audit writer is broken: its
// deletion could not be audited.
func TestNoStaleDeletionWithABrokenAuditWriter(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	var failing atomic.Bool
	calls := env.failMRDeletes(&failing)
	env.sink.SetFailPuts(true)
	if _, err := env.g.writer.Load().Append(env.ctx, audit.Draft{Type: audit.TypeLLMRequest, Actor: testOperator, Intent: envKey}); err == nil {
		t.Fatal("an append with the sink down succeeded")
	}
	env.sink.SetFailPuts(false)
	if !env.g.writer.Load().Broken() {
		t.Fatal("the writer is not broken")
	}
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 1, 8, 9)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d ManagedResource deletes with a broken writer, want none", n)
	}
}

// A ManagedResource already being deleted, held by a finalizer, is neither
// deleted nor audited again.
func TestATerminatingManagedResourceIsNotDeletedAgain(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(1)
	held := controlledMR(lastv1alpha1.ManagedResourceName(envIntent, 1), envIntentUID, true)
	held.Finalizers = []string{"example.com/hold"}
	if err := env.crd.Create(env.ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := env.crd.Delete(env.ctx, held); err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	calls := env.failMRDeletes(&failing)
	before := len(env.entries())
	env.g.sweepRecords(env.ctx)
	env.wantMRs(0, 1, 8, 9)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d deletes of a terminating ManagedResource, want none", n)
	}
	if n := env.countType(before, audit.TypeManagedResourceDeleted); n != 0 {
		t.Fatalf("%d managedresource.deleted entries, want none", n)
	}
}

// gatewayAuditor serves the operator's audit reads from the env's gateway.
type gatewayAuditor struct{ e *testEnv }

func (a gatewayAuditor) QueryAudit(ctx context.Context, in *gatewayv1.QueryAuditRequest, _ ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error) {
	return a.e.g.queryAudit(ctx, in)
}

// operatorSeesExecuted asserts that the operator's audit read finds subject as
// the executed plan.
func (e *testEnv) operatorSeesExecuted(subject string) {
	e.t.Helper()
	_, hash, found, err := agent.ExecutedPlan(e.ctx, gatewayAuditor{e}, ref())
	if err != nil || !found || hash != subject {
		e.t.Fatalf("ExecutedPlan = %s, found %v, %v; want %s", hash, found, err, subject)
	}
}

// The executed plan's phase.begin and phase.end never age out of the index, so
// the operator still finds the plan to update or remediate after many more
// indexed entries; QueryAudit still reports the truncation.
func TestTheExecutedPhaseOutlivesIndexTrimming(t *testing.T) {
	env, _ := executedEnv(t)
	h, steps := env.oneResourceUpdate()
	env.runPhase(lastv1alpha1.StageUpdate, h, steps)
	if ex := env.executed(); ex.BeginSeq == 0 || ex.EndSeq <= ex.BeginSeq {
		t.Fatalf("executed = begin %d, end %d; want both recorded", ex.BeginSeq, ex.EndSeq)
	}
	for range 300 {
		env.indexedEntry()
	}
	resp, err := env.query(&gatewayv1.QueryAuditRequest{Types: []string{audit.TypePhaseBegin}})
	if err != nil || !resp.GetTruncated() {
		t.Fatalf("QueryAudit = %+v, %v; want truncated", resp, err)
	}
	env.operatorSeesExecuted(h)
	if rec, _ := env.g.state.LoadAny(env.ctx, envKey); len(rec.Index) > maxIndexEntries {
		t.Fatalf("index holds %d entries; the limit is %d", len(rec.Index), maxIndexEntries)
	}
}

// A recreated Intent inherits its predecessor's last seq as its index floor,
// so its every query is truncated; its own succeeded Create is still found.
func TestARecreatedIntentFindsItsOwnExecutedPlan(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newProviderEnv(t, in, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	env.indexedEntry()
	head, _, _ := env.g.writer.Load().Head()
	rec, _ := env.g.state.LoadAny(env.ctx, envKey)
	rec.IntentUID = testOldUID
	rec.AddIndex([]IndexEntry{{Seq: head, Type: audit.TypeLLMRequest}})
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.g.index.take(envKey)
	executeAll(t, env, in)
	if resp, err := env.query(&gatewayv1.QueryAuditRequest{}); err != nil || !resp.GetTruncated() {
		t.Fatalf("QueryAudit = %+v, %v; want truncated by the inherited floor", resp, err)
	}
	env.operatorSeesExecuted(in.Status.Plan.Hash)
}

const (
	keptSecret = "web-outputs"
	u1Password = "${steps.u1.json.password}"
)

// dropTheFirstResource seeds what an executed plan [r0, r1 with outputs] left
// for an Update that keeps only r1: web-0 and web-1 controlled by the Intent,
// with web-0 already rewritten to the kept resource, as the operator does, and
// r1's output Secret, controlled by web-1. It records and approves the Update
// plan, with the given outputs, and returns its subject and steps.
func (e *testEnv) dropTheFirstResource(outputs map[string]string) (string, []lastv1alpha1.Command) {
	e.t.Helper()
	e.createMRs(2)
	kept := executedResources()[0]
	kept.OutputsSecretRef, kept.Outputs = &lastv1alpha1.LocalRef{Name: keptSecret}, outputs
	var mr lastv1alpha1.ManagedResource
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: lastv1alpha1.ManagedResourceName(envIntent, 0)}, &mr); err != nil {
		e.t.Fatal(err)
	}
	mr.Spec = *kept.DeepCopy()
	mr.Spec.IntentRef = &lastv1alpha1.LocalRef{Name: envIntent}
	if err := e.crd.Update(e.ctx, &mr); err != nil {
		e.t.Fatal(err)
	}
	e.outputSecret(lastv1alpha1.ManagedResourceName(envIntent, 1), envIntentUID)
	steps := []lastv1alpha1.Command{argvStep("u1", kubectlBin, "delete", "deployment", "web-first", "-n", envNS)}
	h := e.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{kept}})
	e.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	return h, steps
}

// outputSecret creates keptSecret as an output Secret the gateway wrote for the
// Intent of intentUID, controlled by the ManagedResource mr (UID "uid-<mr>").
func (e *testEnv) outputSecret(mr, intentUID string) {
	e.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: keptSecret, Namespace: envNS,
		Labels:          map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelIntentUID: intentUID},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: kindManagedResource, Name: mr, UID: types.UID("uid-" + mr), Controller: new(true)}}},
		Data: map[string][]byte{passwordKey: []byte("old")}}
	if _, err := e.kube.CoreV1().Secrets(envNS).Create(e.ctx, s, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

// wantSecretControlledBy asserts keptSecret's one controller ownerReference.
func (e *testEnv) wantSecretControlledBy(mr string) *corev1.Secret {
	e.t.Helper()
	s := e.secret(keptSecret)
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != mr || s.OwnerReferences[0].UID != types.UID("uid-"+mr) ||
		s.OwnerReferences[0].Controller == nil || !*s.OwnerReferences[0].Controller {
		e.t.Fatalf("secret %s owners = %+v, want %s", keptSecret, s.OwnerReferences, mr)
	}
	return s
}

// An Update that drops r0 and keeps r1 rewrites web-0 to r1; r1's output Secret,
// still controlled by web-1, moves to web-0 when the plan declares it there, so
// it survives web-1's deletion.
func TestAnUpdateMovesAKeptOutputSecretToItsNewManagedResource(t *testing.T) {
	web0, web1 := lastv1alpha1.ManagedResourceName(envIntent, 0), lastv1alpha1.ManagedResourceName(envIntent, 1)
	t.Run("written by the Update", func(t *testing.T) {
		env, _ := executedEnv(t)
		outputs := map[string]string{passwordKey: u1Password}
		h, steps := env.dropTheFirstResource(outputs)
		if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(h, steps[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := env.writeOutputs(h, web0, outputs); err != nil {
			t.Fatalf("WriteOutputSecret = %v", err)
		}
		if last := env.last(); last.Type != audit.TypeOutputWritten || last.Stage != string(lastv1alpha1.StageUpdate) {
			t.Fatalf("last entry = %+v, want output.written", last)
		}
		if _, err := env.end(h, true); err != nil {
			t.Fatal(err)
		}
		env.wantMRs(0, 8, 9)
		if s := env.wantSecretControlledBy(web0); string(s.Data[passwordKey]) != outputValue {
			t.Fatalf("secret data = %q, want the Update's output", s.Data[passwordKey])
		}
	})
	t.Run("left out of the Update", func(t *testing.T) {
		env, _ := executedEnv(t)
		h, steps := env.dropTheFirstResource(nil)
		before := len(env.entries())
		env.runPhase(lastv1alpha1.StageUpdate, h, steps)
		env.wantMRs(0, 8, 9)
		if s := env.wantSecretControlledBy(web0); string(s.Data[passwordKey]) != "old" {
			t.Fatalf("secret data = %q, want it untouched", s.Data[passwordKey])
		}
		var moved []audit.Entry
		for _, en := range env.entries()[before:] {
			if en.Type == audit.TypeOutputMoved {
				moved = append(moved, en)
			}
		}
		if len(moved) != 1 || moved[0].SubjectHash != h || moved[0].AuthorizedBy == nil ||
			string(moved[0].Payload) != `{"from":"web-1","secret":"web-outputs","to":"web-0"}` {
			t.Fatalf("output.moved entries = %+v, want one naming the move", moved)
		}
	})
	t.Run("never a Secret the plan does not declare there", func(t *testing.T) {
		env, _ := executedEnv(t)
		outputs := map[string]string{passwordKey: u1Password}
		env.createMRs(2)
		// The plan declares the Secret for resource 1; the operator points web-0 at it.
		plain := executedResources()[0]
		kept := executedResources()[0]
		kept.Description = "kept"
		for i := range kept.Probes {
			kept.Probes[i].ID += "-kept"
		}
		kept.OutputsSecretRef, kept.Outputs = &lastv1alpha1.LocalRef{Name: keptSecret}, outputs
		var mr lastv1alpha1.ManagedResource
		if err := env.crd.Get(env.ctx, client.ObjectKey{Namespace: envNS, Name: web0}, &mr); err != nil {
			t.Fatal(err)
		}
		mr.Spec.OutputsSecretRef = &lastv1alpha1.LocalRef{Name: keptSecret}
		if err := env.crd.Update(env.ctx, &mr); err != nil {
			t.Fatal(err)
		}
		env.outputSecret(web1, envIntentUID)
		steps := []lastv1alpha1.Command{argvStep("u1", kubectlBin, "get", "ns")}
		h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{plain, kept}})
		env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
		if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(h, steps[0]); err != nil {
			t.Fatal(err)
		}
		_, err := env.writeOutputs(h, web0, outputs)
		env.wantDenied(err, codes.PermissionDenied)
		env.wantSecretControlledBy(web1)
	})
	t.Run("never a Secret of a ManagedResource the Intent does not control", func(t *testing.T) {
		env, _ := executedEnv(t)
		outputs := map[string]string{passwordKey: u1Password}
		h, steps := env.dropTheFirstResource(outputs)
		other := lastv1alpha1.ManagedResourceName(envIntent, 9)
		if err := env.kube.CoreV1().Secrets(envNS).Delete(env.ctx, keptSecret, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		env.outputSecret(other, envIntentUID)
		if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(h, steps[0]); err != nil {
			t.Fatal(err)
		}
		_, err := env.writeOutputs(h, web0, outputs)
		env.wantDenied(err, codes.PermissionDenied)
		if _, err := env.end(h, true); err != nil {
			t.Fatal(err)
		}
		env.wantSecretControlledBy(other)
	})
	t.Run("never a Secret of another Intent", func(t *testing.T) {
		env, _ := executedEnv(t)
		h, steps := env.dropTheFirstResource(nil)
		if err := env.kube.CoreV1().Secrets(envNS).Delete(env.ctx, keptSecret, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		env.outputSecret(web1, "another-intent-uid")
		env.runPhase(lastv1alpha1.StageUpdate, h, steps)
		env.wantSecretControlledBy(web1)
	})
}

// The Intent's Delete never waits for an ending Update's cleanup: deleting the
// Intent garbage-collects its ManagedResources, and the sweep is the backstop.
func TestABeginPhaseForDeleteSkipsAnEndingUpdatesCleanup(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	h, steps := env.oneResourceUpdate()
	var failing atomic.Bool
	failing.Store(true)
	env.failMRDeletes(&failing)
	if _, err := env.beginStage(lastv1alpha1.StageUpdate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("EndPhase with a failing delete = %v, want Unavailable", err)
	}
	del := env.startDelete(teardownCommands())
	env.approve(deleteApproval("teardown", del))
	if _, err := env.beginStage(lastv1alpha1.StageDelete, del); err != nil {
		t.Fatalf("BeginPhase(Delete) while the cleanup fails = %v", err)
	}
	if p := env.g.phases.get(envKey); p == nil || p.stage != lastv1alpha1.StageDelete {
		t.Fatalf("open phase = %+v, want the Delete", p)
	}
	if ex := env.executed(); ex.PlanHash != h {
		t.Fatalf("executed = %s, want the Update %s", ex.PlanHash, h)
	}
}

// A Create that replaces an executed plan (the operator could not read the
// audit log, so it planned a Create) deletes the ManagedResources it dropped,
// as an Update does.
func TestASucceededCreateDeletesStaleManagedResources(t *testing.T) {
	env, _ := executedEnv(t)
	env.createMRs(2)
	steps := []lastv1alpha1.Command{argvStep("c1", kubectlBin, "apply", "-f", "/work/web2.yaml")}
	h := env.recordPlan(lastv1alpha1.StageCreate, plan.Body{Steps: steps, Resources: executedResources()})
	env.approve(approvalFor("create-again", h, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	env.runPhase(lastv1alpha1.StageCreate, h, steps)
	env.wantMRs(0, 8, 9)
	var deleted []audit.Entry
	for _, en := range env.entries()[before:] {
		if en.Type == audit.TypeManagedResourceDeleted {
			deleted = append(deleted, en)
		}
	}
	if len(deleted) != 1 || deleted[0].Stage != string(lastv1alpha1.StageCreate) || deleted[0].SubjectHash != h {
		t.Fatalf("managedresource.deleted entries = %+v, want one for the Create", deleted)
	}
}

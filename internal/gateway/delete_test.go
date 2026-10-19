package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	teardownFinalizer = "last.moogie-labs.com/teardown"
	recreatedUID      = "intent-uid-2"
	verbCreate        = "create"
)

// teardownCommands are the executed plan's teardown commands, in resource order.
func teardownCommands() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{
		argvStep("t1", kubectlBin, "delete", "deployment", "web", "-n", envNS, "--ignore-not-found"),
		argvStep("t2", kubectlBin, "delete", "service", "web", "-n", envNS, "--ignore-not-found"),
	}
}

// executedEnv is an env whose Intent executed a one-step Create plan with one
// probed resource carrying the teardown commands.
func executedEnv(t *testing.T) (*testEnv, *lastv1alpha1.Intent) {
	t.Helper()
	res := probedResource()
	res.Teardown = teardownCommands()
	in := intentWithPlan(t, []lastv1alpha1.Command{argvStep("s1", kubectlBin, "apply", "-f", "/work/web.yaml")}, res)
	env := newProviderEnv(t, in, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	executeAll(t, env, in)
	return env, in
}

func deleteApproval(name, subject string) *lastv1alpha1.IntentApproval {
	a := approvalFor(name, subject, lastv1alpha1.DecisionApprove, approver)
	a.Spec.Stage = lastv1alpha1.StageDelete
	return a
}

// startDelete deletes the stored Intent, held by the teardown finalizer, and
// records a Delete plan of steps and resources, as the operator does. It returns
// the plan's subject.
func (e *testEnv) startDelete(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	e.t.Helper()
	var in lastv1alpha1.Intent
	key := client.ObjectKey{Namespace: envNS, Name: envIntent}
	if err := e.crd.Get(e.ctx, key, &in); err != nil {
		e.t.Fatal(err)
	}
	if in.DeletionTimestamp == nil {
		in.Finalizers = append(in.Finalizers, teardownFinalizer)
		if err := e.crd.Update(e.ctx, &in); err != nil {
			e.t.Fatal(err)
		}
		if err := e.crd.Delete(e.ctx, &in); err != nil {
			e.t.Fatal(err)
		}
	}
	return e.setPlan(lastv1alpha1.StageDelete, steps, resources...)
}

// setPlan records a plan in the stored Intent's status and returns its subject.
func (e *testEnv) setPlan(stage lastv1alpha1.Stage, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	e.t.Helper()
	var in lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		e.t.Fatal(err)
	}
	h, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		e.t.Fatal(err)
	}
	in.Status.Stage, in.Status.Phase = stage, lastv1alpha1.PhaseAwaitingApproval
	in.Status.Plan = &lastv1alpha1.Plan{Hash: h, Steps: steps, Resources: resources}
	if err := e.crd.Update(e.ctx, &in); err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *testEnv) approve(a *lastv1alpha1.IntentApproval) {
	e.t.Helper()
	if err := e.crd.Create(e.ctx, a); err != nil {
		e.t.Fatal(err)
	}
}

// runDelete opens the Delete phase for subject and runs every step; the phase stays open.
func (e *testEnv) runDelete(subject string, steps []lastv1alpha1.Command) {
	e.t.Helper()
	if resp, err := e.beginStage(lastv1alpha1.StageDelete, subject); err != nil || resp.GetSteps() != int32(len(steps)) { //nolint:gosec // a test plan
		e.t.Fatalf("BeginPhase(Delete) = %v, %v", resp, err)
	}
	for _, s := range steps {
		if _, err := e.run(subject, s); err != nil {
			e.t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
}

func (e *testEnv) recordExists() bool {
	e.t.Helper()
	_, err := e.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), recordName(envKey), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	return err == nil
}

// recreate releases the torn-down Intent, as the operator does, and creates a
// new Intent with the same name.
func (e *testEnv) recreate() {
	e.t.Helper()
	var old lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &old); err != nil {
		e.t.Fatal(err)
	}
	old.Finalizers = nil
	if err := e.crd.Update(e.ctx, &old); err != nil {
		e.t.Fatal(err)
	}
	recreated := intentWithPlan(e.t, twoSteps())
	recreated.UID = recreatedUID
	if err := e.crd.Create(e.ctx, recreated); err != nil {
		e.t.Fatal(err)
	}
}

// indexedEntry appends an llm.request entry for the env's Intent, outside any phase.
func (e *testEnv) indexedEntry() {
	e.t.Helper()
	if _, err := e.g.writer.Load().Append(context.Background(), audit.Draft{Type: audit.TypeLLMRequest, Actor: testOperator,
		Intent: envKey, Payload: map[string]string{"model": "scripted"}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) pendingIndex() []IndexEntry {
	e.g.index.mu.Lock()
	defer e.g.index.mu.Unlock()
	return e.g.index.pending[envKey]
}

func TestDeletePhaseRunsAnApprovedTeardown(t *testing.T) {
	env, _ := executedEnv(t)
	steps := teardownCommands()
	reordered := []lastv1alpha1.Command{steps[1], steps[0]} // the model may reorder the teardown
	h := env.startDelete(reordered)
	env.approve(deleteApproval("teardown", h))
	env.runDelete(h, reordered)
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase(Delete, succeeded) = %v", err)
	}

	last := env.last()
	var p phaseEnd
	if last.Type != audit.TypePhaseEnd || last.Stage != string(lastv1alpha1.StageDelete) || last.SubjectHash != h ||
		json.Unmarshal(last.Payload, &p) != nil || p.Outcome != outcomeSucceeded || p.StepsRun != 2 {
		t.Fatalf("last entry = %s %+v %s", last.Type, last, last.Payload)
	}
	if env.recordExists() {
		t.Fatal("the record ConfigMap survived a succeeded Delete")
	}
	if env.g.phases.get(envKey) != nil {
		t.Fatal("the Delete phase is still in memory")
	}
	if es := env.pendingIndex(); len(es) != 0 {
		t.Fatalf("pending index entries after the Delete = %v", es)
	}
	_, err := env.probe()
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "no executed plan") {
		t.Fatalf("RunProbes = %v, want no executed plan", err)
	}
}

func TestDeleteRefusesAPlanWithResources(t *testing.T) {
	env, _ := executedEnv(t)
	h := env.startDelete(teardownCommands(), lastv1alpha1.ManagedResourceSpec{Description: "namespace"})
	env.approve(deleteApproval("teardown", h))
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "resources") {
		t.Fatalf("BeginPhase = %v, want a refusal naming resources", err)
	}
	if env.last().Stage != string(lastv1alpha1.StageDelete) {
		t.Fatalf("policy.denied stage = %q", env.last().Stage)
	}
	for _, e := range env.entries() {
		if e.Type == audit.TypePhaseBegin && e.Stage == string(lastv1alpha1.StageDelete) {
			t.Fatal("a refused Delete wrote phase.begin")
		}
	}
}

func TestDeleteNeedsApproval(t *testing.T) {
	env, _ := executedEnv(t)
	h := env.startDelete(teardownCommands())
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), msgNoApproval) {
		t.Fatalf("BeginPhase without an approval = %v", err)
	}
	// An approval of the same subject for another stage does not authorize a Delete.
	env.approve(approvalFor("create", h, lastv1alpha1.DecisionApprove, approver))
	_, err = env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.FailedPrecondition)
	// A Deny for the Delete wins over its Approve.
	env.approve(deleteApproval("teardown", h))
	deny := deleteApproval("no", h)
	deny.Spec.Decision, deny.Spec.ApprovedBy = lastv1alpha1.DecisionDeny, "bob"
	env.approve(deny)
	_, err = env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.PermissionDenied)
}

func TestDeleteNeedsAnIntentBeingDeleted(t *testing.T) {
	env, _ := executedEnv(t)
	h := env.setPlan(lastv1alpha1.StageDelete, teardownCommands())
	env.approve(deleteApproval("teardown", h))
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("BeginPhase(Delete) on a live Intent = %v", err)
	}
	if !env.recordExists() {
		t.Fatal("a refused Delete dropped the record")
	}
}

func TestAFailedDeleteKeepsTheRecord(t *testing.T) {
	env, in := executedEnv(t)
	before, err := env.g.state.LoadAny(context.Background(), envKey)
	if err != nil || before.Executed == nil {
		t.Fatalf("record before the Delete = %+v, %v", before, err)
	}
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	env.result = func(c runnerapi.Command) runnerapi.Result { return runnerapi.Result{ExitCode: 1, Stderr: "forbidden"} }
	if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
		t.Fatal(err)
	}
	if r, err := env.run(h, steps[0]); err != nil || r.GetResult().GetExitCode() != 1 {
		t.Fatalf("RunCommand = %v, %v", r, err)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatalf("EndPhase(Delete, failed) = %v", err)
	}
	after, err := env.g.state.LoadAny(context.Background(), envKey)
	if err != nil || after.Open != nil || after.IntentUID != envIntentUID {
		t.Fatalf("record after a failed Delete = %+v, %v", after, err)
	}
	if after.Executed == nil || *after.Executed != *before.Executed || after.Executed.PlanHash != in.Status.Plan.Hash {
		t.Fatalf("executed plan after a failed Delete = %+v, want %+v", after.Executed, before.Executed)
	}
	if _, err := env.probe(); err != nil {
		t.Fatalf("RunProbes after a failed Delete = %v", err)
	}
}

func TestAFailedDeleteCanRunAgain(t *testing.T) {
	env, in := executedEnv(t)
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	var fail atomic.Bool
	fail.Store(true)
	env.result = func(c runnerapi.Command) runnerapi.Result {
		if fail.Load() {
			return runnerapi.Result{ExitCode: 1}
		}
		return runnerapi.Result{}
	}
	if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
		t.Fatal(err)
	}
	_, _ = env.run(h, steps[0])
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}

	// The retry replans the same teardown: its approval authorizes the re-run.
	fail.Store(false)
	env.runDelete(h, steps)
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
	var begins int
	for _, e := range env.entries() {
		if e.Type == audit.TypePhaseBegin && e.SubjectHash == h {
			begins++
		}
	}
	if begins != 2 {
		t.Fatalf("Delete phase.begin entries = %d, want 2", begins)
	}

	// Every other stage keeps run-once: the executed Create plan never opens again.
	create := env.setPlan(lastv1alpha1.StageCreate, in.Status.Plan.Steps, in.Status.Plan.Resources...)
	if create != in.Status.Plan.Hash {
		t.Fatalf("restored Create subject = %s, want %s", create, in.Status.Plan.Hash)
	}
	_, err := env.begin(create)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "runs only once") {
		t.Fatalf("re-opening the executed Create plan = %v", err)
	}
}

// tornDown runs an approved Delete of the executed Intent to success.
func tornDown(t *testing.T) (*testEnv, *lastv1alpha1.Intent) {
	t.Helper()
	env, in := executedEnv(t)
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	env.runDelete(h, steps)
	if _, err := env.end(h, true); err != nil {
		t.Fatal(err)
	}
	return env, in
}

func TestNoFlushRecreatesATornDownRecord(t *testing.T) {
	env, _ := tornDown(t)
	ctx := context.Background()
	env.g.flushIndexes(ctx)
	if env.recordExists() {
		t.Fatal("the index flush recreated the record from the Delete's own entries")
	}

	// A late indexed entry for the torn-down Intent, such as a straggling LLM call.
	env.indexedEntry()
	env.g.flushIndexes(ctx)
	if env.recordExists() {
		t.Fatal("a later index flush recreated the record")
	}
	env.indexedEntry()
	if err := env.g.flushIndex(ctx, envKey); err != nil {
		t.Fatal(err)
	}
	if env.recordExists() {
		t.Fatal("QueryAudit's flush recreated the record")
	}
	env.g = env.newGateway()
	if err := env.g.closeInterruptedPhases(ctx, env.g.writer.Load()); err != nil {
		t.Fatal(err)
	}
	if env.recordExists() {
		t.Fatal("closing interrupted phases at lead recreated the record")
	}
}

func TestATornDownNameServesARecreatedIntent(t *testing.T) {
	env, _ := tornDown(t)
	ctx := context.Background()
	env.recreate()
	env.indexedEntry()
	env.g.flushIndexes(ctx)
	rec, err := env.g.state.LoadAny(ctx, envKey)
	if err != nil || rec.IntentUID != recreatedUID || len(rec.Index) != 1 || rec.Executed != nil || len(rec.Used) != 0 {
		t.Fatalf("record for the recreated Intent = %+v, %v", rec.IntentRecord, err)
	}
}

func TestATornDownIntentNeverReopensItsCreatePlan(t *testing.T) {
	env, in := tornDown(t)
	env.g = env.newGateway() // a restart loses the in-memory teardown marker
	// The record and its used subjects are gone, but the Intent is still being
	// deleted: its executed Create plan, approval still standing, must not run again.
	h := env.setPlan(lastv1alpha1.StageCreate, in.Status.Plan.Steps, in.Status.Plan.Resources...)
	_, err := env.begin(h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("BeginPhase(Create) on a torn-down Intent = %v", err)
	}
	if env.recordExists() {
		t.Fatal("a refused Create recreated the record")
	}
}

func TestDeleteRetriesTheRecordDeletion(t *testing.T) {
	env, in := executedEnv(t)
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	env.runDelete(h, steps)
	var fail atomic.Bool
	fail.Store(true)
	env.kube.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.CompareAndSwap(true, false) {
			return true, nil, errors.New("etcd unavailable")
		}
		return false, nil, nil
	})
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("first EndPhase = %v, want Unavailable", err)
	}
	// Until the retry, the record keeps the executed Create plan and no open phase.
	rec, err := env.g.state.LoadAny(context.Background(), envKey)
	if err != nil || rec.Open != nil || rec.Executed == nil || rec.Executed.PlanHash != in.Status.Plan.Hash {
		t.Fatalf("record after the failed deletion = %+v, %v", rec.IntentRecord, err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("retried EndPhase = %v", err)
	}
	if env.recordExists() {
		t.Fatal("the retried EndPhase kept the record")
	}
	var ends int
	for _, e := range env.entries() {
		if e.Type == audit.TypePhaseEnd && e.SubjectHash == h {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("Delete phase.end entries = %d, want 1", ends)
	}
}

func TestATornDownIntentOpensNoPhaseAgain(t *testing.T) {
	env, _ := tornDown(t)
	steps := teardownCommands()
	h, err := plan.Hash(plan.Body{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	// The Delete's approval still stands, and the record with its used subjects is gone.
	_, err = env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "torn down") {
		t.Fatalf("reopening the succeeded Delete = %v", err)
	}
	c := discoveryCommand()
	pc := pendingCommand(t, c, "nonce-after-teardown")
	env.setPending(pc)
	env.approve(discoveryApproval("look", pc.Hash, lastv1alpha1.DecisionApprove))
	_, err = env.beginStage(lastv1alpha1.StageDiscovery, pc.Hash)
	env.wantDenied(err, codes.FailedPrecondition)
	if env.recordExists() {
		t.Fatal("a refused phase recreated the torn-down record")
	}
}

func TestAnInterruptedDeleteRunsAgain(t *testing.T) {
	for _, atLead := range []bool{true, false} {
		t.Run(map[bool]string{true: "closed at lead", false: "closed by BeginPhase"}[atLead], func(t *testing.T) {
			env, _ := executedEnv(t)
			steps := teardownCommands()
			h := env.startDelete(steps)
			env.approve(deleteApproval("teardown", h))
			if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(h, steps[0]); err != nil {
				t.Fatal(err)
			}
			env.g = env.newGateway() // the gateway restarted mid-teardown
			if atLead {
				if err := env.g.closeInterruptedPhases(env.ctx, env.g.writer.Load()); err != nil {
					t.Fatal(err)
				}
			}
			env.runDelete(h, steps)
			if _, err := env.end(h, true); err != nil {
				t.Fatalf("EndPhase of the re-run Delete = %v", err)
			}
			var interrupted int
			for _, e := range env.entries() {
				var p phaseEnd
				if e.Type == audit.TypePhaseEnd && e.SubjectHash == h && json.Unmarshal(e.Payload, &p) == nil && p.Outcome == outcomeInterrupted {
					interrupted++
				}
			}
			if interrupted != 1 || env.recordExists() {
				t.Fatalf("interrupted phase.end entries = %d, record exists = %v", interrupted, env.recordExists())
			}
		})
	}
}

func TestATornDownMarkerOutlivesAFailedSave(t *testing.T) {
	env, _ := tornDown(t)
	ctx := context.Background()
	env.recreate()
	var fail atomic.Bool
	fail.Store(true)
	env.kube.PrependReactor(verbCreate, "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.CompareAndSwap(true, false) {
			return true, nil, errors.New("etcd unavailable")
		}
		return false, nil, nil
	})
	env.indexedEntry()
	env.g.flushIndexes(ctx) // the save fails; the entries stay pending
	env.g.flushIndexes(ctx)
	rec, err := env.g.state.LoadAny(ctx, envKey)
	if err != nil || rec.IntentUID != recreatedUID || len(rec.Index) != 1 {
		t.Fatalf("record for the recreated Intent = %+v, %v", rec.IntentRecord, err)
	}
}

func TestDeleteRetryNeverRecreatesARecordAlreadyDeleted(t *testing.T) {
	env, _ := executedEnv(t)
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	env.runDelete(h, steps)
	var fail atomic.Bool
	fail.Store(true)
	env.kube.PrependReactor("delete", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !fail.CompareAndSwap(true, false) {
			return false, nil, nil
		}
		// The API server deleted the record, but the answer was lost.
		da := a.(k8stesting.DeleteAction)
		if err := env.kube.Tracker().Delete(da.GetResource(), da.GetNamespace(), da.GetName()); err != nil {
			t.Error(err)
		}
		return true, nil, errors.New("connection reset")
	})
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("first EndPhase = %v, want Unavailable", err)
	}
	creates := func() (n int) {
		for _, a := range env.kube.Actions() {
			if a.GetVerb() == verbCreate && a.GetResource().Resource == "configmaps" {
				n++
			}
		}
		return n
	}
	before := creates()
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("retried EndPhase = %v", err)
	}
	if creates() != before || env.recordExists() {
		t.Fatalf("the retry created the record again (%d creates, exists=%v)", creates()-before, env.recordExists())
	}
}

// failFirstRecordDelete makes the first record delete fail; when ambiguous, the
// API server deletes the record but the answer is lost.
func (e *testEnv) failFirstRecordDelete(ambiguous bool) {
	var fail atomic.Bool
	fail.Store(true)
	e.kube.PrependReactor("delete", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !fail.CompareAndSwap(true, false) {
			return false, nil, nil
		}
		if ambiguous {
			da := a.(k8stesting.DeleteAction)
			if err := e.kube.Tracker().Delete(da.GetResource(), da.GetNamespace(), da.GetName()); err != nil {
				e.t.Error(err)
			}
		}
		return true, nil, errors.New("connection reset")
	})
}

func TestNoPhaseOpensWhileATornDownRecordIsDropped(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete failed", true: "delete answer lost"}[ambiguous], func(t *testing.T) {
			env, _ := executedEnv(t)
			steps := teardownCommands()
			h := env.startDelete(steps)
			env.approve(deleteApproval("teardown", h))
			env.runDelete(h, steps)
			env.failFirstRecordDelete(ambiguous)
			if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
				t.Fatalf("first EndPhase = %v, want Unavailable", err)
			}
			// An index flush in between saves into a record that is still there, and
			// never recreates one already deleted.
			env.indexedEntry()
			env.g.flushIndexes(context.Background())
			if rec, err := env.g.state.LoadAny(context.Background(), envKey); err != nil || env.recordExists() == ambiguous ||
				(!ambiguous && (len(rec.Index) == 0 || rec.Index[len(rec.Index)-1].Type != audit.TypeLLMRequest)) {
				t.Fatalf("record after a flush = %+v (exists=%v), %v", rec.IntentRecord, env.recordExists(), err)
			}
			_, err := env.beginStage(lastv1alpha1.StageDelete, h)
			env.wantDenied(err, codes.FailedPrecondition)
			pc := pendingCommand(t, discoveryCommand(), "nonce-mid-drop")
			env.setPending(pc)
			env.approve(discoveryApproval("look", pc.Hash, lastv1alpha1.DecisionApprove))
			_, err = env.beginStage(lastv1alpha1.StageDiscovery, pc.Hash)
			env.wantDenied(err, codes.FailedPrecondition)
			if _, err := env.end(h, true); err != nil {
				t.Fatalf("retried EndPhase = %v", err)
			}
			if env.recordExists() {
				t.Fatal("the record survived the retried EndPhase")
			}
		})
	}
}

func TestDeleteRetryClearsTheOpenPhaseWhenItCannotDropTheRecord(t *testing.T) {
	env, _ := executedEnv(t)
	steps := teardownCommands()
	h := env.startDelete(steps)
	env.approve(deleteApproval("teardown", h))
	env.runDelete(h, steps)
	var failSave atomic.Bool
	failSave.Store(true)
	env.kube.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failSave.CompareAndSwap(true, false) {
			return true, nil, errors.New("etcd unavailable")
		}
		return false, nil, nil
	})
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("first EndPhase = %v, want Unavailable", err)
	}
	env.failFirstRecordDelete(false)
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("retried EndPhase = %v, want Unavailable", err)
	}
	rec, err := env.g.state.LoadAny(context.Background(), envKey)
	if err != nil || rec.Open != nil {
		t.Fatalf("record after the retry = %+v, %v; want no open phase", rec.IntentRecord, err)
	}
	env.g = env.newGateway()
	if err := env.g.closeInterruptedPhases(env.ctx, env.g.writer.Load()); err != nil {
		t.Fatal(err)
	}
	for _, e := range env.entries() {
		var p phaseEnd
		if e.Type == audit.TypePhaseEnd && e.SubjectHash == h && json.Unmarshal(e.Payload, &p) == nil && p.Outcome == outcomeInterrupted {
			t.Fatal("a restart interrupted a Delete that already succeeded")
		}
	}
}

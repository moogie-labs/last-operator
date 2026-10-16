package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// world plays the operator in follow tests: at every pause between reads it
// applies its next step, once that step's precondition holds.
type world struct {
	t     *testing.T
	c     client.Client
	steps []step
	waits int
}

type step struct {
	ready func(in *lastv1alpha1.Intent) bool // nil: at once
	apply func(in *lastv1alpha1.Intent)
}

func (w *world) wait(ctx context.Context) error {
	if w.waits++; w.waits > 50 {
		return errors.New("the follow loop did not finish")
	}
	if len(w.steps) == 0 {
		return nil
	}
	var in lastv1alpha1.Intent
	if err := w.c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: webName}, &in); err != nil {
		return err
	}
	s := w.steps[0]
	if s.ready != nil && !s.ready(&in) {
		return nil
	}
	s.apply(&in)
	w.steps = w.steps[1:]
	return w.c.Status().Update(ctx, &in)
}

// decided reports whether the user decided on subject.
func (w *world) decided(subject string) func(*lastv1alpha1.Intent) bool {
	return func(*lastv1alpha1.Intent) bool {
		for _, a := range approvalsIn(w.t, w.c) {
			if a.Spec.SubjectHash == subject {
				return true
			}
		}
		return false
	}
}

func TestFollowAsksDecidesAndEndsReady(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "2\ny\ny\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseClarifying
			in.Status.Questions = []lastv1alpha1.Question{{ID: "q1", Text: questionTier, Options: []string{"internal", "public"}}}
		}},
		{ready: func(in *lastv1alpha1.Intent) bool { return len(in.Spec.Answers) == 1 }, apply: func(in *lastv1alpha1.Intent) {
			in.Status.Questions = nil
			in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
			in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand("kubectl", "get", "ns")}
		}},
		{ready: w.decided(h1), apply: func(in *lastv1alpha1.Intent) {
			in.Status.PendingCommand = nil
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: nginxName, Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{trueCmd}}}}}
		}},
		{ready: w.decided(h2), apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseExecuting }},
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseReady
			mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: testNS},
				Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: webName}, Description: "nginx web",
					OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"}}}
			if err := c.Create(context.Background(), mr); err != nil {
				t.Fatal(err)
			}
		}},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}

	var in lastv1alpha1.Intent
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: webName}, &in); err != nil {
		t.Fatal(err)
	}
	if want := []lastv1alpha1.Answer{{ID: "q1", Value: "public"}}; !reflect.DeepEqual(in.Spec.Answers, want) {
		t.Errorf("spec.answers = %+v", in.Spec.Answers)
	}
	approvals := approvalsIn(t, c)
	if len(approvals) != 2 {
		t.Fatalf("approvals = %+v", approvals)
	}
	subjects := map[lastv1alpha1.Stage]string{}
	for _, a := range approvals {
		s := a.Spec
		if s.Decision != lastv1alpha1.DecisionApprove || s.ApprovedBy != testUser || s.IntentRef.Name != webName {
			t.Errorf("approval = %+v", s)
		}
		subjects[s.Stage] = s.SubjectHash
	}
	if want := map[lastv1alpha1.Stage]string{lastv1alpha1.StageDiscovery: h1, lastv1alpha1.StageCreate: h2}; !reflect.DeepEqual(subjects, want) {
		t.Errorf("approved subjects = %v, want %v", subjects, want)
	}
	for _, want := range []string{
		"Created Intent team-x/web\n", "Intent team-x/web: Clarifying", questionTier, "2) public", "Recorded 1 answers.",
		"Discovery command " + h1, "Reason: see what exists", "Approved discovery command " + h1,
		"Plan " + h2, "Approved plan " + h2, "Intent team-x/web: Executing",
		"Intent team-x/web is Ready.", "ManagedResource web-0: nginx web", "outputs: Secret web-outputs",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFollowAsksAboutEachSubjectOnce(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
			in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Command: argvCommand("kubectl", "get", "ns")}
		}},
		{apply: func(*lastv1alpha1.Intent) {}}, // a read later, still waiting for the same decision
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseFailed
			setReady(in, "PlanningFailed", "planning exceeded 20 model turns")
		}},
	}
	d.Wait = w.wait
	err := execute(d, "run an nginx web server", "--name", webName)
	if !errors.Is(err, ErrIntentFailed) {
		t.Fatalf("kubectl last = %v, want ErrIntentFailed", err)
	}
	if n := strings.Count(out.String(), "Approve? [y/N/d]"); n != 1 {
		t.Errorf("asked %d times:\n%s", n, out)
	}
	for _, want := range []string{"Left pending; decide later with kubectl last approve web",
		"Intent team-x/web failed: PlanningFailed: planning exceeded 20 model turns"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Errorf("%d IntentApprovals for a subject left pending", n)
	}
}

func TestFollowStopsAtTheEndOfInput(t *testing.T) {
	c := fakeClient()
	d, _ := depsFor(c, "")
	w := &world{t: t, c: c, steps: []step{{apply: func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseClarifying
		in.Status.Questions = []lastv1alpha1.Question{{ID: "q1", Text: questionPort}}
	}}}}
	d.Wait = w.wait
	err := execute(d, "run an nginx web server", "--name", webName)
	if !errors.Is(err, errNoInput) || !strings.Contains(err.Error(), "kubectl last plan web") {
		t.Fatalf("kubectl last = %v", err)
	}
}

func TestCtrlCStopsFollowingAndLeavesTheIntent(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	d.Wait = func(context.Context) error { return context.Canceled }
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v", err)
	}
	if !strings.Contains(out.String(), "Stopped following Intent team-x/web; it carries on.") {
		t.Errorf("output:\n%s", out)
	}
	if n := len(intents(t, c)); n != 1 {
		t.Fatalf("%d Intents after Ctrl-C, want the one created", n)
	}
}

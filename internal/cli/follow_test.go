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
			in.Status.PendingCommand = lookCommand()
		}},
		{ready: w.decided(h1), apply: func(in *lastv1alpha1.Intent) {
			in.Status.PendingCommand = nil
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = webPlan()
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
			in.Status.PendingCommand = lookCommand()
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
	for _, want := range []string{"Left pending. Decide on it later:\n",
		"Approve: kubectl last approve web -n team-x --hash " + h1 + "\n",
		"Deny:    kubectl last deny web -n team-x --hash " + h1 + " --reason R\n",
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

func TestFollowWarnsAboutAForgedSubjectOnceAndKeepsFollowing(t *testing.T) {
	for _, forged := range forgedIntents() {
		c := fakeClient()
		d, out := depsFor(c, "y\n")
		w := &world{t: t, c: c}
		w.steps = []step{
			{apply: func(in *lastv1alpha1.Intent) { in.Status = forged.Status }},
			{apply: func(*lastv1alpha1.Intent) {}}, // a read later, still forged
			{apply: func(in *lastv1alpha1.Intent) {
				in.Status.Phase = lastv1alpha1.PhaseFailed
				setReady(in, "ApprovalDenied", "nobody approved it")
			}},
		}
		d.Wait = w.wait
		if err := execute(d, "run an nginx web server", "--name", webName); !errors.Is(err, ErrIntentFailed) {
			t.Fatalf("kubectl last = %v, want ErrIntentFailed\n%s", err, out)
		}
		_, stated, _ := subjectOf(forged)
		if n := strings.Count(out.String(), "Warning: intent team-x/web: the "); n != 1 {
			t.Errorf("warned %d times, want once:\n%s", n, out)
		}
		if !strings.Contains(out.String(), "does not hash to its stated hash "+stated) {
			t.Errorf("output lacks the warning:\n%s", out)
		}
		for _, never := range []string{"Approve?", "kubectl get ns", trueCmd} {
			if strings.Contains(out.String(), never) {
				t.Errorf("output shows %q of a forged subject:\n%s", never, out)
			}
		}
		if n := len(approvalsIn(t, c)); n != 0 {
			t.Errorf("%d IntentApprovals for a forged subject", n)
		}
	}
}

func TestFollowShowsFilesOnRequest(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "f\ny\n")
	p := webPlan()
	p.Steps[0].Files = map[string]string{webYAML: "kind: Deployment\x1b[2K\n"}
	p.Hash = planHash(p)
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = p
		}},
		{ready: w.decided(p.Hash), apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseFailed }},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); !errors.Is(err, ErrIntentFailed) {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	if n := strings.Count(out.String(), "Approve? [y/N/d/f]: "); n != 2 {
		t.Errorf("asked %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out.String(), "Plan "+p.Hash); n != 2 {
		t.Errorf("showed the plan %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out.String(), "       | kind: Deployment\\u001b[2K\n"); n != 1 {
		t.Errorf("showed the file content %d times, want once, escaped:\n%s", n, out)
	}
	if got := approvalsIn(t, c); len(got) != 1 || got[0].Spec.SubjectHash != p.Hash {
		t.Fatalf("approvals = %+v", got)
	}
}

func TestFollowEndsAtDrifted(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	w := &world{t: t, c: c, steps: []step{{apply: func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseDrifted
		setReady(in, "Drifted", "Probes failed on web-0")
	}}}}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	if !strings.HasSuffix(out.String(), "Intent team-x/web: Drifted (Probes failed on web-0)\n") {
		t.Errorf("output:\n%s", out)
	}
}

// vanishes plays the API server and operator for a followed Intent: the first
// pauses apply status (the Intent being deleted with the finalizer held, if
// deleting), and the last removes the Intent, as a released finalizer or a
// force delete would.
func vanishes(t *testing.T, c client.Client, status func(*lastv1alpha1.Intent), deleting bool) func(context.Context) error {
	t.Helper()
	calls := 0
	return func(ctx context.Context) error {
		calls++
		var in lastv1alpha1.Intent
		if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: webName}, &in); err != nil {
			return err
		}
		switch calls {
		case 1:
			if deleting {
				in.Finalizers = []string{"last.moogie-labs.com/teardown"}
				if err := c.Update(ctx, &in); err != nil {
					return err
				}
				if err := c.Delete(ctx, &in); err != nil {
					return err
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(&in), &in); err != nil {
					return err
				}
			}
			status(&in)
			return c.Status().Update(ctx, &in)
		case 2:
			if !deleting {
				return c.Delete(ctx, &in)
			}
			in.Finalizers = nil
			return c.Update(ctx, &in)
		}
		return errors.New("the follow loop did not finish")
	}
}

func TestFollowEndsWhenTheIntentIsTornDown(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "")
	d.Wait = vanishes(t, c, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageDelete
		in.Status.Phase = lastv1alpha1.PhaseTearingDown
		setReady(in, "TearingDown", "Running the teardown")
	}, true)
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	for _, want := range []string{"Intent team-x/web: TearingDown (Running the teardown)\n", "Intent team-x/web torn down.\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFollowDoesNotClaimATeardownForAnIntentThatJustVanished(t *testing.T) {
	for name, tc := range map[string]struct {
		deleting bool
		status   func(*lastv1alpha1.Intent)
	}{
		"deleted before the teardown, then the finalizer removed by hand": {true, func(in *lastv1alpha1.Intent) {
			in.Status.Stage = lastv1alpha1.StageDelete
			in.Status.Phase = lastv1alpha1.PhasePlanning
		}},
		"not being deleted": {false, func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseTearingDown // not deleted, so not a teardown
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeClient()
			d, out := depsFor(c, "")
			d.Wait = vanishes(t, c, tc.status, tc.deleting)
			if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
				t.Fatalf("kubectl last = %v\n%s", err, out)
			}
			if !strings.HasSuffix(out.String(), "Intent team-x/web was deleted.\n") || strings.Contains(out.String(), "torn down") {
				t.Errorf("output:\n%s", out)
			}
		})
	}
}

func TestPrintReadyEscapesTheOutputSecretName(t *testing.T) {
	mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: testNS},
		Spec: lastv1alpha1.ManagedResourceSpec{IntentRef: &lastv1alpha1.LocalRef{Name: webName}, Description: "web",
			OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web\x1b[8m"}}}
	k := &Kube{Client: fakeClient(mr), Namespace: testNS}
	var b strings.Builder
	if err := printReady(context.Background(), k, &b, testIntent(webName, nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "outputs: Secret web\\u001b[8m\n") {
		t.Errorf("output:\n%q", b.String())
	}
}

func TestFollowShowsRemediating(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "y\n")
	var remediate *lastv1alpha1.Plan
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Stage = lastv1alpha1.StageRemediate
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = webPlan()
			in.Status.Plan.Nonce = "0123456789abcdef0123456789abcdef"
			in.Status.Plan.Hash = planHash(in.Status.Plan)
			remediate = in.Status.Plan
		}},
		{ready: func(*lastv1alpha1.Intent) bool { return len(approvalsIn(t, c)) == 1 }, apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseRemediating
			setReady(in, "Remediating", "Running the remediation")
		}},
		{apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseReady }},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	a := approvalsIn(t, c)
	if len(a) != 1 || a[0].Spec.Stage != lastv1alpha1.StageRemediate || a[0].Spec.SubjectHash != remediate.Hash {
		t.Fatalf("approvals = %+v", a)
	}
	for _, want := range []string{"(stage Remediate)", "Intent team-x/web: Remediating (Running the remediation)\n", "is Ready."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFailedNamesTheRetryAnnotationForEveryStage(t *testing.T) {
	const auto = "retries it automatically"
	modes := func(m lastv1alpha1.Mode) *lastv1alpha1.StageModes {
		return &lastv1alpha1.StageModes{Discovery: m, Create: m, Reconcile: m, Delete: m}
	}
	cases := []struct {
		name   string
		stage  lastv1alpha1.Stage
		reason string
		modes  *lastv1alpha1.StageModes
		auto   bool
	}{
		{"Create, Manual", lastv1alpha1.StageCreate, "ExecutionFailed", modes(lastv1alpha1.ModeManual), false},
		{"no stage, no modes", "", "PlanningFailed", nil, false},
		{"Update, Auto", lastv1alpha1.StageUpdate, "ExecutionFailed", modes(lastv1alpha1.ModeAuto), true},
		{"Remediate, Auto", lastv1alpha1.StageRemediate, reasonRemediationFailed, modes(lastv1alpha1.ModeAuto), true},
		{"Remediate, only create Auto", lastv1alpha1.StageRemediate, reasonRemediationFailed,
			&lastv1alpha1.StageModes{Create: lastv1alpha1.ModeAuto, Reconcile: lastv1alpha1.ModeManual}, false},
		{"RemediationFailed before the Remediate stage, reconcile Auto", lastv1alpha1.StageCreate, reasonRemediationFailed,
			&lastv1alpha1.StageModes{Create: lastv1alpha1.ModeManual, Reconcile: lastv1alpha1.ModeAuto}, true},
		{"Create, Auto, denied", lastv1alpha1.StageCreate, reasonApprovalDenied, modes(lastv1alpha1.ModeAuto), false},
		{"Delete, Manual", lastv1alpha1.StageDelete, "TeardownFailed", modes(lastv1alpha1.ModeManual), false},
		{"Delete, Auto", lastv1alpha1.StageDelete, "TeardownFailed", modes(lastv1alpha1.ModeAuto), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: webName}}
			in.Status.Phase, in.Status.Stage, in.Status.EffectiveModes = lastv1alpha1.PhaseFailed, tc.stage, tc.modes
			setReady(in, tc.reason, "it broke")
			var out strings.Builder
			if err := failed(&out, in); !errors.Is(err, ErrIntentFailed) {
				t.Fatalf("failed = %v, want ErrIntentFailed", err)
			}
			if !strings.Contains(out.String(), "change its "+lastv1alpha1.AnnotationRetry+" annotation") &&
				!strings.Contains(out.String(), "Change its "+lastv1alpha1.AnnotationRetry+" annotation") {
				t.Errorf("the hint does not name the retry annotation:\n%s", out.String())
			}
			if got := strings.Contains(out.String(), auto); got != tc.auto {
				t.Errorf("says it %q: %v, want %v:\n%s", auto, got, tc.auto, out.String())
			}
		})
	}
}

func TestFollowDoesNotPromptInAuto(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "") // no input: a prompt would end the follow with an error
	auto := &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeAuto, Create: lastv1alpha1.ModeAuto,
		Reconcile: lastv1alpha1.ModeAuto, Delete: lastv1alpha1.ModeAuto}
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.EffectiveModes = auto
			in.Status.Stage = lastv1alpha1.StageCreate
			in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
			in.Status.PendingCommand = lookCommand()
			setReady(in, "RunningDiscovery", "Running discovery command")
		}},
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Plan = webPlan()
			in.Status.Phase = lastv1alpha1.PhaseExecuting
			setReady(in, "Executing", "Running plan without approval (Auto)")
		}},
		{apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseReady }},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); err != nil {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	if a := approvalsIn(t, c); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	got := out.String()
	for _, want := range []string{"Discovery command", "running without approval (Auto)", "Plan " + webPlan().Hash} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Approve:", "Deny:", "Approve?"} {
		if strings.Contains(got, bad) {
			t.Errorf("output offers %q:\n%s", bad, got)
		}
	}
}

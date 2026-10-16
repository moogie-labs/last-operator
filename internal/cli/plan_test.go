package cli

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func TestPlanShowsThePendingCommandOrThePlan(t *testing.T) {
	look := testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = lookCommand()
		setReady(in, "AwaitingDiscoveryApproval", "Waiting for an IntentApproval of discovery command "+h1)
	})
	web := testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = webPlan()
	})
	fresh := testIntent("fresh", nil)
	d, out := depsFor(fakeClient(look, web, fresh), "")

	if err := execute(d, "plan", "look"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Intent team-x/look: AwaitingDiscoveryApproval (Waiting for an IntentApproval of discovery command " + h1 + ")\n",
		"Discovery command " + h1,
		"Approve: kubectl last approve look -n team-x --hash " + h1 + "\n",
		"Deny:    kubectl last deny look -n team-x --hash " + h1 + " --reason R\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan look lacks %q:\n%s", want, out)
		}
	}
	out.Reset()
	if err := execute(d, "plan", webName); err != nil || !strings.Contains(out.String(), "Plan "+h2) ||
		!strings.HasSuffix(out.String(), "Approve: kubectl last approve web -n team-x --hash "+h2+"\n"+
			"Deny:    kubectl last deny web -n team-x --hash "+h2+" --reason R\n") {
		t.Fatalf("plan web = %v:\n%s", err, out)
	}
	out.Reset()
	if err := execute(d, "plan", "fresh"); err != nil || out.String() != "Intent team-x/fresh: Pending\nNo plan or pending command yet.\n" {
		t.Fatalf("plan fresh = %v: %q", err, out)
	}
	if err := execute(d, "plan", "gone"); !apierrors.IsNotFound(err) {
		t.Fatalf("plan gone = %v, want NotFound", err)
	}
}

func TestPlanRefusesToShowAForgedSubject(t *testing.T) {
	for _, in := range forgedIntents() {
		stage, stated, _ := subjectOf(in)
		d, out := depsFor(fakeClient(in), "")
		if err := execute(d, "plan", in.Name); err != nil {
			t.Fatal(err)
		}
		want := "Warning: intent team-x/" + in.Name + ": the " + subjectNoun(stage) + " does not hash to its stated hash " + stated +
			"; refusing to show or decide it (is kubectl-last as new as the operator?)\n"
		if !strings.HasSuffix(out.String(), want) {
			t.Errorf("plan %s = %q, want it to end with %q", in.Name, out, want)
		}
		for _, never := range []string{"Approve:", "Plan ", "Discovery command ", "kubectl get ns", trueCmd} {
			if strings.Contains(out.String(), never) {
				t.Errorf("plan %s shows %q:\n%s", in.Name, never, out)
			}
		}
	}
}

func TestPlanShowsFileContentsOnRequest(t *testing.T) {
	in := plannedIntent()
	in.Status.Plan.Steps[0].Files = map[string]string{webYAML: "kind: Deployment\nmetadata: {name: web}"}
	in.Status.Plan.Hash = planHash(in.Status.Plan)
	d, out := depsFor(fakeClient(in), "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "kind: Deployment") {
		t.Errorf("plan without --files shows file content:\n%s", out)
	}
	out.Reset()
	if err := execute(d, "plan", webName, "--files"); err != nil {
		t.Fatal(err)
	}
	want := "     file:      /work/web.yaml (38 bytes)\n       | kind: Deployment\n       | metadata: {name: web}\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("plan --files lacks %q:\n%s", want, out)
	}
}

func TestPlanShowsAnExecutingPlanWithoutDecisionCommands(t *testing.T) {
	in := plannedIntent()
	in.Status.Phase = lastv1alpha1.PhaseExecuting
	d, out := depsFor(fakeClient(in), "")
	if err := execute(d, "plan", webName); err != nil || !strings.Contains(out.String(), "Plan "+h2) || strings.Contains(out.String(), "Approve:") {
		t.Fatalf("plan = %v:\n%s", err, out)
	}
}

func TestPlanShowsTheOpenQuestions(t *testing.T) {
	in := clarifying()
	in.Status.Questions[1].Text = "Which port?\x1b[8m"
	d, out := depsFor(fakeClient(in), "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nWhich tier?\n  1) internal\n  2) public\n", "\nWhich port?\\u001b[8m\n",
		"Answer them with kubectl last answer web -n team-x\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}

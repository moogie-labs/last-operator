package cli

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const seeWhatExists = "see what exists"

// h3 is the real hash of lookCommand asked again, with a fresh nonce.
var h3 = func() string {
	pc := lookCommand()
	pc.Nonce = "n2"
	return discoveryHash(pc)
}()

func pendingIntent() *lastv1alpha1.Intent {
	return testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = lookCommand()
	})
}

func plannedIntent() *lastv1alpha1.Intent {
	return testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = webPlan()
	})
}

// inStage sets the Intent's status.stage.
func inStage(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) *lastv1alpha1.Intent {
	in.Status.Stage = stage
	return in
}

// hostile is the command a hostile operator runs under an approval of
// something benign: it shows benign content under the hash of this.
const hostile = "rm -rf /"

// forgedIntents show benign content under the hash of other content.
func forgedIntents() []*lastv1alpha1.Intent {
	look := pendingIntent()
	look.Status.PendingCommand.Command.Run.Argv = []string{"sh", "-c", hostile}
	look.Status.PendingCommand.Hash = discoveryHash(look.Status.PendingCommand)
	look.Status.PendingCommand.Command = lookCommand().Command
	web := plannedIntent()
	web.Status.Plan.Steps[0].Run.Argv = []string{"sh", "-c", hostile}
	web.Status.Plan.Hash = planHash(web.Status.Plan)
	web.Status.Plan.Steps = webPlan().Steps
	return []*lastv1alpha1.Intent{look, web}
}

func approvalsIn(t *testing.T, c client.Client) []lastv1alpha1.IntentApproval {
	t.Helper()
	var list lastv1alpha1.IntentApprovalList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestApproveRecordsTheDisplayedSubjectAsTheUser(t *testing.T) {
	for _, tc := range []struct {
		in             *lastv1alpha1.Intent
		stage          lastv1alpha1.Stage
		subject, shown string
	}{
		{pendingIntent(), lastv1alpha1.StageDiscovery, h1, "Discovery command " + h1},
		{plannedIntent(), lastv1alpha1.StageCreate, h2, "Plan " + h2},
		// status.stage keeps the session's stage while a command is pending (R4).
		{inStage(pendingIntent(), lastv1alpha1.StageUpdate), lastv1alpha1.StageDiscovery, h1, "Discovery command " + h1},
		{inStage(plannedIntent(), lastv1alpha1.StageUpdate), lastv1alpha1.StageUpdate, h2, "Plan " + h2 + " (stage Update)"},
	} {
		c := fakeClient(tc.in)
		d, out := depsFor(c, "")
		if err := execute(d, "approve", tc.in.Name); err != nil {
			t.Fatalf("approve %s: %v", tc.in.Name, err)
		}
		got := approvalsIn(t, c)
		if len(got) != 1 {
			t.Fatalf("approve %s created %d IntentApprovals", tc.in.Name, len(got))
		}
		s := got[0].Spec
		if s.IntentRef.Name != tc.in.Name || s.Stage != tc.stage || s.SubjectHash != tc.subject ||
			s.Decision != lastv1alpha1.DecisionApprove || s.ApprovedBy != testUser || s.Reason != "" {
			t.Fatalf("approval = %+v", s)
		}
		want := "Approved " + subjectNoun(tc.stage) + " " + tc.subject + " as " + testUser
		for _, w := range []string{tc.shown, want} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("approve %s output lacks %q:\n%s", tc.in.Name, w, out)
			}
		}
	}
}

func TestDenyNeedsAReason(t *testing.T) {
	c := fakeClient(pendingIntent())
	d, out := depsFor(c, "")
	if err := execute(d, "deny", "look"); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("deny without --reason = %v", err)
	}
	if err := execute(d, "deny", "look", "--reason", "  "); err == nil {
		t.Fatal("deny with a blank reason succeeded")
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Fatalf("%d IntentApprovals after refused denials", n)
	}
	if err := execute(d, "deny", "look", "--reason", "too broad"); err != nil {
		t.Fatal(err)
	}
	got := approvalsIn(t, c)
	if len(got) != 1 || got[0].Spec.Decision != lastv1alpha1.DecisionDeny || got[0].Spec.Reason != "too broad" ||
		got[0].Spec.Stage != lastv1alpha1.StageDiscovery || got[0].Spec.SubjectHash != h1 {
		t.Fatalf("approvals = %+v", got)
	}
	if !strings.Contains(out.String(), "Denied discovery command "+h1) {
		t.Errorf("deny output:\n%s", out)
	}
}

func TestDecisionsNeedSomethingToDecide(t *testing.T) {
	planning := testIntent(webName, func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhasePlanning })
	d, _ := depsFor(fakeClient(planning), "")
	if err := execute(d, "approve", webName); err == nil || !strings.Contains(err.Error(), "has nothing to decide (phase Planning)") {
		t.Fatalf("approve while planning = %v", err)
	}
}

func TestApproveRefusesAHashThatIsNotTheCurrentSubject(t *testing.T) {
	c := fakeClient(pendingIntent())
	d, _ := depsFor(c, "")
	err := execute(d, "approve", "look", "--hash", h2)
	if err == nil || !strings.Contains(err.Error(), "asks about "+h1) {
		t.Fatalf("approve --hash of another subject = %v", err)
	}
	if n := len(approvalsIn(t, c)); n != 0 {
		t.Fatalf("%d IntentApprovals after a refused approval", n)
	}
	if err := execute(d, "approve", "look", "--hash", h1); err != nil {
		t.Fatalf("approve --hash of the current subject = %v", err)
	}
}

func TestDecideSaysWhenTheSubjectMovedOn(t *testing.T) {
	base := fakeClient(pendingIntent())
	// The model asks for another command while the user decides.
	c := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := cl.Create(ctx, obj, opts...); err != nil {
			return err
		}
		var in lastv1alpha1.Intent
		if err := cl.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "look"}, &in); err != nil {
			return err
		}
		in.Status.PendingCommand.Nonce = "n2"
		in.Status.PendingCommand.Hash = h3
		return cl.Status().Update(ctx, &in)
	}})
	d, out := depsFor(c, "")
	if err := execute(d, "approve", "look"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approved discovery command " + h1, "now asks about " + h3, "matches nothing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDecisionsRefuseASubjectThatDoesNotHashToItsStatedHash(t *testing.T) {
	for _, in := range forgedIntents() {
		stage, stated, _ := subjectOf(in)
		for _, args := range [][]string{
			{"approve", in.Name},
			{"approve", in.Name, "--hash", stated},
			{"deny", in.Name, "--reason", "no"},
		} {
			c := fakeClient(in.DeepCopy())
			d, out := depsFor(c, "")
			err := execute(d, args...)
			want := "intent team-x/" + in.Name + ": the " + subjectNoun(stage) + " does not hash to its stated hash " + stated +
				"; refusing to show or decide it"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("kubectl last %q = %v, want %q", args, err, want)
			}
			if n := len(approvalsIn(t, c)); n != 0 {
				t.Fatalf("kubectl last %q created %d IntentApprovals", args, n)
			}
			if out.Len() != 0 {
				t.Errorf("kubectl last %q showed the forged subject:\n%s", args, out)
			}
		}
	}
}

func TestApproveSetsTheIntentUID(t *testing.T) {
	in := plannedIntent()
	c := fakeClient(in)
	d, _ := depsFor(c, "")
	if err := execute(d, "approve", in.Name); err != nil {
		t.Fatal(err)
	}
	got := approvalsIn(t, c)
	if len(got) != 1 || in.UID == "" || got[0].Spec.IntentUID != string(in.UID) {
		t.Fatalf("approvals = %+v, want one carrying intent UID %q", got, in.UID)
	}
}

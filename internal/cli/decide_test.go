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

var h3 = "sha256:" + strings.Repeat("3", 64)

func pendingIntent() *lastv1alpha1.Intent {
	return testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: seeWhatExists, Command: argvCommand("kubectl", "get", "ns")}
	})
}

func plannedIntent() *lastv1alpha1.Intent {
	return testIntent("web", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: "nginx", Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}}}
	})
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
	planning := testIntent("web", func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhasePlanning })
	d, _ := depsFor(fakeClient(planning), "")
	if err := execute(d, "approve", "web"); err == nil || !strings.Contains(err.Error(), "has nothing to decide (phase Planning)") {
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

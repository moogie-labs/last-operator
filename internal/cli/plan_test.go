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
		in.Status.PendingCommand = &lastv1alpha1.PendingCommand{Hash: h1, Reason: seeWhatExists, Command: argvCommand("kubectl", "get", "ns")}
		setReady(in, "AwaitingDiscoveryApproval", "Waiting for an IntentApproval of discovery command "+h1)
	})
	web := testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Plan = &lastv1alpha1.Plan{Hash: h2, Summary: nginxName, Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{trueCmd}}}}}
	})
	fresh := testIntent("fresh", nil)
	d, out := depsFor(fakeClient(look, web, fresh), "")

	if err := execute(d, "plan", "look"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Intent team-x/look: AwaitingDiscoveryApproval (Waiting for an IntentApproval of discovery command " + h1 + ")\n",
		"Discovery command " + h1} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan look lacks %q:\n%s", want, out)
		}
	}
	out.Reset()
	if err := execute(d, "plan", webName); err != nil || !strings.Contains(out.String(), "Plan "+h2) {
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

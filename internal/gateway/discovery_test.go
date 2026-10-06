package gateway

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

func discoveryCommand() lastv1alpha1.Command {
	c := argvStep(plan.DiscoveryStepID, kubectlBin, "get", "namespace", envNS, "-o", "json")
	c.Providers = []string{testProvider}
	return c
}

func pendingCommand(t *testing.T, c lastv1alpha1.Command, nonce string) *lastv1alpha1.PendingCommand {
	t.Helper()
	h, _, err := plan.DiscoveryHash(c, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return &lastv1alpha1.PendingCommand{Hash: h, Command: c, Nonce: nonce, Reason: "Look at the namespace"}
}

// intentWithDiscovery is an Intent waiting for approval of one discovery command.
func intentWithDiscovery(t *testing.T, c lastv1alpha1.Command, nonce string) *lastv1alpha1.Intent { //nolint:unparam // the nonce is part of the helper contract
	t.Helper()
	return &lastv1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: envIntent, Namespace: envNS, UID: envIntentUID},
		Spec:       lastv1alpha1.IntentSpec{Request: "run a web server"},
		Status: lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingDiscoveryApproval, Stage: lastv1alpha1.StageDiscovery,
			PendingCommand: pendingCommand(t, c, nonce)},
	}
}

func discoveryApproval(name, subject string, decision lastv1alpha1.Decision) *lastv1alpha1.IntentApproval { //nolint:unparam // the name is part of the helper contract
	a := approvalFor(name, subject, decision, approver)
	a.Spec.Stage = lastv1alpha1.StageDiscovery
	return a
}

func (e *testEnv) beginStage(stage lastv1alpha1.Stage, subject string) (*gatewayv1.BeginPhaseResponse, error) {
	return e.g.beginPhase(e.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: string(stage), SubjectHash: subject})
}

// runDiscovery opens, runs, and closes the pending discovery command's phase.
func (e *testEnv) runDiscovery(c lastv1alpha1.Command, subject string) {
	e.t.Helper()
	if resp, err := e.beginStage(lastv1alpha1.StageDiscovery, subject); err != nil || resp.GetSteps() != 1 {
		e.t.Fatalf("BeginPhase(Discovery) = %v, %v", resp, err)
	}
	if r, err := e.run(subject, c); err != nil || r.GetResult().GetExitCode() != 0 {
		e.t.Fatalf("RunCommand(discovery) = %v, %v", r, err)
	}
	if _, err := e.end(subject, true); err != nil {
		e.t.Fatalf("EndPhase = %v", err)
	}
}

// setPending replaces the stored Intent's pending command, as the operator does.
func (e *testEnv) setPending(pc *lastv1alpha1.PendingCommand) {
	e.t.Helper()
	var in lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		e.t.Fatal(err)
	}
	in.Status.Stage, in.Status.PendingCommand = lastv1alpha1.StageDiscovery, pc
	if err := e.crd.Update(e.ctx, &in); err != nil {
		e.t.Fatal(err)
	}
}

func TestDiscoveryRunsAnApprovedCommandAsAOneStepPhase(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	env.runDiscovery(c, subject)

	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || rec.Executed != nil || !slices.Contains(rec.Used, subject) {
		t.Fatalf("record = %+v, %v; want no executed plan and the subject used", rec, err)
	}
	var types []string
	for _, e := range env.entries() {
		if e.Intent != envKey {
			continue
		}
		types = append(types, e.Type)
		if e.Type == audit.TypePhaseBegin && e.Stage != string(lastv1alpha1.StageDiscovery) {
			t.Fatalf("phase.begin stage = %q", e.Stage)
		}
	}
	want := []string{audit.TypeApprovalObserved, audit.TypePhaseBegin, audit.TypeCommandStart, audit.TypeCredentialIssued, audit.TypeCommandResult, audit.TypePhaseEnd}
	if !slices.Equal(types, want) {
		t.Fatalf("entries = %v, want %v", types, want)
	}
}

func TestDiscoveryRefusesASubjectThatIsNotThePendingCommand(t *testing.T) {
	c := discoveryCommand()
	other, _, _ := plan.DiscoveryHash(c, "other-nonce")
	cases := map[string]struct {
		pending func(*lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand
		subject func(*lastv1alpha1.PendingCommand) string
		code    codes.Code
	}{
		"another subject": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { return pc },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.PermissionDenied},
		"a hash that does not match the command": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Hash = other; return pc },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.PermissionDenied},
		"no nonce": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Nonce = ""; return pc },
			func(pc *lastv1alpha1.PendingCommand) string { return pc.Hash }, codes.InvalidArgument},
		"not the discovery step": {func(pc *lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { pc.Command.ID = "s1"; return pc },
			func(pc *lastv1alpha1.PendingCommand) string { return pc.Hash }, codes.InvalidArgument},
		"no pending command": {func(*lastv1alpha1.PendingCommand) *lastv1alpha1.PendingCommand { return nil },
			func(*lastv1alpha1.PendingCommand) string { return other }, codes.FailedPrecondition},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := intentWithDiscovery(t, c, "n1")
			subject := tc.subject(in.Status.PendingCommand)
			in.Status.PendingCommand = tc.pending(in.Status.PendingCommand)
			env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
			_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
			env.wantDenied(err, tc.code)
		})
	}
}

func TestDiscoveryNeedsADiscoveryStageApproval(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, approvalFor("create", subject, lastv1alpha1.DecisionApprove, approver))
	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestDiscoveryApprovalsAreSingleUse(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	env.runDiscovery(c, subject)

	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "already opened") {
		t.Fatalf("rerun = %v, want the subject refused as used", err)
	}
	// The same command under a new nonce is a new subject; the old approval does not cover it.
	again := pendingCommand(t, c, "n2")
	env.setPending(again)
	_, err = env.beginStage(lastv1alpha1.StageDiscovery, again.Hash)
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestDiscoveryNeverReplacesTheExecutedPlan(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	c := discoveryCommand()
	pc := pendingCommand(t, c, "n1")
	env.setPending(pc)
	if err := env.crd.Create(env.ctx, discoveryApproval("look", pc.Hash, lastv1alpha1.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	env.runDiscovery(c, pc.Hash)

	rec, err := env.g.state.Load(env.ctx, envKey)
	if err != nil || rec.Executed == nil || rec.Executed.PlanHash != in.Status.Plan.Hash {
		t.Fatalf("executed plan = %+v, %v; want the Create plan kept", rec.Executed, err)
	}
	probes, err := env.g.runProbes(env.ctx, &gatewayv1.RunProbesRequest{Intent: ref()})
	if err != nil || probes.GetPlanHash() != in.Status.Plan.Hash {
		t.Fatalf("RunProbes = %v, %v", probes, err)
	}
}

func TestDiscoveryRefusesAnOversizedCommand(t *testing.T) {
	c := discoveryCommand()
	c.Files = map[string]string{"/work/big": strings.Repeat("x", plan.MaxPendingCommandBytes)}
	in := intentWithDiscovery(t, c, "n1")
	subject := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in, discoveryApproval("look", subject, lastv1alpha1.DecisionApprove))
	_, err := env.beginStage(lastv1alpha1.StageDiscovery, subject)
	env.wantDenied(err, codes.InvalidArgument)
}

func TestBeginPhaseStillRefusesOtherStages(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, in.Status.Plan.Hash)
	env.wantDenied(err, codes.FailedPrecondition)
}

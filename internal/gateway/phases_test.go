package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const msgNoApproval = "no IntentApproval"

func twoSteps() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{
		argvStep("s1", "kubectl", "get", "ns", "team-x", "-o", "json"),
		argvStep("s2", "echo", "${steps.s1.json.kind}"),
	}
}

func TestBeginPhaseOpensAnApprovedPlan(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	resp, err := env.begin(h)
	if err != nil || resp.GetSteps() != 2 {
		t.Fatalf("BeginPhase = %v, %v", resp, err)
	}
	es := env.entries()
	observed, begin := es[len(es)-2], es[len(es)-1]
	if observed.Type != audit.TypeApprovalObserved || begin.Type != audit.TypePhaseBegin {
		t.Fatalf("last entries = %s, %s", observed.Type, begin.Type)
	}
	if begin.Actor != testOperator || begin.Intent != envKey || begin.Stage != testStage || begin.SubjectHash != h ||
		begin.AuthorizedBy == nil || begin.AuthorizedBy.Kind != "approval" || begin.AuthorizedBy.Name != envNS+"/ok" {
		t.Fatalf("phase.begin = %+v", begin)
	}
	var p phaseBegin
	if err := json.Unmarshal(begin.Payload, &p); err != nil {
		t.Fatal(err)
	}
	canonical, _ := plan.Canonical(plan.FromStatus(in.Status.Plan))
	if p.Plan != string(canonical) || p.Steps != 2 {
		t.Fatalf("phase.begin payload = %+v", p)
	}
	rec, err := env.g.state.Load(context.Background(), envKey)
	if err != nil || rec.Open == nil || rec.Open.BeginSeq != begin.Seq || rec.Open.Epoch != env.g.epoch {
		t.Fatalf("record = %+v, %v", rec.IntentRecord, err)
	}
	if _, err := env.begin(h); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), h) {
		t.Fatalf("second BeginPhase = %v, want AlreadyExists naming %s", err, h)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatalf("last entry = %s, want policy.denied", env.last().Type)
	}
}

func TestBeginPhaseNeedsAWritableAuditLog(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	env.g.writer.Store(nil)
	if _, err := env.begin(in.Status.Plan.Hash); status.Code(err) != codes.Unavailable {
		t.Fatalf("BeginPhase = %v, want Unavailable", err)
	}
	if len(env.entries()) != before {
		t.Fatal("an entry was written without a writer")
	}
}

func TestBeginPhaseDenials(t *testing.T) {
	steps := twoSteps()
	cases := []struct {
		name    string
		mutate  func(in *lastv1alpha1.Intent) []runtime.Object
		code    codes.Code
		message string
	}{
		{"plan edited after hashing", func(in *lastv1alpha1.Intent) []runtime.Object {
			in.Status.Plan.Steps[0].Run.Argv = []string{"kubectl", "delete", "ns", "team-x"}
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)}
		}, codes.PermissionDenied, "does not match"},
		{"no approval", func(in *lastv1alpha1.Intent) []runtime.Object { return nil }, codes.FailedPrecondition, msgNoApproval},
		{"approval for another plan", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("other", "sha256:"+strings.Repeat("0", 64), lastv1alpha1.DecisionApprove, approver)}
		}, codes.FailedPrecondition, msgNoApproval},
		{"denied", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver),
				approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")}
		}, codes.PermissionDenied, "denies"},
		{"approved by the operator", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, testOperator)}
		}, codes.PermissionDenied, "names the operator"},
		{"provider not allowed", func(in *lastv1alpha1.Intent) []runtime.Object {
			in.Status.Plan.Steps[0].Providers = []string{"prod"}
			h, _ := plan.Hash(plan.FromStatus(in.Status.Plan))
			in.Status.Plan.Hash = h
			return []runtime.Object{approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver)}
		}, codes.PermissionDenied, `provider "prod"`},
		{"approval in another namespace", func(in *lastv1alpha1.Intent) []runtime.Object {
			a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
			a.Namespace = tokOther
			return []runtime.Object{a}
		}, codes.FailedPrecondition, msgNoApproval},
		{"approval for another intent", func(in *lastv1alpha1.Intent) []runtime.Object {
			a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
			a.Spec.IntentRef.Name = tokOther
			return []runtime.Object{a}
		}, codes.FailedPrecondition, msgNoApproval},
		{"approval for another stage", func(in *lastv1alpha1.Intent) []runtime.Object {
			a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
			a.Spec.Stage = lastv1alpha1.StageUpdate
			return []runtime.Object{a}
		}, codes.FailedPrecondition, msgNoApproval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := intentWithPlan(t, append([]lastv1alpha1.Command(nil), steps...))
			in.Status.Plan.Steps = append([]lastv1alpha1.Command(nil), in.Status.Plan.Steps...)
			extra := tc.mutate(in)
			objs := []runtime.Object{in, allowAll()}
			objs = append(objs, extra...)
			env := newTestEnv(t, toClientObjects(objs)...)
			_, err := env.begin(in.Status.Plan.Hash)
			if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("BeginPhase = %v, want %v containing %q", err, tc.code, tc.message)
			}
			last := env.last()
			if last.Type != audit.TypePolicyDenied || last.Intent != envKey || last.Actor != testOperator {
				t.Fatalf("last entry = %s %+v, want policy.denied", last.Type, last)
			}
			for _, e := range env.entries() {
				if e.Type == audit.TypePhaseBegin {
					t.Fatal("a denied BeginPhase wrote phase.begin")
				}
			}
		})
	}
}

func TestBeginPhaseOpensCreateOnly(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	_, err := env.g.beginPhase(env.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: "Update", SubjectHash: in.Status.Plan.Hash})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Update phase = %v, want FailedPrecondition", err)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatalf("last entry = %s, want policy.denied", env.last().Type)
	}
}

func TestRestartClosesTheInterruptedPhaseAndNeverRerunsIt(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	env.g = env.newGateway() // the gateway restarted mid-phase

	if _, err := env.begin(h); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-running the interrupted plan = %v, want FailedPrecondition", err)
	}
	var interrupted bool
	for _, e := range env.entries() {
		var p phaseEnd
		if e.Type == audit.TypePhaseEnd && json.Unmarshal(e.Payload, &p) == nil && p.Outcome == outcomeInterrupted {
			interrupted = e.Actor == sandbox.GatewayActor && e.SubjectHash == h
		}
	}
	if !interrupted {
		t.Fatal("no phase.end interrupted entry by the gateway")
	}

	replanned := intentWithPlan(t, []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "ns", "team-x")})
	if err := env.crd.Update(context.Background(), withVersion(t, env, replanned)); err != nil {
		t.Fatal(err)
	}
	if err := env.crd.Create(context.Background(), approvalFor("ok2", replanned.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.begin(replanned.Status.Plan.Hash); err != nil {
		t.Fatalf("a new plan after the restart = %v", err)
	}
}

func TestEndPhase(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.end(h, false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase without a phase = %v", err)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatalf("last entry = %s, want policy.denied", env.last().Type)
	}
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase(succeeded) before any step = %v, want FailedPrecondition", err)
	}
	if env.last().Type != audit.TypePolicyDenied {
		t.Fatalf("last entry = %s, want policy.denied", env.last().Type)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
	var p phaseEnd
	last := env.last()
	if last.Type != audit.TypePhaseEnd || json.Unmarshal(last.Payload, &p) != nil || p.Outcome != outcomeFailed || last.Actor != testOperator {
		t.Fatalf("last = %s %s", last.Type, last.Payload)
	}
	rec, _ := env.g.state.Load(context.Background(), envKey)
	if rec.Open != nil || rec.Executed != nil {
		t.Fatalf("record after a failed phase = %+v", rec.IntentRecord)
	}
	if _, err := env.begin(h); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("re-opening a plan that already ran = %v, want FailedPrecondition", err)
	}
}

func TestBeginPhaseStoresPlansWithFloatsAsCanonicalText(t *testing.T) {
	res := lastv1alpha1.ManagedResourceSpec{Desired: &runtime.RawExtension{Raw: []byte(`{"ratio":0.75,"big":12345678901234567890}`)}}
	in := intentWithPlan(t, twoSteps(), res)
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(in.Status.Plan.Hash); err != nil {
		t.Fatalf("BeginPhase with floats in desired = %v", err)
	}
	if !strings.Contains(string(env.last().Payload), "0.75") {
		t.Fatalf("phase.begin = %s", env.last().Payload)
	}
}

func toClientObjects(objs []runtime.Object) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(client.Object))
	}
	return out
}

// withVersion copies the stored Intent's resourceVersion onto in so Update succeeds.
func withVersion(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) *lastv1alpha1.Intent {
	t.Helper()
	var cur lastv1alpha1.Intent
	if err := env.crd.Get(context.Background(), client.ObjectKeyFromObject(in), &cur); err != nil {
		t.Fatal(err)
	}
	in.ResourceVersion = cur.ResourceVersion
	return in
}

func TestEndPhaseRetriesTheStateSaveWithoutASecondPhaseEnd(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	p.next = len(p.body.Steps) // both steps ran
	var fail atomic.Bool
	fail.Store(true)
	env.kube.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.CompareAndSwap(true, false) {
			return true, nil, errors.New("etcd unavailable")
		}
		return false, nil, nil
	})
	if _, err := env.end(h, true); status.Code(err) != codes.Unavailable {
		t.Fatalf("first EndPhase = %v, want Unavailable", err)
	}
	if _, err := env.end(h, false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase with another outcome = %v, want FailedPrecondition", err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("retried EndPhase = %v", err)
	}
	var ends int
	for _, e := range env.entries() {
		var pe phaseEnd
		if e.Type == audit.TypePhaseEnd && json.Unmarshal(e.Payload, &pe) == nil && pe.BeginSeq == p.beginSeq {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("phase.end entries for the phase = %d, want 1", ends)
	}
	rec, _ := env.g.state.Load(context.Background(), envKey)
	if rec.Open != nil || rec.Executed == nil || rec.Executed.PlanHash != h {
		t.Fatalf("record = %+v", rec.IntentRecord)
	}
}

func TestEndPhaseMarksTheStalePhaseClosed(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		t.Fatal("a phase held across EndPhase is not marked closed")
	}
}

func TestRestartMarksTheOldPhaseClosed(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	env.g.epoch = "next" // the same process sees its open phase as another epoch's
	if _, err := env.begin(h); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("BeginPhase = %v", err)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		t.Fatal("an interrupted phase is not marked closed")
	}
}

func TestPhaseRequestsRejectMalformedSubjectsWithoutAuditing(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll())
	before := len(env.entries())
	for _, bad := range []string{"", "sha256:abc", "SHA256:" + strings.Repeat("0", 64), "sha256:" + strings.Repeat("G", 64)} {
		if _, err := env.begin(bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("BeginPhase(%q) = %v", bad, err)
		}
		if _, err := env.end(bad, false); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("EndPhase(%q) = %v", bad, err)
		}
	}
	if len(env.entries()) != before {
		t.Fatal("a malformed request wrote an audit entry")
	}
}

func TestTruncateReason(t *testing.T) {
	if got := truncateReason("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	for _, r := range []string{"é", "€"} { // 2- and 3-byte runes; 1024 is mid-rune for "€"
		got := truncateReason(strings.Repeat(r, 1000))
		if !utf8.ValidString(got) || !strings.HasSuffix(got, "…(truncated)") || len(got) > maxReasonBytes+len("…(truncated)") {
			t.Fatalf("truncated %q reason = %d bytes, valid=%v", r, len(got), utf8.ValidString(got))
		}
	}
}

func TestEffectivePolicyErrorCarriesAGRPCStatus(t *testing.T) {
	bad := allowAll()
	bad.Spec.Defaults.Egress.Allowed = []string{"not a pattern"}
	env := newTestEnv(t, bad)
	_, err := env.g.effectivePolicy(env.ctx, envNS)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "invalid policy") {
		t.Fatalf("effectivePolicy = %v, want FailedPrecondition", err)
	}
}

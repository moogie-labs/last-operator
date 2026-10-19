package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		argvStep("s1", kubectlBin, "get", "ns", "team-x", "-o", "json"),
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
	rec, err := env.g.state.LoadAny(context.Background(), envKey)
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
			in.Status.Plan.Steps[0].Run.Argv = []string{kubectlBin, "delete", "ns", "team-x"}
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
		{"approved by a gateway-namespace ServiceAccount", func(in *lastv1alpha1.Intent) []runtime.Object {
			return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, "system:serviceaccount:"+ns+":anything")}
		}, codes.PermissionDenied, "gateway-namespace ServiceAccount"},
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

	replanned := intentWithPlan(t, []lastv1alpha1.Command{argvStep("s1", kubectlBin, "get", "ns", "team-x")})
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
	rec, _ := env.g.state.LoadAny(context.Background(), envKey)
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
	rec, _ := env.g.state.LoadAny(context.Background(), envKey)
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

// bigFilePlan is a one-step plan carrying fill in a file, about n bytes of canonical JSON.
func bigFilePlan(t *testing.T, fill string, n int) *lastv1alpha1.Intent {
	t.Helper()
	step := argvStep("s1", "true")
	step.Files = map[string]string{"/work/big": strings.Repeat(fill, n)}
	return intentWithPlan(t, []lastv1alpha1.Command{step})
}

func TestMarkupHeavyPlanFitsItsRecord(t *testing.T) {
	in := bigFilePlan(t, "<>&", MaxPlanBytes/3-100)
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	if _, err := env.run(h, in.Status.Plan.Steps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase after an accepted BeginPhase = %v", err)
	}
	cm, err := env.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), recordName(envKey), metav1.GetOptions{})
	if err != nil || len(cm.Data[stateKey]) > MaxRecordBytes {
		t.Fatalf("record = %d bytes, %v", len(cm.Data[stateKey]), err)
	}
}

func TestBeginPhaseRefusesAPlanWhoseRecordWouldNotFit(t *testing.T) {
	in := bigFilePlan(t, `"\`, MaxPlanBytes/4-200) // every byte is escaped twice in the record
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	_, err := env.begin(h)
	env.wantDenied(err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), "plan too large to record") {
		t.Fatalf("BeginPhase = %v", err)
	}
	for _, e := range env.entries() {
		if e.Type == audit.TypePhaseBegin {
			t.Fatal("a refused BeginPhase wrote phase.begin")
		}
	}
}

func TestFindApprovalNeedsAnExplicitApprove(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("odd", h, lastv1alpha1.Decision("Maybe"), approver))
	_, err := env.begin(h)
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), msgNoApproval) {
		t.Fatalf("BeginPhase = %v, want %q", err, msgNoApproval)
	}
}

func TestDenyTruncatesItsReason(t *testing.T) {
	env := newTestEnv(t)
	w, release, err := env.g.beginRPC()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	d := denial{ctx: env.ctx, w: w, actor: testOperator, intent: envKey, rpc: "Test"}
	err = d.deny(codes.PermissionDenied, "%s", strings.Repeat("x", 5000))
	if status.Code(err) != codes.PermissionDenied || len(err.Error()) > 2*maxReasonBytes {
		t.Fatalf("deny = %d bytes, %v", len(err.Error()), status.Code(err))
	}
	if n := len(env.last().Payload); n > 2*maxReasonBytes {
		t.Fatalf("policy.denied payload = %d bytes", n)
	}
}

func TestIntentKeyValidatesNames(t *testing.T) {
	long := strings.Repeat("a", 254)
	for _, tc := range []struct {
		ns, name string
		ok       bool
	}{
		{envNS, envIntent, true}, {envNS, envIntent + ".v1", true}, {"Team", envIntent, false}, {envNS, "we b", false},
		{envNS, "../web", false}, {"team/x", envIntent, false}, {envNS, "web\n", false}, {envNS, long, false},
		{long, envIntent, false}, {"", envIntent, false}, {envNS, "", false}, {"-team", envIntent, false},
	} {
		_, _, key, err := intentKey(&gatewayv1.IntentRef{Namespace: tc.ns, Name: tc.name})
		if tc.ok != (err == nil) || (!tc.ok && status.Code(err) != codes.InvalidArgument) {
			t.Errorf("intentKey(%q, %q) = %q, %v; want ok=%v InvalidArgument", tc.ns, tc.name, key, err, tc.ok)
		}
	}
}

func TestInterruptedAndAbortedPhaseEndsCarryTheAuthorization(t *testing.T) {
	phaseEndFor := func(env *testEnv, outcome string) audit.Entry {
		for _, e := range env.entries() {
			var p phaseEnd
			if e.Type == audit.TypePhaseEnd && json.Unmarshal(e.Payload, &p) == nil && p.Outcome == outcome {
				return e
			}
		}
		t.Fatalf("no phase.end %s entry", outcome)
		return audit.Entry{}
	}
	want := envNS + "/ok"
	t.Run("interrupted", func(t *testing.T) {
		in := intentWithPlan(t, twoSteps())
		h := in.Status.Plan.Hash
		env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
		if _, err := env.begin(h); err != nil {
			t.Fatal(err)
		}
		env.g = env.newGateway()
		_, _ = env.begin(h)
		if e := phaseEndFor(env, outcomeInterrupted); e.AuthorizedBy == nil || e.AuthorizedBy.Name != want {
			t.Fatalf("interrupted phase.end AuthorizedBy = %+v", e.AuthorizedBy)
		}
	})
	t.Run("aborted", func(t *testing.T) {
		in := intentWithPlan(t, twoSteps())
		h := in.Status.Plan.Hash
		env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
		env.kube.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("etcd unavailable")
		})
		if _, err := env.begin(h); status.Code(err) != codes.Unavailable {
			t.Fatalf("BeginPhase = %v, want Unavailable", err)
		}
		if e := phaseEndFor(env, outcomeAborted); e.AuthorizedBy == nil || e.AuthorizedBy.Name != want {
			t.Fatalf("aborted phase.end AuthorizedBy = %+v", e.AuthorizedBy)
		}
	})
}

func beginWith(t *testing.T, mutate func(in *lastv1alpha1.Intent) []runtime.Object, cfg *lastv1alpha1.OperatorConfig, at func(*testEnv)) error {
	t.Helper()
	in := intentWithPlan(t, twoSteps())
	objs := append([]runtime.Object{in, cfg}, mutate(in)...)
	env := newTestEnv(t, toClientObjects(objs)...)
	if at != nil {
		at(env)
	}
	_, err := env.begin(in.Status.Plan.Hash)
	return err
}

func TestApprovalForAnotherUIDIsIgnored(t *testing.T) {
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
		a.Spec.IntentUID = "a-previous-intent"
		return []runtime.Object{a}
	}, allowAll(), nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), msgNoApproval) {
		t.Fatalf("BeginPhase = %v, want %q", err, msgNoApproval)
	}
}

func TestStaleApprovalIsIgnored(t *testing.T) {
	cfg := allowAll()
	cfg.Spec.Approvals = &lastv1alpha1.ApprovalsConfig{MaxAge: &metav1.Duration{Duration: time.Hour}}
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
		a.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
		return []runtime.Object{a}
	}, cfg, nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), msgNoApproval) {
		t.Fatalf("BeginPhase = %v, want %q", err, msgNoApproval)
	}
}

func TestApprovalAgesWithTheGatewayClockAndDefaultsTo24h(t *testing.T) {
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)}
	}, allowAll(), func(e *testEnv) {
		e.g.builds.now = func() time.Time { return time.Now().Add(23 * time.Hour) }
	})
	if err != nil {
		t.Fatalf("23h-old approval refused under the 24h default: %v", err)
	}
	err = beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)}
	}, allowAll(), func(e *testEnv) {
		e.g.builds.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("25h-old approval: BeginPhase = %v, want refused", err)
	}
}

func TestDenyForAnotherUIDIsIgnored(t *testing.T) {
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		d := approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
		d.Spec.IntentUID = "a-previous-intent"
		return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver), d}
	}, allowAll(), nil)
	if err != nil {
		t.Fatalf("a foreign-UID Deny stopped a valid Approve: %v", err)
	}
}

func TestDenyForThisUIDWins(t *testing.T) {
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver),
			approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")}
	}, allowAll(), nil)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "denies") {
		t.Fatalf("BeginPhase = %v, want a denial", err)
	}
}

func TestApprovalAgeBoundaryIsInclusive(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cfg := allowAll()
	cfg.Spec.Approvals = &lastv1alpha1.ApprovalsConfig{MaxAge: &metav1.Duration{Duration: time.Hour}}
	try := func(age time.Duration) error {
		return beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
			a := approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
			a.CreationTimestamp = metav1.NewTime(base)
			return []runtime.Object{a}
		}, cfg.DeepCopy(), func(e *testEnv) { e.g.builds.now = func() time.Time { return base.Add(age) } })
	}
	if err := try(time.Hour); err != nil {
		t.Fatalf("an approval exactly maxAge old = %v, want accepted", err)
	}
	if err := try(time.Hour + time.Second); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an approval maxAge+1s old = %v, want ignored", err)
	}
}

func TestStaleDenyIsIgnored(t *testing.T) {
	cfg := allowAll()
	cfg.Spec.Approvals = &lastv1alpha1.ApprovalsConfig{MaxAge: &metav1.Duration{Duration: time.Hour}}
	err := beginWith(t, func(in *lastv1alpha1.Intent) []runtime.Object {
		d := approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
		d.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
		return []runtime.Object{approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver), d}
	}, cfg, nil)
	if err != nil {
		t.Fatalf("a stale Deny stopped a valid Approve: %v", err)
	}
}

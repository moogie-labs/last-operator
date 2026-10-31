package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// fakeAuditor serves QueryAudit from entries, at most pageSize per call when set.
type fakeAuditor struct {
	entries   []*gatewayv1.AuditEntry
	truncated bool
	pageSize  int
	err       error
	calls     int
}

func (f *fakeAuditor) QueryAudit(_ context.Context, in *gatewayv1.QueryAuditRequest, _ ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	resp := &gatewayv1.QueryAuditResponse{Truncated: f.truncated}
	for _, e := range f.entries {
		if e.GetSeq() <= in.GetAfterSeq() || !slices.Contains(in.GetTypes(), e.GetType()) {
			continue
		}
		if f.pageSize > 0 && len(resp.Entries) == f.pageSize {
			resp.More = true
			break
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

func entry(seq uint64, typ, stage, subject string, payload any) *gatewayv1.AuditEntry {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return &gatewayv1.AuditEntry{Seq: seq, Type: typ, Stage: stage, SubjectHash: subject, Payload: b}
}

func TestDiscoveryResultReadsTheCommandsEntry(t *testing.T) {
	a := &fakeAuditor{pageSize: 1, entries: []*gatewayv1.AuditEntry{
		entry(3, audit.TypeCommandResult, "Discovery", "sha256:old", commandResult{Stdout: "old"}),
		entry(4, audit.TypeLLMRequest, "", "", map[string]any{}),
		entry(5, audit.TypeCommandResult, "Create", "sha256:cmd", commandResult{Stdout: "a step"}),
		entry(7, audit.TypeCommandResult, "Discovery", "sha256:cmd",
			commandResult{ExitCode: 2, Stdout: "default\n", Stderr: "boom", StderrTruncated: true}),
	}}
	got, err := DiscoveryResult(context.Background(), a, ref, "sha256:cmd", 1)
	if err != nil || got != "The command exited 2.\nstdout:\ndefault\nstderr (truncated):\nboom\n" {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
	if a.calls != 3 {
		t.Errorf("QueryAudit called %d times, want 3 pages", a.calls)
	}
}

func TestDiscoveryResultReportsALostResult(t *testing.T) {
	got, err := DiscoveryResult(context.Background(), &fakeAuditor{truncated: true}, ref, "sha256:cmd", 0)
	if err != nil || got != "The command ran, but its result could not be read back: the audit log index no longer holds it" {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
	got, err = DiscoveryResult(context.Background(), &fakeAuditor{}, ref, "sha256:cmd", 0)
	if err != nil || !strings.HasSuffix(got, "the audit log has no result entry for it") {
		t.Fatalf("DiscoveryResult = %q, %v", got, err)
	}
}

func TestDiscoveryResultReturnsGatewayErrorsUnchanged(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "the index could not be saved")}
	if _, err := DiscoveryResult(context.Background(), a, ref, "sha256:cmd", 0); status.Code(err) != codes.Unavailable {
		t.Fatalf("DiscoveryResult = %v", err)
	}
}

func TestResultTurnClipsEachStream(t *testing.T) {
	big := strings.Repeat("é", maxResultBytes) // two bytes each
	got := resultTurn(commandResult{Stdout: big, Error: timedOut})
	if !strings.HasPrefix(got, "The command could not finish: timed out\nstdout (truncated):\n") ||
		!strings.HasSuffix(got, "\nstderr: empty\n") || !utf8.ValidString(got) || len(got) > maxResultBytes+100 {
		t.Fatalf("resultTurn = %d bytes: %.120q", len(got), got)
	}
}

// sessionEntries is the audit index of an Intent whose current planning
// session started at llm.request 5, after an earlier session at 2.
func sessionEntries() []*gatewayv1.AuditEntry {
	msg := func(role, content string) map[string]string {
		return map[string]string{"role": role, "content": content}
	}
	sys, user := msg("system", "sys"), msg("user", "run nginx")
	asked, answers := msg("assistant", questionsAnswer), msg("user", "Answers:\n- q1: default\n")
	asks, result := msg("assistant", commandAnswer), msg("user", "The command exited 0.")
	garbage, correction := msg("assistant", "garbage"), msg("user", "That answer is not a valid turn")
	req := func(seq uint64, msgs ...map[string]string) *gatewayv1.AuditEntry {
		return entry(seq, audit.TypeLLMRequest, "", "", map[string]any{"messages": msgs})
	}
	resp := func(seq, request uint64, content, errText string) *gatewayv1.AuditEntry {
		return entry(seq, audit.TypeLLMResponse, "", "", llmResponse{RequestSeq: request, Content: content, Error: errText})
	}
	return []*gatewayv1.AuditEntry{
		req(2, sys, user), resp(3, 2, commandAnswer, ""),
		req(5, sys, user), resp(6, 5, questionsAnswer, ""),
		req(8, sys, user, asked, answers), resp(9, 8, commandAnswer, ""),
		req(10, sys, user, asked, answers, asks, result), resp(11, 10, "garbage", ""),
		req(12, sys, user, asked, answers, asks, result, garbage, correction), resp(13, 12, "", "llm: unavailable"),
	}
}

func TestReloadRebuildsTheSessionFromItsLatestValidAnswer(t *testing.T) {
	a := &fakeAuditor{pageSize: 2, entries: sessionEntries()}
	conv, err := Reload(context.Background(), a, ref, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	// llm.request 12 failed and 10's answer is not a turn: 8's answer is the last one the model gave.
	if roles(conv) != "system,user,assistant,user,assistant" || conv[4].GetContent() != commandAnswer ||
		conv.ModelTurns() != 2 || conv.Commands() != 1 {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestReloadRefusesASessionTheIndexNoLongerHolds(t *testing.T) {
	unanswered := []*gatewayv1.AuditEntry{
		entry(5, audit.TypeLLMRequest, "", "", map[string]any{"messages": []any{}}),
		entry(6, audit.TypeLLMResponse, "", "", llmResponse{RequestSeq: 5, Content: "garbage"}),
	}
	for name, a := range map[string]*fakeAuditor{
		"truncated":          {entries: sessionEntries(), truncated: true},
		"first request lost": {entries: sessionEntries()[4:]},
		"no valid answer":    {entries: unanswered},
	} {
		if _, err := Reload(context.Background(), a, ref, 5, nil); !errors.Is(err, ErrNoReload) {
			t.Errorf("%s: Reload = %v, want ErrNoReload", name, err)
		}
	}
}

func TestReloadReturnsGatewayErrorsUnchanged(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "the index could not be saved")}
	if _, err := Reload(context.Background(), a, ref, 5, nil); status.Code(err) != codes.Unavailable || errors.Is(err, ErrNoReload) {
		t.Fatalf("Reload = %v", err)
	}
}

func TestReloadSkipsAnswersTheIntentDoesNotRecord(t *testing.T) {
	a := &fakeAuditor{entries: sessionEntries()}
	// 8's and 5's answers are valid; only the earlier one is recorded.
	conv, err := Reload(context.Background(), a, ref, 5, func(turn Turn) bool { return turn.Kind == KindQuestions })
	if err != nil {
		t.Fatal(err)
	}
	if roles(conv) != "system,user,assistant" || conv[2].GetContent() != questionsAnswer {
		t.Fatalf("conversation = %v", conv)
	}
	_, err = Reload(context.Background(), a, ref, 5, func(Turn) bool { return false })
	if !errors.Is(err, ErrNoReload) {
		t.Fatalf("Reload = %v, want ErrNoReload", err)
	}
}

func TestUsedPlansSkipsDiscovery(t *testing.T) {
	a := &fakeAuditor{pageSize: 1, entries: []*gatewayv1.AuditEntry{
		entry(1, audit.TypePhaseBegin, "Discovery", "sha256:cmd", nil),
		entry(2, audit.TypePhaseBegin, "Create", "sha256:one", nil),
		entry(3, audit.TypePhaseBegin, "Update", "sha256:two", nil),
		entry(4, audit.TypeLLMRequest, "", "sha256:other", nil),
	}}
	got, err := UsedPlans(context.Background(), a, ref)
	if err != nil {
		t.Fatalf("UsedPlans = %v", err)
	}
	if len(got) != 2 || !got["sha256:one"] || !got["sha256:two"] {
		t.Errorf("UsedPlans = %v, want the Create and Update subjects only", got)
	}
	if _, err := UsedPlans(context.Background(), &fakeAuditor{err: status.Error(codes.Unavailable, "down")}, ref); err == nil {
		t.Error("UsedPlans hid a gateway error")
	}
}

func planBody(argv string) plan.Body {
	return plan.Body{Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{argv}}}}}
}

// executedPhase returns the phase.begin for body at seq and its phase.end at seq+1.
func executedPhase(t *testing.T, seq uint64, stage string, body plan.Body, outcome string) []*gatewayv1.AuditEntry {
	t.Helper()
	canonical, err := plan.Canonical(body)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		t.Fatal(err)
	}
	return []*gatewayv1.AuditEntry{
		entry(seq, audit.TypePhaseBegin, stage, hash, map[string]any{"plan": string(canonical), "steps": 1}),
		entry(seq+1, audit.TypePhaseEnd, stage, hash, map[string]any{"beginSeq": seq, "outcome": outcome}),
	}
}

func TestExecutedPlanIsTheLatestSucceededCreateOrUpdate(t *testing.T) {
	entries := make([]*gatewayv1.AuditEntry, 0, 12)
	for i, ph := range []struct{ stage, argv, outcome string }{
		{"Create", "p1", outcomeSucceeded}, {"Discovery", "d", outcomeSucceeded}, {"Create", "p2", "failed"},
		{"Update", "p3", outcomeSucceeded}, {"Remediate", "p4", outcomeSucceeded}, {"Delete", "p5", outcomeSucceeded},
	} {
		entries = append(entries, executedPhase(t, uint64(i*2+1), ph.stage, planBody(ph.argv), ph.outcome)...)
	}
	want := planBody("p3")
	wantHash, _ := plan.Hash(want)
	body, hash, found, err := ExecutedPlan(context.Background(), &fakeAuditor{pageSize: 1, entries: entries}, ref)
	if err != nil || !found {
		t.Fatalf("ExecutedPlan = found %v, err %v", found, err)
	}
	if hash != wantHash || !reflect.DeepEqual(body, want) {
		t.Errorf("ExecutedPlan = %v %s, want %v %s", body, hash, want, wantHash)
	}
}

func TestExecutedPlanNotFound(t *testing.T) {
	entries := append(executedPhase(t, 1, "Create", planBody("p1"), "failed"), executedPhase(t, 3, "Remediate", planBody("p2"), outcomeSucceeded)...)
	for name, a := range map[string]*fakeAuditor{"none": {}, "only failures and other stages": {entries: entries}} {
		_, _, found, err := ExecutedPlan(context.Background(), a, ref)
		if found || err != nil {
			t.Errorf("%s: ExecutedPlan = found %v, err %v, want not found", name, found, err)
		}
	}
}

// A truncated index drops the oldest entries first and the phases of one Intent
// never overlap, so a succeeded pair still visible is newer than any dropped one.
func TestExecutedPlanIsFoundWhileTheIndexIsTruncated(t *testing.T) {
	entries := append(executedPhase(t, 5, "Create", planBody("p1"), outcomeSucceeded), executedPhase(t, 7, "Update", planBody("p2"), outcomeSucceeded)...)
	want := planBody("p2")
	wantHash, _ := plan.Hash(want)
	body, hash, found, err := ExecutedPlan(context.Background(), &fakeAuditor{truncated: true, entries: entries}, ref)
	if err != nil || !found || hash != wantHash || !reflect.DeepEqual(body, want) {
		t.Errorf("ExecutedPlan = %v %s, found %v, err %v; want p2 %s", body, hash, found, err, wantHash)
	}
}

func TestExecutedPlanIsNotFoundWhenTruncationTookItsBegin(t *testing.T) {
	a := &fakeAuditor{truncated: true, entries: executedPhase(t, 1, "Create", planBody("p1"), outcomeSucceeded)[1:]}
	if _, _, found, err := ExecutedPlan(context.Background(), a, ref); found || err != nil {
		t.Errorf("ExecutedPlan = found %v, err %v, want not found", found, err)
	}
}

func TestExecutedPlanSkipsAnEndWithoutItsBegin(t *testing.T) {
	entries := append(executedPhase(t, 1, "Create", planBody("p1"), outcomeSucceeded),
		entry(9, audit.TypePhaseEnd, "Update", "sha256:gone", map[string]any{"beginSeq": 8, "outcome": outcomeSucceeded}))
	body, _, found, err := ExecutedPlan(context.Background(), &fakeAuditor{entries: entries}, ref)
	if err != nil || !found || !reflect.DeepEqual(body, planBody("p1")) {
		t.Errorf("ExecutedPlan = %v, found %v, err %v, want p1", body, found, err)
	}
}

func TestExecutedPlanRejectsAHashMismatch(t *testing.T) {
	entries := executedPhase(t, 1, "Create", planBody("p1"), outcomeSucceeded)
	entries[0].SubjectHash = "sha256:other"
	if _, _, found, err := ExecutedPlan(context.Background(), &fakeAuditor{entries: entries}, ref); err == nil || found {
		t.Errorf("ExecutedPlan = found %v, err %v, want an error", found, err)
	}
}

func TestExecutedPlanReturnsGatewayErrors(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "down")}
	if _, _, _, err := ExecutedPlan(context.Background(), a, ref); err == nil {
		t.Error("ExecutedPlan hid a gateway error")
	}
}

// begun returns the phase.begin of stage for body at seq, and body's hash.
func begun(t *testing.T, seq uint64, stage string, body plan.Body) (*gatewayv1.AuditEntry, string) {
	t.Helper()
	e := executedPhase(t, seq, stage, body, "")[0]
	return e, e.GetSubjectHash()
}

// ran returns a command.result of stage for subject at seq.
func ran(seq uint64, stage, subject string) *gatewayv1.AuditEntry {
	return entry(seq, audit.TypeCommandResult, stage, subject, commandResult{})
}

// ended returns the phase.end at seq of the phase.begin at beginSeq.
func ended(seq, beginSeq uint64, stage, subject, outcome string) *gatewayv1.AuditEntry {
	return entry(seq, audit.TypePhaseEnd, stage, subject, phaseEnd{BeginSeq: beginSeq, Outcome: outcome})
}

func TestPartialPlansFindsFailedAndInterruptedPhasesThatRanSteps(t *testing.T) {
	var entries []*gatewayv1.AuditEntry
	add := func(e ...*gatewayv1.AuditEntry) { entries = append(entries, e...) }
	old, oldHash := begun(t, 1, "Create", planBody("old"))
	add(old, ran(2, "Create", oldHash), ended(3, 1, "Create", oldHash, "failed")) // not after 3
	a, aHash := begun(t, 4, "Create", planBody("a"))
	add(a, ran(5, "Create", aHash), ended(6, 4, "Create", aHash, "failed")) // failed after a step
	b, bHash := begun(t, 7, "Create", planBody("b"))
	add(b, ended(8, 7, "Create", bHash, "failed")) // failed before any step
	c, cHash := begun(t, 9, "Update", planBody("c"))
	add(c, ran(10, "Update", cHash), ended(11, 9, "Update", cHash, "interrupted")) // interrupted after a step
	d, dHash := begun(t, 12, "Create", planBody("d"))
	add(d, ran(13, "Create", dHash), ended(14, 12, "Create", dHash, outcomeSucceeded)) // succeeded
	e, eHash := begun(t, 15, "Remediate", planBody("e"))
	add(e, ran(16, "Remediate", eHash), ended(17, 15, "Remediate", eHash, "failed")) // not a Create or Update
	f, fHash := begun(t, 18, "Update", planBody("f"))
	add(f, ran(19, "Update", "sha256:another"), ended(20, 18, "Update", fHash, "failed")) // another subject's result
	add(ran(21, "Create", bHash))                                                         // after b's phase.end: not b's step
	g, gHash := begun(t, 22, "Update", planBody("g"))
	add(g, ran(23, "Update", gHash)) // still open, or its end lost

	got, err := PartialPlans(context.Background(), &fakeAuditor{pageSize: 2, entries: entries}, ref, 3)
	if err != nil {
		t.Fatalf("PartialPlans = %v", err)
	}
	want := []plan.Body{planBody("a"), planBody("c"), planBody("g")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PartialPlans = %v, want %v", got, want)
	}
}

func TestPartialPlansGivesWhatATruncatedIndexHolds(t *testing.T) {
	a, aHash := begun(t, 5, "Create", planBody("a"))
	entries := []*gatewayv1.AuditEntry{ran(4, "Create", "sha256:lost"), a, ran(6, "Create", aHash)}
	got, err := PartialPlans(context.Background(), &fakeAuditor{truncated: true, entries: entries}, ref, 0)
	if err != nil || !reflect.DeepEqual(got, []plan.Body{planBody("a")}) {
		t.Errorf("PartialPlans = %v, %v, want a", got, err)
	}
	got, err = PartialPlans(context.Background(), &fakeAuditor{}, ref, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("PartialPlans of no entries = %v, %v", got, err)
	}
}

func TestPartialPlansRejectsAHashMismatch(t *testing.T) {
	a, _ := begun(t, 1, "Create", planBody("a"))
	a.SubjectHash = "sha256:other"
	entries := []*gatewayv1.AuditEntry{a, ran(2, "Create", "sha256:other")}
	if got, err := PartialPlans(context.Background(), &fakeAuditor{entries: entries}, ref, 0); err == nil {
		t.Errorf("PartialPlans = %v, want an error", got)
	}
}

func TestPartialPlansReturnsGatewayErrors(t *testing.T) {
	if _, err := PartialPlans(context.Background(), &fakeAuditor{err: status.Error(codes.Unavailable, "down")}, ref, 0); err == nil {
		t.Error("PartialPlans hid a gateway error")
	}
}

func TestExecutedPhaseGivesTheBeginSeq(t *testing.T) {
	entries := append(executedPhase(t, 1, "Create", planBody("p1"), outcomeSucceeded),
		executedPhase(t, 3, "Update", planBody("p2"), "failed")...)
	got, found, err := ExecutedPhase(context.Background(), &fakeAuditor{entries: entries}, ref)
	if err != nil || !found || got.BeginSeq != 1 || !reflect.DeepEqual(got.Plan, planBody("p1")) || got.Hash != entries[0].GetSubjectHash() {
		t.Errorf("ExecutedPhase = %+v, found %v, err %v; want p1 at 1", got, found, err)
	}
}

func TestPhaseSucceededPairsBeginAndEnd(t *testing.T) {
	hash := func(argv string) string { h, _ := plan.Hash(planBody(argv)); return h }
	entries := append(executedPhase(t, 1, "Remediate", planBody("ok"), outcomeSucceeded),
		executedPhase(t, 3, "Remediate", planBody("bad"), "failed")...)
	entries = append(entries, executedPhase(t, 5, "Update", planBody("upd"), outcomeSucceeded)...)
	for name, c := range map[string]struct {
		a       *fakeAuditor
		stage   lastv1alpha1.Stage
		subject string
		want    bool
	}{
		"succeeded":       {&fakeAuditor{entries: entries}, lastv1alpha1.StageRemediate, hash("ok"), true},
		"failed":          {&fakeAuditor{entries: entries}, lastv1alpha1.StageRemediate, hash("bad"), false},
		"another subject": {&fakeAuditor{entries: entries}, lastv1alpha1.StageRemediate, hash("other"), false},
		"another stage":   {&fakeAuditor{entries: entries}, lastv1alpha1.StageRemediate, hash("upd"), false},
		"truncated":       {&fakeAuditor{entries: entries, truncated: true}, lastv1alpha1.StageRemediate, hash("ok"), true},
		"begin truncated": {&fakeAuditor{entries: entries[1:], truncated: true}, lastv1alpha1.StageRemediate, hash("ok"), false},
	} {
		got, err := PhaseSucceeded(context.Background(), c.a, ref, c.stage, c.subject)
		if err != nil || got != c.want {
			t.Errorf("%s: PhaseSucceeded = %v, %v, want %v", name, got, err, c.want)
		}
	}
	if _, err := PhaseSucceeded(context.Background(), &fakeAuditor{err: status.Error(codes.Unavailable, "down")}, ref, "Remediate", "x"); err == nil {
		t.Error("PhaseSucceeded hid a gateway error")
	}
}

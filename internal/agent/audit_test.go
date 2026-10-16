package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
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
	got := resultTurn(commandResult{Stdout: big, Error: "timed out"})
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
	conv, err := Reload(context.Background(), a, ref, 5)
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
		if _, err := Reload(context.Background(), a, ref, 5); !errors.Is(err, ErrNoReload) {
			t.Errorf("%s: Reload = %v, want ErrNoReload", name, err)
		}
	}
}

func TestReloadReturnsGatewayErrorsUnchanged(t *testing.T) {
	a := &fakeAuditor{err: status.Error(codes.Unavailable, "the index could not be saved")}
	if _, err := Reload(context.Background(), a, ref, 5); status.Code(err) != codes.Unavailable || errors.Is(err, ErrNoReload) {
		t.Fatalf("Reload = %v", err)
	}
}

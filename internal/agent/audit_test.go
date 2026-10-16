package agent

import (
	"context"
	"encoding/json"
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

package agent

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// Auditor is the part of the gateway client that reads the audit log.
type Auditor interface {
	QueryAudit(ctx context.Context, in *gatewayv1.QueryAuditRequest, opts ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error)
}

// queryLimit is the page size of every QueryAudit call: the gateway's maximum.
const queryLimit = 200

// scan calls each with every indexed entry of type typ after sequence number
// after, oldest first, and reports whether the gateway's index had already
// dropped some of them. QueryAudit is read-your-writes, so every entry written
// before the call is there unless truncated says otherwise.
func scan(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, typ string, after uint64,
	each func(*gatewayv1.AuditEntry)) (bool, error) {
	truncated := false
	for {
		resp, err := a.QueryAudit(ctx, &gatewayv1.QueryAuditRequest{Intent: ref, Types: []string{typ}, AfterSeq: after, Limit: queryLimit})
		if err != nil {
			return false, err
		}
		truncated = truncated || resp.GetTruncated()
		for _, e := range resp.GetEntries() {
			each(e)
			after = e.GetSeq()
		}
		if !resp.GetMore() || len(resp.GetEntries()) == 0 {
			return truncated, nil
		}
	}
}

// commandResult is the payload of a command.result audit entry, as the
// gateway's executor writes it. Output is redacted before it is audited.
type commandResult struct {
	ExitCode        int    `json:"exitCode"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdoutTruncated"`
	StderrTruncated bool   `json:"stderrTruncated"`
	Error           string `json:"error"`
}

// DiscoveryResult returns the user turn that reports a discovery command's
// result, read from its command.result audit entry: the Discovery entry for
// subject after sequence number after. Gateway errors are returned unchanged.
func DiscoveryResult(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, subject string, after uint64) (string, error) {
	var found *commandResult
	truncated, err := scan(ctx, a, ref, audit.TypeCommandResult, after, func(e *gatewayv1.AuditEntry) {
		var r commandResult
		if e.GetStage() == string(lastv1alpha1.StageDiscovery) && e.GetSubjectHash() == subject &&
			json.Unmarshal(e.GetPayload(), &r) == nil {
			found = &r
		}
	})
	if err != nil {
		return "", err
	}
	if found == nil {
		why := "the audit log has no result entry for it"
		if truncated {
			why = "the audit log index no longer holds it"
		}
		return ResultLostTurn(why), nil
	}
	return resultTurn(*found), nil
}

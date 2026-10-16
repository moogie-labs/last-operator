package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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

// ErrNoReload means the audit log no longer holds the whole planning session,
// so it cannot be rebuilt.
var ErrNoReload = errors.New("agent: the conversation cannot be reloaded")

// llmRequest and llmResponse are the payloads of llm.request and llm.response
// audit entries, as the gateway writes them.
type llmRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type llmResponse struct {
	RequestSeq uint64 `json:"requestSeq"`
	Content    string `json:"content"`
	Error      string `json:"error,omitempty"`
}

// Reload rebuilds a planning session from the audit log (M3 spec §4.3): the
// messages of the session's latest llm.request whose llm.response holds a valid
// turn, followed by that turn. session is the sequence number of the session's
// first llm.request. Gateway errors are returned unchanged; a session the
// audit index no longer wholly holds gives an error wrapping ErrNoReload.
func Reload(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, session uint64) (Conversation, error) {
	if session == 0 {
		return nil, fmt.Errorf("%w: the Intent has no planning session", ErrNoReload)
	}
	var requests []*gatewayv1.AuditEntry
	truncated, err := scan(ctx, a, ref, audit.TypeLLMRequest, session-1, func(e *gatewayv1.AuditEntry) {
		requests = append(requests, e)
	})
	if err != nil {
		return nil, err
	}
	// The index can also lose entries in a gateway crash, which truncated does
	// not report: the session's first request must be there.
	if truncated || len(requests) == 0 || requests[0].GetSeq() != session {
		return nil, fmt.Errorf("%w: the audit index no longer holds llm.request %d", ErrNoReload, session)
	}
	answers := map[uint64]string{}
	truncated, err = scan(ctx, a, ref, audit.TypeLLMResponse, session-1, func(e *gatewayv1.AuditEntry) {
		var p llmResponse
		if json.Unmarshal(e.GetPayload(), &p) == nil && p.Error == "" {
			answers[p.RequestSeq] = p.Content
		}
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("%w: the audit index dropped part of the session", ErrNoReload)
	}
	for _, request := range slices.Backward(requests) {
		seq := request.GetSeq()
		content, ok := answers[seq]
		if !ok {
			continue
		}
		if _, err := ParseTurn(content); err != nil {
			continue
		}
		var p llmRequest
		if err := json.Unmarshal(request.GetPayload(), &p); err != nil {
			return nil, fmt.Errorf("%w: llm.request %d: %v", ErrNoReload, seq, err)
		}
		conv := make(Conversation, 0, len(p.Messages)+1)
		for _, m := range p.Messages {
			conv = append(conv, &gatewayv1.Message{Role: m.Role, Content: m.Content})
		}
		if len(conv) == 0 || conv[0].GetRole() != roleSystem {
			return nil, fmt.Errorf("%w: llm.request %d does not start with a system prompt", ErrNoReload, seq)
		}
		return append(conv, &gatewayv1.Message{Role: roleAssistant, Content: content}), nil
	}
	return nil, fmt.Errorf("%w: no valid answer since llm.request %d", ErrNoReload, session)
}

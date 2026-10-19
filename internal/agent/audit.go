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
	"github.com/moogie-labs/last-operator/internal/plan"
)

// Auditor is the part of the gateway client that reads the audit log.
type Auditor interface {
	QueryAudit(ctx context.Context, in *gatewayv1.QueryAuditRequest, opts ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error)
}

// queryLimit is the page size of every QueryAudit call: the gateway's maximum.
const queryLimit = 200

// outcomeSucceeded is the outcome of a phase.end whose phase ran to the end;
// the gateway writes it.
const outcomeSucceeded = "succeeded"

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

// UsedPlans returns the hashes of the plans this Intent has already opened a
// phase for: the subjects of its phase.begin entries outside Discovery. The
// gateway refuses to run any of them again. A truncated index gives what it
// still holds.
func UsedPlans(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef) (map[string]bool, error) {
	used := map[string]bool{}
	_, err := scan(ctx, a, ref, audit.TypePhaseBegin, 0, func(e *gatewayv1.AuditEntry) {
		if e.GetStage() != string(lastv1alpha1.StageDiscovery) {
			used[e.GetSubjectHash()] = true
		}
	})
	if err != nil {
		return nil, err
	}
	return used, nil
}

// ExecutedPlan returns the plan the Intent last executed successfully, and its
// hash: the plan in the newest phase.begin with stage Create or Update whose
// phase.end, paired by beginSeq, has outcome succeeded. Remediate, Discovery
// and Delete phases never count. found is false when no such phase exists or
// when the index is truncated, since the newest one may then be missing. A plan
// that does not hash to its entry's subject is an error. Gateway errors are
// returned unchanged.
func ExecutedPlan(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef) (plan.Body, string, bool, error) {
	begins := map[uint64]*gatewayv1.AuditEntry{}
	truncated, err := scan(ctx, a, ref, audit.TypePhaseBegin, 0, func(e *gatewayv1.AuditEntry) {
		if stage := e.GetStage(); stage == string(lastv1alpha1.StageCreate) || stage == string(lastv1alpha1.StageUpdate) {
			begins[e.GetSeq()] = e
		}
	})
	if err != nil {
		return plan.Body{}, "", false, err
	}
	var newest *gatewayv1.AuditEntry
	endTruncated, err := scan(ctx, a, ref, audit.TypePhaseEnd, 0, func(e *gatewayv1.AuditEntry) {
		var end struct {
			BeginSeq uint64 `json:"beginSeq"`
			Outcome  string `json:"outcome"`
		}
		if json.Unmarshal(e.GetPayload(), &end) != nil || end.Outcome != outcomeSucceeded {
			return
		}
		// An end whose begin is not in the scan, lost to truncation, is skipped.
		if b, ok := begins[end.BeginSeq]; ok && (newest == nil || b.GetSeq() > newest.GetSeq()) {
			newest = b
		}
	})
	if err != nil {
		return plan.Body{}, "", false, err
	}
	if truncated || endTruncated || newest == nil {
		return plan.Body{}, "", false, nil
	}
	var p struct {
		Plan string `json:"plan"`
	}
	var body plan.Body
	if err := json.Unmarshal(newest.GetPayload(), &p); err != nil {
		return plan.Body{}, "", false, fmt.Errorf("agent: phase.begin %d: %w", newest.GetSeq(), err)
	}
	if err := json.Unmarshal([]byte(p.Plan), &body); err != nil {
		return plan.Body{}, "", false, fmt.Errorf("agent: phase.begin %d holds an unreadable plan: %w", newest.GetSeq(), err)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		return plan.Body{}, "", false, fmt.Errorf("agent: phase.begin %d: %w", newest.GetSeq(), err)
	}
	if hash != newest.GetSubjectHash() {
		return plan.Body{}, "", false, fmt.Errorf("agent: the plan in phase.begin %d hashes to %s, not its subject %s", newest.GetSeq(), hash, newest.GetSubjectHash())
	}
	return body, hash, true, nil
}

// PartialCreate reports the newest Create phase that did not succeed but ran
// steps, which may have created something: the subject of its phase.end and the
// stepsRun it records. A truncated index gives what it still holds. Gateway
// errors are returned unchanged.
func PartialCreate(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef) (subject string, stepsRun int, found bool, err error) {
	_, err = scan(ctx, a, ref, audit.TypePhaseEnd, 0, func(e *gatewayv1.AuditEntry) {
		var end struct {
			Outcome  string `json:"outcome"`
			StepsRun int    `json:"stepsRun"`
		}
		if e.GetStage() != string(lastv1alpha1.StageCreate) || json.Unmarshal(e.GetPayload(), &end) != nil ||
			end.Outcome == outcomeSucceeded || end.StepsRun <= 0 {
			return
		}
		subject, stepsRun, found = e.GetSubjectHash(), end.StepsRun, true
	})
	if err != nil {
		return "", 0, false, err
	}
	return subject, stepsRun, found, nil
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
// turn that accept admits (nil admits every turn), followed by that turn. accept
// ties the rebuilt conversation to the Intent's recorded state, so that an
// answer the operator never recorded, lost to a crash or rejected by a check,
// is skipped. session is the sequence number of the session's first
// llm.request. Gateway errors are returned unchanged; a session the
// audit index no longer wholly holds gives an error wrapping ErrNoReload.
func Reload(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, session uint64, accept func(Turn) bool) (Conversation, error) {
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
	// not report. The first request must be there, which catches the loss of
	// the session's start; accept catches a lost tail.
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
		turn, err := ParseTurn(content)
		if err != nil || (accept != nil && !accept(turn)) {
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
	return nil, fmt.Errorf("%w: no answer since llm.request %d matches the Intent's state", ErrNoReload, session)
}

// PhaseSucceeded reports whether the Intent has a phase.begin of stage and
// subject whose phase.end, paired by beginSeq, has outcome succeeded. A
// truncated index gives false. Gateway errors are returned unchanged.
func PhaseSucceeded(ctx context.Context, a Auditor, ref *gatewayv1.IntentRef, stage lastv1alpha1.Stage, subject string) (bool, error) {
	begins := map[uint64]bool{}
	truncated, err := scan(ctx, a, ref, audit.TypePhaseBegin, 0, func(e *gatewayv1.AuditEntry) {
		if e.GetStage() == string(stage) && e.GetSubjectHash() == subject {
			begins[e.GetSeq()] = true
		}
	})
	if err != nil {
		return false, err
	}
	found := false
	endTruncated, err := scan(ctx, a, ref, audit.TypePhaseEnd, 0, func(e *gatewayv1.AuditEntry) {
		var end struct {
			BeginSeq uint64 `json:"beginSeq"`
			Outcome  string `json:"outcome"`
		}
		if json.Unmarshal(e.GetPayload(), &end) == nil && end.Outcome == outcomeSucceeded && begins[end.BeginSeq] {
			found = true
		}
	})
	if err != nil {
		return false, err
	}
	return found && !truncated && !endTruncated, nil
}

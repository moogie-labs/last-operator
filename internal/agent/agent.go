// Package agent runs the planning conversation. It asks the LLM, through the
// gateway's CallLLM, for one turn at a time and checks each answer (spec §4.1,
// §7; M3 spec §4).
package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// Caps on one turn and one planning session (M3 spec §4.1, D7).
const (
	// MaxAttempts is how many answers the model gets to produce one valid turn:
	// a malformed answer draws a correction, at most MaxAttempts-1 in a row.
	MaxAttempts = 3
	// MaxModelTurns caps the model's answers in one session, corrections included.
	MaxModelTurns = 20
	// MaxCommands caps the discovery commands in one session.
	MaxCommands = 10
)

// maxTokens caps each answer, thinking included. The gateway streams, so a long
// answer does not hit an HTTP timeout (M3 spec §3.2).
const maxTokens = 64000

// CallTimeout bounds one CallLLM. It is sized for a streamed answer of
// maxTokens with thinking, which can take well over ten minutes.
const CallTimeout = 30 * time.Minute

// Message roles.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

var (
	// ErrNoTurn means the model gave no valid turn within MaxAttempts answers.
	ErrNoTurn = errors.New("agent: the model gave no valid turn")
	// ErrNoSeq means the gateway did not say where the call's llm.request landed
	// in the audit log, so the session cannot be anchored for reload. A gateway
	// older than M3 does this.
	ErrNoSeq = errors.New("agent: the gateway reported no audit sequence number for the call")
)

// CapError reports a planning session that reached one of its caps (M3 spec D7).
type CapError struct {
	Max  int
	What string // "model turns" or "discovery commands"
}

func (e *CapError) Error() string { return fmt.Sprintf("planning exceeded %d %s", e.Max, e.What) }

// LLM is the part of the gateway client the agent uses.
type LLM interface {
	CallLLM(ctx context.Context, in *gatewayv1.CallLLMRequest, opts ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error)
}

// Request is what a planning session starts from.
type Request struct {
	Namespace, Name string
	Text            string // spec.request
	Answers         []lastv1alpha1.Answer
	Feedback        string // why the previous plan failed, if it did
	Note            string // why the session started over, if it did
	Env             Environment
	// Executed is the plan the Intent already executed; set, it makes this an
	// Update session.
	Executed *plan.Body
}

// Planner asks the LLM for turns.
type Planner struct{ LLM LLM }

// Conversation is one planning session's messages, the system prompt first.
// Messages are never changed in place: With and Next return new slices.
type Conversation []*gatewayv1.Message

// NewConversation starts a session whose first user message is first.
func NewConversation(first string) Conversation {
	return Conversation{{Role: roleSystem, Content: systemPrompt}, {Role: roleUser, Content: first}}
}

// With returns a copy of c followed by a user message.
func (c Conversation) With(content string) Conversation {
	return append(slices.Clip(c), &gatewayv1.Message{Role: roleUser, Content: content})
}

// ModelTurns counts the model's answers so far, corrections included.
func (c Conversation) ModelTurns() int {
	n := 0
	for _, m := range c {
		if m.GetRole() == roleAssistant {
			n++
		}
	}
	return n
}

// Commands counts the discovery commands the model has asked for so far.
func (c Conversation) Commands() int {
	n := 0
	for _, m := range c {
		if m.GetRole() != roleAssistant {
			continue
		}
		if t, err := ParseTurn(m.GetContent()); err == nil && t.Kind == KindCommand {
			n++
		}
	}
	return n
}

// Waiting reports whether c ends with a user message: something for the model
// to answer. A conversation ending with the model's own answer would be sent
// as a prefill.
func (c Conversation) Waiting() bool { return len(c) > 0 && c[len(c)-1].GetRole() == roleUser }

// Next asks the model for the session's next turn; conv must be Waiting. It
// returns the turn, the conversation with every answer and correction
// appended, and the audit sequence number of the first llm.request it caused.
// check, when set, refuses turns the caller cannot accept; its error becomes a
// correction like a malformed answer's. Gateway errors are returned unchanged
// so callers can classify their codes. Each call is bounded by CallTimeout.
// ErrNoTurn, ErrNoSeq and *CapError end
// the session.
func (p *Planner) Next(ctx context.Context, ref *gatewayv1.IntentRef, conv Conversation, check func(Turn) error) (Turn, Conversation, uint64, error) {
	if !conv.Waiting() {
		return Turn{}, nil, 0, errors.New("agent: the conversation has nothing for the model to answer")
	}
	var first uint64
	var lastErr error
	for range MaxAttempts {
		if conv.ModelTurns() >= MaxModelTurns {
			return Turn{}, nil, 0, &CapError{Max: MaxModelTurns, What: "model turns"}
		}
		cctx, cancel := context.WithTimeout(ctx, CallTimeout)
		resp, err := p.LLM.CallLLM(cctx, &gatewayv1.CallLLMRequest{Intent: ref, Messages: conv, MaxTokens: maxTokens,
			ResponseSchema: Schema()})
		cancel()
		if err != nil {
			return Turn{}, nil, 0, err
		}
		if resp.GetRequestSeq() == 0 {
			return Turn{}, nil, 0, ErrNoSeq
		}
		if first == 0 {
			first = resp.GetRequestSeq()
		}
		commands := conv.Commands()
		conv = append(slices.Clip(conv), &gatewayv1.Message{Role: roleAssistant, Content: resp.GetContent()})
		t, err := ParseTurn(resp.GetContent())
		if err == nil && check != nil {
			err = check(t)
		}
		if err == nil {
			if t.Kind == KindCommand && commands >= MaxCommands {
				return Turn{}, nil, 0, &CapError{Max: MaxCommands, What: "discovery commands"}
			}
			return t, conv, first, nil
		}
		lastErr = err
		conv = conv.With(correctionTurn(err))
	}
	return Turn{}, nil, 0, fmt.Errorf("%w: %w", ErrNoTurn, lastErr)
}

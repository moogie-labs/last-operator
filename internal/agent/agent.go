// Package agent runs the planning conversation. It asks the LLM, through the
// gateway's CallLLM, for a plan and checks the answer (spec §4.1, §7).
package agent

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// MaxTurns is how many answers the model gets to produce a valid plan.
const MaxTurns = 3

// maxTokens caps each answer.
const maxTokens = 8192

// ErrNoPlan means the model gave no valid plan within MaxTurns answers.
var ErrNoPlan = errors.New("agent: the model gave no valid plan")

// LLM is the part of the gateway client the agent uses.
type LLM interface {
	CallLLM(ctx context.Context, in *gatewayv1.CallLLMRequest, opts ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error)
}

// Request is what the agent plans for.
type Request struct {
	Namespace, Name string
	Text            string // spec.request
	Answers         []lastv1alpha1.Answer
	Feedback        string // why the previous plan failed, if it did
}

// Planner asks the LLM for a plan.
type Planner struct{ LLM LLM }

// Plan runs one planning conversation and returns a valid plan with its hash
// set. Gateway errors are returned unchanged so callers can classify their
// codes. When the model gives no valid plan within MaxTurns answers, the error
// wraps ErrNoPlan.
func (p *Planner) Plan(ctx context.Context, req Request) (*lastv1alpha1.Plan, error) {
	ref := &gatewayv1.IntentRef{Namespace: req.Namespace, Name: req.Name}
	msgs := []*gatewayv1.Message{{Role: "system", Content: SystemPrompt}, {Role: "user", Content: userPrompt(req)}}
	var lastErr error
	for range MaxTurns {
		resp, err := p.LLM.CallLLM(ctx, &gatewayv1.CallLLMRequest{Intent: ref, Messages: msgs, MaxTokens: maxTokens})
		if err != nil {
			return nil, err
		}
		pl, err := Parse(resp.GetContent())
		if err == nil {
			return pl, nil
		}
		lastErr = err
		msgs = append(msgs, &gatewayv1.Message{Role: "assistant", Content: resp.GetContent()},
			&gatewayv1.Message{Role: "user", Content: correction(err)})
	}
	return nil, fmt.Errorf("%w: %v", ErrNoPlan, lastErr)
}

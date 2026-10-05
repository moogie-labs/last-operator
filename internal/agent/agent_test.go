package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// fakeLLM answers in order and records every request.
type fakeLLM struct {
	answers []string
	err     error
	reqs    []*gatewayv1.CallLLMRequest
}

func (f *fakeLLM) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	n := len(f.reqs) - 1
	if n >= len(f.answers) {
		n = len(f.answers) - 1
	}
	return &gatewayv1.CallLLMResponse{Content: f.answers[n]}, nil
}

var req = Request{Namespace: "team-x", Name: "web", Text: "run nginx",
	Answers: []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}}}

func TestPlannerRetriesUntilTheAnswerIsAPlan(t *testing.T) {
	llm := &fakeLLM{answers: []string{"I think you want nginx.", validAnswer}}
	p, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if err != nil || p.Hash == "" {
		t.Fatalf("Plan = %+v, %v", p, err)
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("CallLLM called %d times, want 2", len(llm.reqs))
	}
	first, second := llm.reqs[0], llm.reqs[1]
	if first.GetIntent().GetNamespace() != "team-x" || first.GetIntent().GetName() != "web" || first.GetMaxTokens() <= 0 {
		t.Fatalf("request = %+v", first)
	}
	roles := make([]string, 0, len(second.GetMessages()))
	for _, m := range second.GetMessages() {
		roles = append(roles, m.GetRole())
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" {
		t.Fatalf("second conversation roles = %v", roles)
	}
	if second.GetMessages()[2].GetContent() != "I think you want nginx." ||
		!strings.Contains(second.GetMessages()[3].GetContent(), "not a valid plan") {
		t.Fatalf("correction turn = %+v", second.GetMessages()[2:])
	}
}

func TestPlannerGivesUpAfterMaxTurns(t *testing.T) {
	llm := &fakeLLM{answers: []string{"no"}}
	_, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if !errors.Is(err, ErrNoPlan) || len(llm.reqs) != MaxTurns {
		t.Fatalf("Plan = %v after %d calls, want ErrNoPlan after %d", err, len(llm.reqs), MaxTurns)
	}
}

func TestPlannerReturnsGatewayErrorsUnchanged(t *testing.T) {
	llm := &fakeLLM{err: status.Error(codes.Unavailable, "audit down")}
	_, err := (&Planner{LLM: llm}).Plan(context.Background(), req)
	if status.Code(err) != codes.Unavailable || len(llm.reqs) != 1 {
		t.Fatalf("Plan = %v after %d calls", err, len(llm.reqs))
	}
}

func TestUserPromptCarriesTheRequestAnswersAndFeedback(t *testing.T) {
	r := req
	r.Feedback = "step s1 exited 1: boom"
	got := userPrompt(r)
	for _, want := range []string{"team-x/web", "run nginx", "q1: internal only", "step s1 exited 1: boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(userPrompt(req), "previous plan failed") {
		t.Error("the prompt mentions a failure that did not happen")
	}
}

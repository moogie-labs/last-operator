package agent

import (
	"bytes"
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

// fakeLLM answers in order and records every request. Its llm.request
// sequence numbers start at 101, or are 0 when noSeq is set.
type fakeLLM struct {
	answers []string
	err     error
	noSeq   bool
	reqs    []*gatewayv1.CallLLMRequest
}

func (f *fakeLLM) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	n := min(len(f.reqs), len(f.answers)) - 1
	seq := uint64(100 + len(f.reqs))
	if f.noSeq {
		seq = 0
	}
	return &gatewayv1.CallLLMResponse{Content: f.answers[n], RequestSeq: seq}, nil
}

const (
	questionsAnswer = `{"kind":"questions","questions":[{"id":"q1","text":"Which namespace?"}]}`
	commandAnswer   = `{"kind":"command","reason":"look","command":{"argv":["kubectl","get","ns"]}}`
)

// Literals the tests repeat; goconst wants them named.
const (
	teamNamespace = "team-x"
	nginxRequest  = "run nginx"
	nginxReply    = "I think you want nginx."
	boomFeedback  = "step s1 exited 1: boom"
	k8sProvider   = "k8s"
	cidrA         = "10.0.0.0/8:443"
)

var ref = &gatewayv1.IntentRef{Namespace: teamNamespace, Name: webName}

func roles(c Conversation) string {
	out := make([]string, 0, len(c))
	for _, m := range c {
		out = append(out, m.GetRole())
	}
	return strings.Join(out, ",")
}

func TestNextAsksForOneTurnWithTheSchema(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	turn, conv, seq, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), nil)
	if err != nil || turn.Kind != KindQuestions || seq != 101 {
		t.Fatalf("Next = %+v, %d, %v", turn, seq, err)
	}
	if roles(conv) != "system,user,assistant" || conv[2].GetContent() != questionsAnswer {
		t.Fatalf("conversation = %v", conv)
	}
	req := llm.reqs[0]
	if req.GetIntent() != ref || req.GetMaxTokens() != maxTokens || !bytes.Equal(req.GetResponseSchema(), Schema()) {
		t.Fatalf("request = %+v", req)
	}
	if req.GetMessages()[0].GetContent() != systemPrompt || req.GetMessages()[1].GetContent() != nginxRequest {
		t.Errorf("messages = %v", req.GetMessages())
	}
}

func TestNextCorrectsMalformedAnswers(t *testing.T) {
	llm := &fakeLLM{answers: []string{nginxReply, questionsAnswer}}
	turn, conv, seq, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), nil)
	if err != nil || turn.Kind != KindQuestions || seq != 101 {
		t.Fatalf("Next = %+v, %d, %v; want the first call's sequence number", turn, seq, err)
	}
	if roles(conv) != "system,user,assistant,user,assistant" || !strings.Contains(conv[3].GetContent(), "not a valid turn") {
		t.Fatalf("conversation = %v", conv)
	}
}

func TestNextGivesUpAfterMaxAttempts(t *testing.T) {
	llm := &fakeLLM{answers: []string{"no"}}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), nil)
	if !errors.Is(err, ErrNoTurn) || len(llm.reqs) != MaxAttempts {
		t.Fatalf("Next = %v after %d calls, want ErrNoTurn after %d", err, len(llm.reqs), MaxAttempts)
	}
}

func TestNextTurnsACheckFailureIntoACorrection(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer, commandAnswer}}
	check := func(turn Turn) error {
		if turn.Kind == KindQuestions {
			return errors.New(`question id "q1" was answered earlier`)
		}
		return nil
	}
	turn, conv, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), check)
	if err != nil || turn.Kind != KindCommand || !strings.Contains(conv[3].GetContent(), `"q1" was answered earlier`) {
		t.Fatalf("Next = %+v, %v; conversation %v", turn, err, conv)
	}
}

func TestNextStopsAtMaxModelTurns(t *testing.T) {
	conv := NewConversation(nginxRequest)
	for range MaxModelTurns {
		conv = append(conv, &gatewayv1.Message{Role: roleAssistant, Content: questionsAnswer})
		conv = conv.With("Answers:\n- q1: default\n")
	}
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, conv, nil)
	var capped *CapError
	if !errors.As(err, &capped) || err.Error() != "planning exceeded 20 model turns" || len(llm.reqs) != 0 {
		t.Fatalf("Next = %v after %d calls", err, len(llm.reqs))
	}
}

func TestNextStopsAtMaxCommands(t *testing.T) {
	conv := NewConversation(nginxRequest)
	for range MaxCommands - 1 {
		conv = append(conv, &gatewayv1.Message{Role: roleAssistant, Content: commandAnswer})
		conv = conv.With("The command exited 0.")
	}
	p := &Planner{LLM: &fakeLLM{answers: []string{commandAnswer}}}
	turn, conv, _, err := p.Next(context.Background(), ref, conv, nil)
	if err != nil || turn.Kind != KindCommand || conv.Commands() != MaxCommands {
		t.Fatalf("command %d: Next = %v", MaxCommands, err)
	}
	_, _, _, err = p.Next(context.Background(), ref, conv.With("The command exited 0."), nil)
	if err == nil || err.Error() != "planning exceeded 10 discovery commands" {
		t.Fatalf("command %d: Next = %v", MaxCommands+1, err)
	}
}

func TestNextReturnsGatewayErrorsUnchanged(t *testing.T) {
	llm := &fakeLLM{err: status.Error(codes.Unavailable, "audit down")}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), nil)
	if status.Code(err) != codes.Unavailable || len(llm.reqs) != 1 {
		t.Fatalf("Next = %v after %d calls", err, len(llm.reqs))
	}
}

func TestNextRefusesACallWithoutASequenceNumber(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}, noSeq: true}
	_, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, NewConversation(nginxRequest), nil)
	if !errors.Is(err, ErrNoSeq) {
		t.Fatalf("Next = %v, want ErrNoSeq", err)
	}
}

func TestNextNeedsSomethingToAnswer(t *testing.T) {
	llm := &fakeLLM{answers: []string{questionsAnswer}}
	conv := append(NewConversation(nginxRequest), &gatewayv1.Message{Role: roleAssistant, Content: questionsAnswer})
	if _, _, _, err := (&Planner{LLM: llm}).Next(context.Background(), ref, conv, nil); err == nil || len(llm.reqs) != 0 {
		t.Fatalf("Next = %v after %d calls; a conversation ending with an answer would be sent as a prefill", err, len(llm.reqs))
	}
}

func TestConversationWithCopies(t *testing.T) {
	base := NewConversation(nginxRequest)
	a, b := base.With("one"), base.With("two")
	if len(base) != 2 || a[2].GetContent() != "one" || b[2].GetContent() != "two" || !a.Waiting() {
		t.Fatalf("base %v, a %v, b %v", base, a, b)
	}
}

func TestFirstMessageCarriesTheRequestAndWhatCommandsMayUse(t *testing.T) {
	got := FirstMessage(Request{Namespace: teamNamespace, Name: webName, Text: nginxRequest,
		Answers:  []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}},
		Feedback: boomFeedback, Note: "planning starts over",
		Env: Environment{Providers: []Provider{{Name: k8sProvider, Description: "this cluster"}, {Name: "aws"}},
			Egress: [][]string{{cidrA}, {"10.1.0.0/16:443"}}, NoScripts: true}})
	for _, want := range []string{"Note: planning starts over", "team-x/web", nginxRequest, "- q1: internal only",
		"- k8s: this cluster\n", "- aws\n", "allowed by every one of these lists", "- 10.0.0.0/8:443\n",
		"- 10.1.0.0/16:443\n", "Scripts are not allowed", boomFeedback} {
		if !strings.Contains(got, want) {
			t.Errorf("first message lacks %q:\n%s", want, got)
		}
	}

	bare := FirstMessage(Request{Namespace: teamNamespace, Name: webName, Text: nginxRequest})
	for _, want := range []string{"- none: commands run without credentials", "Egress: none beyond what the Providers imply"} {
		if !strings.Contains(bare, want) {
			t.Errorf("bare first message lacks %q:\n%s", want, bare)
		}
	}
	for _, unwanted := range []string{"Note:", "Answers", "previous plan failed", "Scripts are not allowed"} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("bare first message has %q:\n%s", unwanted, bare)
		}
	}
	one := FirstMessage(Request{Text: "x", Env: Environment{Egress: [][]string{{cidrA, "10.2.0.1/32:5432"}}}})
	if !strings.Contains(one, "Egress commands may request: 10.0.0.0/8:443, 10.2.0.1/32:5432\n") {
		t.Errorf("one egress layer:\n%s", one)
	}
	closed := FirstMessage(Request{Text: "x", Env: Environment{Egress: [][]string{{cidrA}, {}}}})
	if !strings.Contains(closed, "Egress: none beyond what the Providers imply") {
		t.Errorf("a layer that allows nothing:\n%s", closed)
	}
}

func TestUserTurns(t *testing.T) {
	qs := []lastv1alpha1.Question{{ID: "q1", Text: "Which namespace?"}, {ID: "q2", Text: "Public?"}}
	as := []lastv1alpha1.Answer{{ID: "q2", Value: "no"}, {ID: "q1", Value: "default"}, {ID: "q9", Value: "x"}}
	if got := AnswersTurn(qs, as); got != "Answers:\n- q1: default\n- q2: no\n" {
		t.Errorf("AnswersTurn = %q", got)
	}
	if got := DeniedTurn("too broad"); got != "The command was denied: too broad" {
		t.Errorf("DeniedTurn = %q", got)
	}
	if got := DeniedTurn(" "); got != "The command was denied." {
		t.Errorf("DeniedTurn without a reason = %q", got)
	}
	if got := NotRunTurn("the gateway refused it: policy"); got != "The command did not run: the gateway refused it: policy" {
		t.Errorf("NotRunTurn = %q", got)
	}
}

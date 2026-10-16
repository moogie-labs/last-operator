package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func newTestPrompter(input string) (*prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return newPrompter(strings.NewReader(input), &out), &out
}

func TestAskQuestionWithOptions(t *testing.T) {
	q := lastv1alpha1.Question{ID: "q1", Text: questionTier, Options: []string{tierInternal, tierPublic}}
	p, out := newTestPrompter("9\nfoo\n2\n")
	got, err := p.askQuestion(context.Background(), q)
	if err != nil || got != tierPublic {
		t.Fatalf("askQuestion = %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "Which tier?\n  1) internal\n  2) public\n") || !strings.Contains(out.String(), "Answer [1-2]: ") {
		t.Errorf("menu:\n%s", out)
	}
	if n := strings.Count(out.String(), "Enter a number from 1 to 2."); n != 2 {
		t.Errorf("re-asked %d times, want 2:\n%s", n, out)
	}
	p, _ = newTestPrompter("internal\n")
	if got, err := p.askQuestion(context.Background(), q); err != nil || got != tierInternal {
		t.Fatalf("askQuestion by option text = %q, %v", got, err)
	}
}

func TestAskQuestionFreeText(t *testing.T) {
	p, out := newTestPrompter("\n  port 80  \n")
	got, err := p.askQuestion(context.Background(), lastv1alpha1.Question{ID: "q2", Text: questionPort})
	if err != nil || got != port80 {
		t.Fatalf("askQuestion = %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "An answer cannot be empty.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAskDecision(t *testing.T) {
	for input, want := range map[string]struct {
		decision lastv1alpha1.Decision
		reason   string
	}{
		"y\n":                     {lastv1alpha1.DecisionApprove, ""},
		"YES\n":                   {lastv1alpha1.DecisionApprove, ""},
		"\n":                      {"", ""},
		"n\n":                     {"", ""},
		"maybe\nd\n\ntoo broad\n": {lastv1alpha1.DecisionDeny, "too broad"},
	} {
		p, out := newTestPrompter(input)
		decision, reason, err := p.askDecision(context.Background())
		if err != nil || decision != want.decision || reason != want.reason {
			t.Errorf("input %q: askDecision = %q, %q, %v", input, decision, reason, err)
		}
		if input == "maybe\nd\n\ntoo broad\n" &&
			(!strings.Contains(out.String(), "Answer y to approve") || !strings.Contains(out.String(), "A reason cannot be empty.")) {
			t.Errorf("re-asks:\n%s", out)
		}
	}
}

func TestAskGivesWayToCtrlC(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newPrompter(r, io.Discard).ask(ctx, "? "); !errors.Is(err, context.Canceled) {
		t.Fatalf("ask after Ctrl-C = %v", err)
	}
}

func TestAskReportsTheEndOfInput(t *testing.T) {
	p, _ := newTestPrompter("")
	if _, err := p.ask(context.Background(), "? "); !errors.Is(err, errNoInput) {
		t.Fatalf("ask at the end of input = %v", err)
	}
}

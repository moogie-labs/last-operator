package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// errNoInput means the input ended while a prompt waited for an answer.
var errNoInput = errors.New("input ended")

// prompter reads the user's answers line by line. A goroutine does the
// reading, so a waiting prompt still gives way to Ctrl-C.
type prompter struct {
	w     io.Writer
	lines chan string // closed at the end of input
}

func newPrompter(r io.Reader, w io.Writer) *prompter {
	p := &prompter{w: w, lines: make(chan string)}
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			p.lines <- s.Text()
		}
		close(p.lines)
	}()
	return p
}

// ask prints prompt and returns the next line, trimmed.
func (p *prompter) ask(ctx context.Context, prompt string) (string, error) {
	writef(p.w, "%s", prompt)
	select {
	case <-ctx.Done():
		writeln(p.w)
		return "", ctx.Err()
	case line, ok := <-p.lines:
		if !ok {
			writeln(p.w)
			return "", errNoInput
		}
		return strings.TrimSpace(line), nil
	}
}

// askQuestion asks one clarifying question: a numbered menu when it has
// options, otherwise a free-text line. It asks again until the answer is usable.
func (p *prompter) askQuestion(ctx context.Context, q lastv1alpha1.Question) (string, error) {
	writef(p.w, "\n%s\n", clean(q.Text))
	for i, o := range q.Options {
		writef(p.w, "  %d) %s\n", i+1, clean(o))
	}
	prompt := "Answer: "
	if len(q.Options) > 0 {
		prompt = fmt.Sprintf("Answer [1-%d]: ", len(q.Options))
	}
	for {
		line, err := p.ask(ctx, prompt)
		if err != nil {
			return "", err
		}
		if len(q.Options) == 0 {
			if line != "" {
				return line, nil
			}
			writeln(p.w, "An answer cannot be empty.")
			continue
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1], nil
		}
		if slices.Contains(q.Options, line) {
			return line, nil
		}
		writef(p.w, "Enter a number from 1 to %d.\n", len(q.Options))
	}
}

// askDecision asks whether to approve what was just shown. It returns
// DecisionApprove, DecisionDeny with a reason, or "" to leave it pending.
func (p *prompter) askDecision(ctx context.Context) (lastv1alpha1.Decision, string, error) {
	for {
		line, err := p.ask(ctx, "Approve? [y/N/d]: ")
		if err != nil {
			return "", "", err
		}
		switch strings.ToLower(line) {
		case "y", "yes":
			return lastv1alpha1.DecisionApprove, "", nil
		case "", "n", "no":
			return "", "", nil
		case "d", "deny":
			for {
				reason, err := p.ask(ctx, "Reason for denying (the model reads it): ")
				if err != nil {
					return "", "", err
				}
				if reason != "" {
					return lastv1alpha1.DecisionDeny, reason, nil
				}
				writeln(p.w, "A reason cannot be empty.")
			}
		}
		writeln(p.w, "Answer y to approve, n to leave it pending, or d to deny.")
	}
}

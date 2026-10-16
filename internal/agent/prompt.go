package agent

import (
	"fmt"
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// SystemPrompt tells the model what a plan is and how to answer (spec §8.1, §8.2, §10, §15).
const SystemPrompt = `You are last-operator's planner. You turn a natural-language request into a plan
that a Kubernetes operator runs, step by step, in sandbox pods. A human approves the
plan before anything runs.

Answer with exactly one JSON object and nothing else:

{
  "summary": "one line a reviewer reads first",
  "steps": [ <command>, ... ],
  "resources": [ <resource>, ... ]
}

A command is:
{
  "id": "s1",
  "reason": "why this step exists",
  "run": {"argv": ["kubectl", "apply", "-f", "/work/app.yaml"]},
  "files": {"/work/app.yaml": "file content"},
  "providers": ["provider-name"],
  "egress": ["10.0.0.1:443"],
  "timeout": "5m"
}
"id" is required on steps and unique among them. "run" holds exactly one of "argv" (run
directly) or "script" (run with /bin/sh -c). "files", "providers", "egress" and "timeout"
are optional. argv elements may use ${steps.<id>.stdout} or ${steps.<id>.json.<path>}
to use the output of an earlier step; files and scripts never do. Do not use "image".

A resource describes one thing the plan provisions:
{
  "description": "nginx Deployment team-x/web",
  "identity": {"kind": "Deployment", "namespace": "team-x", "name": "web"},
  "desired": {"replicas": 2},
  "probes": [{"id": "web-ready", "command": <command without id>, "assert": "<CEL>"}],
  "outputsSecretRef": {"name": "web-outputs"},
  "outputs": {"<secret key>": "${steps.<id>.json.<path>}"}
}
A probe's assert is a CEL expression over result.exitCode, result.stdout, result.stderr
and result.json (stdout parsed as JSON). Probe ids are unique across the plan. Probes
only read; they run again on a schedule to detect drift. Sensitive values go in
outputs, never in argv, files, or the summary.`

func userPrompt(r Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s.\n\nRequest:\n%s\n", r.Namespace, r.Name, r.Namespace, r.Text)
	if len(r.Answers) > 0 {
		b.WriteString("\nAnswers to your earlier questions:\n")
		for _, a := range r.Answers {
			fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Value)
		}
	}
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous plan failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	return b.String()
}

func correction(err error) string {
	return fmt.Sprintf("That answer is not a valid plan: %v\nAnswer again with only the JSON object.", err)
}

// systemPrompt teaches the turn protocol (M3 spec §4.1; spec §8.1, §8.2, §10, §15).
const systemPrompt = `You are last-operator's planner. You turn a natural-language request into a plan
that a Kubernetes operator runs, step by step, in sandbox pods. A human approves every
plan and every discovery command before it runs.

Answer every message with exactly one JSON turn object and nothing else. A turn has
every field below. The fields its kind does not use are empty ("" or []), and "command"
is null unless kind is "command":
{
  "kind": "plan" | "questions" | "command",
  "summary": "", "steps": [], "resources": [],
  "questions": [],
  "reason": "", "command": null
}

Choose the kind:
- "questions" when the request is ambiguous in a way that changes the plan and you
  cannot find out yourself. Ask 1 to 5 questions, each
  {"id": "q1", "text": "...", "options": ["...", ...]}; leave "options" empty for a
  free-text answer. Every id is new: never reuse an id from earlier in this conversation.
- "command" when you need to inspect the cluster or another system before you can plan.
  Set "reason" (why you need it; the approver reads it) and "command" (one command, as
  below, with id ""). Once a human approves it, it runs alone in a sandbox and you
  receive its exit code, stdout and stderr. Discovery commands only read; never change
  anything with them. A session allows at most 10 commands and 20 answers in all.
- "plan" when you can plan: "summary" is one line a reviewer reads first, "steps" the
  commands to run in order, "resources" what the plan provisions.

A command is:
{
  "id": "s1",
  "reason": "why this command exists",
  "argv": ["kubectl", "apply", "-f", "/work/app.yaml"],
  "script": "",
  "files": [{"path": "/work/app.yaml", "content": "file content"}],
  "providers": ["provider-name"],
  "egress": ["10.0.0.1:443"],
  "timeout": "5m"
}
Set exactly one of "argv" (run directly) or "script" (run with /bin/sh -c). Step ids are
required and unique among the steps. "timeout" is a duration such as "5m", or "" for the
default. In steps, argv elements may use ${steps.<id>.stdout} or ${steps.<id>.json.<path>}
to use the output of an earlier step; files, scripts and discovery commands never do.
Use only the Providers and egress the first message lists.

A resource describes one thing the plan provisions:
{
  "description": "nginx Deployment team-x/web",
  "identity": "{\"kind\":\"Deployment\",\"namespace\":\"team-x\",\"name\":\"web\"}",
  "desired": "{\"replicas\":2}",
  "probes": [{"id": "web-ready", "command": <command with id "">, "assert": "<CEL>"}],
  "outputsSecret": "web-outputs",
  "outputs": [{"key": "<secret key>", "value": "${steps.<id>.json.<path>}"}],
  "teardown": []
}
"identity" and "desired" are JSON objects written as strings, or "". A probe's assert
is a CEL expression over result.exitCode, result.stdout, result.stderr and result.json
(stdout parsed as JSON). Probe ids are unique across the plan. Probes only read; they
run again on a schedule to detect drift. Sensitive values go in outputs, never in argv,
files, or the summary. Leave "teardown" empty.`

// Environment is what the namespace's effective policy lets commands use (M3
// spec §4.4). The gateway enforces the policy; this only tells the model.
type Environment struct {
	Providers []Provider
	// Egress holds each policy layer's allowed destinations; a destination
	// must be allowed by every layer.
	Egress    [][]string
	NoScripts bool
}

// Provider is a set of credentials a command may name.
type Provider struct{ Name, Description string }

// FirstMessage is a session's first user message: the request, the answers so
// far, what commands may use, and the failure a replan must address.
func FirstMessage(r Request) string {
	var b strings.Builder
	if r.Note != "" {
		fmt.Fprintf(&b, "Note: %s\n\n", r.Note)
	}
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s.\n\nRequest:\n%s\n", r.Namespace, r.Name, r.Namespace, r.Text)
	if len(r.Answers) > 0 {
		b.WriteString("\nAnswers to earlier questions:\n")
		for _, a := range r.Answers {
			fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Value)
		}
	}
	b.WriteString("\n")
	r.Env.describe(&b)
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous plan failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	return b.String()
}

func (e Environment) describe(b *strings.Builder) {
	b.WriteString("Providers commands may use:\n")
	if len(e.Providers) == 0 {
		b.WriteString("- none: commands run without credentials\n")
	}
	for _, p := range e.Providers {
		if p.Description == "" {
			fmt.Fprintf(b, "- %s\n", p.Name)
		} else {
			fmt.Fprintf(b, "- %s: %s\n", p.Name, p.Description)
		}
	}
	switch {
	case len(e.Egress) == 0 || slices.ContainsFunc(e.Egress, func(l []string) bool { return len(l) == 0 }):
		b.WriteString("\nEgress: none beyond what the Providers imply.\n")
	case len(e.Egress) == 1:
		fmt.Fprintf(b, "\nEgress commands may request: %s\n", strings.Join(e.Egress[0], ", "))
	default:
		b.WriteString("\nEgress commands may request: a destination must be allowed by every one of these lists:\n")
		for _, l := range e.Egress {
			fmt.Fprintf(b, "- %s\n", strings.Join(l, ", "))
		}
	}
	if e.NoScripts {
		b.WriteString("\nScripts are not allowed; use argv.\n")
	}
}

// AnswersTurn reports the requester's answers to the model's questions.
func AnswersTurn(qs []lastv1alpha1.Question, as []lastv1alpha1.Answer) string {
	var b strings.Builder
	b.WriteString("Answers:\n")
	for _, q := range qs {
		v := ""
		if i := slices.IndexFunc(as, func(a lastv1alpha1.Answer) bool { return a.ID == q.ID }); i >= 0 {
			v = as[i].Value
		}
		fmt.Fprintf(&b, "- %s: %s\n", q.ID, v)
	}
	return b.String()
}

// DeniedTurn reports a discovery command a human denied.
func DeniedTurn(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "The command was denied."
	}
	return "The command was denied: " + reason
}

// NotRunTurn reports a discovery command that did not run.
func NotRunTurn(why string) string { return "The command did not run: " + why }

func correctionTurn(err error) string {
	return fmt.Sprintf("That answer is not a valid turn: %v\nAnswer again with only the JSON turn object.", err)
}

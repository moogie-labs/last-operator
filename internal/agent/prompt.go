package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

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
  "timeout": "5m",
  "install": []
}
Set exactly one of "argv" (run directly) or "script" (run with /bin/sh -c). Step ids are
required and unique among the steps. "timeout" is a duration such as "5m", or "" for the
default. In steps, argv elements may use ${steps.<id>.stdout} or ${steps.<id>.json.<path>}
to use the output of an earlier step; files, scripts, discovery commands and teardown
commands never do.
Use only the Providers and egress the first message lists.

The sandbox image is Debian bookworm with sh, curl, kubectl, jq, git and python3, and
no pip. For any other tool, list its install commands in "install", an array of argv
arrays; "install" is [] when the base image is enough. The base image ships no apt
package lists, so an apt install starts with an update:
[["apt-get","update"],["apt-get","install","-y","--no-install-recommends","awscli=2.9.19-1"]]
Each install argv runs as root at build time, with no
credentials and only the build egress, so pin versions. The image is reused while
the spec is unchanged.

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
files, or the summary. "teardown" lists the commands that remove what the resource
created, or [] when nothing needs removing. They run only when the Intent is deleted,
long after the plan's steps, in an order you are asked for then and a human approves,
so they never use ${steps…} references.

A later message may ask for an Update or a Remediate of a plan that already ran, and a Remediate answer always has "resources": [].`

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
	if r.Executed != nil {
		raw, err := executedJSON(*r.Executed)
		if err == nil {
			fmt.Fprintf(&b, "\nThis Intent already executed this plan:\n%s\n\n"+
				"Plan an Update: steps that take the cluster from what that plan created to what the request now asks, "+
				"and resources that list everything the Intent manages afterwards. "+
				"Your steps must remove the real objects of any resource you drop.\n", raw)
		} else {
			fmt.Fprintf(&b, "\nThis Intent already executed a plan, but it cannot be shown: %v\n", err)
		}
	}
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous plan failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	return b.String()
}

// Teardown is what a Delete planning session starts from (design D6): the
// teardown commands of what the Intent's executed plan created, in resource order.
type Teardown struct {
	Namespace, Name string
	Text            string // spec.request
	Commands        []lastv1alpha1.Command
	Feedback        string // why the previous teardown failed, if it did
}

// TeardownMessage is a Delete planning session's first user message: the
// teardown commands to order into a Delete plan.
func TeardownMessage(t Teardown) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s, is being deleted.\n\nIts request was:\n%s\n\n", t.Namespace, t.Name, t.Namespace, t.Text)
	b.WriteString("Tear down what its executed plan created. These are the teardown commands of its resources, in resource order:\n")
	for i, c := range t.Commands {
		raw, err := json.Marshal(commandToWire(c))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, raw)
	}
	b.WriteString("\nAnswer with a plan turn whose \"steps\" are these commands, each exactly once, in the order that " +
		"tears things down safely, and whose \"resources\" are []. Copy every command as given; you may change only an " +
		"id, and the ids must be unique among the steps. Add, drop or change no command: a human approves the plan, and " +
		"any other plan is refused. The summary says what the teardown removes.\n")
	if t.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous teardown failed and was not completed:\n%s\n"+
			"Order the commands again; the teardown must succeed from the cluster's current state.\n", t.Feedback)
	}
	return b.String(), nil
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

// LostTurn reports a discovery command whose run the operator cannot account
// for: it may have run, so asking again could run it twice.
func LostTurn(why string) string { return "The command may have run, but its result was lost: " + why }

func correctionTurn(err error) string {
	return fmt.Sprintf("That answer is not a valid turn: %v\nAnswer again with only the JSON turn object.", err)
}

// maxResultBytes bounds each of a discovery command's stdout and stderr in the
// conversation (M3 spec §4.2).
const maxResultBytes = 16 << 10

// resultTurn reports a discovery command's result.
func resultTurn(r commandResult) string {
	var b strings.Builder
	if r.Error != "" {
		fmt.Fprintf(&b, "The command could not finish: %s\n", r.Error)
	} else {
		fmt.Fprintf(&b, "The command exited %d.\n", r.ExitCode)
	}
	writeStream(&b, "stdout", r.Stdout, r.StdoutTruncated, maxResultBytes)
	writeStream(&b, "stderr", r.Stderr, r.StderrTruncated, maxResultBytes)
	return b.String()
}

// writeStream writes one output stream, clipped to limit bytes.
func writeStream(b *strings.Builder, name, s string, truncated bool, limit int) {
	s, clipped := head(s, limit)
	switch {
	case s == "" && !truncated:
		fmt.Fprintf(b, "%s: empty\n", name)
		return
	case truncated || clipped:
		fmt.Fprintf(b, "%s (truncated):\n", name)
	default:
		fmt.Fprintf(b, "%s:\n", name)
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
	}
}

// head returns the first n bytes of s, cut on a UTF-8 boundary, and whether it cut.
func head(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// ResultLostTurn reports a discovery command that ran but whose result cannot
// be read back.
func ResultLostTurn(why string) string {
	return "The command ran, but its result could not be read back: " + why
}

// maxProbeBytes bounds each of a failing probe's stdout and stderr in a
// Remediate message.
const maxProbeBytes = 2 << 10

// ProbeFailure is a probe that failed when the Remediate was confirmed.
type ProbeFailure struct {
	Resource, ProbeID string
	Assert            string // the probe's CEL assert
	Result            *gatewayv1.CommandResult
	Error             string // why the probe gave no result, if it did not
}

// Remediation is what a Remediate planning session starts from.
type Remediation struct {
	Namespace, Name string
	Text            string // spec.request
	Executed        plan.Body
	Failures        []ProbeFailure
	Env             Environment
	Feedback        string // why the previous remediation failed, if it did
}

// RemediateMessage is a Remediate planning session's first user message: the
// executed plan, the probes that fail and the environment. It has no Answers:
// a Remediate asks no questions.
func RemediateMessage(r Remediation) (string, error) {
	raw, err := executedJSON(r.Executed)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Intent %s/%s, in namespace %s, has drifted from its executed plan.\n\nRequest:\n%s\n\n", r.Namespace, r.Name, r.Namespace, r.Text)
	fmt.Fprintf(&b, "Executed plan:\n%s\n\nProbes that fail:\n", raw)
	for _, f := range r.Failures {
		fmt.Fprintf(&b, "- %s, probe %s, assert: %s\n", f.Resource, f.ProbeID, f.Assert)
		res := f.Result
		switch {
		case f.Error != "":
			fmt.Fprintf(&b, "The probe could not finish: %s\n", f.Error)
		case res.GetError() != "":
			fmt.Fprintf(&b, "The probe could not finish: %s\n", res.GetError())
		default:
			fmt.Fprintf(&b, "The probe exited %d.\n", res.GetExitCode())
		}
		if res != nil {
			writeStream(&b, "stdout", res.GetStdout(), res.GetStdoutTruncated(), maxProbeBytes)
			writeStream(&b, "stderr", res.GetStderr(), res.GetStderrTruncated(), maxProbeBytes)
		}
	}
	b.WriteString("\n")
	r.Env.describe(&b)
	if r.Feedback != "" {
		fmt.Fprintf(&b, "\nThe previous remediation failed and was not completed:\n%s\n"+
			"Plan again; the new plan must succeed from the cluster's current state.\n", r.Feedback)
	}
	b.WriteString("\nPlan a Remediate: steps that bring what the executed plan manages back to its desired state. " +
		"Answer with \"resources\": []; what is managed does not change. " +
		"You may run discovery commands; you may not ask questions.\n")
	return b.String(), nil
}

// executedJSON writes a plan in the wire form of a plan turn, without its nonce.
func executedJSON(b plan.Body) (string, error) {
	return Encode(Turn{Kind: KindPlan, Plan: &lastv1alpha1.Plan{Steps: b.Steps, Resources: b.Resources}})
}

package agent

import (
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const (
	applyVerb    = "apply"
	webReady     = "web-ready"
	manifestPath = "/work/a.yaml"
	exitZero     = "result.exitCode == 0"
	webResource  = "nginx Deployment team-x/web"
	timedOut     = "timed out"
	runWeb       = "run web"
)

func TestPromptExplainsImageInstall(t *testing.T) {
	prompt := strings.Join(strings.Fields(systemPrompt), " ") // the prompt wraps its lines
	for _, want := range []string{`"install"`, "Debian bookworm", "pin versions", `["apt-get","update"]`, "at build time", "no credentials"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

func TestPromptAsksForTeardown(t *testing.T) {
	prompt := strings.Join(strings.Fields(systemPrompt), " ")
	if strings.Contains(prompt, `Leave "teardown" empty`) {
		t.Error("the system prompt still asks for an empty teardown")
	}
	for _, want := range []string{`"teardown" lists the commands that remove what the resource created`, "when the Intent is deleted"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

func TestTeardownMessageListsTheCommandsInOrder(t *testing.T) {
	cmds := []lastv1alpha1.Command{
		{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "delete", "deployment", webName}}},
		{ID: "t2", Run: lastv1alpha1.CommandRun{Script: "kubectl delete service web"}},
	}
	msg, err := TeardownMessage(Teardown{Namespace: teamNamespace, Name: webName, Text: runWeb, Commands: cmds, Feedback: "step t1 exited 1: boom"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Intent team-x/web, in namespace team-x, is being deleted.",
		"Its request was:\nrun web\n",
		`1. {"id":"t1","reason":"","argv":["kubectl","delete","deployment","web"],"script":""`,
		`2. {"id":"t2","reason":"","argv":[],"script":"kubectl delete service web"`,
		`"resources" are []`,
		"you may change only an id",
		"The previous teardown failed and was not completed:\nstep t1 exited 1: boom\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("teardown message lacks %q:\n%s", want, msg)
		}
	}
}

func updateBody() plan.Body {
	return plan.Body{
		Steps: []lastv1alpha1.Command{{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, applyVerb, "-f", manifestPath}}}},
		Resources: []lastv1alpha1.ManagedResourceSpec{{
			Description: webResource,
			Probes: []lastv1alpha1.Probe{{ID: webReady, Assert: exitZero,
				Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "get", "deploy", "web"}}}}},
		}},
		Nonce: "0123456789abcdef0123456789abcdef",
	}
}

func TestFirstMessageForAnUpdateShowsTheExecutedPlan(t *testing.T) {
	body := updateBody()
	msg := FirstMessage(Request{Namespace: teamNamespace, Name: webName, Text: "run web with 3 replicas", Executed: &body})
	for _, want := range []string{
		"This Intent already executed this plan (do not repeat it as your answer):\n",
		`"argv":["kubectl","apply","-f","/work/a.yaml"]`,
		`"description":"nginx Deployment team-x/web"`,
		`"assert":"result.exitCode == 0"`,
		"Plan an Update: steps that take the cluster from what that plan created to what the request now asks, " +
			"and resources that list everything the Intent manages afterwards. " +
			"Your steps must remove the real objects of any resource you drop. " +
			"List the resources you keep in their executed order. " +
			`A kept resource's outputs must name this plan's steps; leave its "outputs" out to keep its output Secret as it is.`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("update message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "0123456789abcdef") {
		t.Error("update message shows the nonce")
	}
	if strings.Index(msg, "Request:") > strings.Index(msg, "already executed") {
		t.Error("the executed plan comes before the request")
	}
	if plain := FirstMessage(Request{Namespace: teamNamespace, Name: webName, Text: "x"}); strings.Contains(plain, "already executed") {
		t.Error("a Create message mentions an executed plan")
	}
}

func failure(stdout string) ProbeFailure {
	return ProbeFailure{Resource: webResource, ProbeID: webReady, Assert: exitZero,
		Result: &gatewayv1.CommandResult{ExitCode: 1, Stdout: stdout, Stderr: "boom"}}
}

func TestRemediateMessageTruncatesProbeOutput(t *testing.T) {
	msg, err := RemediateMessage(Remediation{Namespace: teamNamespace, Name: webName, Text: runWeb, Executed: updateBody(),
		Failures: []ProbeFailure{failure(strings.Repeat("x", 10<<10))}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, strings.Repeat("x", 2<<10)+"\n") || strings.Contains(msg, strings.Repeat("x", 2<<10+1)) {
		t.Errorf("stdout is not cut to 2 KiB:\n%s", msg)
	}
	for _, want := range []string{"Executed plan (do not repeat it as your answer; its resources stay as they are):\n",
		"stdout (truncated):", "stderr:\nboom\n", "web-ready", exitZero,
		webResource, "exited 1", "Providers commands may use:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("remediate message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "0123456789abcdef") {
		t.Error("remediate message shows the nonce")
	}
}

func TestRemediateMessageNeverAsksForResources(t *testing.T) {
	msg, err := RemediateMessage(Remediation{Namespace: teamNamespace, Name: webName, Text: runWeb, Executed: updateBody(),
		Failures: []ProbeFailure{failure("a"), {Resource: "resource 1", ProbeID: "p2", Error: timedOut}}, Feedback: "step s1 exited 1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Plan a Remediate: steps that bring what the executed plan manages back to its desired state. " +
			`Answer with "resources": []; what is managed does not change. ` +
			"You may run discovery commands; you may not ask questions.",
		"resource 1", timedOut, "step s1 exited 1",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("remediate message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Answers to earlier") {
		t.Error("remediate message has an Answers section")
	}
	if strings.Index(msg, "web-ready") > strings.Index(msg, "p2") {
		t.Error("failures are out of order")
	}
	prompt := strings.Join(strings.Fields(systemPrompt), " ")
	if !strings.Contains(prompt, `a Remediate answer always has "resources": []`) {
		t.Error("system prompt lacks the Remediate rule")
	}
}

func TestRemediateMessageSaysWhenAProbeGaveNoResult(t *testing.T) {
	msg, err := RemediateMessage(Remediation{Namespace: teamNamespace, Name: webName, Text: runWeb, Executed: updateBody(),
		Failures: []ProbeFailure{{Resource: webResource, ProbeID: webReady, Assert: exitZero}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "The probe gave no result.\n") || strings.Contains(msg, "exited 0") {
		t.Errorf("remediate message for a probe with no result:\n%s", msg)
	}
}

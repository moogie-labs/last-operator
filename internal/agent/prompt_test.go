package agent

import (
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
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
		{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "delete", "deployment", "web"}}},
		{ID: "t2", Run: lastv1alpha1.CommandRun{Script: "kubectl delete service web"}},
	}
	msg, err := TeardownMessage(Teardown{Namespace: "team-x", Name: "web", Text: "run web", Commands: cmds, Feedback: "step t1 exited 1: boom"})
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

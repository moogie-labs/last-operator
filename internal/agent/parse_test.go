package agent

import (
	"encoding/json"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const validAnswer = `{"summary":"list namespaces","steps":[{"id":"s1","run":{"argv":["kubectl","get","ns"]}}],
"resources":[{"description":"ns","probes":[{"id":"p1","command":{"run":{"argv":["true"]}},"assert":"result.exitCode == 0"}],
"outputsSecretRef":{"name":"out"},"outputs":{"names":"${steps.s1.stdout}"}}]}`

func TestParseAcceptsAPlanAndHashesIt(t *testing.T) {
	for name, content := range map[string]string{
		"plain":         validAnswer,
		"fenced":        "```json\n" + validAnswer + "\n```",
		"bare fence":    "```\n" + validAnswer + "\n```",
		"surrounded ws": "\n  " + validAnswer + "  \n",
	} {
		p, err := Parse(content)
		if err != nil {
			t.Fatalf("%s: Parse = %v", name, err)
		}
		want, err := plan.Hash(plan.Body{Steps: p.Steps, Resources: p.Resources})
		if err != nil || p.Hash != want || p.Summary != "list namespaces" || len(p.Steps) != 1 || p.Resources[0].Outputs["names"] != "${steps.s1.stdout}" {
			t.Fatalf("%s: plan = %+v (hash want %s, %v)", name, p, want, err)
		}
	}
}

const exactlyOne = "exactly one of run.argv and run.script"

func TestParse(t *testing.T) {
	step := `{"id":"s1","run":{"argv":["true"]}}`
	big := strings.Repeat("x", MaxPlanBytes)
	bigStep, _ := json.Marshal(lastv1alpha1.Command{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", big}}})
	cases := map[string]struct{ content, want string }{
		"prose":                {"Here is the plan: " + validAnswer, "not a plan object"},
		"trailing text":        {validAnswer + " hope this helps", "text after the plan object"},
		"unknown field":        {`{"steps":[` + step + `],"extra":1}`, "unknown field"},
		"no steps":             {`{"steps":[]}`, "at least one step"},
		"argv and script":      {`{"steps":[{"id":"s1","run":{"argv":["a"],"script":"b"}}]}`, exactlyOne},
		"neither":              {`{"steps":[{"id":"s1","run":{}}]}`, exactlyOne},
		"image install":        {`{"steps":[{"id":"s1","run":{"argv":["a"]},"image":{"install":[{"argv":["pip","install","x"]}]}}]}`, "image.install"},
		"probe without id":     {`{"steps":[` + step + `],"resources":[{"probes":[{"command":{"run":{"argv":["true"]}},"assert":"true"}]}]}`, "probe 0 of resource 0 has no id"},
		"probe without assert": {`{"steps":[` + step + `],"resources":[{"probes":[{"id":"p","command":{"run":{"argv":["true"]}}}]}]}`, `probe "p" has no assert`},
		"probe bad command":    {`{"steps":[` + step + `],"resources":[{"probes":[{"id":"p","command":{"run":{}},"assert":"true"}]}]}`, exactlyOne},
		"intentRef":            {`{"steps":[` + step + `],"resources":[{"intentRef":{"name":"other"}}]}`, "sets intentRef"},
		"literal output":       {`{"steps":[` + step + `],"resources":[{"outputsSecretRef":{"name":"o"},"outputs":{"k":"hunter2"}}]}`, "not a single step reference"},
		"too large":            {`{"steps":[` + string(bigStep) + `]}`, "the limit is"},
	}
	for name, tc := range cases {
		_, err := Parse(tc.content)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Parse = %v, want an error containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats an output value", name)
		}
	}
}

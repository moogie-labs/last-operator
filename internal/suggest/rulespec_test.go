//nolint:goconst // table-driven test data repeats argv words
package suggest_test

import (
	"reflect"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

func withProviders(c lastv1alpha1.Command, p ...string) lastv1alpha1.Command {
	c.Providers = p
	return c
}

func TestRuleSpecFor(t *testing.T) {
	create := []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageRemediate}
	plain := cmd("kubectl", "apply", "-f", "web.yaml")
	body := plan.Body{
		Steps: []lastv1alpha1.Command{plain, withProviders(cmd("aws", "s3", "ls"), "aws")},
		Resources: []lastv1alpha1.ManagedResourceSpec{{
			Probes:   []lastv1alpha1.Probe{{ID: "p1", Command: withProviders(cmd("kubectl", "get", "x"), "kubernetes")}},
			Teardown: []lastv1alpha1.Command{withProviders(cmd("gcloud", "x"), "gcp", "aws")},
		}},
	}
	installed := withProviders(cmd("tool", "run"), "kubernetes")
	installed.Egress = []string{"10.0.0.0/8:443"}
	installed.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "tool==1.0"}}}}

	tests := []struct {
		name  string
		body  plan.Body
		cmd   lastv1alpha1.Command
		extra []string
		want  lastv1alpha1.CommandRuleSpec
		ok    bool
	}{
		{name: "own providers, egress and exact install", body: body, cmd: installed, want: lastv1alpha1.CommandRuleSpec{
			Stages: create, Providers: []string{"kubernetes"}, Egress: []string{"10.0.0.0/8:443"},
			Install: []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", "install", "tool==1.0"}}}}, ok: true},
		{name: "union over steps, probes and teardown", body: body, cmd: plain, extra: []string{"ignored"},
			want: lastv1alpha1.CommandRuleSpec{Stages: create, Providers: []string{"aws", "gcp", "kubernetes"}}, ok: true},
		{name: "extra providers when no command names one", body: plan.Body{Steps: []lastv1alpha1.Command{plain}}, cmd: plain,
			extra: []string{"kubernetes"}, want: lastv1alpha1.CommandRuleSpec{Stages: create, Providers: []string{"kubernetes"}}, ok: true},
		{name: "no provider at all", body: plan.Body{Steps: []lastv1alpha1.Command{plain}}, cmd: plain},
		{name: "a script", body: body, cmd: withProviders(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "echo"}}, "aws")},
		{name: "a step reference", body: body, cmd: withProviders(cmd("kubectl", "get", "${steps.s1.stdout}"), "aws")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := suggest.RuleSpecFor(tt.body, tt.cmd, create, tt.extra)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RuleSpecFor = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestUnrulable(t *testing.T) {
	if why := suggest.Unrulable(cmd("kubectl", "get", "pods")); why != "" {
		t.Errorf("an argv command is unrulable: %s", why)
	}
	c := cmd("tool")
	c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "${steps.a.stdout}"}}}}
	if suggest.Unrulable(c) == "" {
		t.Error("a ${steps. reference in image.install is rulable")
	}
}

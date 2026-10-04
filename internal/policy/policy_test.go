package policy_test

import (
	"errors"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func opConfig(providers []string, egress []string, scripts *bool) *lastv1alpha1.OperatorConfig {
	d := &lastv1alpha1.IntentPolicySpec{Providers: providers, AllowScripts: scripts}
	if egress != nil {
		d.Egress = &lastv1alpha1.EgressPolicy{Allowed: egress}
	}
	return &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Defaults: d}}
}

func rule(t *testing.T, s string) sandbox.EgressRule {
	t.Helper()
	r, err := sandbox.ParseEgress(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestShippedDefaultsAllowNothing(t *testing.T) {
	e, err := policy.Resolve(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.AllowsProvider("any") || e.AllowsEgress(rule(t, "10.0.0.1:443")) || !e.AllowScripts {
		t.Fatalf("nil config: providers/egress must be denied and scripts allowed: %+v", e)
	}
}

func TestProvidersIntersectAcrossLayers(t *testing.T) {
	ip := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Providers: []string{"b", "c"}}}
	e, _ := policy.Resolve(opConfig([]string{"a", "b"}, nil, nil), []lastv1alpha1.IntentPolicy{ip})
	if e.AllowsProvider("a") || !e.AllowsProvider("b") || e.AllowsProvider("c") {
		t.Fatal("providers must be the intersection")
	}
	inherit := lastv1alpha1.IntentPolicy{}
	e, _ = policy.Resolve(opConfig([]string{"a"}, nil, nil), []lastv1alpha1.IntentPolicy{inherit})
	if !e.AllowsProvider("a") {
		t.Fatal("an IntentPolicy without providers must inherit")
	}
	none := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Providers: []string{}}}
	e, _ = policy.Resolve(opConfig([]string{"a"}, nil, nil), []lastv1alpha1.IntentPolicy{none})
	if e.AllowsProvider("a") {
		t.Fatal("an empty provider list must allow nothing")
	}
}

func TestEgressCoverageComparesParsedCIDRs(t *testing.T) {
	ip := lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.1.0.0/16:443"}}}}
	//nolint:goconst // test data reuse
	e, err := policy.Resolve(opConfig(nil, []string{"10.0.0.0/8:443", "api.example.com:443"}, nil), []lastv1alpha1.IntentPolicy{ip})
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"10.1.2.3:443", "10.1.0.0/24:443", "[::ffff:10.1.2.3]:443"}
	denied := []string{"10.2.0.1:443", "10.1.2.3:80", "10.0.0.0/8:443", "[::ffff:0:0/96]:443", "0.0.0.0/0:443"}
	for _, s := range allowed {
		if !e.AllowsEgress(rule(t, s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
	for _, s := range denied {
		if e.AllowsEgress(rule(t, s)) {
			t.Errorf("%s must be denied", s)
		}
	}
}

func TestMalformedPolicyPatternIsAnError(t *testing.T) {
	if _, err := policy.Resolve(opConfig(nil, []string{"10.0.0.0/8"}, nil), nil); err == nil {
		t.Fatal("a pattern without a port must be rejected")
	}
}

func TestCheckCommand(t *testing.T) {
	no := false
	e, _ := policy.Resolve(opConfig([]string{"p"}, []string{"10.0.0.0/8:443"}, &no), nil)
	ok := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}, Providers: []string{"p"}, Egress: []string{"10.1.1.1:443"}}
	if err := e.CheckCommand(ok); err != nil {
		t.Fatalf("CheckCommand(ok) = %v", err)
	}
	bad := map[string]lastv1alpha1.Command{
		"script":   {Run: lastv1alpha1.CommandRun{Script: "true"}},
		"provider": {Run: ok.Run, Providers: []string{"q"}},
		"egress":   {Run: ok.Run, Egress: []string{"192.168.0.1:443"}},
		"badrule":  {Run: ok.Run, Egress: []string{"nope"}},
		"install":  {Run: ok.Run, Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "x"}}}}},
	}
	for name, c := range bad {
		err := e.CheckCommand(c)
		if !errors.Is(err, policy.ErrDenied) {
			t.Errorf("%s: CheckCommand = %v, want ErrDenied", name, err)
		}
	}
	if err := e.CheckCommand(bad["install"]); !strings.Contains(err.Error(), "image.install") {
		t.Errorf("install denial should name image.install: %v", err)
	}
}

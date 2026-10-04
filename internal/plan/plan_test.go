package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const echoStdoutRef = "${steps.echo.stdout}"

func step(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func TestHashIsSHA256OfCanonicalBody(t *testing.T) {
	b := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "get", "ns")}}
	c, err := audit.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(c)
	got, err := plan.Hash(b)
	if err != nil || got != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("Hash = %q, %v", got, err)
	}
}

func TestHashIgnoresMapOrderAndNilVersusEmpty(t *testing.T) {
	a := step("s1", "true")
	a.Files = map[string]string{"/work/a": "1", "/work/b": "2"}
	b := step("s1", "true")
	b.Files = map[string]string{"/work/b": "2", "/work/a": "1"}
	ha, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{a}})
	hb, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{b}})
	if ha != hb {
		t.Fatalf("map order changed the hash: %s vs %s", ha, hb)
	}
	h1, _ := plan.Hash(plan.FromStatus(&lastv1alpha1.Plan{}))
	h2, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{}})
	if h1 != h2 {
		t.Fatalf("nil and empty steps hash differently")
	}
}

func TestHashChangesWithAnyStepField(t *testing.T) {
	base, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "apply")}})
	changed, _ := plan.Hash(plan.Body{Steps: []lastv1alpha1.Command{step("s1", "kubectl", "delete")}})
	if base == changed {
		t.Fatal("hash did not change")
	}
}

func TestHashAcceptsFloatsAndLargeNumbersInDesired(t *testing.T) {
	b := plan.Body{
		Steps: []lastv1alpha1.Command{step("s1", "true")},
		Resources: []lastv1alpha1.ManagedResourceSpec{{
			Desired: &runtime.RawExtension{Raw: []byte(`{"ratio":0.75,"big":12345678901234567890}`)},
		}},
	}
	if _, err := plan.Hash(b); err != nil {
		t.Fatalf("Hash: %v", err)
	}
}

func TestValidate(t *testing.T) {
	probe := func(id string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Command: step("", "true"), Assert: "true"}
	}
	cases := []struct {
		name string
		body plan.Body
		want string
	}{
		{"no steps", plan.Body{}, "at least one step"},
		{"empty id", plan.Body{Steps: []lastv1alpha1.Command{step("", "true")}}, "has no id"},
		{"duplicate id", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a"), step("s1", "b")}}, `duplicate step id "s1"`},
		{"duplicate probe", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Probes: []lastv1alpha1.Probe{probe("p")}}, {Probes: []lastv1alpha1.Probe{probe("p")}}}}, `duplicate probe id "p"`},
	}
	for _, tc := range cases {
		err := plan.Validate(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.want)
		}
	}
	ok := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a"), step("s2", "b")}}
	if err := plan.Validate(ok); err != nil {
		t.Fatalf("Validate(ok) = %v", err)
	}
}

func results() plan.Results {
	return plan.Results{
		"get":   {ID: "get", Stdout: `{"spec":{"replicas":2,"ports":[{"port":80}]},"ready":true,"name":"web","obj":{}}` + "\n"},
		"echo":  {ID: "echo", Stdout: "10.0.0.5\n"},
		"bad":   {ID: "bad", ExitCode: 1, Stdout: "x"},
		"trunc": {ID: "trunc", Stdout: `{"a":`, StdoutTruncated: true},
		"evil":  {ID: "evil", Stdout: "${steps.get.stdout}"},
	}
}

func TestValue(t *testing.T) {
	r := results()
	ok := map[string]string{
		echoStdoutRef:                         "10.0.0.5",
		"${steps.get.json.spec.replicas}":     "2",
		"${steps.get.json.spec.ports.0.port}": "80",
		"${steps.get.json.ready}":             "true",
		"${steps.get.json.name}":              "web",
	}
	for expr, want := range ok {
		if got, err := r.Value(expr); err != nil || got != want {
			t.Errorf("Value(%s) = %q, %v; want %q", expr, got, err, want)
		}
	}
	bad := []string{
		"${steps.missing.stdout}", "${steps.bad.stdout}", "${steps.get.json.spec.nope}",
		"${steps.get.json.obj}", "${steps.get.json.spec.ports.9.port}", "${steps.trunc.json.a}",
		"${steps.echo.json.x}", "steps.echo.stdout", echoStdoutRef + " ",
	}
	for _, expr := range bad {
		if _, err := r.Value(expr); !errors.Is(err, plan.ErrReference) {
			t.Errorf("Value(%q) = %v, want ErrReference", expr, err)
		}
	}
}

func TestResolveReplacesArgvReferencesOnly(t *testing.T) {
	c := step("s2", "kubectl", "scale", "--replicas=${steps.get.json.spec.replicas}", echoStdoutRef)
	c.Files = map[string]string{"/work/x": echoStdoutRef}
	got, err := plan.Resolve(c, results())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "scale", "--replicas=2", "10.0.0.5"}
	if strings.Join(got.Run.Argv, "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %q", got.Run.Argv)
	}
	if got.Files["/work/x"] != "${steps.echo.stdout}" {
		t.Fatalf("files were resolved: %q", got.Files)
	}
	if c.Run.Argv[2] != "--replicas=${steps.get.json.spec.replicas}" {
		t.Fatal("Resolve mutated its input")
	}
}

func TestResolveDoesNotReexpandValues(t *testing.T) {
	got, err := plan.Resolve(step("s", "echo", "${steps.evil.stdout}"), results())
	if err != nil || got.Run.Argv[1] != "${steps.get.stdout}" {
		t.Fatalf("argv = %q, %v", got.Run.Argv, err)
	}
}

func TestResolveRejectsMalformedAndFailedReferences(t *testing.T) {
	for _, arg := range []string{"${steps.echo}", "${steps.echo.stderr}", "x${steps.bad.stdout}"} {
		if _, err := plan.Resolve(step("s", "echo", arg), results()); !errors.Is(err, plan.ErrReference) {
			t.Errorf("Resolve(%q) = %v, want ErrReference", arg, err)
		}
	}
}

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

const outSecret = "out"

const echoStdoutRef = "${steps.echo.stdout}"

const (
	s1StdoutRef = "${steps.s1.stdout}"
	literal     = "hunter2" // a mistaken literal output value; errors must never repeat it
)

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
		{"teardown with argv and script", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Teardown: []lastv1alpha1.Command{{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{"a"}, Script: "b"}}}}}}, "exactly one of run.argv and run.script"},
		{"teardown with neither", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Teardown: []lastv1alpha1.Command{{ID: "t1"}}}}}, "exactly one of run.argv and run.script"},
		{"teardown argv with a step reference", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Teardown: []lastv1alpha1.Command{step("t1", "kubectl", "delete", "ns", "${steps.s1.stdout}")}}}}, "teardown command 0 uses a ${steps…} reference"},
		{"teardown script with a step reference", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{}, {Teardown: []lastv1alpha1.Command{{ID: "t1", Run: lastv1alpha1.CommandRun{Script: "kubectl delete ns ${steps.s1.json.name}"}}}}}},
			"resource 1 teardown command 0 uses a ${steps…} reference"},
		{"outputs without secret", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{Outputs: map[string]string{"k": s1StdoutRef}}}}, "has outputs but no outputsSecretRef"},
		{"literal output", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"k": literal}}}}, `output "k" is not a single step reference`},
		{"unknown step output", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"k": "${steps.nope.stdout}"}}}}, `names unknown step "nope"`},
		{"duplicate output secret", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}}, {OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}}}}, `duplicate output secret "out"`},
		{"output key with a space", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"db password": literal}}}}, `output "db password" is not a valid Secret key`},
		{"output key dot-dot", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"..": s1StdoutRef}}}}, `output ".." is not a valid Secret key`},
		{"output key too long", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{strings.Repeat("k", 254): s1StdoutRef}}}}, "is not a valid Secret key"},
		{"secret name with uppercase", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "Web_Outputs"}, Outputs: map[string]string{"k": literal}}}}, `output secret "Web_Outputs" is not a valid Secret name`},
		{"secret name empty", plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
			{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: ""}}}}, `output secret "" is not a valid Secret name`},
	}
	for _, tc := range cases {
		err := plan.Validate(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), literal) {
			t.Errorf("%s: the error repeats an output value: %v", tc.name, err)
		}
	}
	ok := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a"), step("s2", "b")}}
	if err := plan.Validate(ok); err != nil {
		t.Fatalf("Validate(ok) = %v", err)
	}
	withOutputs := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
		{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"k": "${steps.s1.json.a.b}", "s": s1StdoutRef}}}}
	if err := plan.Validate(withOutputs); err != nil {
		t.Fatalf("Validate(withOutputs) = %v", err)
	}
	// An Update keeps a resource's output Secret as it is by leaving its outputs out.
	keptSecret := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
		{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}}}}
	if err := plan.Validate(keptSecret); err != nil {
		t.Fatalf("Validate(keptSecret) = %v", err)
	}
	dotted := plan.Body{Steps: []lastv1alpha1.Command{step("s1", "a")}, Resources: []lastv1alpha1.ManagedResourceSpec{
		{OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web.outputs-1"}, Outputs: map[string]string{"tls.crt": s1StdoutRef, "DB_URL": s1StdoutRef}}}}
	if err := plan.Validate(dotted); err != nil {
		t.Fatalf("Validate(dotted) = %v", err)
	}
}

func TestValidSecretKey(t *testing.T) {
	for k, want := range map[string]bool{
		"a": true, "tls.crt": true, "DB_URL": true, "-x": true, "a.b-c_d": true, strings.Repeat("k", 253): true,
		"": false, ".": false, "..": false, "db password": false, "a/b": false, "ü": false, strings.Repeat("k", 254): false,
	} {
		if got := plan.ValidSecretKey(k); got != want {
			t.Errorf("ValidSecretKey(%q) = %v, want %v", k, got, want)
		}
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
		"${steps.get.json.obj}", "${steps.get.json.spec.ports.9.port}", "${steps.trunc.json.a}", "${steps.trunc.stdout}",
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

const (
	testNonce   = "0123456789abcdef0123456789abcdef"
	nonceReason = "needs a nonce"
)

// fixtureBody is a plan whose hash was recorded before Body gained a nonce.
func fixtureBody() plan.Body {
	return plan.Body{
		Steps: []lastv1alpha1.Command{step("s1", "kubectl", "get", "ns")},
		Resources: []lastv1alpha1.ManagedResourceSpec{{Description: "namespace",
			OutputsSecretRef: &lastv1alpha1.LocalRef{Name: outSecret}, Outputs: map[string]string{"k": s1StdoutRef}}},
	}
}

func TestHashIncludesTheNonce(t *testing.T) {
	// Without a nonce, a plan hashes as it did before nonces existed: existing
	// Create, Update and Delete subjects keep their approvals.
	const pinned = "sha256:e15b57b8a849300ded5c7a1b4c6ea6f736198fcab07476d186d7170cf9b5ee7e"
	b := fixtureBody()
	if h, err := plan.Hash(b); err != nil || h != pinned {
		t.Fatalf("Hash(fixture) = %q, %v; want the pinned %q", h, err, pinned)
	}
	b.Nonce = testNonce
	h1, err := plan.Hash(b)
	if err != nil || h1 == pinned {
		t.Fatalf("Hash with a nonce = %q, %v; want a different subject", h1, err)
	}
	b.Nonce = strings.Repeat("f", 32)
	if h2, _ := plan.Hash(b); h2 == h1 {
		t.Fatal("two nonces gave the same subject")
	}
	if got := plan.FromStatus(&lastv1alpha1.Plan{Steps: b.Steps, Nonce: testNonce}); got.Nonce != testNonce {
		t.Fatalf("FromStatus dropped the nonce: %+v", got)
	}
}

func TestSameResourcesIgnoresIntentRef(t *testing.T) {
	a := fixtureBody().Resources
	b := fixtureBody().Resources
	b[0].IntentRef = &lastv1alpha1.LocalRef{Name: "web"}
	if same, err := plan.SameResources(a, b); err != nil || !same {
		t.Fatalf("SameResources(intentRef set) = %v, %v; want equal", same, err)
	}
	b[0].Outputs = map[string]string{"k": s1StdoutRef}
	if same, _ := plan.SameResources(a, b); !same {
		t.Fatal("an equal map built separately compared unequal")
	}
	b[0].Description = "another namespace"
	if same, err := plan.SameResources(a, b); err != nil || same {
		t.Fatalf("SameResources(description changed) = %v, %v; want different", same, err)
	}
	if same, _ := plan.SameResources(nil, []lastv1alpha1.ManagedResourceSpec{}); !same {
		t.Fatal("nil and empty resources compared unequal")
	}
	if same, _ := plan.SameResources(a, append(fixtureBody().Resources, lastv1alpha1.ManagedResourceSpec{})); same {
		t.Fatal("an added resource compared equal")
	}
	if a[0].IntentRef != nil {
		t.Fatal("SameResources changed its argument")
	}
}

func TestValidateRemediateRequiresEqualResourcesAndANonce(t *testing.T) {
	executed := fixtureBody()
	// The repair's steps are its own; the copied outputs still name the executed plan's step.
	ok := plan.Body{Steps: []lastv1alpha1.Command{step("fix", "kubectl", "apply", "-f", "/work/ns.yaml")},
		Resources: fixtureBody().Resources, Nonce: testNonce}
	if err := plan.ValidateRemediate(ok, executed); err != nil {
		t.Fatalf("ValidateRemediate(ok) = %v", err)
	}
	if err := plan.Validate(ok); err == nil {
		t.Fatal("Validate accepted outputs that name no step of the plan; ValidateRemediate is needed")
	}

	changed := ok
	changed.Resources = []lastv1alpha1.ManagedResourceSpec{{Description: "something else"}}
	if err := plan.ValidateRemediate(changed, executed); !errors.Is(err, plan.ErrResourcesChanged) {
		t.Fatalf("changed resources = %v, want ErrResourcesChanged", err)
	}
	dropped := ok
	dropped.Resources = nil
	if err := plan.ValidateRemediate(dropped, executed); !errors.Is(err, plan.ErrResourcesChanged) {
		t.Fatalf("dropped resources = %v, want ErrResourcesChanged", err)
	}

	cases := []struct {
		name string
		edit func(b *plan.Body)
		want string
	}{
		{"no nonce", func(b *plan.Body) { b.Nonce = "" }, nonceReason},
		{"short nonce", func(b *plan.Body) { b.Nonce = "abc" }, nonceReason},
		{"uppercase nonce", func(b *plan.Body) { b.Nonce = strings.ToUpper(testNonce) }, nonceReason},
		{"no steps", func(b *plan.Body) { b.Steps = nil }, "at least one step"},
		{"empty id", func(b *plan.Body) { b.Steps = []lastv1alpha1.Command{step("", "true")} }, "has no id"},
		{"duplicate id", func(b *plan.Body) { b.Steps = []lastv1alpha1.Command{step("a", "x"), step("a", "y")} }, `duplicate step id "a"`},
	}
	for _, tc := range cases {
		b := ok
		tc.edit(&b)
		err := plan.ValidateRemediate(b, executed)
		if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, plan.ErrResourcesChanged) {
			t.Errorf("%s: ValidateRemediate = %v, want %q", tc.name, err, tc.want)
		}
	}

	// Resources equal to an executed plan that fails today's checks are still refused.
	bad := fixtureBody()
	bad.Resources[0].Teardown = []lastv1alpha1.Command{{ID: "t1"}}
	withBad := ok
	withBad.Resources = bad.Resources
	if err := plan.ValidateRemediate(withBad, bad); err == nil || !strings.Contains(err.Error(), "exactly one of run.argv and run.script") {
		t.Fatalf("an invalid teardown = %v", err)
	}
	unknown := fixtureBody()
	unknown.Resources[0].Outputs = map[string]string{"k": "${steps.gone.stdout}"}
	withUnknown := ok
	withUnknown.Resources = unknown.Resources
	if err := plan.ValidateRemediate(withUnknown, unknown); err == nil || !strings.Contains(err.Error(), `unknown step "gone"`) {
		t.Fatalf("an output naming no executed step = %v", err)
	}
}

func TestCommandKeyIgnoresOnlyTheID(t *testing.T) {
	a := lastv1alpha1.Command{ID: "a", Run: lastv1alpha1.CommandRun{Argv: []string{kubectl, "delete", "ns", "x"}}}
	b := a
	b.ID = "b"
	ka, err := plan.CommandKey(a)
	if err != nil {
		t.Fatal(err)
	}
	if kb, err := plan.CommandKey(b); err != nil || kb != ka {
		t.Fatalf("CommandKey(%q) = %q, %v; want %q", b.ID, kb, err, ka)
	}
	c := a
	c.Run.Argv = []string{kubectl, "delete", "ns", "y"}
	if kc, err := plan.CommandKey(c); err != nil || kc == ka {
		t.Fatalf("CommandKey of a changed command = %q, %v; want it to differ", kc, err)
	}
	d := a
	d.Providers = []string{"cluster"}
	if kd, err := plan.CommandKey(d); err != nil || kd == ka {
		t.Fatalf("CommandKey with a provider = %q, %v; want it to differ", kd, err)
	}
	if a.ID != "a" {
		t.Fatal("CommandKey changed its argument")
	}
}

// extendedKeys returns plan.ExtendedKeys(steps), failing the test on an error.
func extendedKeys(t *testing.T, steps ...lastv1alpha1.Command) []string {
	t.Helper()
	keys, err := plan.ExtendedKeys(steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(steps) {
		t.Fatalf("ExtendedKeys returned %d keys for %d steps", len(keys), len(steps))
	}
	return keys
}

func TestExtendedKeysIgnoreIDsButFollowReferences(t *testing.T) {
	a := step("a", kubectl, "get", "ns", "x", "-o", "json")
	c := step("c", kubectl, "get", "deployment", "web", "-o", "json")
	b := step("b", "echo", "${steps.a.json.kind}")
	executed := extendedKeys(t, a, c, b)

	// The same chain under other ids has the same keys.
	a2, b2 := a, step("y", "echo", "${steps.x.json.kind}")
	a2.ID = "x"
	if got := extendedKeys(t, a2, b2); got[0] != executed[0] || got[1] != executed[2] {
		t.Fatalf("renamed chain keys = %v, want %v and %v", got, executed[0], executed[2])
	}

	// B's text, but its reference now points at C's command: a different key.
	rewired := c
	rewired.ID = "a"
	if got := extendedKeys(t, rewired, b); got[1] == executed[2] {
		t.Fatal("a step whose reference points at another command kept its key")
	}

	// Without references, the extended key changes exactly when CommandKey does.
	same := extendedKeys(t, c, step("other", kubectl, "get", "deployment", "web", "-o", "json"))
	if same[0] != same[1] || same[0] != executed[1] {
		t.Fatalf("keys of equal commands = %v, want all %s", same, executed[1])
	}
}

func TestExtendedKeysFollowAChainThreeDeep(t *testing.T) {
	a := step("a", kubectl, "get", "ns", "x", "-o", "json")
	b := step("b", "echo", "${steps.a.json.kind}")
	d := step("d", "echo", "${steps.b.stdout}", "and", "${steps.a.stdout}")
	executed := extendedKeys(t, a, b, d)

	renamed := extendedKeys(t, step("1", kubectl, "get", "ns", "x", "-o", "json"),
		step("2", "echo", "${steps.1.json.kind}"), step("3", "echo", "${steps.2.stdout}", "and", "${steps.1.stdout}"))
	if renamed[2] != executed[2] {
		t.Fatal("a renamed three-deep chain changed its last key")
	}
	// A change at the root reaches the top of the chain.
	changed := extendedKeys(t, step("a", kubectl, "get", "ns", "y", "-o", "json"), b, d)
	if changed[1] == executed[1] || changed[2] == executed[2] {
		t.Fatal("a changed root left the keys of the steps that depend on it unchanged")
	}
}

func TestExtendedKeysRefuseReferencesThatCannotResolve(t *testing.T) {
	a := step("a", kubectl, "get", "ns", "x", "-o", "json")
	for name, steps := range map[string][]lastv1alpha1.Command{
		"an unknown step": {a, step("b", "echo", "${steps.zz.stdout}")},
		"a later step":    {step("b", "echo", "${steps.a.stdout}"), a},
		"itself":          {step("b", "echo", "${steps.b.stdout}")},
		"a malformed one": {a, step("b", "echo", "${steps.a.nope}")},
		"an unclosed one": {a, step("b", "echo", "${steps.a.stdout")},
	} {
		if _, err := plan.ExtendedKeys(steps); !errors.Is(err, plan.ErrReference) {
			t.Errorf("%s: ExtendedKeys = %v, want ErrReference", name, err)
		}
	}
}

func TestExtendedKeysLeaveScriptsAlone(t *testing.T) {
	// Only argv references resolve; a script's text is compared as written.
	s := lastv1alpha1.Command{ID: "s", Run: lastv1alpha1.CommandRun{Script: "echo ${steps.zz.stdout}"}}
	keys := extendedKeys(t, s)
	other := s
	other.Run.Script = "echo ${steps.yy.stdout}"
	if extendedKeys(t, other)[0] == keys[0] {
		t.Fatal("two different scripts share a key")
	}
}

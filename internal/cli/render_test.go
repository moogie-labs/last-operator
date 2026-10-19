package cli

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	wordKubectl = "kubectl"
	webYAML     = "/work/web.yaml"
)

func TestCleanEscapesWhatCouldHideText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":       "plain text",
		"red\x1b[31mtext":  `red\u001b[31mtext`,
		"line\nbreak":      `line\u000abreak`,
		"carriage\rreturn": `carriage\u000dreturn`,
		"evil\u202etxt":    `evil\u202etxt`,
		"café ✓":           "café ✓",
		"zero\u200bwidth":  `zero\u200bwidth`,
		"para\u2028sep":    `para\u2028sep`,
		"tab\there":        "tab\there",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellWordQuotesAmbiguousArguments(t *testing.T) {
	for in, want := range map[string]string{
		wordKubectl:    wordKubectl,
		webYAML:        webYAML,
		"--timeout=3m": "--timeout=3m",
		"a b":          `"a b"`,
		"$(rm -rf /)":  `"$(rm -rf /)"`,
		"":             `""`,
		"x\u202ey":     `"x\u202ey"`,
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderPlanShowsWhatTheApproverDecides(t *testing.T) {
	p := &lastv1alpha1.Plan{Hash: h2, Summary: "nginx \x1b[8mhidden", Steps: []lastv1alpha1.Command{
		{ID: "apply", Reason: "create it", Run: lastv1alpha1.CommandRun{Argv: []string{wordKubectl, "apply", "-f", webYAML}},
			Files: map[string]string{webYAML: "kind: Deployment\n"}, Providers: []string{"k8s"},
			Egress: []string{"10.0.0.1:443"}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
		{ID: "check", Run: lastv1alpha1.CommandRun{Script: "set -e\necho done \x1b[2K"}},
	}, Resources: []lastv1alpha1.ManagedResourceSpec{{
		Description: webName,
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "ready", Command: argvCommand(wordKubectl, "get", "deploy", webName),
			Assert: "result.exitCode == 0"}},
		Teardown:         []lastv1alpha1.Command{{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{wordKubectl, "delete", "deploy", webName}}}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"ip": "${steps.svc.json.spec.clusterIP}"},
	}}}
	var b strings.Builder
	renderPlan(&b, p, lastv1alpha1.StageCreate, h2, false)
	got := b.String()
	for _, want := range []string{
		"Plan " + h2 + " (stage Create)\n",
		"Summary: nginx \\u001b[8mhidden\n",
		"  1. apply: create it\n",
		"     run:       kubectl apply -f /work/web.yaml\n",
		"     file:      /work/web.yaml (17 bytes)\n",
		"     providers: k8s\n",
		"     egress:    10.0.0.1:443\n",
		"     timeout:   2m0s\n",
		"  2. check\n",
		"     script:\n",
		"       | set -e\n",
		"       | echo done \\u001b[2K\n",
		"  - web\n",
		"    identity: {\"kind\":\"Deployment\"}\n",
		"    probe ready, assert result.exitCode == 0\n",
		"      run:       kubectl get deploy web\n",
		"    outputs to Secret web-outputs:\n",
		"      ip = ${steps.svc.json.spec.clusterIP}\n",
		"    teardown t1\n",
		"      run:       kubectl delete deploy web\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Error("an escape sequence reached the terminal")
	}
	if strings.Contains(got, "kind: Deployment") {
		t.Error("file content is shown; files are listed by path and size")
	}
}

func TestRenderPendingShowsTheCommandAndItsReason(t *testing.T) {
	var b strings.Builder
	renderPending(&b, &lastv1alpha1.PendingCommand{Hash: "sha256:stated", Reason: seeWhatExists, Command: argvCommand(wordKubectl, "get", "ns")}, h1, false)
	want := "Discovery command " + h1 + " (stage Discovery)\nReason: see what exists\n  run:       kubectl get ns\n"
	if b.String() != want {
		t.Fatalf("pending command =\n%s\nwant\n%s", b.String(), want)
	}
}

func TestRenderPlanEscapesEveryModelWrittenField(t *testing.T) {
	c := argvCommand(wordKubectl, "get")
	c.ID = "id\x1b[1m"
	c.Files = map[string]string{"/f\x1b[2K\nx": "da\x1b[8mta\u202e"}
	c.Providers = []string{"prov\x1bp"}
	c.Egress = []string{"1.2.3.4:1\nbad"}
	c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{aptGet, "install", "-y", "pk\x1bg"}}}}
	p := &lastv1alpha1.Plan{Hash: h2, Steps: []lastv1alpha1.Command{c}, Resources: []lastv1alpha1.ManagedResourceSpec{{
		Description: "res\x1b[8m\ndesc",
		Probes:      []lastv1alpha1.Probe{{ID: "pr\x1bobe", Command: argvCommand(wordKubectl), Assert: "a\nb"}},
	}}}
	var b strings.Builder
	renderPlan(&b, p, lastv1alpha1.StageCreate, h2, true)
	got := b.String()
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("an escape sequence reached the output:\n%q", got)
	}
	for _, want := range []string{`/f\u001b[2K\u000ax`, `prov\u001bp`, `1.2.3.4:1\u000abad`, `res\u001b[8m\u000adesc`,
		`probe pr\u001bobe, assert a\u000ab`, `id\u001b[1m`, `install:   apt-get install -y "pk\x1bg"` + "\n",
		"  | da\\u001b[8mta\\u202e\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	// Every line starts at a known structural prefix, so no injected newline made a fake line.
	for line := range strings.SplitSeq(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "bad") && !strings.Contains(line, `\u000a`) {
			t.Errorf("injected text on its own line: %q", line)
		}
	}
}

const aptGet = "apt-get"

func TestRenderShowsEachInstallCommand(t *testing.T) {
	c := argvCommand("aws", "s3", "ls")
	c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{
		{Argv: []string{aptGet, "update"}}, {Argv: []string{aptGet, "install", "-y", "awscli=2.9.19-1"}}}}
	var b strings.Builder
	renderCommand(&b, "  ", c, false)
	want := "  run:       aws s3 ls\n  install:   apt-get update\n  install:   apt-get install -y awscli=2.9.19-1\n"
	if b.String() != want {
		t.Errorf("command =\n%s\nwant\n%s", b.String(), want)
	}
	if strings.Contains(b.String(), "install commands") {
		t.Error("the install command count is still shown")
	}
}

func TestRenderShowsTheStageBeingApproved(t *testing.T) {
	in := testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Stage = lastv1alpha1.StageDelete
		in.Status.Plan = webPlan()
	})
	stage, hash, ok, err := verifiedSubject(in)
	if err != nil || !ok || stage != lastv1alpha1.StageDelete {
		t.Fatalf("verifiedSubject = %v, %q, %v, %v; want stage Delete", stage, hash, ok, err)
	}
	var b strings.Builder
	renderSubject(&b, in, stage, hash, false)
	if want := "Plan " + hash + " (stage Delete)\n"; !strings.HasPrefix(b.String(), want) {
		t.Errorf("output:\n%s\nwant prefix %q", b.String(), want)
	}
	in.Status.Stage = ""
	if stage, _, _, _ := verifiedSubject(in); stage != lastv1alpha1.StageCreate {
		t.Errorf("a plan with no stage = %v, want Create", stage)
	}
}

func TestAPlanForAnUnknownStageIsNeverDecidedAsCreate(t *testing.T) {
	in := testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Stage = lastv1alpha1.StageRemediate
		in.Status.Plan = webPlan()
	})
	_, _, ok, err := verifiedSubject(in)
	if !ok || err == nil || !strings.Contains(err.Error(), "stage Remediate") {
		t.Fatalf("verifiedSubject = ok %v, err %v; want a refusal naming stage Remediate", ok, err)
	}
}

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
		Description: "web",
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "ready", Command: argvCommand(wordKubectl, "get", "deploy", "web"),
			Assert: "result.exitCode == 0"}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"ip": "${steps.svc.json.spec.clusterIP}"},
	}}}
	var b strings.Builder
	renderPlan(&b, p)
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
	renderPending(&b, &lastv1alpha1.PendingCommand{Hash: h1, Reason: "see what exists", Command: argvCommand(wordKubectl, "get", "ns")})
	want := "Discovery command " + h1 + " (stage Discovery)\nReason: see what exists\n  run:       kubectl get ns\n"
	if b.String() != want {
		t.Fatalf("pending command =\n%s\nwant\n%s", b.String(), want)
	}
}

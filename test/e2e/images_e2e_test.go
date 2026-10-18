//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
)

var runImageRe = regexp.MustCompile(`^localhost:5000/last/images@sha256:[0-9a-f]{64}$`)

// imageScenario is a namespace, a scripted model and the OperatorConfig wiring for
// Intents that bake an install into their command images.
type imageScenario struct {
	t       *testing.T
	crc     client.Client
	ns      string
	install [][]string
	hash    string
}

// newImageScenario creates the namespace and scripted LLM for intents (names in ns)
// whose plan runs `last-e2e-tool` from an image built with install. The OperatorConfig
// change is restored in cleanup.
func newImageScenario(t *testing.T, prefix string, install [][]string, intents ...string) *imageScenario {
	t.Helper()
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	kubectl(t, "rollout", "status", "-n", "last-system", "deploy/last-operator", "--timeout=5m")
	ctx := context.Background()
	crc, _ := e2eClients(t)
	suffix := fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	sc := &imageScenario{t: t, crc: crc, ns: prefix + suffix, install: install}
	llmName := sc.ns
	create := func(o client.Object) {
		t.Helper()
		if err := crc.Create(ctx, o); err != nil {
			t.Fatalf("create %T: %v", o, err)
		}
	}

	var op lastv1alpha1.OperatorConfig
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); apierrors.IsNotFound(err) {
		create(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	} else if err != nil {
		t.Fatal(err)
	}
	var prevLLM *lastv1alpha1.LocalRef
	t.Cleanup(func() {
		bg := context.Background()
		for _, o := range []client.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sc.ns}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"}},
			&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}},
		} {
			if err := crc.Delete(bg, o); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("cleanup %T: %v", o, err)
			}
		}
		for range 5 {
			var cur lastv1alpha1.OperatorConfig
			if err := crc.Get(bg, client.ObjectKey{Name: "default"}, &cur); err != nil {
				t.Errorf("cleanup: read OperatorConfig: %v", err)
				return
			}
			if cur.Spec.LLMProviderRef != nil && cur.Spec.LLMProviderRef.Name == llmName {
				cur.Spec.LLMProviderRef = prevLLM
			}
			if err := crc.Update(bg, &cur); err == nil {
				return
			}
		}
		t.Errorf("cleanup: could not restore OperatorConfig")
	})
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sc.ns}})

	// The same install and argv for the step and the probe, so one image serves both.
	image := &lastv1alpha1.ImageSpec{}
	for _, argv := range install {
		image.Install = append(image.Install, lastv1alpha1.ArgvCommand{Argv: argv})
	}
	run := lastv1alpha1.CommandRun{Argv: []string{"last-e2e-tool"}}
	steps := []lastv1alpha1.Command{{ID: "s1", Reason: "Run the tool baked into the image",
		Run: run, Image: image, Timeout: &metav1.Duration{Duration: 20 * time.Minute}}}
	resources := []lastv1alpha1.ManagedResourceSpec{{
		Description: "image tool " + sc.ns,
		Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Tool","namespace":"` + sc.ns + `","name":"last-e2e-tool"}`)},
		Probes: []lastv1alpha1.Probe{{ID: "tool-ok",
			Command: lastv1alpha1.Command{Run: run, Image: image},
			Assert:  "result.exitCode == 0 && result.stdout.contains('last-e2e-tool-ok')"}},
	}}
	a, err := agent.Encode(agent.Turn{Kind: agent.KindPlan, Plan: &lastv1alpha1.Plan{
		Summary: "Run last-e2e-tool from a built image in " + sc.ns, Steps: steps, Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]string{}
	for _, name := range intents {
		data[llm.ScriptKey(sc.ns+"/"+name)] = string(script)
	}
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: llmName, Namespace: "last-gateway"}, Data: data})
	create(&lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: llmName}, Spec: lastv1alpha1.LLMProviderSpec{
		Type: lastv1alpha1.LLMScripted, Model: "scripted", Scripted: &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: llmName}}}})
	if err := crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	prevLLM = op.Spec.LLMProviderRef
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: llmName}
	if err := crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}
	return sc
}

// request runs kubectl last as the user, who approves the plan, and returns its output and error.
func (sc *imageScenario) request(name string) (string, error) {
	sc.t.Helper()
	return runLastInput(sc.t, 30*time.Minute, "y\n", "Run last-e2e-tool in namespace "+sc.ns, "-n", sc.ns, "--name", name)
}

func (sc *imageScenario) intent(name string) lastv1alpha1.Intent {
	sc.t.Helper()
	var in lastv1alpha1.Intent
	if err := sc.crc.Get(context.Background(), client.ObjectKey{Namespace: sc.ns, Name: name}, &in); err != nil {
		sc.t.Fatal(err)
	}
	return in
}

func payloadOf(e audit.Entry) map[string]any {
	var p map[string]any
	// Entries whose payload is not an object read as empty.
	_ = json.Unmarshal(e.Payload, &p)
	return p
}

// TestImageBuildIsReused is M4b's done-criterion: a plan's install is baked into an
// image once, and every command, probe and later Intent with the same install runs
// that image's digest.
func TestImageBuildIsReused(t *testing.T) {
	// The install is unique per run, so its spec hash is fresh and exactly one build is expected.
	install := [][]string{{"python3", "-c",
		`import os; p='/usr/local/bin/last-e2e-tool'; open(p,'w').write('#!/bin/sh\necho last-e2e-tool-ok\n'); os.chmod(p,0o755)`,
		fmt.Sprintf("run-%x", time.Now().UnixNano())}}
	sc := newImageScenario(t, "e2e-img-", install, "one", "two")

	for _, name := range []string{"one", "two"} {
		out, err := sc.request(name)
		if err != nil {
			t.Fatalf("kubectl last (%s): %v\n%s", name, err, out)
		}
		if in := sc.intent(name); in.Status.Phase != lastv1alpha1.PhaseReady {
			t.Fatalf("intent %s phase = %s, want Ready\n%s", name, in.Status.Phase, out)
		}
	}

	entries := auditEntries(t, "")
	var specHash string
	for _, e := range entries {
		if e.Intent == sc.ns+"/one" && e.Type == audit.TypeImageBuildStart {
			specHash, _ = payloadOf(e)["specHash"].(string)
			break
		}
	}
	if specHash == "" {
		t.Fatalf("the first Intent has no image.build.start entry")
	}
	var starts, succeeded int
	var digest string
	for _, e := range entries {
		p := payloadOf(e)
		if p["specHash"] != specHash {
			continue
		}
		switch e.Type {
		case audit.TypeImageBuildStart:
			starts++
		case audit.TypeImageBuildResult:
			if p["outcome"] == "succeeded" {
				succeeded++
				digest, _ = p["digest"].(string)
			}
		}
	}
	if starts != 1 || succeeded != 1 || digest == "" {
		t.Fatalf("image.build.start x%d, succeeded results x%d, digest %q for spec hash %s; want exactly one build", starts, succeeded, digest, specHash)
	}

	var runs int
	var ref string
	for _, name := range []string{"one", "two"} {
		for _, e := range auditEntries(t, sc.ns+"/"+name) {
			if e.Type != audit.TypeCommandStart && e.Type != audit.TypeProbeStart {
				continue
			}
			runs++
			img, _ := payloadOf(e)["image"].(string)
			if !runImageRe.MatchString(img) {
				t.Errorf("%s of %s runs image %q, want localhost:5000/last/images@sha256:...", e.Type, name, img)
			}
			if ref == "" {
				ref = img
			}
			if img != ref {
				t.Errorf("%s of %s runs %q, want the same image %q", e.Type, name, img, ref)
			}
		}
	}
	// One step and one probe in each of the two Intents, at least.
	if runs < 4 {
		t.Errorf("found %d command.start and probe.start entries, want at least 4", runs)
	}
	if !strings.HasSuffix(ref, "@"+digest) {
		t.Errorf("the commands ran %q, the build result names %q", ref, digest)
	}
	verifyAuditChain(t)
}

// TestImageBuildFailureFailsTheStep: an install that exits non-zero fails the build, and the step.
func TestImageBuildFailureFailsTheStep(t *testing.T) {
	sc := newImageScenario(t, "e2e-imgfail-", [][]string{{"false"}}, "bad")
	out, _ := sc.request("bad")
	in := sc.intent("bad")
	if in.Status.Phase != lastv1alpha1.PhaseFailed {
		t.Fatalf("intent phase = %s, want Failed\n%s", in.Status.Phase, out)
	}
	// The operator replans after the failed step, so the Intent's later status is not the
	// build error; the phase.end entry the gateway wrote for the step is.
	var failed, ended bool
	for _, e := range auditEntries(t, sc.ns+"/bad") {
		if e.Type == audit.TypePhaseEnd && strings.Contains(fmt.Sprint(payloadOf(e)["reason"]), "ImageBuildFailed: ") {
			ended = true
		}
		if e.Type == audit.TypeImageBuildResult && payloadOf(e)["outcome"] == "failed" {
			failed = true
		}
	}
	if !ended {
		t.Errorf("no phase.end entry whose reason starts the step failure with ImageBuildFailed\n%s", out)
	}
	if !failed {
		t.Errorf("no failed image.build.result entry for the Intent")
	}
}

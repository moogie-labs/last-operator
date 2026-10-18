package gateway

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	okOutput = "ok\n"
	trueBin  = "true"
)

// newImageRunEnv is an image env holding an approved plan of steps and resources.
func newImageRunEnv(t *testing.T, op *lastv1alpha1.OperatorConfig, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) (*imageEnv, *lastv1alpha1.Intent) {
	t.Helper()
	in := intentWithPlan(t, steps, resources...)
	ie := newImageEnv(t, op, in, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	ie.result = func(runnerapi.Command) runnerapi.Result { return runnerapi.Result{Stdout: okOutput} }
	return ie, in
}

func withInstall(c lastv1alpha1.Command, inst []lastv1alpha1.ArgvCommand) lastv1alpha1.Command {
	c.Image = &lastv1alpha1.ImageSpec{Install: inst}
	return c
}

// podImages is the image of every sandbox pod created so far.
func (e *testEnv) podImages() []string {
	var out []string
	for _, act := range e.kube.Actions() {
		if ca, ok := act.(k8stesting.CreateAction); ok && act.GetResource().Resource == "pods" {
			out = append(out, ca.GetObject().(*corev1.Pod).Spec.Containers[0].Image)
		}
	}
	return out
}

func (e *testEnv) startPayloads(typ string) []map[string]any {
	var out []map[string]any
	for _, en := range e.entries() {
		if en.Type == typ {
			var m map[string]any
			if err := json.Unmarshal(en.Payload, &m); err != nil {
				e.t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}

func imageSteps() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{
		withInstall(argvStep("s1", "jq", "--version"), toolInstall),
		argvStep("s2", "echo", "hi"),
	}
}

func TestRunCommandRunsOnTheResolvedImage(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), imageSteps())
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	r, err := ie.run(h, in.Status.Plan.Steps[0])
	if err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("run = %v, %v", r, err)
	}
	starts := ie.startPayloads(audit.TypeCommandStart)
	if len(starts) != 1 {
		t.Fatalf("command.start entries = %d", len(starts))
	}
	image, _ := starts[0]["image"].(string)
	if !strings.HasPrefix(image, testPullHost+"/last/images@sha256:") {
		t.Fatalf("command.start image = %q", image)
	}
	if pods := ie.podImages(); !slices.Equal(pods, []string{image}) {
		t.Fatalf("pod images = %v, want [%s]", pods, image)
	}
	// A step without install still runs on the base.
	if _, err := ie.run(h, in.Status.Plan.Steps[1]); err != nil {
		t.Fatal(err)
	}
	if pods := ie.podImages(); len(pods) != 2 || pods[1] != testBase {
		t.Fatalf("pod images = %v", pods)
	}
}

func TestRunCommandReportsAFailedBuild(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), imageSteps())
	ie.fb.fail = errors.New("apt exploded")
	ie.fb.log = "E: unable to locate package jq"
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	r, err := ie.run(h, in.Status.Plan.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	res := r.GetResult()
	if res.GetExitCode() != -1 || res.GetError() != "ImageBuildFailed: apt exploded" || res.GetStderr() != ie.fb.log {
		t.Fatalf("result = %+v", res)
	}
	if !ie.g.phases.get(envKey).failed {
		t.Fatal("a failed build must fail the phase")
	}
	if n := len(ie.startPayloads(audit.TypeCommandStart)); n != 0 || len(ie.podImages()) != 0 {
		t.Fatalf("a failed build still ran a command: %d command.start, pods %v", n, ie.podImages())
	}
	_, err = ie.run(h, in.Status.Plan.Steps[1])
	ie.wantDenied(err, codes.FailedPrecondition)
}

func TestRunCommandStopsOnAnUnpinnedBase(t *testing.T) {
	ie, in := newImageRunEnv(t, allowAll(), imageSteps()) // no pinned base: the Helm fallback image
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := ie.run(h, in.Status.Plan.Steps[0])
	wantCode(t, err, codes.FailedPrecondition)
	if !ie.g.phases.get(envKey).failed {
		t.Fatal("an unpinned base must fail the phase")
	}
}

func TestRunCommandKeepsThePhaseOpenWhenTheBuildCannotBeAttempted(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), imageSteps())
	ie.fb.buildErr = errors.New("kubernetes unreachable")
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := ie.run(h, in.Status.Plan.Steps[0])
	wantCode(t, err, codes.Unavailable)
	if ie.g.phases.get(envKey).failed {
		t.Fatal("a retryable error must not fail the phase")
	}
	ie.fb.buildErr = nil
	if r, err := ie.run(h, in.Status.Plan.Steps[0]); err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("retry = %v, %v", r, err)
	}
}

func probesWithImages() lastv1alpha1.ManagedResourceSpec {
	mk := func(id string, inst []lastv1alpha1.ArgvCommand) lastv1alpha1.Probe {
		c := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{trueBin}}}
		if inst != nil {
			c.Image = &lastv1alpha1.ImageSpec{Install: inst}
		}
		return lastv1alpha1.Probe{ID: id, Assert: "result.exitCode == 0", Command: c}
	}
	return lastv1alpha1.ManagedResourceSpec{Description: "tools", Probes: []lastv1alpha1.Probe{
		mk("plain", nil),
		mk("jq", toolInstall),
		mk("yq", install([]string{"apt-get", "install", "-y", "yq"})),
		mk("jq2", toolInstall),
	}}
}

func TestRunProbesGroupsByImage(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), []lastv1alpha1.Command{argvStep("s1", trueBin)}, probesWithImages())
	executeAll(t, ie.testEnv, in)
	resp, err := ie.probe()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetOutcomes()) != 4 {
		t.Fatalf("outcomes = %+v", resp.GetOutcomes())
	}
	for i, id := range []string{"plain", "jq", "yq", "jq2"} {
		if o := resp.GetOutcomes()[i]; o.GetId() != id || !o.GetPassed() {
			t.Errorf("outcome %d = %+v", i, o)
		}
	}
	if ie.fb.builds() != 2 {
		t.Fatalf("builds = %d, want 2 (one per distinct install)", ie.fb.builds())
	}
	images := map[string]string{}
	for _, s := range ie.startPayloads(audit.TypeProbeStart) {
		probes := s["probes"].([]any)
		images[probes[0].(map[string]any)["id"].(string)] = s["image"].(string)
		if len(probes) == 2 && probes[1].(map[string]any)["id"] != "jq2" {
			t.Errorf("group of %v is not the jq group", probes)
		}
	}
	if len(images) != 3 || images["plain"] != testBase || images["jq"] == images["yq"] ||
		!strings.HasPrefix(images["jq"], testPullHost+"/last/images@sha256:") || !strings.HasPrefix(images["yq"], testPullHost+"/last/images@sha256:") {
		t.Fatalf("probe.start images = %v", images)
	}
	// 1 step pod + 3 probe pods.
	if pods := ie.podImages(); len(pods) != 4 {
		t.Fatalf("pod images = %v", pods)
	}
}

func TestRunProbesReportsAFailedBuildPerGroupAndRunsTheRest(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), []lastv1alpha1.Command{argvStep("s1", trueBin)}, probesWithImages())
	executeAll(t, ie.testEnv, in)
	ie.fb.fail = errors.New("no such package")
	ie.fb.log = "build log tail"
	resp, err := ie.probe()
	if err != nil {
		t.Fatal(err)
	}
	outs := resp.GetOutcomes()
	if len(outs) != 4 || outs[0].GetId() != "plain" || !outs[0].GetPassed() {
		t.Fatalf("outcomes = %+v", outs)
	}
	for _, o := range outs[1:] {
		if o.GetPassed() || o.GetError() != "ImageBuildFailed: no such package" || o.GetResult().GetExitCode() != -1 ||
			o.GetResult().GetStderr() != "build log tail" {
			t.Errorf("outcome %s = %+v", o.GetId(), o)
		}
	}
	if starts := ie.startPayloads(audit.TypeProbeStart); len(starts) != 1 {
		t.Fatalf("probe.start entries = %d, want 1 (only the plain group ran)", len(starts))
	}
}

func TestRunProbesFailsWhenABuildCannotBeAttempted(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), []lastv1alpha1.Command{argvStep("s1", trueBin)}, probesWithImages())
	executeAll(t, ie.testEnv, in)
	ie.fb.buildErr = errors.New("kubernetes unreachable")
	_, err := ie.probe()
	wantCode(t, err, codes.Unavailable)
	if n := len(ie.startPayloads(audit.TypeProbeStart)); n != 0 {
		t.Fatalf("probes ran despite the resolve error: %d probe.start", n)
	}
}

func TestDrainWaitsForADetachedBuildFlight(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.fb.block = make(chan struct{})
	ie.g.inflight.Add(1) // the RPC that waits on the build
	done := make(chan error, 1)
	go func() { _, err := ie.resolve(toolInstall); done <- err }()
	ie.waitStarted()
	ie.g.inflight.Done() // the RPC's own count ends; only the flight remains
	if ie.g.drain(50 * time.Millisecond) {
		t.Fatal("drain finished while a build flight was running")
	}
	close(ie.fb.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !ie.g.drain(time.Second) {
		t.Fatal("drain did not finish after the flight ended")
	}
}

// denials returns the reasons of every policy.denied entry.
func (e *testEnv) denials() []string {
	payloads := e.startPayloads(audit.TypePolicyDenied)
	out := make([]string, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, p["reason"].(string))
	}
	return out
}

func hostnameEgressOp() *lastv1alpha1.OperatorConfig {
	return imageOp([]string{"example.com:443"})
}

func wantImageDenial(t *testing.T, ie *imageEnv) {
	t.Helper()
	ds := ie.denials()
	if len(ds) != 1 || !strings.HasPrefix(ds[0], "image: ") || !strings.Contains(ds[0], "example.com") {
		t.Fatalf("policy.denied reasons = %v", ds)
	}
}

func TestRunCommandAuditsAnImageRefusalAndFailsThePhase(t *testing.T) {
	ie, in := newImageRunEnv(t, hostnameEgressOp(), imageSteps())
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := ie.run(h, in.Status.Plan.Steps[0])
	wantCode(t, err, codes.FailedPrecondition)
	wantImageDenial(t, ie)
	if !ie.g.phases.get(envKey).failed {
		t.Fatal("an image refusal must fail the phase")
	}
}

func TestRunProbesAuditsAnImageRefusal(t *testing.T) {
	ie, in := newImageRunEnv(t, hostnameEgressOp(), []lastv1alpha1.Command{argvStep("s1", trueBin)}, probesWithImages())
	executeAll(t, ie.testEnv, in)
	_, err := ie.probe()
	wantCode(t, err, codes.FailedPrecondition)
	wantImageDenial(t, ie)
	if n := len(ie.startPayloads(audit.TypeProbeStart)); n != 0 {
		t.Fatalf("probes ran despite the refusal: %d probe.start", n)
	}
}

func TestRunProbesMapsAnExecutorStatusErrorLikeAnyOther(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, status.Error(codes.FailedPrecondition, "pod refused")
	})
	resp, err := env.probe()
	if status.Code(err) != codes.Internal || resp != nil {
		t.Fatalf("executor status error = %v, %v; want Internal", resp, err)
	}
}

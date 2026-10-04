package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runner"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDefaultTimingsFlowIntoDeadlineTokenAndExpiry(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	var tokenSecs int64
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tokenSecs = *tr.Spec.ExpirationSeconds
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	var pod *corev1.Pod
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod = a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		return false, nil, nil
	})
	aud, _ := newAudit(t)
	// Default DeadlineOverhead (60s) and StartupGrace (2m): only polling and ids are set.
	ex := sandbox.NewExecutor(kube, providers(k8sProvider(clusterName)), aud, sandbox.NewScheduler(1), envelopeLogs(runnerapi.Result{ID: "c1"}),
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: sandboxNS, Image: probeImage},
			ProviderNamespace: gatewayNS,
			PollInterval:      time.Millisecond,
			NewRunID:          func() string { return runID },
		})
	c := cmd([]string{clusterName}, nil)
	c.Timeout = &metav1.Duration{Duration: 5 * time.Minute}
	before := time.Now()
	if _, err := ex.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c}); err != nil {
		t.Fatal(err)
	}
	if pod == nil {
		t.Fatal("no pod was created")
	}
	if got := *pod.Spec.ActiveDeadlineSeconds; got != 360 {
		t.Fatalf("activeDeadlineSeconds = %d, want 360 (5m + 60s)", got)
	}
	// The pod can live for 360s + 120s grace = 480s, but the API server never mints a
	// token for less than 10m (DefaultTokenTTL), so the request is the larger of the two.
	if want := int64(sandbox.DefaultTokenTTL.Seconds()); tokenSecs != want || tokenSecs < 480 {
		t.Fatalf("token ExpirationSeconds = %d, want %d (>= the pod's 480s)", tokenSecs, want)
	}
	secs, err := strconv.ParseInt(pod.Labels[sandbox.LabelExpiresAt], 10, 64)
	if err != nil {
		t.Fatalf("pod expires-at %q: %v", pod.Labels[sandbox.LabelExpiresAt], err)
	}
	if d := time.Unix(secs, 0).Sub(before.Add(480 * time.Second)); d < -2*time.Second || d > 5*time.Second {
		t.Fatalf("pod expires-at is %s off now+480s", d)
	}
}

// The gateway's SpecSecret, the runner's strict decoder, the runner itself and the
// gateway's envelope parser must agree on one format.
func TestSpecSurvivesTheWholeGatewayToRunnerRoundTrip(t *testing.T) {
	cmds := []lastv1alpha1.Command{
		{ID: "argv", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", "hello"}}},
		{ID: "script", Run: lastv1alpha1.CommandRun{Script: "echo out; echo err >&2; exit 3"}},
	}
	spec, _, err := sandbox.RunnerSpec(cmds)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sandbox.SpecSecret(sandboxNS, runID, spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := runnerapi.DecodeSpec(bytes.NewReader(secret.Data["spec.json"]))
	if err != nil {
		t.Fatalf("the runner's strict decoder rejects the gateway's spec: %v", err)
	}
	if !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("decoded spec = %+v, want %+v", decoded, spec)
	}
	env := runner.Run(context.Background(), decoded, runner.Options{MaxOutput: runnerapi.MaxOutput, WorkDir: t.TempDir(), Shell: "/bin/sh"})
	out, err := json.Marshal(env) // as cmd/sandbox-runner prints it
	if err != nil {
		t.Fatal(err)
	}
	log := append([]byte("noise before\n"), []byte(runnerapi.ResultPrefix+string(out)+"\n")...)
	parsed, err := runnerapi.ParseEnvelope(log)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, env) || len(parsed.Results) != 2 {
		t.Fatalf("parsed = %+v, want %+v", parsed, env)
	}
	if r := parsed.Results[0]; r.ID != "argv" || r.ExitCode != 0 || r.Stdout != "hello\n" {
		t.Fatalf("argv result = %+v", r)
	}
	if r := parsed.Results[1]; r.ID != "script" || r.ExitCode != 3 || r.Stdout != "out\n" || r.Stderr != "err\n" {
		t.Fatalf("script result = %+v", r)
	}
}

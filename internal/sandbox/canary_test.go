package sandbox_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	canaryAddr  = "10.96.5.5:8080"
	canaryAddr2 = "172.18.0.2:10250"
)

func newCanaryExec(t *testing.T, canaries ...string) (*sandbox.Executor, *[]runnerapi.Spec, func() int) {
	t.Helper()
	kube, _ := fakeCluster(corev1.PodSucceeded)
	specs := &[]runnerapi.Spec{}
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s, ok := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); ok {
			if raw, ok := s.Data["spec.json"]; ok {
				var spec runnerapi.Spec
				_ = json.Unmarshal(raw, &spec)
				*specs = append(*specs, spec)
			}
		}
		return false, nil, nil
	})
	aud, sink := newAudit(t)
	e := sandbox.NewExecutor(kube, providers(), aud, sandbox.NewScheduler(2), envelopeLogs(runnerapi.Result{ID: "c1"}),
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: sandboxNS, Image: probeImage},
			ProviderNamespace: gatewayNS, PollInterval: time.Millisecond, DeadlineOverhead: time.Millisecond,
			StartupGrace: 50 * time.Millisecond, CleanupTimeout: time.Second, EgressCanaries: canaries,
			NewRunID: func() string { return runID },
		})
	return e, specs, func() int { return len(entries(t, sink)) }
}

func TestRunPutsTheEgressCanaryInTheSpec(t *testing.T) {
	e, specs, _ := newCanaryExec(t, canaryAddr, canaryAddr2)
	if _, err := e.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	if len(*specs) != 1 || !slices.Equal((*specs)[0].EgressCanaries, []string{canaryAddr, canaryAddr2}) {
		t.Fatalf("specs = %+v", *specs)
	}
}

func TestRunProbesPutTheEgressCanaryInTheSpec(t *testing.T) {
	e, specs, _ := newCanaryExec(t, canaryAddr)
	if _, err := e.RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor,
		Probes: []lastv1alpha1.Probe{{ID: "c1", Assert: "result.exitCode == 0", Command: cmd(nil, nil)}}}); err != nil {
		t.Fatal(err)
	}
	if len(*specs) != 1 || !slices.Equal((*specs)[0].EgressCanaries, []string{canaryAddr}) {
		t.Fatalf("specs = %+v", *specs)
	}
}

func TestRunWithoutACanaryLeavesTheSpecAlone(t *testing.T) {
	e, specs, _ := newCanaryExec(t)
	if _, err := e.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	if len((*specs)[0].EgressCanaries) != 0 {
		t.Fatalf("spec = %+v", (*specs)[0])
	}
}

func TestRunRejectsGrantsThatCoverTheCanaryBeforeAuditing(t *testing.T) {
	for grant, canary := range map[string]string{
		"10.96.5.5:8080": canaryAddr, "10.96.5.5/32:8080": canaryAddr, "10.96.0.0/16:8080": canaryAddr,
		"[::ffff:10.96.5.5]:8080": canaryAddr, "172.18.0.2:10250": canaryAddr2, "172.18.0.0/24:10250": canaryAddr2,
	} {
		e, specs, n := newCanaryExec(t, canaryAddr, canaryAddr2)
		base := n() // the chain's own start entry
		_, err := e.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, []string{grant})})
		if err == nil || !strings.Contains(err.Error(), "egress grant would allow the egress canary "+canary) {
			t.Fatalf("grant %s: err = %v", grant, err)
		}
		if n() != base || len(*specs) != 0 {
			t.Fatalf("grant %s: %d audit entries, %d specs", grant, n(), len(*specs))
		}
	}
}

func TestRunAllowsAnotherPortOnTheCanaryIP(t *testing.T) {
	e, _, _ := newCanaryExec(t, canaryAddr)
	if _, err := e.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, []string{"10.96.5.5:9090"})}); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsACanaryThatIsNotAnIPPort(t *testing.T) {
	for _, bad := range []string{"gateway.local:8080", "10.0.0.1", "10.0.0.1:0"} {
		e, _, n := newCanaryExec(t, canaryAddr, bad)
		base := n()
		_, err := e.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)})
		if err == nil || !strings.Contains(err.Error(), "egress canary "+strconv.Quote(bad)+" must be an IP:port literal") || n() != base {
			t.Fatalf("canary %q: err = %v, entries = %d", bad, err, n())
		}
	}
}

package sandbox_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	probeBin    = "kubectl"
	probeIntent = "team-x/web"
)

func probeOf(id, assert string, egress ...string) lastv1alpha1.Probe {
	return lastv1alpha1.Probe{ID: id, Assert: assert, Command: lastv1alpha1.Command{
		Run: lastv1alpha1.CommandRun{Argv: []string{probeBin, "get", "deploy", "-o", "json"}}, Egress: egress,
		Timeout: &metav1.Duration{Duration: 10 * time.Millisecond}}}
}

func count(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

func TestRunProbesGroupsByProvidersAndEgressWithCompactAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	logs := envelopeLogs(
		runnerapi.Result{ID: "replicas", Stdout: `{"spec":{"replicas":2}}`},
		runnerapi.Result{ID: "ready", Stdout: `{"status":{"readyReplicas":1}}`},
		runnerapi.Result{ID: "remote", Stdout: "ok"},
	)
	outs, err := newExec(kube, aud, logs).RunProbes(context.Background(), sandbox.ProbeRunRequest{Intent: probeIntent, Probes: []lastv1alpha1.Probe{
		probeOf("replicas", "result.json.spec.replicas == 2"),
		probeOf("ready", "result.json.status.readyReplicas == 2"),
		probeOf("remote", `result.stdout == "ok"`, "10.1.2.3:443"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 || !outs[0].Passed || outs[1].Passed || outs[1].Error != "" || !outs[2].Passed {
		t.Fatalf("outcomes = %+v", outs)
	}
	if n := count(*created, "pods"); n != 2 {
		t.Fatalf("created %d pods, want 2 (one per provider/egress group)", n)
	}
	want := []string{audit.TypeProbeStart, audit.TypeProbeResult, audit.TypeProbeStart, audit.TypeProbeResult}
	if got := entryTypes(entries(t, sink)); !slices.Equal(got, want) {
		t.Fatalf("audit types = %v, want %v (no per-step entries for probes)", got, want)
	}
	assertCleaned(t, kube)
}

func TestRunProbesBadAssertionIsAFailedProbeNotAnError(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "p", Stdout: "not json"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("p", "result.json.x == 1")}})
	if err != nil || outs[0].Passed || outs[0].Error == "" {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
}

func TestRunProbesInfrastructureFailureFailsEveryProbeInTheGroup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	outs, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("b", "true")}})
	if err == nil || len(outs) != 2 || outs[0].Passed || outs[1].Passed || !strings.Contains(outs[1].Error, "did not finish") {
		t.Fatalf("outs=%+v err=%v", outs, err)
	}
	es := entries(t, sink)
	if last := es[len(es)-1]; last.Type != audit.TypeProbeResult || !strings.Contains(string(last.Payload), "did not finish") {
		t.Fatalf("last entry = %s %s", last.Type, last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunProbesRejectsDuplicateIDs(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true"), probeOf("a", "true")}})
	if err == nil || len(*created) != 0 {
		t.Fatalf("err=%v created=%v", err, *created)
	}
}

func TestRunProbesFailsClosedWithoutAudit(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "a"})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("a", "true")}})
	if !errors.Is(err, audit.ErrAuditUnavailable) || count(*created, "pods") != 0 {
		t.Fatalf("err=%v created=%v", err, *created)
	}
}

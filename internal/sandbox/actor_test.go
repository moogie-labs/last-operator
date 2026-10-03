package sandbox_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const testActor = "alice@example.com"

func TestRunRecordsTheCallerOnEveryEntryButRevocation(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		audit.TypeCommandStart: testActor, audit.TypeCredentialIssued: testActor,
		audit.TypeEgressGranted: testActor, audit.TypeCommandResult: testActor,
		audit.TypeEgressRevoked: sandbox.GatewayActor,
	}
	seen := 0
	for _, e := range entries(t, sink) {
		w, ok := want[e.Type]
		if !ok {
			continue
		}
		seen++
		if e.Actor != w {
			t.Errorf("%s actor = %q, want %q", e.Type, e.Actor, w)
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d expected entry types", seen, len(want))
	}
}

func TestRunProbesRecordsTheCallerOnStartAndResult(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	_, err := newProbeExec(kube, aud, podLogs(kube, map[string]runnerapi.Result{"p": {ID: "p"}})).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Actor: testActor, Intent: probeIntent, Probes: []lastv1alpha1.Probe{probeOf("p", "true")}})
	if err != nil {
		t.Fatal(err)
	}
	starts, results := probeEntries(t, sink)
	if len(starts) != 1 || len(results) != 1 || starts[0].Actor != testActor || results[0].Actor != testActor {
		t.Fatalf("starts=%+v results=%+v", starts, results)
	}
}

func TestRunRejectsAnEmptyActorBeforeAnythingIsWritten(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Command: cmd([]string{demoProvider}, nil)})
	if err == nil {
		t.Fatal("empty actor was accepted")
	}
	if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
		t.Fatal("rejected run still produced audit entries or objects")
	}
}

func TestRunProbesRejectsAnEmptyActorBeforeAnythingIsWritten(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	_, err := newProbeExec(kube, aud, envelopeLogs()).RunProbes(context.Background(),
		sandbox.ProbeRunRequest{Probes: []lastv1alpha1.Probe{probeOf("p", "true")}})
	if err == nil {
		t.Fatal("empty actor was accepted")
	}
	if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
		t.Fatal("rejected probes still produced audit entries or objects")
	}
}

func revokedPayloads(t *testing.T, es []audit.Entry) []string {
	t.Helper()
	var out []string
	for _, e := range es {
		if e.Type == audit.TypeEgressRevoked {
			out = append(out, string(e.Payload))
		}
	}
	return out
}

func TestExecutorEgressRevokedCarriesReasonCompleted(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	ex := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"}))
	if _, err := ex.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})}); err != nil {
		t.Fatal(err)
	}
	got := revokedPayloads(t, entries(t, sink))
	if len(got) != 1 || got[0] != `{"reason":"completed","runId":"`+runID+`"}` {
		t.Fatalf("executor egress.revoked = %v", got)
	}
}

func TestRunDeduplicatesProviders(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider, demoProvider}, nil)})
	if err != nil {
		t.Fatalf("a repeated provider must be collapsed, got %v", err)
	}
	for _, e := range entries(t, sink) {
		if e.Type != audit.TypeCommandStart {
			continue
		}
		var p struct {
			Providers []string `json:"providers"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Providers) != 1 || p.Providers[0] != demoProvider {
			t.Fatalf("command.start providers = %v", p.Providers)
		}
	}
}

func TestRunRejectsCommandFilesOnProviderMounts(t *testing.T) {
	providerFile := "/work/.demo/" + configKey // envProvider mounts the config key here
	for name, path := range map[string]string{
		"equal":  providerFile,
		"nested": providerFile + "/child",
		"dirty":  "/work/.demo/./" + configKey,
	} {
		t.Run(name, func(t *testing.T) {
			kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
			aud, sink := newAudit(t)
			c := cmd([]string{demoProvider}, nil)
			c.Files = map[string]string{path: "overwrite"}
			_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c})
			if err == nil || !strings.Contains(err.Error(), providerFile) {
				t.Fatalf("err = %v, want a conflict naming %s", err, providerFile)
			}
			if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
				t.Fatal("rejected command still produced audit entries or objects")
			}
		})
	}
}

func TestRunAllowsCommandFilesBesideProviderMounts(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, _ := newAudit(t)
	c := cmd([]string{demoProvider}, nil)
	c.Files = map[string]string{"/work/.demo/" + configKey + "-extra": "ok", "/work/app.yaml": "ok"}
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c}); err != nil {
		t.Fatal(err)
	}
}

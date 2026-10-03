package sandbox_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// singleSlotExec returns an executor whose scheduler has one slot, plus the scheduler
// and a counter of Provider lookups.
func singleSlotExec(t *testing.T, logs sandbox.LogReader) (*sandbox.Executor, *sandbox.Scheduler, *atomic.Int64) {
	t.Helper()
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, _ := newAudit(t)
	inner := providers(envProvider())
	var lookups atomic.Int64
	get := func(ctx context.Context, name string) (*lastv1alpha1.Provider, error) {
		lookups.Add(1)
		return inner(ctx, name)
	}
	sched := sandbox.NewScheduler(1)
	ex := sandbox.NewExecutor(kube, get, aud, sched, logs, sandbox.ExecutorConfig{
		Pod:               sandbox.PodConfig{Namespace: sandboxNS, Image: probeImage},
		ProviderNamespace: gatewayNS,
		PollInterval:      time.Millisecond, DeadlineOverhead: time.Millisecond, StartupGrace: 50 * time.Millisecond,
		CleanupTimeout: time.Second, NewRunID: func() string { return runID },
	})
	return ex, sched, &lookups
}

func TestRunReadsProvidersOnlyAfterItsSlotIsAcquired(t *testing.T) {
	ex, sched, lookups := singleSlotExec(t, envelopeLogs(runnerapi.Result{ID: "c1"}))
	release, err := sched.Acquire(context.Background(), sandbox.PriorityExecution)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ex.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, nil)})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for sched.Waiting(sandbox.PriorityExecution) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the run never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if n := lookups.Load(); n != 0 {
		t.Fatalf("%d provider lookups happened while the run was still queued; its data would go stale", n)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lookups.Load() == 0 {
		t.Fatal("the run never read its provider")
	}
}

func TestRunReleasesItsSlotWhenValidationFails(t *testing.T) {
	ex, _, _ := singleSlotExec(t, envelopeLogs(runnerapi.Result{ID: "c1"}))
	bad := cmd(nil, nil)
	bad.Run = lastv1alpha1.CommandRun{} // neither argv nor script
	if _, err := ex.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: bad}); err == nil {
		t.Fatal("invalid command was accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ex.Run(ctx, sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)}); err != nil {
		t.Fatalf("the slot leaked after a validation failure: %v", err)
	}
}

func TestRunProbesReadProvidersOnlyAfterTheSlotIsAcquired(t *testing.T) {
	ex, sched, lookups := singleSlotExec(t, envelopeLogs(runnerapi.Result{ID: "p"}))
	release, err := sched.Acquire(context.Background(), sandbox.PriorityExecution)
	if err != nil {
		t.Fatal(err)
	}
	p := probeOf("p", "true")
	p.Command.Providers = []string{demoProvider}
	done := make(chan error, 1)
	go func() {
		_, err := ex.RunProbes(context.Background(), sandbox.ProbeRunRequest{Actor: testActor, Probes: []lastv1alpha1.Probe{p}})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for sched.Waiting(sandbox.PriorityProbe) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the probe group never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if n := lookups.Load(); n != 0 {
		t.Fatalf("%d provider lookups happened while the probe group was queued", n)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

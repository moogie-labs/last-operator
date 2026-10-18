//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

// uniqueInstall is an install whose spec hash is fresh for this run.
func uniqueInstall(tag string) [][]string {
	return [][]string{{"python3", "-c",
		`import os; p='/usr/local/bin/last-e2e-tool'; open(p,'w').write('#!/bin/sh\necho last-e2e-tool-ok\n'); os.chmod(p,0o755)`,
		fmt.Sprintf("%s-%x", tag, time.Now().UnixNano())}}
}

// buildHash returns the spec hash of the image built for intent (namespace/name).
func buildHash(t *testing.T, intent string) string {
	t.Helper()
	for _, e := range auditEntries(t, intent) {
		if e.Type == audit.TypeImageBuildStart {
			if h, _ := payloadOf(e)["specHash"].(string); h != "" {
				return h
			}
		}
	}
	t.Fatalf("%s has no image.build.start entry", intent)
	return ""
}

// TestImageGCReclaimsUnusedImages: with the retention shrunk, the daily (here one
// minute) collection frees the image no Intent references and keeps the one that is.
func TestImageGCReclaimsUnusedImages(t *testing.T) {
	ctx := context.Background()
	installA, installB := uniqueInstall("gc-a"), uniqueInstall("gc-b")
	scA := newImageScenario(t, "e2e-gca-", installA, "a")
	scB := newImageScenario(t, "e2e-gcb-", installB, "b")
	// B's model is the active one (created last); A's is switched in for its request.
	if out, err := scB.request("b"); err != nil {
		t.Fatalf("kubectl last (b): %v\n%s", err, out)
	}
	pointModelAt(t, scA)
	if out, err := scA.request("a"); err != nil {
		t.Fatalf("kubectl last (a): %v\n%s", err, out)
	}
	for _, p := range []struct {
		sc   *imageScenario
		name string
	}{{scA, "a"}, {scB, "b"}} {
		if in := p.sc.intent(p.name); in.Status.Phase != lastv1alpha1.PhaseReady {
			t.Fatalf("intent %s phase = %s, want Ready", p.name, in.Status.Phase)
		}
	}
	hashA, hashB := buildHash(t, scA.ns+"/a"), buildHash(t, scB.ns+"/b")
	if hashA == hashB {
		t.Fatalf("A and B share spec hash %s", hashA)
	}

	// Release A: its Intent and, through ownership, its ManagedResources go away.
	inA := scA.intent("a")
	if err := scA.crc.Delete(ctx, &inA); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Minute, "A's Intent and ManagedResources to be gone", func() (bool, string) {
		var mrs lastv1alpha1.ManagedResourceList
		if err := scA.crc.List(ctx, &mrs, client.InNamespace(scA.ns)); err != nil {
			return false, err.Error()
		}
		err := scA.crc.Get(ctx, client.ObjectKeyFromObject(&inA), &lastv1alpha1.Intent{})
		return len(mrs.Items) == 0 && apierrors.IsNotFound(err), fmt.Sprintf("%d ManagedResources, intent get: %v", len(mrs.Items), err)
	})

	// Shrink the retention, restoring it in cleanup.
	var marker uint64
	for _, e := range auditEntries(t, "") {
		marker = max(marker, e.Seq)
	}
	restoreRetention := setRetention(t, scA.crc, &metav1.Duration{Duration: time.Second})

	var result *audit.Entry
	var deleted map[string]bool
	waitFor(t, 4*time.Minute, "an image.gc.result that deleted an image", func() (bool, string) {
		deleted = map[string]bool{}
		result = nil
		n := 0
		for _, e := range auditEntries(t, "") {
			if e.Seq <= marker {
				continue
			}
			p := payloadOf(e)
			switch e.Type {
			case audit.TypeImageDeleted:
				if h, _ := p["specHash"].(string); h != "" {
					deleted[h] = true
				}
			case audit.TypeImageGCResult:
				n++
				if d, _ := p["deleted"].(float64); d >= 1 {
					r := e
					result = &r
				}
			}
		}
		return result != nil, fmt.Sprintf("%d gc results since seq %d, deleted %v", n, marker, deleted)
	})
	p := payloadOf(*result)
	if freed, _ := p["bytesFreed"].(float64); freed <= 0 {
		t.Errorf("image.gc.result = %v, want bytesFreed > 0", p)
	}
	t.Logf("image.gc.result: %v", p)
	if !deleted[hashA] {
		t.Errorf("no image.deleted names A's spec hash %s (deleted: %v)", hashA, deleted)
	}
	if deleted[hashB] {
		t.Errorf("image.deleted names B's spec hash %s, which its Intent still references", hashB)
	}
	for _, e := range auditEntries(t, "") {
		if e.Seq > marker && e.Type == audit.TypeImageDeleted && e.Intent != "" {
			t.Errorf("image.deleted seq %d carries Intent %s, want none", e.Seq, e.Intent)
		}
	}

	// B is still Ready and its probes still pass after the GC.
	gcAt, err := time.Parse(audit.TimeFormat, result.TS)
	if err != nil {
		t.Fatal(err)
	}
	if in := scB.intent("b"); in.Status.Phase != lastv1alpha1.PhaseReady {
		t.Errorf("B phase = %s after GC, want Ready", in.Status.Phase)
	}
	waitFor(t, 4*time.Minute, "B's probe to pass after the GC", func() (bool, string) {
		var mrs lastv1alpha1.ManagedResourceList
		if err := scB.crc.List(ctx, &mrs, client.InNamespace(scB.ns)); err != nil || len(mrs.Items) == 0 {
			return false, fmt.Sprintf("list: %v (%d items)", err, len(mrs.Items))
		}
		mr := mrs.Items[0]
		ok := mr.Status.LastProbeTime != nil && mr.Status.LastProbeTime.After(gcAt) && meta.IsStatusConditionTrue(mr.Status.Conditions, "InSync")
		return ok, fmt.Sprintf("lastProbe %v, conditions %v", mr.Status.LastProbeTime, mr.Status.Conditions)
	})

	// Restore the retention first, so a GC tick cannot collect the fresh image.
	restoreRetention()

	// A new Intent with A's install rebuilds A's image.
	scC := newImageScenario(t, "e2e-gcc-", installA, "again")
	if out, err := scC.request("again"); err != nil {
		t.Fatalf("kubectl last (again): %v\n%s", err, out)
	}
	if in := scC.intent("again"); in.Status.Phase != lastv1alpha1.PhaseReady {
		t.Fatalf("again phase = %s, want Ready", in.Status.Phase)
	}
	if got := buildHash(t, scC.ns+"/again"); got != hashA {
		t.Errorf("rebuilt spec hash = %s, want A's %s", got, hashA)
	}
	verifyAuditChain(t)
}

// setRetention sets images.unusedRetention and restores the previous value in cleanup.
func setRetention(t *testing.T, crc client.Client, d *metav1.Duration) (restore func()) {
	t.Helper()
	ctx := context.Background()
	update := func(f func(*lastv1alpha1.OperatorConfig)) error {
		var err error
		for range 5 {
			var op lastv1alpha1.OperatorConfig
			if err = crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
				return err
			}
			f(&op)
			if err = crc.Update(ctx, &op); err == nil {
				return nil
			}
		}
		return err
	}
	var prev *metav1.Duration
	if err := update(func(op *lastv1alpha1.OperatorConfig) {
		if op.Spec.Images != nil {
			prev = op.Spec.Images.UnusedRetention
		} else {
			op.Spec.Images = &lastv1alpha1.ImagesConfig{}
		}
		op.Spec.Images.UnusedRetention = d
	}); err != nil {
		t.Fatal(err)
	}
	restore = func() {
		if err := update(func(op *lastv1alpha1.OperatorConfig) {
			if op.Spec.Images != nil {
				op.Spec.Images.UnusedRetention = prev
			}
		}); err != nil {
			t.Errorf("restore unusedRetention: %v", err)
		}
	}
	t.Cleanup(restore) // the safety net; restoring twice is harmless
	return restore
}

// pointModelAt makes sc's scripted model the OperatorConfig's LLM provider.
func pointModelAt(t *testing.T, sc *imageScenario) {
	t.Helper()
	ctx := context.Background()
	var op lastv1alpha1.OperatorConfig
	if err := sc.crc.Get(ctx, client.ObjectKey{Name: "default"}, &op); err != nil {
		t.Fatal(err)
	}
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: sc.ns}
	if err := sc.crc.Update(ctx, &op); err != nil {
		t.Fatal(err)
	}
}

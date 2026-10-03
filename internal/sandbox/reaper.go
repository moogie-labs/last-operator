package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type reapedRef struct {
	RunID  string `json:"runId"`
	Reason string `json:"reason"`
}

// ReapOrphans deletes sandbox runs whose objects were created more than olderThan
// before now: leftovers of a gateway that stopped mid-run. Credentials and egress
// grants must not outlive their run (spec §11.1).
//
// A run is identified by its newest object: if any object of the run has a
// creationTimestamp after the cutoff (now - olderThan), the run is not reaped.
// With olderThan > 0, the age is compared to API-server timestamps; callers
// should choose olderThan well above the maximum run lifetime plus skew.
// With olderThan <= 0, all objects are considered old, skipping the age check.
func (e *Executor) ReapOrphans(ctx context.Context, olderThan time.Duration, now time.Time) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}

	// Group objects by run ID, tracking the newest timestamp per run.
	// A run is reaped only if ALL its objects are old (at or before cutoff).
	runs := make(map[string]time.Time) // run ID -> newest object timestamp
	record := func(meta metav1.ObjectMeta) {
		if id := meta.Labels[LabelRun]; id != "" {
			ts := meta.CreationTimestamp.Time
			if newest, exists := runs[id]; !exists || ts.After(newest) {
				runs[id] = ts
			}
		}
	}

	pods, err := e.kube.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list pods: %w", err)
	}
	for _, p := range pods.Items {
		record(p.ObjectMeta)
	}

	secrets, err := e.kube.CoreV1().Secrets(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		record(s.ObjectMeta)
	}

	nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list network policies: %w", err)
	}
	for _, np := range nps.Items {
		record(np.ObjectMeta)
	}

	// Determine which runs are old enough to reap.
	var idsToReap []string
	if olderThan <= 0 {
		// olderThan <= 0 means reap everything; skip age check.
		idsToReap = make([]string, 0, len(runs))
		for id := range runs {
			idsToReap = append(idsToReap, id)
		}
	} else {
		cutoff := now.Add(-olderThan)
		idsToReap = make([]string, 0, len(runs))
		for id, newest := range runs {
			if !newest.After(cutoff) {
				idsToReap = append(idsToReap, id)
			}
		}
	}

	slices.Sort(idsToReap)
	var errs []error
	reaped := 0
	for _, id := range idsToReap {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}

		onRevoked := func(ctx context.Context) error {
			_, err := e.auditor.Append(ctx, audit.Draft{Type: audit.TypeEgressRevoked, Actor: GatewayActor,
				Payload: reapedRef{RunID: id, Reason: "reaped"}})
			return classifyAudit(err)
		}
		if err := e.cleanup(ctx, id, onRevoked); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: reap run %s: %w", id, err))
		} else {
			reaped++
		}
	}
	return reaped, errors.Join(errs...)
}

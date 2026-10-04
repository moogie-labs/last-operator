package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// ReapOrphans deletes sandbox runs that outlived their deadline: leftovers of a
// gateway that stopped mid-run. Credentials and egress grants must not outlive their
// run (spec §11.1).
//
// Every object of a run carries LabelExpiresAt (launch start + activeDeadline +
// StartupGrace), so a live run is never mistaken for an orphan, however long its
// commands run. A run is reaped when the latest expiry among its objects, plus margin,
// is before now. An object without a parsable expiry counts from its creationTimestamp
// instead, so the newest such object keeps a run alive. margin covers clock skew
// between the gateway and the API server plus the time cleanup takes; 5m is recommended.
// With margin <= 0 every run is reaped (leader start: no run of a previous gateway
// is still supervised).
func (e *Executor) ReapOrphans(ctx context.Context, margin time.Duration, now time.Time) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}

	// Group objects by run ID, tracking the latest point at which any object of the
	// run is still due: its expiry, or its creation time when it has no valid expiry.
	runs := make(map[string]time.Time)
	record := func(meta metav1.ObjectMeta) {
		id := meta.Labels[LabelRun]
		if id == "" {
			return
		}
		due := meta.CreationTimestamp.Time
		if secs, err := strconv.ParseInt(meta.Labels[LabelExpiresAt], 10, 64); err == nil {
			due = time.Unix(secs, 0)
		}
		if latest, exists := runs[id]; !exists || due.After(latest) {
			runs[id] = due
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

	if pns := e.cfg.ProviderNamespace; pns != "" && pns != ns {
		anchors, err := e.kube.CoreV1().Secrets(pns).List(ctx, opts)
		if err != nil {
			return 0, fmt.Errorf("sandbox: list anchors: %w", err)
		}
		for _, s := range anchors.Items {
			record(s.ObjectMeta)
		}
	}

	nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list network policies: %w", err)
	}
	for _, np := range nps.Items {
		record(np.ObjectMeta)
	}

	idsToReap := make([]string, 0, len(runs))
	for id, due := range runs {
		if margin <= 0 || due.Add(margin).Before(now) {
			idsToReap = append(idsToReap, id)
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
				Payload: egressRevoked{RunID: id, Reason: "reaped"}})
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

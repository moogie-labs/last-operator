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

const gatewayActor = "gateway"

type reapedRef struct {
	RunID  string `json:"runId"`
	Reason string `json:"reason"`
}

// ReapOrphans deletes sandbox runs whose objects were created more than olderThan
// before now: leftovers of a gateway that stopped mid-run. Credentials and egress
// grants must not outlive their run (spec §11.1).
func (e *Executor) ReapOrphans(ctx context.Context, olderThan time.Duration, now time.Time) (int, error) {
	ns := e.cfg.Pod.Namespace
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue}
	cutoff := now.Add(-olderThan)
	runs := map[string]bool{}
	note := func(meta metav1.ObjectMeta) {
		if id := meta.Labels[LabelRun]; id != "" && !meta.CreationTimestamp.After(cutoff) {
			runs[id] = true
		}
	}
	pods, err := e.kube.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list pods: %w", err)
	}
	for _, p := range pods.Items {
		note(p.ObjectMeta)
	}
	secrets, err := e.kube.CoreV1().Secrets(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		note(s.ObjectMeta)
	}
	nps, err := e.kube.NetworkingV1().NetworkPolicies(ns).List(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list network policies: %w", err)
	}
	for _, np := range nps.Items {
		note(np.ObjectMeta)
	}

	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var errs []error
	for _, id := range ids {
		onRevoked := func(ctx context.Context) error {
			_, err := e.auditor.Append(ctx, audit.Draft{Type: audit.TypeEgressRevoked, Actor: gatewayActor,
				Payload: reapedRef{RunID: id, Reason: "reaped"}})
			return classifyAudit(err)
		}
		errs = append(errs, e.cleanup(ctx, id, onRevoked))
	}
	return len(ids), errors.Join(errs...)
}

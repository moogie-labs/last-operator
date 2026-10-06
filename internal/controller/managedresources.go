package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// managedResourceName names the ManagedResource for resource i of an Intent's plan.
func managedResourceName(intent string, i int) string { return fmt.Sprintf("%s-%d", intent, i) }

// ensureManagedResources creates or updates one ManagedResource per plan
// resource. Each is controlled by the Intent and names it in spec.intentRef:
// the gateway requires both before it writes outputs (spec §15). A new plan
// voids what earlier probe runs observed, so each one's probe status is cleared.
func (r *IntentReconciler) ensureManagedResources(ctx context.Context, in *lastv1alpha1.Intent) error {
	for i, res := range in.Status.Plan.Resources {
		mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: managedResourceName(in.Name, i), Namespace: in.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, mr, func() error {
			if owner := metav1.GetControllerOf(mr); owner != nil && owner.UID != in.UID {
				return fmt.Errorf("managed resource %s is controlled by another object", mr.Name)
			}
			mr.Spec = *res.DeepCopy()
			mr.Spec.IntentRef = &lastv1alpha1.LocalRef{Name: in.Name}
			return controllerutil.SetControllerReference(in, mr, r.Scheme)
		}); err != nil {
			return err
		}
		if err := r.clearProbeStatus(ctx, client.ObjectKeyFromObject(mr)); err != nil {
			return err
		}
	}
	return nil
}

// clearProbeStatus removes an earlier plan's probe outcomes from a
// ManagedResource through the status subresource. It decides from an uncached
// read: CreateOrUpdate may have returned the informer cache's copy.
func (r *IntentReconciler) clearProbeStatus(ctx context.Context, key client.ObjectKey) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var mr lastv1alpha1.ManagedResource
		if err := r.Reader.Get(ctx, key, &mr); err != nil {
			return err
		}
		if mr.Status.Observed == nil && mr.Status.LastProbeTime == nil &&
			meta.FindStatusCondition(mr.Status.Conditions, CondInSync) == nil {
			return nil
		}
		mr.Status.Observed = nil
		mr.Status.LastProbeTime = nil
		meta.RemoveStatusCondition(&mr.Status.Conditions, CondInSync)
		return r.Status().Update(ctx, &mr)
	})
}

// planManagedResources returns the ManagedResources of the Intent's current
// plan that the Intent controls, read through c. A ManagedResource that an
// earlier plan left behind is not among them: its status says nothing about
// this plan. One that does not exist yet is skipped.
func planManagedResources(ctx context.Context, c client.Reader, in *lastv1alpha1.Intent) ([]lastv1alpha1.ManagedResource, error) {
	if in.Status.Plan == nil {
		return nil, nil
	}
	var mrs []lastv1alpha1.ManagedResource
	for i := range in.Status.Plan.Resources {
		var mr lastv1alpha1.ManagedResource
		err := c.Get(ctx, client.ObjectKey{Namespace: in.Namespace, Name: managedResourceName(in.Name, i)}, &mr)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, err
		case metav1.IsControlledBy(&mr, in):
			mrs = append(mrs, mr)
		}
	}
	return mrs, nil
}

// observedProbe is one probe's outcome in ManagedResource.status.observed.
// Probe output stays in the audit log (spec §10.2); status carries the verdict.
type observedProbe struct {
	Passed   bool   `json:"passed"`
	ExitCode int32  `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// applyProbeOutcomes records a probe run on the ManagedResources of the
// Intent's plan, read uncached through rd and written through c. Each gets the
// outcomes of its own probes, matched by probe ID, and an InSync condition. A
// ManagedResource with no probe in the run is left alone.
func applyProbeOutcomes(ctx context.Context, c client.Client, rd client.Reader, in *lastv1alpha1.Intent,
	outcomes []*gatewayv1.ProbeOutcome, at time.Time) error {
	byID := make(map[string]*gatewayv1.ProbeOutcome, len(outcomes))
	for _, o := range outcomes {
		byID[o.GetId()] = o
	}
	mrs, err := planManagedResources(ctx, rd, in)
	if err != nil {
		return err
	}
	for i := range mrs {
		observed := map[string]observedProbe{}
		var failed []string
		for _, p := range mrs[i].Spec.Probes {
			o, ok := byID[p.ID]
			if !ok {
				continue
			}
			observed[p.ID] = observedProbe{Passed: o.GetPassed(), ExitCode: o.GetResult().GetExitCode(), Error: clip(o.GetError())}
			if !o.GetPassed() {
				failed = append(failed, p.ID)
			}
		}
		if len(observed) == 0 {
			continue
		}
		raw, err := json.Marshal(map[string]any{"probes": observed})
		if err != nil {
			return err
		}
		key := client.ObjectKeyFromObject(&mrs[i])
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var mr lastv1alpha1.ManagedResource
			if err := rd.Get(ctx, key, &mr); err != nil {
				return err
			}
			mr.Status.Observed = &runtime.RawExtension{Raw: raw}
			mr.Status.LastProbeTime = &metav1.Time{Time: at}
			if len(failed) == 0 {
				setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, metav1.ConditionTrue, ReasonProbesPassed, "Every probe passed")
			} else {
				setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, metav1.ConditionFalse, ReasonProbesFailed,
					"Probes failed: "+strings.Join(failed, ", "))
			}
			return c.Status().Update(ctx, &mr)
		}); err != nil {
			return err
		}
	}
	return nil
}

// failedProbes returns the IDs of the probes that did not pass.
func failedProbes(outcomes []*gatewayv1.ProbeOutcome) []string {
	var failed []string
	for _, o := range outcomes {
		if !o.GetPassed() {
			failed = append(failed, o.GetId())
		}
	}
	return failed
}

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
// the gateway requires both before it writes outputs (spec §15).
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
	}
	return nil
}

// ownedManagedResources lists the ManagedResources the Intent controls.
func ownedManagedResources(ctx context.Context, c client.Reader, in *lastv1alpha1.Intent) ([]lastv1alpha1.ManagedResource, error) {
	var list lastv1alpha1.ManagedResourceList
	if err := c.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return nil, err
	}
	var owned []lastv1alpha1.ManagedResource
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], in) {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

// observedProbe is one probe's outcome in ManagedResource.status.observed.
// Probe output stays in the audit log (spec §10.2); status carries the verdict.
type observedProbe struct {
	Passed   bool   `json:"passed"`
	ExitCode int32  `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// applyProbeOutcomes records a probe run on the Intent's ManagedResources.
// Each gets the outcomes of its own probes, matched by probe ID, and an InSync
// condition. A ManagedResource with no probe in the run is left alone.
func applyProbeOutcomes(ctx context.Context, c client.Client, in *lastv1alpha1.Intent, outcomes []*gatewayv1.ProbeOutcome, at time.Time) error {
	byID := make(map[string]*gatewayv1.ProbeOutcome, len(outcomes))
	for _, o := range outcomes {
		byID[o.GetId()] = o
	}
	mrs, err := ownedManagedResources(ctx, c, in)
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
			if err := c.Get(ctx, key, &mr); err != nil {
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

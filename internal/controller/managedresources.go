package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
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

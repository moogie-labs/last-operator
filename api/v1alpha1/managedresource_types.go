package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ManagedResourceSpec describes one provisioned thing. It is also used as a
// template inside Plan.resources, where IntentRef is omitted.
type ManagedResourceSpec struct {
	// +optional
	IntentRef *LocalRef `json:"intentRef,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
	// Identity is free-form: how humans and the agent find this thing again.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Identity *runtime.RawExtension `json:"identity,omitempty"`
	// Desired is the approved desired-state snapshot.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Desired *runtime.RawExtension `json:"desired,omitempty"`
	// +optional
	Probes []Probe `json:"probes,omitempty"`
	// +optional
	OutputsSecretRef *LocalRef `json:"outputsSecretRef,omitempty"`
	// Outputs maps output Secret keys to step references such as
	// "${steps.s1.json.password}". The gateway writes exactly these values to
	// outputsSecretRef (spec §15).
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
	// +optional
	Teardown []Command `json:"teardown,omitempty"`
}

// ManagedResourceName names the ManagedResource for resource i of an Intent's
// plan. Names are positional: the operator creates them, and after an Update the
// gateway deletes the ones the Intent controls beyond the new plan's resources.
func ManagedResourceName(intent string, i int) string { return fmt.Sprintf("%s-%d", intent, i) }

// ManagedResourceStatus holds the latest probe observations.
type ManagedResourceStatus struct {
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Observed *runtime.RawExtension `json:"observed,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`
// +kubebuilder:printcolumn:name="InSync",type=string,JSONPath=`.status.conditions[?(@.type=="InSync")].status`

// ManagedResource is one thing an Intent provisioned.
type ManagedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ManagedResourceSpec `json:"spec"`
	// +optional
	Status ManagedResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedResourceList contains a list of ManagedResource.
type ManagedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ManagedResource{}, &ManagedResourceList{})
		return nil
	})
}

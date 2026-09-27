package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EgressPolicy bounds sandbox egress.
type EgressPolicy struct {
	// Allowed lists CIDR:port or host:port patterns (host names need Cilium).
	// +optional
	Allowed []string `json:"allowed,omitempty"`
}

// ProbePolicy sets drift-check cadence (spec §10.1).
type ProbePolicy struct {
	// Steady-state interval (the adaptive ceiling).
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// Interval used right after a change or drift.
	// +optional
	FastInterval *metav1.Duration `json:"fastInterval,omitempty"`
}

// IntentPolicySpec can only tighten OperatorConfig.defaults (spec §6).
type IntentPolicySpec struct {
	// +optional
	Modes *StageModes `json:"modes,omitempty"`
	// +optional
	AllowAutoModes *StageFlags `json:"allowAutoModes,omitempty"`
	// +optional
	AllowScripts *bool `json:"allowScripts,omitempty"`
	// +optional
	Providers []string `json:"providers,omitempty"`
	// +optional
	Egress *EgressPolicy `json:"egress,omitempty"`
	// +optional
	Probes *ProbePolicy `json:"probes,omitempty"`
}

// +kubebuilder:object:root=true

// IntentPolicy is a namespace's policy for Intents.
type IntentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true

// IntentPolicyList contains a list of IntentPolicy.
type IntentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IntentPolicy{}, &IntentPolicyList{})
		return nil
	})
}

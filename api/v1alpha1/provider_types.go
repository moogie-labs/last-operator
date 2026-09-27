package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProviderType selects how credentials are materialized in a sandbox.
// +kubebuilder:validation:Enum=kubernetes;env-secret
type ProviderType string

const (
	ProviderKubernetes ProviderType = "kubernetes"
	ProviderEnvSecret  ProviderType = "env-secret"
)

// KubernetesProvider mints short-lived tokens for a ServiceAccount in last-gateway.
type KubernetesProvider struct {
	ServiceAccountRef LocalRef `json:"serviceAccountRef"`
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`
}

// ProviderSpec describes one set of platform credentials (spec §13).
// +kubebuilder:validation:XValidation:rule="self.type != 'kubernetes' || has(self.kubernetes)",message="kubernetes is required when type is kubernetes"
// +kubebuilder:validation:XValidation:rule="self.type != 'env-secret' || has(self.secretRef)",message="secretRef is required when type is env-secret"
type ProviderSpec struct {
	Type ProviderType `json:"type"`
	// +optional
	Description string `json:"description,omitempty"`
	// +optional
	Kubernetes *KubernetesProvider `json:"kubernetes,omitempty"`
	// SecretRef names a Secret in last-gateway.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// Env maps sandbox env var name -> Secret key.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// Files maps Secret key -> file path in the sandbox.
	// +optional
	Files map[string]string `json:"files,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`

// Provider is a set of platform credentials held only by the gateway.
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ProviderList contains a list of Provider.
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Provider{}, &ProviderList{})
		return nil
	})
}

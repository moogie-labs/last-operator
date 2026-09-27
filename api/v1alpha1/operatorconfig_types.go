package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SandboxConfig configures sandbox pods.
type SandboxConfig struct {
	// BaseImage must be pinned by digest.
	// +kubebuilder:validation:Pattern=`@sha256:[0-9a-f]{64}$`
	BaseImage string `json:"baseImage"`
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}

// RegistryMode selects the bundled or an external image registry.
// +kubebuilder:validation:Enum=bundled;external
type RegistryMode string

const (
	RegistryBundled  RegistryMode = "bundled"
	RegistryExternal RegistryMode = "external"
)

// ExternalRegistry points at a customer-managed registry.
type ExternalRegistry struct {
	// +kubebuilder:validation:MinLength=1
	Repository    string   `json:"repository"`
	PushSecretRef LocalRef `json:"pushSecretRef"`
	PullSecretRef LocalRef `json:"pullSecretRef"`
}

// RegistryConfig configures where built images live (spec §11.3).
// +kubebuilder:validation:XValidation:rule="self.mode != 'external' || has(self.external)",message="external is required when mode is external"
type RegistryConfig struct {
	// +kubebuilder:default=bundled
	// +optional
	Mode RegistryMode `json:"mode,omitempty"`
	// +optional
	External *ExternalRegistry `json:"external,omitempty"`
}

// ImagesConfig configures automatic image builds.
type ImagesConfig struct {
	// +optional
	BuildEgress []string `json:"buildEgress,omitempty"`
	// +optional
	UnusedRetention *metav1.Duration `json:"unusedRetention,omitempty"`
}

// ProbeLimits bounds probe intervals cluster-wide.
type ProbeLimits struct {
	// +optional
	MinInterval *metav1.Duration `json:"minInterval,omitempty"`
}

// SuggestionsConfig tunes rule suggestions (spec §9.5).
type SuggestionsConfig struct {
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	RepeatThreshold int32 `json:"repeatThreshold,omitempty"`
	// +optional
	ReviewInterval *metav1.Duration `json:"reviewInterval,omitempty"`
}

// OperatorConfigSpec holds cluster defaults.
type OperatorConfigSpec struct {
	// +optional
	LLMProviderRef *LocalRef `json:"llmProviderRef,omitempty"`
	// +optional
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`
	// +optional
	Registry *RegistryConfig `json:"registry,omitempty"`
	// +optional
	Images *ImagesConfig `json:"images,omitempty"`
	// +optional
	Probes *ProbeLimits `json:"probes,omitempty"`
	// +optional
	Suggestions *SuggestionsConfig `json:"suggestions,omitempty"`
	// +optional
	Defaults *IntentPolicySpec `json:"defaults,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="OperatorConfig must be named 'default'"

// OperatorConfig is the cluster-wide singleton configuration.
type OperatorConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec OperatorConfigSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// OperatorConfigList contains a list of OperatorConfig.
type OperatorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OperatorConfig{}, &OperatorConfigList{})
		return nil
	})
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LLMProviderType selects a gateway LLM adapter.
// +kubebuilder:validation:Enum=anthropic;openai;openai-compatible;scripted
type LLMProviderType string

const (
	LLMAnthropic        LLMProviderType = "anthropic"
	LLMOpenAI           LLMProviderType = "openai"
	LLMOpenAICompatible LLMProviderType = "openai-compatible"
	// LLMScripted replays fixed responses from a ConfigMap; for tests and demos (spec §18).
	LLMScripted LLMProviderType = "scripted"
)

// ScriptedLLM points at a ConfigMap in last-gateway. Each key is an Intent
// ("<namespace>.<name>") and each value a JSON array of response strings; the
// n-th response answers a conversation that already holds n assistant messages.
type ScriptedLLM struct {
	ConfigMapRef LocalRef `json:"configMapRef"`
}

// LLMProviderSpec configures one model endpoint.
// +kubebuilder:validation:XValidation:rule="self.type != 'openai-compatible' || has(self.endpoint)",message="endpoint is required for openai-compatible"
// +kubebuilder:validation:XValidation:rule="self.type == 'scripted' || has(self.secretRef)",message="secretRef is required unless type is scripted"
// +kubebuilder:validation:XValidation:rule="self.type != 'scripted' || has(self.scripted)",message="scripted is required when type is scripted"
type LLMProviderSpec struct {
	Type LLMProviderType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// SecretRef names a Secret in last-gateway holding the API key.
	// +optional
	SecretRef *LocalRef `json:"secretRef,omitempty"`
	// +optional
	Scripted *ScriptedLLM `json:"scripted,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`

// LLMProvider is a model endpoint the gateway can call.
type LLMProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LLMProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// LLMProviderList contains a list of LLMProvider.
type LLMProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &LLMProvider{}, &LLMProviderList{})
		return nil
	})
}

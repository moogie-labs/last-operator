package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ArgvPattern matches a command's resolved argv position by position (spec §9.2):
// a literal, a glob within one argument (*, ?, [...]), "re:<RE2>" anchored to one
// argument, or "..." (last position only) for any remaining arguments.
// +kubebuilder:validation:XValidation:rule="size(self.argv) == 0 || (!self.argv[0].contains('*') && !self.argv[0].contains('?') && !self.argv[0].contains('[') && !self.argv[0].startsWith('re:') && self.argv[0] != '...')",message="argv[0] must be a literal"
// +kubebuilder:validation:XValidation:rule="!self.argv.exists(a, a == '...') || (self.argv[size(self.argv) - 1] == '...' && size(self.argv.filter(a, a == '...')) == 1)",message="'...' is only allowed in the last position"
type ArgvPattern struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=1024
	Argv []string `json:"argv"`
}

// RuleOrigin records how a rule was created.
// +kubebuilder:validation:Enum=approval;suggestion;manual
type RuleOrigin string

const (
	RuleOriginApproval   RuleOrigin = "approval"
	RuleOriginSuggestion RuleOrigin = "suggestion"
	RuleOriginManual     RuleOrigin = "manual"
)

// RuleSource is provenance set by the CLI.
type RuleSource struct {
	CreatedFrom RuleOrigin `json:"createdFrom"`
	// +optional
	SuggestionRef *LocalRef `json:"suggestionRef,omitempty"`
}

// CommandRuleSpec lets matching commands skip approval (spec §9).
type CommandRuleSpec struct {
	// +optional
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Stages []Stage `json:"stages"`
	// A command's providers must be a subset of this list.
	// +kubebuilder:validation:MinItems=1
	Providers []string    `json:"providers"`
	Match     ArgvPattern `json:"match"`
	// Every image.install command of a matching command must match one of these.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Install []ArgvPattern `json:"install,omitempty"`
	// A command's declared egress must be a subset of this list.
	// +optional
	Egress []string `json:"egress,omitempty"`
	// +optional
	Source *RuleSource `json:"source,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Stages",type=string,JSONPath=`.spec.stages`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.match.argv`

// CommandRule is a namespace-scoped allow rule.
type CommandRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommandRuleSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// CommandRuleList contains a list of CommandRule.
type CommandRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CommandRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CommandRule{}, &CommandRuleList{})
		return nil
	})
}

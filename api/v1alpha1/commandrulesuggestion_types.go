package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RuleScope is which kind of rule a suggestion would create.
// +kubebuilder:validation:Enum=Namespace;Cluster
type RuleScope string

const (
	RuleScopeNamespace RuleScope = "Namespace"
	RuleScopeCluster   RuleScope = "Cluster"
)

// SuggestionTrigger is why the operator made a suggestion (spec §9.5).
// +kubebuilder:validation:Enum=PlanTime;RepeatedApproval;PeriodicReview
type SuggestionTrigger string

const (
	TriggerPlanTime         SuggestionTrigger = "PlanTime"
	TriggerRepeatedApproval SuggestionTrigger = "RepeatedApproval"
	TriggerPeriodicReview   SuggestionTrigger = "PeriodicReview"
)

// SuggestionDecision is a human's verdict on a suggestion.
// +kubebuilder:validation:Enum=Pending;Accepted;Denied
type SuggestionDecision string

const (
	SuggestionPending  SuggestionDecision = "Pending"
	SuggestionAccepted SuggestionDecision = "Accepted"
	SuggestionDenied   SuggestionDecision = "Denied"
)

// CommandRuleSuggestionSpec is an operator-proposed allow rule awaiting a human decision.
// +kubebuilder:validation:XValidation:rule="self.decision == 'Pending' || has(self.decidedBy)",message="decidedBy is required once decided"
// +kubebuilder:validation:XValidation:rule="oldSelf.decision == 'Pending' || (self.decision == oldSelf.decision && has(self.decidedBy) == has(oldSelf.decidedBy) && (!has(self.decidedBy) || self.decidedBy == oldSelf.decidedBy) && has(self.decisionReason) == has(oldSelf.decisionReason) && (!has(self.decisionReason) || self.decisionReason == oldSelf.decisionReason))",message="decision cannot change once decided; decidedBy and decisionReason are frozen too"
// +kubebuilder:validation:XValidation:rule="has(self.rationale) == has(oldSelf.rationale) && has(self.examples) == has(oldSelf.examples) && has(self.breadthWarnings) == has(oldSelf.breadthWarnings)",message="a suggestion is immutable apart from its decision"
type CommandRuleSuggestionSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	TargetScope RuleScope `json:"targetScope"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	Trigger SuggestionTrigger `json:"trigger"`
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	Rationale string `json:"rationale,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	Proposed CommandRuleSpec `json:"proposed"`
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	Examples []Command `json:"examples,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a suggestion is immutable apart from its decision"
	BreadthWarnings []string `json:"breadthWarnings,omitempty"`
	// +kubebuilder:default=Pending
	// +optional
	Decision SuggestionDecision `json:"decision,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecisionReason string `json:"decisionReason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.proposed.match.argv`

// CommandRuleSuggestion is an allow rule the operator proposes; only humans decide it.
type CommandRuleSuggestion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommandRuleSuggestionSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// CommandRuleSuggestionList contains a list of CommandRuleSuggestion.
type CommandRuleSuggestionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CommandRuleSuggestion `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CommandRuleSuggestion{}, &CommandRuleSuggestionList{})
		return nil
	})
}

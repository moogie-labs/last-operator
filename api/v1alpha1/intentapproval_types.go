package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Decision is an approver's verdict.
// +kubebuilder:validation:Enum=Approve;Deny
type Decision string

const (
	DecisionApprove Decision = "Approve"
	DecisionDeny    Decision = "Deny"
)

// IntentApprovalSpec binds a human decision to exactly one approval subject.
type IntentApprovalSpec struct {
	IntentRef LocalRef `json:"intentRef"`
	Stage     Stage    `json:"stage"`
	// SubjectHash is the plan hash, or the pending command hash for Discovery.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SubjectHash string   `json:"subjectHash"`
	Decision    Decision `json:"decision"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// ApprovedBy must equal the creating user; enforced by admission policy in M2.
	// +kubebuilder:validation:MinLength=1
	ApprovedBy string `json:"approvedBy"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="By",type=string,JSONPath=`.spec.approvedBy`

// IntentApproval is an immutable approval or denial of one plan or discovery command.
type IntentApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec IntentApprovalSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// IntentApprovalList contains a list of IntentApproval.
type IntentApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IntentApproval{}, &IntentApprovalList{})
		return nil
	})
}

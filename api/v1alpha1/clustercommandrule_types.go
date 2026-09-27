package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Stages",type=string,JSONPath=`.spec.stages`
// +kubebuilder:printcolumn:name="Match",type=string,JSONPath=`.spec.match.argv`

// ClusterCommandRule is a cluster-wide allow rule.
type ClusterCommandRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CommandRuleSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ClusterCommandRuleList contains a list of ClusterCommandRule.
type ClusterCommandRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCommandRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterCommandRule{}, &ClusterCommandRuleList{})
		return nil
	})
}

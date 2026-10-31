package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Phase is where an Intent is in its lifecycle (spec §7).
// +kubebuilder:validation:Enum=Pending;Clarifying;Planning;AwaitingDiscoveryApproval;AwaitingApproval;Executing;Verifying;Ready;Drifted;Remediating;TearingDown;Failed
type Phase string

const (
	PhasePending                   Phase = "Pending"
	PhaseClarifying                Phase = "Clarifying"
	PhasePlanning                  Phase = "Planning"
	PhaseAwaitingDiscoveryApproval Phase = "AwaitingDiscoveryApproval"
	PhaseAwaitingApproval          Phase = "AwaitingApproval"
	PhaseExecuting                 Phase = "Executing"
	PhaseVerifying                 Phase = "Verifying"
	PhaseReady                     Phase = "Ready"
	PhaseDrifted                   Phase = "Drifted"
	PhaseRemediating               Phase = "Remediating"
	PhaseTearingDown               Phase = "TearingDown"
	PhaseFailed                    Phase = "Failed"
)

const (
	// FinalizerTeardown holds an Intent until its teardown succeeds, or until it
	// is clear that nothing ever executed.
	FinalizerTeardown = "last.moogie-labs.com/teardown"
	// AnnotationRetry retries a Failed Intent, in any stage, whenever its value
	// changes; status.observedRetry records the value the operator acted on.
	AnnotationRetry = "last.moogie-labs.com/retry"
)

// Answer answers one of status.questions.
type Answer struct {
	// +kubebuilder:validation:MinLength=1
	ID    string `json:"id"`
	Value string `json:"value"`
}

// IntentSpec is a natural-language request plus the requester's inputs.
type IntentSpec struct {
	// +kubebuilder:validation:MinLength=1
	Request string `json:"request"`
	// +optional
	Answers []Answer `json:"answers,omitempty"`
	// Modes may only be as strict as, or stricter than, the effective policy.
	// +optional
	Modes *StageModes `json:"modes,omitempty"`
}

// Question is a clarifying question from the agent.
type Question struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// +optional
	Options []string `json:"options,omitempty"`
}

// Generalization is an LLM-proposed wildcard pattern for an allow rule (advice only).
type Generalization struct {
	Argv []string `json:"argv"`
	// +optional
	Lint []string `json:"lint,omitempty"`
}

// PendingCommand is a discovery command awaiting approval.
type PendingCommand struct {
	Hash    string  `json:"hash"`
	Command Command `json:"command"`
	// Nonce makes this request its own approval subject: Hash is the SHA-256 of
	// the canonical JSON of {command, nonce}, so an approval is single-use.
	// +optional
	Nonce string `json:"nonce,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Generalization *Generalization `json:"generalization,omitempty"`
}

// Plan is the agent's reviewable plan (spec §8.2).
type Plan struct {
	Hash string `json:"hash"`
	// +optional
	Summary string    `json:"summary,omitempty"`
	Steps   []Command `json:"steps"`
	// +optional
	Resources []ManagedResourceSpec `json:"resources,omitempty"`
	// +optional
	UncoveredSteps []string `json:"uncoveredSteps,omitempty"`
	// +optional
	Generalizations map[string]Generalization `json:"generalizations,omitempty"`
	// Nonce makes a Remediate plan its own approval subject: it is part of the
	// hashed body, so the same repair for a recurring drift needs a new
	// approval. Only Remediate plans carry one (32 lowercase hex characters).
	// +optional
	Nonce string `json:"nonce,omitempty"`
}

// IntentStatus is written by the operator.
type IntentStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Stage Stage `json:"stage,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedRequestHash is the SHA-256 of spec.request for the current planning
	// session; answers to clarifying questions do not start a new session.
	// +optional
	ObservedRequestHash string `json:"observedRequestHash,omitempty"`
	// PlanningSessionSeq is the audit sequence number of the current planning
	// session's first llm.request; conversation reload reads from there.
	// +optional
	PlanningSessionSeq int64 `json:"planningSessionSeq,omitempty"`
	// +optional
	Questions []Question `json:"questions,omitempty"`
	// +optional
	PendingCommand *PendingCommand `json:"pendingCommand,omitempty"`
	// +optional
	Plan *Plan `json:"plan,omitempty"`
	// +optional
	SuggestedRules []string `json:"suggestedRules,omitempty"`
	// ObservedRetry is the last.moogie-labs.com/retry annotation's value when the
	// current planning session started; a different value retries a Failed Intent.
	// +optional
	ObservedRetry string `json:"observedRetry,omitempty"`
	// Remediations counts the consecutive remediations drift started. At 3,
	// the next drift fails the Intent with RemediationFailed instead of
	// planning. A scheduled probe run in which every probe passes, after the
	// Intent last became Ready, resets it to 0.
	// +optional
	Remediations int32 `json:"remediations,omitempty"`
	// RemediationTimes holds the start times, in RFC 3339, of the remediations
	// started in the last 24 hours, drift-started and retried alike; older ones
	// are dropped. With 6 in the window, the next remediation fails the Intent
	// with RemediationFailed instead of starting. A spec edit clears it; a
	// passing scheduled probe run does not. A drift that clears before
	// anything is remediated gives back the entry its start added.
	// +optional
	RemediationTimes []string `json:"remediationTimes,omitempty"`
	// EffectiveModes is the Auto or Manual mode each stage key resolved to across
	// the policy layers and spec.modes.
	// +optional
	EffectiveModes *StageModes `json:"effectiveModes,omitempty"`
	// Replans counts the consecutive replans a failed step started. At 3, the
	// next failed step fails the Intent with PlanningFailed instead of planning.
	// A spec edit, a retry and reaching Ready reset it to 0.
	// +optional
	Replans int32 `json:"replans,omitempty"`
	// Retries counts the retries of a Failed Intent, by the retry annotation or
	// automatically in an Auto stage; it sets the backoff before the next
	// automatic one (1m, doubled per retry, at most 1h). Reaching Ready and a
	// spec edit reset it to 0.
	// +optional
	Retries int32 `json:"retries,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Intent is a natural-language provisioning request.
type Intent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentSpec `json:"spec"`
	// +optional
	Status IntentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IntentList contains a list of Intent.
type IntentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Intent `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Intent{}, &IntentList{})
		return nil
	})
}

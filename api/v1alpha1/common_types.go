package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode selects whether a lifecycle stage needs human approval.
// +kubebuilder:validation:Enum=Auto;Manual
type Mode string

const (
	ModeAuto   Mode = "Auto"
	ModeManual Mode = "Manual"
)

// Stage identifies what an approval subject or allow rule applies to.
// +kubebuilder:validation:Enum=Discovery;Create;Update;Remediate;Delete
type Stage string

const (
	StageDiscovery Stage = "Discovery"
	StageCreate    Stage = "Create"
	StageUpdate    Stage = "Update"
	StageRemediate Stage = "Remediate"
	StageDelete    Stage = "Delete"
)

// StageModes sets Auto/Manual per lifecycle stage. Create covers the Create and Update stages.
type StageModes struct {
	// +optional
	Discovery Mode `json:"discovery,omitempty"`
	// +optional
	Create Mode `json:"create,omitempty"`
	// +optional
	Reconcile Mode `json:"reconcile,omitempty"`
	// +optional
	Delete Mode `json:"delete,omitempty"`
}

// StageFlags sets a boolean per lifecycle stage.
type StageFlags struct {
	// +optional
	Discovery bool `json:"discovery,omitempty"`
	// +optional
	Create bool `json:"create,omitempty"`
	// +optional
	Reconcile bool `json:"reconcile,omitempty"`
	// +optional
	Delete bool `json:"delete,omitempty"`
}

// ArgvCommand is a command executed directly, without a shell.
type ArgvCommand struct {
	// +kubebuilder:validation:MinItems=1
	Argv []string `json:"argv"`
}

// CommandRun is exactly one of argv (exec'd directly) or script (run with /bin/sh -c).
// +kubebuilder:validation:XValidation:rule="has(self.argv) != has(self.script)",message="exactly one of argv or script must be set"
type CommandRun struct {
	// +optional
	// +kubebuilder:validation:MinItems=1
	Argv []string `json:"argv,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	Script string `json:"script,omitempty"`
}

// ImageSpec lists install commands baked into the command's image at build time (spec §11.2).
type ImageSpec struct {
	// +kubebuilder:validation:MinItems=1
	Install []ArgvCommand `json:"install"`
}

// Command is the unit of action: one sandboxed execution (spec §8.1).
type Command struct {
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	Reason string     `json:"reason,omitempty"`
	Run    CommandRun `json:"run"`
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
	// Files are written under /work before the command runs, keyed by absolute path.
	// +optional
	Files map[string]string `json:"files,omitempty"`
	// Providers names the Provider credentials to inject.
	// +optional
	Providers []string `json:"providers,omitempty"`
	// Egress lists host:port or CIDR:port destinations beyond what providers imply.
	// +optional
	Egress []string `json:"egress,omitempty"`
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// Probe is a command plus a CEL assertion over its result (spec §10).
type Probe struct {
	// +kubebuilder:validation:MinLength=1
	ID      string  `json:"id"`
	Command Command `json:"command"`
	// +kubebuilder:validation:MinLength=1
	Assert string `json:"assert"`
}

// LocalRef names an object in the same namespace (or a cluster-scoped object).
type LocalRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

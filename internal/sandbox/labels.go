// Package sandbox runs commands in locked-down pods on behalf of the gateway (spec §11.1).
package sandbox

import (
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "last-gateway"
	LabelRun       = "last.moogie-labs.com/run"
	// LabelExpiresAt holds the unix time (decimal seconds) after which a run is
	// orphaned: when launch started + the pod's activeDeadline + StartupGrace.
	LabelExpiresAt = "last.moogie-labs.com/expires-at"
	LabelName      = "app.kubernetes.io/name"
	NameValue      = "last-sandbox"
	GatewayActor   = "gateway"
)

// RunLabels are carried by every object a run creates, so cleanup and the reaper can find them.
func RunLabels(runID string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedByValue, LabelRun: runID}
}

// stampExpiry marks an object of a run with the time after which the run is orphaned.
func stampExpiry(meta *metav1.ObjectMeta, expires time.Time) {
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[LabelExpiresAt] = strconv.FormatInt(expires.Unix(), 10)
}

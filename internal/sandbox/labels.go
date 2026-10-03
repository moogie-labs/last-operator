// Package sandbox runs commands in locked-down pods on behalf of the gateway (spec §11.1).
package sandbox

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "last-gateway"
	LabelRun       = "last.moogie-labs.com/run"
	LabelName      = "app.kubernetes.io/name"
	NameValue      = "last-sandbox"
	GatewayActor   = "gateway"
)

// RunLabels are carried by every object a run creates, so cleanup and the reaper can find them.
func RunLabels(runID string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedByValue, LabelRun: runID}
}

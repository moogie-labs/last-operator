package v1alpha1

import "time"

// DefaultApprovalMaxAge is how long an IntentApproval counts when OperatorConfig sets no approvals.maxAge.
const DefaultApprovalMaxAge = 24 * time.Hour

// ApprovalMaxAge returns OperatorConfig's approvals.maxAge, or the default. op may be nil.
func ApprovalMaxAge(op *OperatorConfig) time.Duration {
	if op != nil && op.Spec.Approvals != nil && op.Spec.Approvals.MaxAge != nil {
		return op.Spec.Approvals.MaxAge.Duration
	}
	return DefaultApprovalMaxAge
}

// ApprovalApplies reports whether a decision (Approve or Deny) counts for the
// Intent: it names the Intent's UID and is no older than maxAge at now. The
// gateway and the operator share it so they agree on which decisions exist.
func ApprovalApplies(a *IntentApproval, intent *Intent, now time.Time, maxAge time.Duration) bool {
	return a.Spec.IntentUID == string(intent.UID) && !a.CreationTimestamp.Time.Before(now.Add(-maxAge))
}

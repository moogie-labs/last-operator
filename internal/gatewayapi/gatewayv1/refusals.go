package gatewayv1

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MsgTornDown is the gateway's refusal of any phase call for an Intent whose
// Delete phase already succeeded: BeginPhase, and RunCommand or EndPhase with
// no open phase. The operator reads it as the teardown having succeeded, for
// instance when the answer to the EndPhase that ended it was lost.
const MsgTornDown = "intent was already torn down; no phase opens for it again"

// TornDown reports whether err is the gateway's MsgTornDown refusal.
func TornDown(err error) bool {
	s, ok := status.FromError(err)
	return ok && err != nil && s.Code() == codes.FailedPrecondition && s.Message() == MsgTornDown
}

// MsgNeedsApproval is BeginPhase's refusal of a phase whose stage is Auto but
// whose plan is outside Auto's limits (design D9): a Remediate step the executed
// plan does not have, or a Delete that is not a permutation of the executed
// plan's teardown. The operator reads it as a cue to wait for an IntentApproval.
const MsgNeedsApproval = "needs approval: the plan is outside Auto mode's limits, so only an IntentApproval can authorize it"

// NeedsApproval reports whether err is the gateway's MsgNeedsApproval refusal.
func NeedsApproval(err error) bool {
	s, ok := status.FromError(err)
	return ok && err != nil && s.Code() == codes.PermissionDenied && s.Message() == MsgNeedsApproval
}

// MsgDeniedPrefix starts every refusal an IntentApproval's Deny caused: a
// BeginPhase refused for a denied subject, a step whose phase a Deny stopped,
// in an Auto or an approval phase, and a probe run of a denied executed plan.
// The operator reads it as the plan having been denied, which never retries by
// itself (design D10), however the stage's mode stands.
const MsgDeniedPrefix = "denied: "

// Denied reports whether err is a refusal a Deny caused (MsgDeniedPrefix).
func Denied(err error) bool {
	s, ok := status.FromError(err)
	return ok && err != nil && s.Code() == codes.PermissionDenied && strings.HasPrefix(s.Message(), MsgDeniedPrefix)
}

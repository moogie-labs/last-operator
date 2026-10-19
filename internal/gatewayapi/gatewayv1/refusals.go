package gatewayv1

import (
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

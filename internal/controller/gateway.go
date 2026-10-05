package controller

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// terminal reports gateway errors that retrying cannot fix. Everything else
// (Unavailable, DeadlineExceeded, Aborted, Unauthenticated, …) is retried.
func terminal(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.PermissionDenied, codes.InvalidArgument,
		codes.NotFound, codes.AlreadyExists, codes.Unimplemented:
		return true
	}
	return false
}

// describe returns an error's message without the gRPC prefix.
func describe(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}

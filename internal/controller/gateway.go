package controller

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
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

func intentRef(in *lastv1alpha1.Intent) *gatewayv1.IntentRef {
	return &gatewayv1.IntentRef{Namespace: in.Namespace, Name: in.Name}
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

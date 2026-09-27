package audit

import (
	"context"
	"errors"
	"fmt"
)

// Do is the fail-closed primitive (spec §14.1): it records before, runs action only
// if that record is durable, then records the outcome built by after.
func (w *Writer) Do(ctx context.Context, before Draft, action func(context.Context) (any, error), after func(result any, actionErr error) Draft) error {
	if _, err := w.Append(ctx, before); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	result, actionErr := action(ctx)
	if _, err := w.Append(ctx, after(result, actionErr)); err != nil {
		return errors.Join(actionErr, fmt.Errorf("audit: outcome not recorded: %w", err))
	}
	return actionErr
}

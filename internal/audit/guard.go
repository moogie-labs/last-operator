package audit

import (
	"context"
	"errors"
	"fmt"
)

// Do is the fail-closed primitive (spec §14.1): it records before, runs action only
// if that record is durable, then records the outcome built by after, which
// receives the stored before-entry so the outcome can reference its Seq and Hash.
func (w *Writer) Do(ctx context.Context, before Draft, action func(context.Context) (any, error), after func(before Entry, result any, actionErr error) Draft) error {
	beforeEntry, err := w.Append(ctx, before)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	result, actionErr := action(ctx)
	switch _, err := w.Append(ctx, after(beforeEntry, result, actionErr)); {
	case errors.Is(err, ErrCheckpointFailed):
		return errors.Join(actionErr, fmt.Errorf("audit: outcome recorded but checkpoint failed: %w", err))
	case err != nil:
		return errors.Join(actionErr, fmt.Errorf("audit: outcome not recorded: %w", err))
	}
	return actionErr
}

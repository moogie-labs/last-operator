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
	_, err := w.DoEntry(ctx, before, action, after)
	return err
}

// DoEntry is Do, also returning the stored outcome entry, for callers that must
// point at it later (the image index keeps the image.build.result seq). The entry
// is the zero Entry when the outcome was not recorded; it is set when the outcome
// was recorded but the checkpoint after it failed.
func (w *Writer) DoEntry(ctx context.Context, before Draft, action func(context.Context) (any, error), after func(before Entry, result any, actionErr error) Draft) (Entry, error) {
	beforeEntry, err := w.Append(ctx, before)
	if err != nil {
		return Entry{}, errors.Join(ErrAuditUnavailable, err)
	}
	result, actionErr := action(ctx)
	outcome, err := w.Append(ctx, after(beforeEntry, result, actionErr))
	switch {
	case errors.Is(err, ErrCheckpointFailed):
		return outcome, errors.Join(actionErr, fmt.Errorf("audit: outcome recorded but checkpoint failed: %w", err))
	case err != nil:
		return Entry{}, errors.Join(actionErr, fmt.Errorf("audit: outcome not recorded: %w", err))
	}
	return outcome, actionErr
}

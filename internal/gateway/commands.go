package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// priorityFor derives the sandbox queue priority from the phase (spec §10.1); the
// operator never chooses it.
func priorityFor(stage lastv1alpha1.Stage) sandbox.Priority {
	if stage == lastv1alpha1.StageDiscovery {
		return sandbox.PriorityDiscovery
	}
	return sandbox.PriorityExecution
}

func toProtoResult(runID string, r runnerapi.Result) *gatewayv1.CommandResult {
	return &gatewayv1.CommandResult{RunId: runID, ExitCode: int32(r.ExitCode), Stdout: r.Stdout, Stderr: r.Stderr, //nolint:gosec // exit codes fit
		StdoutTruncated: r.StdoutTruncated, StderrTruncated: r.StderrTruncated, Error: r.Error}
}

// sameCommand compares by canonical JSON: map order and nil-versus-empty do not
// matter; every other difference does.
func sameCommand(a, b lastv1alpha1.Command) bool {
	ca, errA := audit.Canonical(a)
	cb, errB := audit.Canonical(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

func decodeCommand(raw []byte) (lastv1alpha1.Command, error) {
	var c lastv1alpha1.Command
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return lastv1alpha1.Command{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return lastv1alpha1.Command{}, errors.New("trailing data after the command")
	}
	return c, nil
}

func (g *Gateway) runCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	subject := req.GetSubjectHash()
	if !subjectPattern.MatchString(subject) {
		return nil, status.Error(codes.InvalidArgument, "subject_hash must be sha256:<64 lowercase hex digits>")
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, subject: subject, rpc: "RunCommand"}
	p := g.phases.get(key)
	if p == nil || p.subject != subject {
		return nil, d.deny(codes.FailedPrecondition, "no open phase for this plan")
	}
	d.stage = string(p.stage)
	if !p.mu.TryLock() {
		return nil, status.Error(codes.Aborted, "a step of this phase is already running")
	}
	defer p.mu.Unlock()
	if p.closed {
		return nil, d.deny(codes.FailedPrecondition, "phase is closed")
	}
	if p.failed {
		return nil, d.deny(codes.FailedPrecondition, "an earlier step failed or could not run; end the phase")
	}
	if p.next >= len(p.body.Steps) {
		return nil, d.deny(codes.FailedPrecondition, "every step of this phase has run")
	}
	planned := p.body.Steps[p.next]
	if req.GetStepId() != planned.ID {
		return nil, d.deny(codes.FailedPrecondition, "step %q is next, not %q", planned.ID, req.GetStepId())
	}
	given, err := decodeCommand(req.GetCommandJson())
	if err != nil {
		return nil, d.deny(codes.InvalidArgument, "command_json: %v", err)
	}
	expected, err := plan.Resolve(planned, p.results)
	if err != nil {
		p.failed = true
		return nil, d.deny(codes.FailedPrecondition, "step %s: %v", planned.ID, err)
	}
	if !sameCommand(given, expected) {
		return nil, d.deny(codes.PermissionDenied, "command does not match approved step %s", planned.ID)
	}
	// Approval and policy are re-checked before every step (spec §8.3): a Deny or a
	// narrowed policy after BeginPhase stops the phase.
	eff, err := g.effectivePolicy(ctx, ns)
	if invalid, ok := errors.AsType[errInvalidPolicy](err); ok {
		p.failed = true
		return nil, d.deny(codes.FailedPrecondition, "%v", invalid)
	}
	if err != nil {
		return nil, err
	}
	if err := eff.CheckCommand(expected); err != nil {
		p.failed = true
		return nil, d.deny(codes.PermissionDenied, "policy: %v", err)
	}
	approval, err := g.findApprovalFor(ctx, ns, name, p.stage, p.subject)
	switch {
	case errors.Is(err, errApprovalDenied):
		p.failed = true
		return nil, d.deny(codes.PermissionDenied, "approval withdrawn: %s denies this %s", approval.Name, subjectNoun(p.stage))
	case err != nil:
		return nil, err
	case approval == nil:
		p.failed = true
		return nil, d.deny(codes.PermissionDenied, "approval withdrawn: no IntentApproval approves this %s", subjectNoun(p.stage))
	}
	var install []lastv1alpha1.ArgvCommand
	if expected.Image != nil {
		install = expected.Image.Install
	}
	// The runner always comes from the base, whatever image the command runs on (spec §11.1).
	base, err := g.sandboxImage(ctx)
	image := ""
	if err == nil {
		image, err = g.resolveImageOn(ctx, w, base, imageRequest{Intent: key, Stage: string(p.stage), Subject: p.subject,
			Actor: actor, Install: install, Policy: eff})
	}
	if failed, ok := errors.AsType[errImageBuildFailed](err); ok {
		p.failed = true
		return &gatewayv1.RunCommandResponse{Result: &gatewayv1.CommandResult{ExitCode: -1, Error: failed.Error(),
			Stderr: failed.LogTail}}, nil
	}
	if err != nil {
		out, refused := d.imageError(err)
		p.failed = refused // anything else (Unavailable, context) can be retried
		return nil, out
	}
	ex, err := g.runExecutor(ctx, w, base, image)
	if err != nil {
		out, refused := d.imageError(err)
		p.failed = refused
		return nil, out
	}
	res, err := ex.Run(ctx, sandbox.RunRequest{Actor: actor, Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth, Command: expected, Priority: priorityFor(p.stage)})
	if err != nil {
		// The outcome may not be recorded: withhold any output, and stop the phase.
		p.failed = true
		if errors.Is(err, audit.ErrAuditUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "step %s: %v", planned.ID, err)
		}
		if errors.Is(err, sandbox.ErrForbiddenProvider) {
			return nil, d.deny(codes.PermissionDenied, "step %s: %v", planned.ID, err)
		}
		return nil, status.Errorf(codes.Internal, "step %s: %v", planned.ID, err)
	}
	p.results[planned.ID] = res.Result
	if res.Result.ExitCode == 0 && res.Result.Error == "" {
		p.next++
	} else {
		p.failed = true
	}
	return &gatewayv1.RunCommandResponse{Result: toProtoResult(res.RunID, res.Result)}, nil
}

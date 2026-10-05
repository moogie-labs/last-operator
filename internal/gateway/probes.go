package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func (g *Gateway) runProbes(ctx context.Context, req *gatewayv1.RunProbesRequest) (*gatewayv1.RunProbesResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	if _, busy := g.probing.LoadOrStore(key, struct{}{}); busy {
		return nil, status.Error(codes.Aborted, "a probe run for this intent is already in flight")
	}
	defer g.probing.Delete(key)

	var existing lastv1alpha1.Intent // read only to answer NotFound for a missing Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &existing); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, rpc: "RunProbes"}
	if rec.Executed == nil {
		return nil, d.deny(codes.FailedPrecondition, "intent has no executed plan to probe")
	}
	d.subject = rec.Executed.PlanHash
	// The record is canonical plan JSON: it must still hash to the approved subject.
	sum := sha256.Sum256([]byte(rec.Executed.Plan))
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != rec.Executed.PlanHash {
		return nil, d.deny(codes.Internal, "executed plan record failed integrity check: it hashes to %s", got)
	}
	var body plan.Body
	dec := json.NewDecoder(strings.NewReader(rec.Executed.Plan))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return nil, d.deny(codes.Internal, "executed plan record failed integrity check: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, d.deny(codes.Internal, "executed plan record failed integrity check: trailing data after the plan")
	}
	// The approval must still stand: a Deny after execution stops the probes too.
	approval, err := g.findApproval(ctx, ns, name, lastv1alpha1.Stage(rec.Executed.Stage), rec.Executed.PlanHash)
	switch {
	case errors.Is(err, errApprovalDenied):
		return nil, d.deny(codes.PermissionDenied, "approval %s denies this plan", approval.Name)
	case err != nil:
		return nil, err
	case approval == nil:
		return nil, d.deny(codes.PermissionDenied, "no IntentApproval approves this plan")
	}
	var probes []lastv1alpha1.Probe
	for _, r := range body.Resources {
		probes = append(probes, r.Probes...)
	}
	resp := &gatewayv1.RunProbesResponse{PlanHash: rec.Executed.PlanHash}
	if len(probes) == 0 {
		return resp, nil
	}
	eff, err := g.effectivePolicy(ctx, ns)
	if invalid, ok := errors.AsType[errInvalidPolicy](err); ok {
		return nil, d.deny(codes.FailedPrecondition, "%v", invalid)
	}
	if err != nil {
		return nil, err
	}
	for _, p := range probes {
		if err := eff.CheckCommand(p.Command); err != nil {
			return nil, d.deny(codes.PermissionDenied, "probe %s: %v", p.ID, err)
		}
	}
	image, err := g.sandboxImage(ctx)
	if err != nil {
		return nil, err
	}
	outs, err := g.executor(w, image).RunProbes(ctx, sandbox.ProbeRunRequest{Actor: actor, Intent: key, Probes: probes})
	if err != nil {
		if errors.Is(err, audit.ErrAuditUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "probe run: %v", err)
		}
		if errors.Is(err, sandbox.ErrForbiddenProvider) {
			return nil, d.deny(codes.PermissionDenied, "probe run: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "probe run: %v", err)
	}
	if len(outs) != len(probes) {
		return nil, status.Errorf(codes.Internal, "probe run returned %d outcomes for %d probes", len(outs), len(probes))
	}
	for i, o := range outs {
		if o.ID != probes[i].ID {
			return nil, status.Errorf(codes.Internal, "probe run returned outcome %q where probe %q was expected", o.ID, probes[i].ID)
		}
	}
	for _, o := range outs {
		resp.Outcomes = append(resp.Outcomes, &gatewayv1.ProbeOutcome{Id: o.ID, Passed: o.Passed, Result: toProtoResult("", o.Result), Error: o.Error})
	}
	return resp, nil
}

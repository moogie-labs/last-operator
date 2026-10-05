package gateway

import (
	"context"
	"encoding/json"
	"errors"

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

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
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
	var body plan.Body
	if err := json.Unmarshal([]byte(rec.Executed.Plan), &body); err != nil {
		return nil, status.Errorf(codes.Internal, "executed plan record is corrupt: %v", err)
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
		return nil, status.Errorf(codes.Internal, "probe run: %v", err)
	}
	for _, o := range outs {
		resp.Outcomes = append(resp.Outcomes, &gatewayv1.ProbeOutcome{Id: o.ID, Passed: o.Passed, Result: toProtoResult("", o.Result), Error: o.Error})
	}
	return resp, nil
}

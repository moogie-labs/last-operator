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
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
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
	refs, buildFailed, err := g.resolveProbeImages(ctx, w, imageRequest{Intent: key, Stage: rec.Executed.Stage,
		Subject: rec.Executed.PlanHash, Actor: actor, Policy: eff}, probes)
	if err != nil {
		out, _ := d.imageError(err)
		return nil, out
	}
	ex, err := g.runExecutor(ctx, w, image, image)
	if err != nil {
		out, _ := d.imageError(err)
		return nil, out
	}
	outs, err := g.runProbeImages(ctx, ex, actor, key, probes, refs, buildFailed)
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

// resolveProbeImages resolves each distinct image spec of probes once, before any probe
// runs. A failed build is recorded per spec; any other error is returned as it is.
func (g *Gateway) resolveProbeImages(ctx context.Context, w *audit.Writer, req imageRequest, probes []lastv1alpha1.Probe) (map[string]string, map[string]errImageBuildFailed, error) {
	refs := map[string]string{}
	buildFailed := map[string]errImageBuildFailed{}
	for _, p := range probes {
		k := sandbox.ImageKey(p.Command)
		_, resolved := refs[k]
		_, failed := buildFailed[k]
		if k == "" || resolved || failed {
			continue
		}
		req.Install = p.Command.Image.Install
		ref, err := g.resolveImage(ctx, w, req)
		if bf, ok := errors.AsType[errImageBuildFailed](err); ok {
			buildFailed[k] = bf
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		refs[k] = ref
	}
	return refs, buildFailed, nil
}

// runProbeImages runs, with ex, the probes whose build did not fail on their images;
// a failed build fails its probes. Outcomes follow probe order.
func (g *Gateway) runProbeImages(ctx context.Context, ex *sandbox.Executor, actor, intent string, probes []lastv1alpha1.Probe,
	refs map[string]string, buildFailed map[string]errImageBuildFailed) ([]sandbox.ProbeOutcome, error) {
	var runnable []lastv1alpha1.Probe
	for _, p := range probes {
		if _, bad := buildFailed[sandbox.ImageKey(p.Command)]; !bad {
			runnable = append(runnable, p)
		}
	}
	var ran []sandbox.ProbeOutcome
	if len(runnable) > 0 {
		var err error
		ran, err = ex.RunProbes(ctx, sandbox.ProbeRunRequest{Actor: actor, Intent: intent, Probes: runnable, Images: refs})
		if err != nil {
			return nil, err
		}
	}
	outs := make([]sandbox.ProbeOutcome, 0, len(probes))
	for _, p := range probes {
		if bf, bad := buildFailed[sandbox.ImageKey(p.Command)]; bad {
			outs = append(outs, sandbox.ProbeOutcome{ID: p.ID, Error: bf.Error(),
				Result: runnerapi.Result{ID: p.ID, ExitCode: -1, Stderr: bf.LogTail}})
		} else if len(ran) > 0 {
			outs, ran = append(outs, ran[0]), ran[1:]
		}
	}
	return outs, nil
}

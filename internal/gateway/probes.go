package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	rec, err := g.state.Load(ctx, key, string(existing.UID))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, rpc: "RunProbes"}
	if rec.Executed == nil {
		return nil, d.deny(codes.FailedPrecondition, "intent has no executed plan to probe")
	}
	d.subject = rec.Executed.PlanHash
	body, err := executedBody(d, rec.Executed)
	if err != nil {
		return nil, err
	}
	// Probes are part of the executed plan: the record that its approved phase
	// succeeded authorizes them, with no standing Approve and no age limit. A Deny
	// of the executed plan for this Intent's UID, created since its phase began,
	// still stops them.
	if deny, err := g.findDeny(ctx, &existing, rec.Executed); err != nil {
		return nil, err
	} else if deny != nil {
		return nil, d.deny(codes.PermissionDenied, "%sapproval %s denies this plan", gatewayv1.MsgDeniedPrefix, deny.Name)
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

// executedBody decodes the record's executed plan (parseExecuted). Every
// refusal is audited through d.
func executedBody(d denial, ex *ExecutedPlan) (plan.Body, error) {
	body, err := parseExecuted(ex)
	if err != nil {
		return plan.Body{}, d.deny(codes.Internal, "%v", err)
	}
	return body, nil
}

// parseExecuted decodes the record's executed plan. The record is canonical plan
// JSON: it must still hash to the approved subject and decode with no unknown
// field or trailing data.
func parseExecuted(ex *ExecutedPlan) (plan.Body, error) {
	sum := sha256.Sum256([]byte(ex.Plan))
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != ex.PlanHash {
		return plan.Body{}, fmt.Errorf("executed plan record failed integrity check: it hashes to %s", got)
	}
	var body plan.Body
	dec := json.NewDecoder(strings.NewReader(ex.Plan))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return plan.Body{}, fmt.Errorf("executed plan record failed integrity check: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return plan.Body{}, errors.New("executed plan record failed integrity check: trailing data after the plan")
	}
	return body, nil
}

// findDeny returns a Deny of the executed plan for the Intent's UID, or nil. A
// Deny created before the plan's phase began is ignored: BeginPhase already
// weighed it, and it had aged out or the phase would not have opened (R6). A
// record without Began, or with one that does not parse, counts every Deny.
func (g *Gateway) findDeny(ctx context.Context, intent *lastv1alpha1.Intent, ex *ExecutedPlan) (*lastv1alpha1.IntentApproval, error) {
	stage, subject := lastv1alpha1.Stage(ex.Stage), ex.PlanHash
	began, err := time.Parse(time.RFC3339, ex.Began)
	if err != nil {
		began = time.Time{}
	}
	var list lastv1alpha1.IntentApprovalList
	if err := g.crd.List(ctx, &list, client.InNamespace(intent.Namespace)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list IntentApprovals: %v", err)
	}
	var found *lastv1alpha1.IntentApproval
	for i := range list.Items {
		a := &list.Items[i]
		if a.Spec.IntentRef.Name != intent.Name || a.Spec.Stage != stage || a.Spec.SubjectHash != subject ||
			a.Spec.Decision != lastv1alpha1.DecisionDeny || a.Spec.IntentUID != string(intent.UID) ||
			a.CreationTimestamp.Time.Before(began) {
			continue
		}
		if found == nil || a.Name < found.Name {
			found = a
		}
	}
	return found, nil
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

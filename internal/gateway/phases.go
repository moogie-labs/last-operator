package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// MaxPlanBytes bounds a plan's canonical JSON; the executed plan is kept in a ConfigMap.
const MaxPlanBytes = 512 << 10

const (
	outcomeSucceeded   = "succeeded"
	outcomeFailed      = "failed"
	outcomeInterrupted = "interrupted"
	outcomeAborted     = "aborted"
)

// phase is an open phase of this gateway process.
//
// Every holder of mu must check closed immediately after acquiring it: a closed
// phase (ended by EndPhase, or interrupted) must never run anything, even when
// a caller still holds a stale pointer to it.
type phase struct {
	mu       sync.Mutex // held while a step or output write runs
	closed   bool       // phase.end is durable; set under mu
	endSeq   uint64     // seq of the phase.end already written; non-zero while the state save is pending
	outcome  string     // outcome recorded by that phase.end
	stage    lastv1alpha1.Stage
	subject  string
	body     plan.Body
	auth     *audit.Authorization
	beginSeq uint64
	next     int          // index of the next step to run; also the count of steps that succeeded
	results  plan.Results // results of steps that ran, by step ID
	failed   bool         // a step failed or could not run; only EndPhase(failed) remains
}

type phaseTable struct {
	mu   sync.Mutex
	open map[string]*phase
}

func (t *phaseTable) get(key string) *phase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open[key]
}

func (t *phaseTable) put(key string, p *phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = map[string]*phase{}
	}
	t.open[key] = p
}

func (t *phaseTable) remove(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, key)
}

type phaseBegin struct {
	Plan  string `json:"plan"` // canonical JSON, kept as text so any number survives the audit writer
	Steps int    `json:"steps"`
	// Nonce is a Discovery phase's pending-command nonce, so an auditor can
	// recompute the subject, sha256(canonical {command, nonce}), from this entry alone.
	Nonce string `json:"nonce,omitempty"`
}

type phaseEnd struct {
	BeginSeq uint64 `json:"beginSeq"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	StepsRun int    `json:"stepsRun"`
}

type approvalObserved struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	ApprovedBy string `json:"approvedBy"`
	Decision   string `json:"decision"`
	Created    string `json:"created"`
}

type policyDenied struct {
	RPC    string `json:"rpc"`
	Reason string `json:"reason"`
}

var subjectPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const maxReasonBytes = 1 << 10

// truncateReason caps s at maxReasonBytes on a UTF-8 boundary.
func truncateReason(s string) string {
	if len(s) <= maxReasonBytes {
		return s
	}
	cut := maxReasonBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "") + "…(truncated)"
}

var errApprovalDenied = errors.New("approval denied")

func intentKey(ref *gatewayv1.IntentRef) (ns, name, key string, err error) {
	if ref.GetNamespace() == "" || ref.GetName() == "" {
		return "", "", "", status.Error(codes.InvalidArgument, "intent namespace and name are required")
	}
	// Both parts end up in an audit key, a ConfigMap name hash, and error text.
	if errs := validation.IsDNS1123Label(ref.GetNamespace()); len(errs) > 0 {
		return "", "", "", status.Errorf(codes.InvalidArgument, "intent namespace %q is not a DNS label", truncateReason(ref.GetNamespace()))
	}
	if errs := validation.IsDNS1123Subdomain(ref.GetName()); len(errs) > 0 {
		return "", "", "", status.Errorf(codes.InvalidArgument, "intent name %q is not a DNS subdomain", truncateReason(ref.GetName()))
	}
	return ref.GetNamespace(), ref.GetName(), ref.GetNamespace() + "/" + ref.GetName(), nil
}

func notFoundOr(err error, what string) error {
	if apierrors.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "%s not found", what)
	}
	return status.Errorf(codes.Unavailable, "read %s: %v", what, err)
}

// denial writes policy.denied before refusing an RPC (spec §8.3).
type denial struct {
	ctx                                context.Context
	w                                  *audit.Writer
	actor, intent, stage, subject, rpc string
}

func (d denial) deny(code codes.Code, format string, args ...any) error {
	reason := truncateReason(fmt.Sprintf(format, args...))
	if _, err := d.w.Append(d.ctx, audit.Draft{Type: audit.TypePolicyDenied, Actor: d.actor, Intent: d.intent,
		Stage: d.stage, SubjectHash: d.subject, Payload: policyDenied{RPC: d.rpc, Reason: reason}}); err != nil {
		return status.Errorf(codes.Unavailable, "record policy.denied: %v", err)
	}
	return status.Error(code, reason)
}

// imageError classifies an image resolve error. A FailedPrecondition or InvalidArgument
// status is a refusal: it is audited as a denial and returned with refused set. Any
// other error (Unavailable, the caller's context) is returned as it is.
func (d denial) imageError(err error) (out error, refused bool) {
	switch c := status.Code(err); c {
	case codes.FailedPrecondition, codes.InvalidArgument:
		return d.deny(c, "image: %s", status.Convert(err).Message()), true
	}
	return err, false
}

// errInvalidPolicy marks a policy that does not resolve; callers audit it as a denial.
type errInvalidPolicy struct{ error }

func (e errInvalidPolicy) Error() string { return "invalid policy: " + e.error.Error() }

// GRPCStatus keeps the code when a caller returns the error unchanged.
func (e errInvalidPolicy) GRPCStatus() *status.Status {
	return status.New(codes.FailedPrecondition, e.Error())
}

func (g *Gateway) effectivePolicy(ctx context.Context, ns string) (policy.Effective, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return policy.Effective{}, err
	}
	var list lastv1alpha1.IntentPolicyList
	if err := g.crd.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return policy.Effective{}, status.Errorf(codes.Unavailable, "list IntentPolicies: %v", err)
	}
	eff, err := policy.Resolve(op, list.Items)
	if err != nil {
		return policy.Effective{}, errInvalidPolicy{err}
	}
	return eff, nil
}

// checkPlanPolicy checks every command the plan can ever run: steps, probes, teardown.
func checkPlanPolicy(eff policy.Effective, b plan.Body) error {
	for _, s := range b.Steps {
		if err := eff.CheckCommand(s); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
	}
	for _, r := range b.Resources {
		for _, p := range r.Probes {
			if err := eff.CheckCommand(p.Command); err != nil {
				return fmt.Errorf("probe %s: %w", p.ID, err)
			}
		}
		for i, c := range r.Teardown {
			if err := eff.CheckCommand(c); err != nil {
				return fmt.Errorf("teardown command %d: %w", i, err)
			}
		}
	}
	return nil
}

// clock is the gateway's time source, the one the image builds use, so tests age approvals.
func (g *Gateway) clock() time.Time {
	if g.builds != nil && g.builds.now != nil {
		return g.builds.now()
	}
	return time.Now()
}

// findApproval returns the approving IntentApproval for the subject. Any matching
// Deny wins over every Approve (errApprovalDenied, with the denying object).
func (g *Gateway) findApproval(ctx context.Context, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) (*lastv1alpha1.IntentApproval, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return nil, err
	}
	maxAge, now := lastv1alpha1.ApprovalMaxAge(op), g.clock()
	var list lastv1alpha1.IntentApprovalList
	if err := g.crd.List(ctx, &list, client.InNamespace(intent.Namespace)); err != nil {
		return nil, status.Errorf(codes.Unavailable, "list IntentApprovals: %v", err)
	}
	var found *lastv1alpha1.IntentApproval
	for i := range list.Items {
		a := &list.Items[i]
		if a.Spec.IntentRef.Name != intent.Name || a.Spec.Stage != stage || a.Spec.SubjectHash != subject {
			continue
		}
		// A foreign-UID or stale decision, Deny included, is ignored.
		if !lastv1alpha1.ApprovalApplies(a, intent, now, maxAge) {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return a, errApprovalDenied
		}
		if a.Spec.Decision != lastv1alpha1.DecisionApprove {
			continue
		}
		if found == nil || a.Name < found.Name {
			found = a
		}
	}
	return found, nil
}

// findApprovalFor is findApproval for a caller that holds only the Intent's key.
func (g *Gateway) findApprovalFor(ctx context.Context, ns, name string, stage lastv1alpha1.Stage, subject string) (*lastv1alpha1.IntentApproval, error) {
	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	return g.findApproval(ctx, &intent, stage, subject)
}

// verifyPlan checks that the Intent's plan is encodable, bounded, valid and the
// one the subject names; every refusal is audited through d.
func verifyPlan(d denial, intent *lastv1alpha1.Intent, subject string) (plan.Body, []byte, error) {
	body := plan.FromStatus(intent.Status.Plan)
	canonical, err := plan.Canonical(body)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "plan cannot be encoded: %v", err)
	}
	if len(canonical) > MaxPlanBytes {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "plan is %d bytes; the limit is %d", len(canonical), MaxPlanBytes)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "%v", err)
	}
	if hash != subject || intent.Status.Plan.Hash != subject {
		return plan.Body{}, nil, d.deny(codes.PermissionDenied, "plan hash %s does not match the subject", hash)
	}
	if err := plan.Validate(body); err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "%v", err)
	}
	return body, canonical, nil
}

// verifyDiscovery checks the Intent's pending discovery command against the
// subject and returns it as a one-step plan. Every refusal is audited through d.
func verifyDiscovery(d denial, intent *lastv1alpha1.Intent, subject string) (plan.Body, []byte, error) {
	pc := intent.Status.PendingCommand
	if pc == nil {
		return plan.Body{}, nil, d.deny(codes.FailedPrecondition, "intent has no pending discovery command")
	}
	if pc.Command.ID != plan.DiscoveryStepID {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "a discovery command's id must be %q", plan.DiscoveryStepID)
	}
	if pc.Nonce == "" {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "the pending discovery command has no nonce")
	}
	hash, size, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command cannot be encoded: %v", err)
	}
	if size > plan.MaxPendingCommandBytes {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command is %d bytes; the limit is %d", size, plan.MaxPendingCommandBytes)
	}
	if hash != subject || pc.Hash != subject {
		return plan.Body{}, nil, d.deny(codes.PermissionDenied, "pending command hash %s does not match the subject", hash)
	}
	body := plan.Body{Steps: []lastv1alpha1.Command{pc.Command}}
	if err := plan.Validate(body); err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "%v", err)
	}
	canonical, err := plan.Canonical(body)
	if err != nil {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "discovery command cannot be encoded: %v", err)
	}
	return body, canonical, nil
}

// verifySubject checks the subject against the Intent for the stage and returns
// the phase body, its canonical JSON and, for Discovery, the pending command's
// nonce (recorded in phase.begin so an auditor can recompute the subject).
func verifySubject(d denial, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) (plan.Body, []byte, string, error) {
	if stage == lastv1alpha1.StageDiscovery {
		body, canonical, err := verifyDiscovery(d, intent, subject)
		if err != nil {
			return plan.Body{}, nil, "", err
		}
		return body, canonical, intent.Status.PendingCommand.Nonce, nil
	}
	if intent.Status.Plan == nil {
		return plan.Body{}, nil, "", d.deny(codes.FailedPrecondition, "intent has no plan")
	}
	body, canonical, err := verifyPlan(d, intent, subject)
	return body, canonical, "", err
}

// recordFits checks that the record still fits its ConfigMap once the plan is
// recorded as Executed, or EndPhase could not save it after the steps ran.
func recordFits(rec *LoadedRecord, stage lastv1alpha1.Stage, subject string, canonical []byte) error {
	prospective := rec.IntentRecord
	prospective.Open = nil
	prospective.Used = append(slices.Clone(rec.Used), subject)
	prospective.Executed = &ExecutedPlan{Stage: string(stage), PlanHash: subject, Plan: string(canonical), EndSeq: math.MaxUint64}
	_, err := encodeRecord(prospective)
	return err
}

// subjectNoun names a phase's approval subject in refusals that the model and
// the user read.
func subjectNoun(stage lastv1alpha1.Stage) string {
	if stage == lastv1alpha1.StageDiscovery {
		return "discovery command"
	}
	return "plan"
}

func (g *Gateway) beginPhase(ctx context.Context, req *gatewayv1.BeginPhaseRequest) (*gatewayv1.BeginPhaseResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	stage, subject := lastv1alpha1.Stage(req.GetStage()), req.GetSubjectHash()
	if !subjectPattern.MatchString(subject) {
		return nil, status.Error(codes.InvalidArgument, "subject_hash must be sha256:<64 lowercase hex digits>")
	}
	unlock := g.locks.lock(key)
	defer unlock()
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, stage: string(stage), subject: subject, rpc: "BeginPhase"}
	if stage != lastv1alpha1.StageCreate && stage != lastv1alpha1.StageDiscovery {
		return nil, d.deny(codes.FailedPrecondition, "stage %q is not supported yet; this gateway opens Create and Discovery phases only", stage)
	}

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	body, canonical, nonce, err := verifySubject(d, &intent, stage, subject)
	if err != nil {
		return nil, err
	}

	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if rec.Open != nil {
		if rec.Open.Epoch == g.epoch && g.phases.get(key) != nil {
			return nil, d.deny(codes.AlreadyExists, "a phase is already open for this intent (subject %s)", rec.Open.SubjectHash)
		}
		if err := g.closeInterrupted(ctx, w, key, rec); err != nil {
			return nil, err
		}
	}
	if rec.WasUsed(subject) {
		if stage == lastv1alpha1.StageDiscovery {
			return nil, d.deny(codes.FailedPrecondition, "this discovery command already ran, and its approval is used up; ask for it again to run it again")
		}
		return nil, d.deny(codes.FailedPrecondition, "this plan already opened a phase; a plan runs only once, so plan a different one")
	}
	if stage == lastv1alpha1.StageCreate {
		if err := recordFits(rec, stage, subject, canonical); err != nil {
			return nil, d.deny(codes.InvalidArgument, "plan too large to record: %v", err)
		}
	}
	eff, err := g.effectivePolicy(ctx, ns)
	if invalid, ok := errors.AsType[errInvalidPolicy](err); ok {
		return nil, d.deny(codes.FailedPrecondition, "%v", invalid)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPlanPolicy(eff, body); err != nil {
		return nil, d.deny(codes.PermissionDenied, "%v", err)
	}
	approval, err := g.findApproval(ctx, &intent, stage, subject)
	switch {
	case errors.Is(err, errApprovalDenied):
		return nil, d.deny(codes.PermissionDenied, "approval %s denies this %s", approval.Name, subjectNoun(stage))
	case err != nil:
		return nil, err
	case approval == nil:
		return nil, d.deny(codes.FailedPrecondition, "no IntentApproval approves this %s", subjectNoun(stage))
	case approval.Spec.ApprovedBy == g.cfg.OperatorIdentity:
		return nil, d.deny(codes.PermissionDenied, "approval %s names the operator as approver", approval.Name)
	case strings.HasPrefix(approval.Spec.ApprovedBy, "system:serviceaccount:"+g.cfg.Namespace+":"):
		return nil, d.deny(codes.PermissionDenied, "approval %s names a gateway-namespace ServiceAccount as approver", approval.Name)
	}

	auth := &audit.Authorization{Kind: "approval", Name: ns + "/" + approval.Name}
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypeApprovalObserved, Actor: actor, Intent: key, Stage: string(stage), SubjectHash: subject,
		Payload: approvalObserved{Name: approval.Name, UID: string(approval.UID), ApprovedBy: approval.Spec.ApprovedBy,
			Decision: string(approval.Spec.Decision), Created: approval.CreationTimestamp.UTC().Format(time.RFC3339)}}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "record approval.observed: %v", err)
	}
	begin, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseBegin, Actor: actor, Intent: key, Stage: string(stage),
		SubjectHash: subject, AuthorizedBy: auth, Payload: phaseBegin{Plan: string(canonical), Steps: len(body.Steps), Nonce: nonce}})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "record phase.begin: %v", err)
	}
	rec.Open = &OpenPhase{Stage: string(stage), SubjectHash: subject, BeginSeq: begin.Seq, Epoch: g.epoch, Auth: auth}
	rec.MarkUsed(stage, subject)
	if err := g.saveRecord(ctx, rec); err != nil {
		if _, aerr := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: actor, Intent: key, Stage: string(stage),
			SubjectHash: subject, AuthorizedBy: auth, Payload: phaseEnd{BeginSeq: begin.Seq, Outcome: outcomeAborted, Reason: "could not record the open phase"}}); aerr != nil {
			slog.Warn("Could not record aborted phase", "err", aerr)
		}
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.phases.put(key, &phase{stage: stage, subject: subject, body: body, auth: auth, beginSeq: begin.Seq, results: plan.Results{}})
	return &gatewayv1.BeginPhaseResponse{BeginSeq: begin.Seq, Steps: int32(len(body.Steps))}, nil //nolint:gosec // bounded by MaxPlanBytes
}

// closeInterrupted ends a phase that an earlier gateway process opened. Its
// subject stays in Used, so the same plan never runs twice without replanning.
func (g *Gateway) closeInterrupted(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord) error {
	open := *rec.Open
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: sandbox.GatewayActor, Intent: key, Stage: open.Stage,
		SubjectHash: open.SubjectHash, AuthorizedBy: open.Auth, Payload: phaseEnd{BeginSeq: open.BeginSeq, Outcome: outcomeInterrupted,
			Reason: "the gateway restarted or lost leadership during the phase"}}); err != nil {
		return status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	if p := g.phases.get(key); p != nil {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	}
	g.phases.remove(key)
	rec.Open = nil
	if err := g.saveRecord(ctx, rec); err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	return nil
}

func (g *Gateway) endPhase(ctx context.Context, req *gatewayv1.EndPhaseRequest) (*gatewayv1.EndPhaseResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	_, _, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	subject := req.GetSubjectHash()
	if !subjectPattern.MatchString(subject) {
		return nil, status.Error(codes.InvalidArgument, "subject_hash must be sha256:<64 lowercase hex digits>")
	}
	unlock := g.locks.lock(key)
	defer unlock()
	actor := CallerFrom(ctx)
	d := denial{ctx: ctx, w: w, actor: actor, intent: key, subject: subject, rpc: "EndPhase"}
	p := g.phases.get(key)
	if p == nil || p.subject != subject {
		return nil, d.deny(codes.FailedPrecondition, "no open phase for this plan")
	}
	d.stage = string(p.stage)
	// Never wait for a running step while holding the Intent's lock: the step can
	// run for minutes, and the lock would stall the leader's index flush and every
	// other RPC for the Intent. Refuse before writing anything, as RunCommand does.
	if !p.mu.TryLock() {
		return nil, status.Error(codes.Aborted, "a step of this phase is still running; retry")
	}
	defer p.mu.Unlock()
	if p.closed && p.endSeq == 0 { // an interrupted phase; a closed phase with endSeq set is a save retry
		return nil, d.deny(codes.FailedPrecondition, "this phase is already closed")
	}
	want := outcomeFailed
	if req.GetSucceeded() {
		want = outcomeSucceeded
	}
	if p.endSeq != 0 {
		// A previous attempt wrote phase.end but could not save the record; only retry the save.
		if p.outcome != want {
			return nil, d.deny(codes.FailedPrecondition, "this phase already ended as %s", p.outcome)
		}
	} else {
		if req.GetSucceeded() && (p.failed || p.next < len(p.body.Steps)) {
			return nil, d.deny(codes.FailedPrecondition, "the phase cannot succeed: %d of %d steps succeeded", p.next, len(p.body.Steps))
		}
	}
	rec, err := g.state.Load(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if p.endSeq == 0 {
		end, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: actor, Intent: key, Stage: string(p.stage),
			SubjectHash: p.subject, AuthorizedBy: p.auth,
			Payload: phaseEnd{BeginSeq: p.beginSeq, Outcome: want, Reason: truncateReason(req.GetReason()), StepsRun: p.next}})
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "record phase.end: %v", err)
		}
		p.endSeq, p.outcome, p.closed = end.Seq, want, true
	}
	rec.Open = nil
	// A Discovery phase ran one inspection command; probes and later stages keep
	// following the last executed Create plan.
	if p.outcome == outcomeSucceeded && p.stage != lastv1alpha1.StageDiscovery {
		canonical, err := plan.Canonical(p.body)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode executed plan: %v", err)
		}
		rec.Executed = &ExecutedPlan{Stage: string(p.stage), PlanHash: p.subject, Plan: string(canonical), EndSeq: p.endSeq}
	}
	if err := g.saveRecord(ctx, rec); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.phases.remove(key)
	return &gatewayv1.EndPhaseResponse{EndSeq: p.endSeq}, nil
}

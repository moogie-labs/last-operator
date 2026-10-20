package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	uid      string // the Intent's UID when the phase opened
	body     plan.Body
	auth     *audit.Authorization
	beginSeq uint64
	began    time.Time    // the gateway clock at BeginPhase; the executed plan's Began
	next     int          // index of the next step to run; also the count of steps that succeeded
	results  plan.Results // results of steps that ran, by step ID
	failed   bool         // a step failed or could not run; only EndPhase(failed) remains
	// stale holds a succeeded Update's deletions of the ManagedResources its plan
	// dropped, by UID, so an EndPhase retry repeats only what is left.
	stale map[types.UID]staleDeletion
}

// staleDeletion is a stale ManagedResource the gateway deleted; audited is false
// while its managedresource.deleted entry is still to be written.
type staleDeletion struct {
	name    string
	audited bool
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

// managedResourceDeleted is the managedresource.deleted payload. It names the
// ManagedResource only; its output Secret's contents never reach the audit log.
type managedResourceDeleted struct {
	ManagedResource string `json:"managedResource"`
	UID             string `json:"uid"`
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

// verifyPlan checks that the Intent's plan is encodable, bounded, the one the
// subject names, and carries a nonce exactly when the stage is Remediate. It
// validates the plan for every other stage; a Remediate plan is validated
// against the executed plan once the record is loaded (checkExecuted). Every
// refusal is audited through d.
func verifyPlan(d denial, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) (plan.Body, []byte, error) {
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
	if stage == lastv1alpha1.StageRemediate {
		if !plan.ValidNonce(body.Nonce) {
			return plan.Body{}, nil, d.deny(codes.InvalidArgument, "a Remediate plan needs a nonce of 32 lowercase hex characters")
		}
		return body, canonical, nil
	}
	if body.Nonce != "" {
		return plan.Body{}, nil, d.deny(codes.InvalidArgument, "only a Remediate plan carries a nonce; this %s plan has one", stage)
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
	body, canonical, err := verifyPlan(d, intent, stage, subject)
	return body, canonical, "", err
}

// recordFits checks that the record still fits its ConfigMap once the plan is
// recorded as Executed, or EndPhase could not save it after the steps ran.
func recordFits(rec *LoadedRecord, stage lastv1alpha1.Stage, subject string, canonical []byte, began time.Time) error {
	prospective := rec.IntentRecord
	prospective.Open = nil
	prospective.Used = append(slices.Clone(rec.Used), subject)
	prospective.Executed = &ExecutedPlan{Stage: string(stage), PlanHash: subject, Plan: string(canonical), EndSeq: math.MaxUint64,
		BeginSeq: math.MaxUint64, Began: formatBegan(began)}
	_, err := encodeRecord(prospective)
	return err
}

// formatBegan is how the executed plan records when its phase began.
func formatBegan(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// replacesExecuted reports whether a succeeded phase of stage becomes the
// executed plan. A Discovery phase ran one inspection command, a Delete phase
// only tears down, and a Remediate phase repairs the executed plan's resources
// without changing them (design D5); probes and later stages keep following
// the last executed Create or Update plan.
func replacesExecuted(stage lastv1alpha1.Stage) bool {
	return stage == lastv1alpha1.StageCreate || stage == lastv1alpha1.StageUpdate
}

// checkExecuted applies the rules Update and Remediate add once the record is
// loaded: both change an executed plan, so one must exist, and a Remediate
// plan must carry its resources unchanged (plan.ValidateRemediate).
func checkExecuted(d denial, rec *LoadedRecord, stage lastv1alpha1.Stage, body plan.Body) error {
	if stage != lastv1alpha1.StageUpdate && stage != lastv1alpha1.StageRemediate {
		return nil
	}
	if rec.Executed == nil {
		return d.deny(codes.FailedPrecondition, "intent has no executed plan to %s", strings.ToLower(string(stage)))
	}
	if stage == lastv1alpha1.StageUpdate {
		return nil
	}
	executed, err := executedBody(d, rec.Executed)
	if err != nil {
		return err
	}
	err = plan.ValidateRemediate(body, executed)
	switch {
	case errors.Is(err, plan.ErrResourcesChanged):
		return d.deny(codes.FailedPrecondition, "a Remediate plan must carry the executed plan's resources unchanged")
	case err != nil:
		return d.deny(codes.InvalidArgument, "%v", err)
	}
	return nil
}

// subjectNoun names a phase's approval subject in refusals that the model and
// the user read.
func subjectNoun(stage lastv1alpha1.Stage) string {
	if stage == lastv1alpha1.StageDiscovery {
		return "discovery command"
	}
	return "plan"
}

// openableStages are the stages this gateway opens phases for.
var openableStages = []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageRemediate,
	lastv1alpha1.StageDelete, lastv1alpha1.StageDiscovery}

// checkDeletePlan applies a Delete phase's own rules: the Intent is being deleted,
// and the plan has no resources, since it only orders the executed plan's teardown.
func checkDeletePlan(d denial, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, body plan.Body) error {
	switch {
	case stage != lastv1alpha1.StageDelete:
		return nil
	case intent.DeletionTimestamp == nil:
		return d.deny(codes.FailedPrecondition, "intent is not being deleted; a Delete phase opens only for an Intent being deleted")
	case len(body.Resources) > 0:
		return d.deny(codes.FailedPrecondition, "a Delete plan has no resources; this one has %d", len(body.Resources))
	}
	return nil
}

// checkReplay refuses a subject that must not run again. A Delete is exempt from
// run-once: a retried or interrupted teardown replans a permutation of the same
// commands, often the same plan, and the UID-bound, unexpired approval of that
// Delete subject authorizes the re-run. A succeeded Delete drops the record and
// its used subjects, so run-once cannot stop a replay there; an Intent being
// deleted opens no Create phase at all.
func checkReplay(d denial, rec *LoadedRecord, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) error {
	if stage != lastv1alpha1.StageDelete && rec.WasUsed(subject) {
		if stage == lastv1alpha1.StageDiscovery {
			return d.deny(codes.FailedPrecondition, "this discovery command already ran, and its approval is used up; ask for it again to run it again")
		}
		return d.deny(codes.FailedPrecondition, "this plan already opened a phase; a plan runs only once, so plan a different one")
	}
	if intent.DeletionTimestamp != nil && stage != lastv1alpha1.StageDelete && stage != lastv1alpha1.StageDiscovery {
		return d.deny(codes.FailedPrecondition, "intent is being deleted; only a Delete or Discovery phase can open")
	}
	return nil
}

// checkTornDown refuses every phase for an Intent whose Delete succeeded: its
// record and used subjects are gone, and opening a phase would recreate them.
// The guard lives in memory, so a gateway restart loses it; the Intent is then
// still terminating, which keeps Create shut, but its Delete and Discovery
// approvals could open once more within their age limit.
func (g *Gateway) checkTornDown(d denial, key string, intent *lastv1alpha1.Intent) error {
	if gone, ok := g.tornDown.Load(key); ok && gone == string(intent.UID) {
		return d.deny(codes.FailedPrecondition, "%s", gatewayv1.MsgTornDown)
	}
	return nil
}

// noOpenPhase refuses a RunCommand or EndPhase that names no open phase. For an
// Intent whose Delete succeeded the refusal says so (gatewayv1.MsgTornDown), so
// an operator that lost the answer to the EndPhase that ended its teardown
// knows the teardown succeeded rather than failing it.
func (g *Gateway) noOpenPhase(ctx context.Context, d denial, ns, name, key string) error {
	if gone, ok := g.tornDown.Load(key); ok {
		var intent lastv1alpha1.Intent
		if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err == nil && gone == string(intent.UID) {
			return d.deny(codes.FailedPrecondition, "%s", gatewayv1.MsgTornDown)
		}
	}
	return d.deny(codes.FailedPrecondition, "no open phase for this plan")
}

// requireApproval returns the IntentApproval that authorizes opening a phase for
// the subject: an Approve, with no Deny, from neither the operator nor a
// gateway-namespace ServiceAccount. Every refusal is audited through d.
func (g *Gateway) requireApproval(ctx context.Context, d denial, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject string) (*lastv1alpha1.IntentApproval, error) {
	approval, err := g.findApproval(ctx, intent, stage, subject)
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
	return approval, nil
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
	if !slices.Contains(openableStages, stage) {
		return nil, d.deny(codes.FailedPrecondition, "stage %q is not supported; this gateway opens Create, Update, Remediate, Delete and Discovery phases only", stage)
	}

	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	body, canonical, nonce, err := verifySubject(d, &intent, stage, subject)
	if err != nil {
		return nil, err
	}
	if err := checkDeletePlan(d, &intent, stage, body); err != nil {
		return nil, err
	}
	if err := g.checkTornDown(d, key, &intent); err != nil {
		return nil, err
	}

	// A record of a previous Intent with this name may hold an open phase; close
	// it before the reset would drop it without a phase.end.
	if err := g.closePredecessorPhase(ctx, w, key, string(intent.UID)); err != nil {
		return nil, err
	}
	rec, err := g.state.Load(ctx, key, string(intent.UID))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if err := checkExecuted(d, rec, stage, body); err != nil {
		return nil, err
	}
	if rec.Open != nil {
		if rec.Open.Epoch == g.epoch && g.phases.get(key) != nil {
			return nil, d.deny(codes.AlreadyExists, "a phase is already open for this intent (subject %s)", rec.Open.SubjectHash)
		}
		if err := g.closeInterrupted(ctx, w, key, rec); err != nil {
			return nil, err
		}
	}
	if err := g.finishEnding(ctx, w, actor, key, ns, name, string(intent.UID)); err != nil {
		return nil, err
	}
	if err := checkReplay(d, rec, &intent, stage, subject); err != nil {
		return nil, err
	}
	began := g.clock()
	if replacesExecuted(stage) {
		if err := recordFits(rec, stage, subject, canonical, began); err != nil {
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
	approval, err := g.requireApproval(ctx, d, &intent, stage, subject)
	if err != nil {
		return nil, err
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
	g.phases.put(key, &phase{stage: stage, subject: subject, uid: string(intent.UID), body: body, auth: auth, beginSeq: begin.Seq,
		began: began, results: plan.Results{}})
	return &gatewayv1.BeginPhaseResponse{BeginSeq: begin.Seq, Steps: int32(len(body.Steps))}, nil //nolint:gosec // bounded by MaxPlanBytes
}

// finishEnding finishes the cleanup of the Intent's succeeded Update before a
// new phase opens. Such an Update is saved as the executed plan without its open
// phase, but stays in memory while stale ManagedResources are left to delete; a
// new phase would replace it and leave them behind. Its operator may have moved
// on (an EndPhase(failed) for the Intent's deletion is refused as already
// succeeded), so BeginPhase does the work rather than wait for an EndPhase retry
// that may never come. A step-holder of the phase answers Aborted and a failed
// deletion Unavailable; the operator retries both. The caller holds the
// Intent's lock, under which endSeq and outcome are written, and has checked
// that the record has no open phase.
func (g *Gateway) finishEnding(ctx context.Context, w *audit.Writer, actor, key, ns, name, uid string) error {
	p := g.phases.get(key)
	if p == nil || p.endSeq == 0 || p.outcome != outcomeSucceeded || p.stage != lastv1alpha1.StageUpdate || p.uid != uid {
		return nil
	}
	if !p.mu.TryLock() {
		return status.Error(codes.Aborted, "the previous phase is still ending; retry")
	}
	defer p.mu.Unlock()
	if err := g.afterSave(ctx, w, actor, key, ns, name, p); err != nil {
		return status.Errorf(codes.Unavailable, "finish the cleanup of the previous Update (subject %s): %v", p.subject, err)
	}
	g.phases.remove(key)
	return nil
}

// closePredecessorPhase ends the open phase of a record that belongs to another
// UID (the Intent was recreated), so the record's reset never drops a phase.begin
// without its phase.end.
func (g *Gateway) closePredecessorPhase(ctx context.Context, w *audit.Writer, key, uid string) error {
	stale, err := g.state.LoadAny(ctx, key)
	if err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	if stale.IntentUID == "" || stale.IntentUID == uid {
		return nil
	}
	if stale.Open == nil {
		return g.flushPredecessorIndex(ctx, key)
	}
	// Never wait for a running step of the predecessor while holding the Intent's
	// lock; refuse before writing anything, as RunCommand and EndPhase do.
	var held *phase
	if p := g.phases.get(key); p != nil {
		if !p.mu.TryLock() {
			return status.Error(codes.Aborted, "a step of the previous Intent's phase is still running; retry")
		}
		held = p
		defer p.mu.Unlock()
	}
	if err := g.closeOpen(ctx, w, key, stale, held); err != nil {
		return err
	}
	return g.flushPredecessorIndex(ctx, key)
}

// flushPredecessorIndex saves pending index entries, which still belong to the
// predecessor, into its record so the reset folds them into the truncation floor.
func (g *Gateway) flushPredecessorIndex(ctx context.Context, key string) error {
	if err := g.saveIndexLocked(ctx, key); err != nil {
		return status.Errorf(codes.Unavailable, "could not save the audit index: %v", err)
	}
	return nil
}

// closeInterrupted ends a phase that an earlier gateway process opened. Its
// subject stays in Used, so the same plan never runs twice without replanning.
func (g *Gateway) closeInterrupted(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord) error {
	return g.closeOpen(ctx, w, key, rec, nil)
}

// closeOpen is closeInterrupted; held is the key's in-memory phase when the
// caller already holds its mutex, nil when closeOpen should take it.
func (g *Gateway) closeOpen(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord, held *phase) error {
	open := *rec.Open
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: sandbox.GatewayActor, Intent: key, Stage: open.Stage,
		SubjectHash: open.SubjectHash, AuthorizedBy: open.Auth, Payload: phaseEnd{BeginSeq: open.BeginSeq, Outcome: outcomeInterrupted,
			Reason: "the gateway restarted or lost leadership during the phase"}}); err != nil {
		return status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	if held != nil {
		held.closed = true
	} else if p := g.phases.get(key); p != nil {
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
	ns, name, key, err := intentKey(req.GetIntent())
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
		return nil, g.noOpenPhase(ctx, d, ns, name, key)
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
		if p.stage == lastv1alpha1.StageDelete && p.outcome == outcomeSucceeded {
			if err := g.retryTeardown(ctx, key, p.uid); err != nil {
				return nil, status.Errorf(codes.Unavailable, "%v", err)
			}
			g.phases.remove(key)
			return &gatewayv1.EndPhaseResponse{EndSeq: p.endSeq}, nil
		}
	} else {
		if req.GetSucceeded() && (p.failed || p.next < len(p.body.Steps)) {
			return nil, d.deny(codes.FailedPrecondition, "the phase cannot succeed: %d of %d steps succeeded", p.next, len(p.body.Steps))
		}
	}
	rec, err := g.state.Load(ctx, key, p.uid)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if p.endSeq == 0 {
		if err := g.endOpenPhase(ctx, w, actor, key, p, want, req.GetReason()); err != nil {
			return nil, err
		}
	}
	rec.Open = nil
	if p.outcome == outcomeSucceeded && replacesExecuted(p.stage) {
		canonical, err := plan.Canonical(p.body)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode executed plan: %v", err)
		}
		rec.Executed = &ExecutedPlan{Stage: string(p.stage), PlanHash: p.subject, Plan: string(canonical), EndSeq: p.endSeq,
			BeginSeq: p.beginSeq, Began: formatBegan(p.began)}
	}
	// The record is saved without the open phase before a succeeded Delete drops
	// it, so a failed deletion never leaves a phase for the next leader to interrupt.
	if err := g.saveRecord(ctx, rec); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	if err := g.afterSave(ctx, w, actor, key, ns, name, p); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.phases.remove(key)
	return &gatewayv1.EndPhaseResponse{EndSeq: p.endSeq}, nil
}

// afterSave finishes a succeeded phase once its record is saved without the open
// phase: a Delete drops the record, and an Update, now the executed plan,
// deletes the ManagedResources it dropped. On an error EndPhase keeps the phase,
// and the operator's retry saves again and repeats what is left.
func (g *Gateway) afterSave(ctx context.Context, w *audit.Writer, actor, key, ns, name string, p *phase) error {
	if p.outcome != outcomeSucceeded {
		return nil
	}
	switch p.stage {
	case lastv1alpha1.StageDelete:
		return g.dropRecord(ctx, key)
	case lastv1alpha1.StageUpdate:
		if p.stale == nil {
			p.stale = map[types.UID]staleDeletion{}
		}
		entry := audit.Draft{Actor: actor, Intent: key, Stage: string(p.stage), SubjectHash: p.subject, AuthorizedBy: p.auth}
		return g.deleteStaleManagedResources(ctx, w, entry, ns, name, types.UID(p.uid), len(p.body.Resources), p.stale)
	}
	return nil
}

// deleteStaleManagedResources deletes the ManagedResources an Update dropped
// (ruling R1): those in namespace ns whose controller is the Intent of UID uid,
// named other than ManagedResourceName(intent, i) for the executed plan's kept
// resources. EndPhase(Update, succeeded) calls it, and the record sweep finishes
// a cleanup a restart cut short. The operator cannot delete them (spec §4.1):
// each deletion garbage-collects an output Secret, so each is audited as
// managedresource.deleted, from the entry template, once the delete succeeds. A
// ManagedResource already gone counts as deleted and writes no entry. The caller
// holds the Intent's lock. seen makes a retry repeat only what is left: a
// deletion whose entry could not be written is audited first, and one already
// audited is never deleted or audited again, even while it is still terminating.
func (g *Gateway) deleteStaleManagedResources(ctx context.Context, w *audit.Writer, entry audit.Draft, ns, intent string, uid types.UID,
	kept int, seen map[types.UID]staleDeletion) error {
	record := func(mrUID types.UID) error {
		del := seen[mrUID]
		e := entry
		e.Type, e.Payload = audit.TypeManagedResourceDeleted, managedResourceDeleted{ManagedResource: del.name, UID: string(mrUID)}
		if _, err := w.Append(ctx, e); err != nil {
			return fmt.Errorf("record %s: %w", audit.TypeManagedResourceDeleted, err)
		}
		del.audited = true
		seen[mrUID] = del
		return nil
	}
	pending := slices.SortedFunc(maps.Keys(seen), func(a, b types.UID) int { return strings.Compare(seen[a].name, seen[b].name) })
	for _, mrUID := range pending {
		if !seen[mrUID].audited {
			if err := record(mrUID); err != nil {
				return err
			}
		}
	}

	keep := make(map[string]bool, kept)
	for i := range kept {
		keep[lastv1alpha1.ManagedResourceName(intent, i)] = true
	}
	var list lastv1alpha1.ManagedResourceList
	if err := g.crd.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list ManagedResources: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b lastv1alpha1.ManagedResource) int { return strings.Compare(a.Name, b.Name) })
	for i := range list.Items {
		mr := &list.Items[i]
		owner := metav1.GetControllerOf(mr)
		if owner == nil || owner.UID != uid || keep[mr.Name] {
			continue
		}
		if _, done := seen[mr.UID]; done || mr.DeletionTimestamp != nil {
			continue // already deleted: a finalizer may hold it, and it is never audited twice
		}
		// Never delete what could not be audited.
		if w.Broken() {
			return errors.New("the audit writer is broken; no ManagedResource is deleted unaudited")
		}
		// The UID precondition deletes only the object listed, never one recreated under its name.
		err := g.crd.Delete(ctx, mr, client.Preconditions{UID: &mr.UID})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("delete ManagedResource %s: %w", mr.Name, err)
		}
		slog.Info("Deleted ManagedResource the Update dropped", "intent", entry.Intent, "name", mr.Name)
		seen[mr.UID] = staleDeletion{name: mr.Name}
		if err := record(mr.UID); err != nil {
			return err
		}
	}
	return nil
}

// endOpenPhase writes the phase's phase.end and marks it closed; the caller holds
// p.mu and the Intent's lock.
func (g *Gateway) endOpenPhase(ctx context.Context, w *audit.Writer, actor, key string, p *phase, outcome, reason string) error {
	end, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: actor, Intent: key, Stage: string(p.stage),
		SubjectHash: p.subject, AuthorizedBy: p.auth,
		Payload: phaseEnd{BeginSeq: p.beginSeq, Outcome: outcome, Reason: truncateReason(reason), StepsRun: p.next}})
	if err != nil {
		return status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	p.endSeq, p.outcome, p.closed = end.Seq, outcome, true
	if outcome == outcomeSucceeded && p.stage == lastv1alpha1.StageDelete {
		// From the teardown's phase.end on, no phase opens for the Intent, even
		// while dropping its record fails and EndPhase is retried.
		g.tornDown.Store(key, p.uid)
	}
	return nil
}

// dropRecord deletes the record of an Intent whose teardown succeeded, with the
// index entries still pending for it. EndPhase already stored the Intent's UID
// in tornDown, so no later index flush recreates the record. The caller holds
// the Intent's lock, which every record writer takes; phase.end is already in
// the audit log, the source of truth the record only indexes.
func (g *Gateway) dropRecord(ctx context.Context, key string) error {
	if err := g.state.Delete(ctx, key); err != nil {
		return err
	}
	g.index.take(key)
	return nil
}

// retryTeardown finishes a succeeded Delete whose record save or drop failed. A
// record still there is saved without its open phase first, so a restart never
// interrupts the ended phase; one already gone (an earlier deletion went through
// unanswered) is never saved, which would recreate it. A record a recreated
// Intent now owns is left alone.
func (g *Gateway) retryTeardown(ctx context.Context, key, uid string) error {
	rec, err := g.state.LoadAny(ctx, key)
	if err != nil {
		return err
	}
	if rec.cm != nil && rec.IntentUID != uid {
		return nil
	}
	if rec.cm != nil && rec.Open != nil {
		rec.Open = nil
		if err := g.saveRecord(ctx, rec); err != nil {
			return err
		}
	}
	return g.dropRecord(ctx, key)
}

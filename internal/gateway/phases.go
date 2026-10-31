package gateway

import (
	"context"
	"encoding/json"
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
	"github.com/moogie-labs/last-operator/internal/rules"
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
	mu      sync.Mutex // held while a step or output write runs
	closed  bool       // phase.end is durable; set under mu
	endSeq  uint64     // seq of the phase.end already written; non-zero while the state save is pending
	outcome string     // outcome recorded by that phase.end
	stage   lastv1alpha1.Stage
	subject string
	uid     string // the Intent's UID when the phase opened
	body    plan.Body
	// auth is what authorizes the phase's steps now, as its entries record it:
	// what BeginPhase found, or the Approve a later step's re-check fell back
	// to (recheckAuthorization). For a rule phase it is {rule}, and each step's
	// entries carry {rule, <its rule>} instead. Set under mu after BeginPhase.
	auth *audit.Authorization
	// auto is whether Auto opened the phase; the re-check keeps passing it
	// while the stage is still Auto, whatever auth now names.
	auto bool
	// rule is whether allow rules opened the phase; the re-check passes each
	// step that a current rule covers, whatever auth now names.
	rule bool
	// rules maps a rule phase's entry IDs (ruleCoverage) to the rule that last
	// covered each: at BeginPhase, then, for steps, at each step's re-check.
	// Set under mu after BeginPhase.
	rules map[string]rules.Ref
	// ruleSeen holds the rule versions (ruleObject.key) this phase's
	// rule.observed entries name. Set under mu after BeginPhase.
	ruleSeen map[string]bool
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
	// Rules names, for a phase allow rules authorized, the rule that covered each
	// entry: its ID ("<stepID>", "probe:<probeID>", "teardown:<resource>.<i>")
	// to "<scope>/<namespace or ->/<name>" (rules.Ref.String).
	Rules map[string]string `json:"rules,omitempty"`
}

// ruleObserved is the rule.observed payload: the allow rule object, and the
// version of it, that let a phase or a step run without an approval.
type ruleObserved struct {
	Scope           string `json:"scope"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
}

// ruleObject is the identity and version of the object a rules.Rule was read from.
type ruleObject struct {
	uid, resourceVersion string
	generation           int64
}

// key names this version of the rule object.
func (o ruleObject) key() string { return o.uid + "@" + o.resourceVersion }

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

// effectivePolicy resolves the command policy for namespace ns; it reads no
// allow rules, which only authorize phases and steps.
func (g *Gateway) effectivePolicy(ctx context.Context, ns string) (policy.Effective, error) {
	eff, _, err := g.readPolicyLayers(ctx, ns)
	return eff, err
}

// policyLayers are the policy objects one read returned: the OperatorConfig, or
// nil, the namespace's IntentPolicies and the allow rules that apply to it.
type policyLayers struct {
	op       *lastv1alpha1.OperatorConfig
	policies []lastv1alpha1.IntentPolicy
	// rules are every ClusterCommandRule, then the namespace's CommandRules,
	// each sorted by name: rules.Coverage and covering pick the first that covers.
	rules []rules.Rule
	// ruleObjects identifies the object each rule was read from, for rule.observed.
	ruleObjects map[rules.Ref]ruleObject
}

// covering returns the first rule, in order, that covers cmd at stage, and its object.
func (l policyLayers) covering(stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) (rules.Rule, ruleObject, bool) {
	i := slices.IndexFunc(l.rules, func(r rules.Rule) bool { return rules.Covers(r.Spec, stage, cmd) })
	if i < 0 {
		return rules.Rule{}, ruleObject{}, false
	}
	return l.rules[i], l.ruleObjects[l.rules[i].Ref], true
}

// mode is the stage's effective Auto/Manual mode for the Intent (design D8).
func (l policyLayers) mode(intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage) lastv1alpha1.Mode {
	return policy.Modes(l.op, l.policies, intent.Spec.Modes).For(stage)
}

// readPolicy reads the policy layers and the allow rules once, uncached, and
// resolves them, so the command policy, the modes and the rules always come
// from the same read. A rule list that fails refuses as Unavailable: nothing is
// authorized by rules that could not be read.
func (g *Gateway) readPolicy(ctx context.Context, ns string) (policy.Effective, policyLayers, error) {
	eff, layers, err := g.readPolicyLayers(ctx, ns)
	if err != nil {
		return policy.Effective{}, policyLayers{}, err
	}
	layers.rules, layers.ruleObjects, err = g.readRules(ctx, ns)
	if err != nil {
		return policy.Effective{}, policyLayers{}, err
	}
	return eff, layers, nil
}

// readRules lists every ClusterCommandRule and namespace ns's CommandRules
// through the uncached client, and returns them as rules sorted cluster first,
// then by name, with the object each came from. A CommandRule of any other
// namespace never applies.
func (g *Gateway) readRules(ctx context.Context, ns string) ([]rules.Rule, map[rules.Ref]ruleObject, error) {
	var cluster lastv1alpha1.ClusterCommandRuleList
	if err := g.crd.List(ctx, &cluster); err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "list ClusterCommandRules: %v", err)
	}
	var local lastv1alpha1.CommandRuleList
	if err := g.crd.List(ctx, &local, client.InNamespace(ns)); err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "list CommandRules: %v", err)
	}
	slices.SortFunc(cluster.Items, func(a, b lastv1alpha1.ClusterCommandRule) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(local.Items, func(a, b lastv1alpha1.CommandRule) int { return strings.Compare(a.Name, b.Name) })
	out := make([]rules.Rule, 0, len(cluster.Items)+len(local.Items))
	objects := make(map[rules.Ref]ruleObject, cap(out))
	add := func(ref rules.Ref, meta metav1.ObjectMeta, spec lastv1alpha1.CommandRuleSpec) {
		r := rules.Rule{Ref: ref, Spec: spec}
		obj := ruleObject{uid: string(meta.UID), resourceVersion: meta.ResourceVersion, generation: meta.Generation}
		g.warnInvalidRegex(r, obj)
		out = append(out, r)
		objects[ref] = obj
	}
	for _, c := range cluster.Items {
		add(rules.Ref{Scope: "cluster", Name: c.Name}, c.ObjectMeta, c.Spec)
	}
	for _, c := range local.Items {
		if c.Namespace != ns {
			continue
		}
		add(rules.Ref{Scope: "namespace", Namespace: ns, Name: c.Name}, c.ObjectMeta, c.Spec)
	}
	return out, objects, nil
}

// warnInvalidRegex logs a rule whose re: patterns do not compile, and so match
// nothing, once for each rule object and resourceVersion. It reports whether it logged.
func (g *Gateway) warnInvalidRegex(r rules.Rule, obj ruleObject) bool {
	var invalid []string
	for _, w := range rules.Lint(r.Spec) {
		if strings.Contains(w, "invalid regex") {
			invalid = append(invalid, w)
		}
	}
	if len(invalid) == 0 {
		return false
	}
	if _, seen := g.ruleRegexWarned.LoadOrStore(obj.key(), true); seen {
		return false
	}
	slog.Warn("Allow rule has an invalid regex that matches nothing", "rule", r.Ref.String(), "uid", obj.uid,
		"resourceVersion", obj.resourceVersion, "findings", invalid)
	return true
}

// readPolicyLayers reads the policy layers once and resolves them, so the
// command policy and the modes always come from the same objects.
func (g *Gateway) readPolicyLayers(ctx context.Context, ns string) (policy.Effective, policyLayers, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return policy.Effective{}, policyLayers{}, err
	}
	var list lastv1alpha1.IntentPolicyList
	if err := g.crd.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return policy.Effective{}, policyLayers{}, status.Errorf(codes.Unavailable, "list IntentPolicies: %v", err)
	}
	eff, err := policy.Resolve(op, list.Items)
	if err != nil {
		return policy.Effective{}, policyLayers{}, errInvalidPolicy{err}
	}
	return eff, policyLayers{op: op, policies: list.Items}, nil
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

// findApproval returns the approving IntentApproval for the subject: the one
// named prefer when it applies, else the first by name. Any matching Deny wins
// over every Approve (errApprovalDenied, with the denying object).
func (g *Gateway) findApproval(ctx context.Context, intent *lastv1alpha1.Intent, stage lastv1alpha1.Stage, subject,
	prefer string) (*lastv1alpha1.IntentApproval, error) {
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
		if found == nil || (found.Name != prefer && (a.Name == prefer || a.Name < found.Name)) {
			found = a
		}
	}
	return found, nil
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

// The kinds of audit.Authorization a phase carries.
const (
	authApproval = "approval"
	authAuto     = "auto"
	authRule     = "rule"
)

// checkApprover refuses an Approve that the operator or a gateway-namespace
// ServiceAccount gave: neither may authorize what the operator runs. BeginPhase
// and RunCommand's per-step re-check both use it, so the refusals cannot drift
// apart. The refusal is audited through d.
func (g *Gateway) checkApprover(d denial, approval *lastv1alpha1.IntentApproval) error {
	switch {
	case approval.Spec.ApprovedBy == g.cfg.OperatorIdentity:
		return d.deny(codes.PermissionDenied, "approval %s names the operator as approver", approval.Name)
	case strings.HasPrefix(approval.Spec.ApprovedBy, "system:serviceaccount:"+g.cfg.Namespace+":"):
		return d.deny(codes.PermissionDenied, "approval %s names a gateway-namespace ServiceAccount as approver", approval.Name)
	}
	return nil
}

// authorizePhase decides what authorizes opening a phase for the subject, in
// this order (main spec §8.3, M6 design R4):
//  1. a Deny for the subject refuses, whatever the mode, with
//     gatewayv1.MsgDeniedPrefix;
//  2. an Auto stage whose plan is within design D9's limits is authorized as
//     {auto, <stage key>}, whatever Approve exists;
//  3. allow rules that cover every step, probe and teardown command
//     (ruleCoverage), in any stage and mode and outside D9's limits, authorize
//     it as {rule}; an empty plan is never covered;
//  4. an Approve, from neither the operator nor a gateway-namespace
//     ServiceAccount, authorizes it as {approval, <namespace>/<name>};
//  5. with no Approve, an Auto stage refuses with gatewayv1.MsgNeedsApproval,
//     and a Manual stage with FailedPrecondition and gatewayv1.MsgNoApprovalPrefix.
//
// It also returns the approval that authorized the phase, nil otherwise, and,
// for a rule phase, the rule covering each entry by entry ID. The caller has
// read layers once, for the command policy too, and has checked the plan
// against it: rules never widen policy (R1). Every refusal is audited through d.
func (g *Gateway) authorizePhase(ctx context.Context, d denial, layers policyLayers, rec *LoadedRecord, intent *lastv1alpha1.Intent,
	stage lastv1alpha1.Stage, subject string, body plan.Body) (*audit.Authorization, *lastv1alpha1.IntentApproval, map[string]rules.Ref, error) {
	approval, err := g.findApproval(ctx, intent, stage, subject, "")
	switch {
	case errors.Is(err, errApprovalDenied):
		return nil, nil, nil, d.deny(codes.PermissionDenied, "%sapproval %s denies this %s", gatewayv1.MsgDeniedPrefix, approval.Name, subjectNoun(stage))
	case err != nil:
		return nil, nil, nil, err
	}
	auto := layers.mode(intent, stage) == lastv1alpha1.ModeAuto
	if auto {
		within, err := withinAutoLimits(d, rec, stage, body)
		if err != nil {
			return nil, nil, nil, err
		}
		if within {
			return autoBy(stage), nil, nil, nil
		}
	}
	if covered, ok := ruleCoverage(layers.rules, stage, body); ok {
		return ruleBy(), nil, covered, nil
	}
	if approval == nil {
		if auto {
			return nil, nil, nil, d.deny(codes.PermissionDenied, "%s", gatewayv1.MsgNeedsApproval)
		}
		return nil, nil, nil, d.deny(codes.FailedPrecondition, "%s%s", gatewayv1.MsgNoApprovalPrefix, subjectNoun(stage))
	}
	if err := g.checkApprover(d, approval); err != nil {
		return nil, nil, nil, err
	}
	return approvedBy(intent.Namespace, approval), approval, nil, nil
}

// ruleCoverage returns the rule that covers each entry of the plan, by entry
// ID (steps by ruleStepID), when rules cover every entry: every step and probe
// at stage and every teardown command at Delete (rules.PlanCoverage, M6 design
// R14), since a succeeded Create or Update plan goes on to authorize its
// probes and teardown. For every other stage only the steps count: a
// Remediate plan carries the executed plan's resources unchanged (checked
// before authorization) and never becomes the executed plan, and a Delete
// plan has none. An empty plan is never covered.
func ruleCoverage(rs []rules.Rule, stage lastv1alpha1.Stage, body plan.Body) (map[string]rules.Ref, bool) {
	if len(body.Steps) == 0 {
		return nil, false
	}
	covered, uncovered := rules.PlanCoverage(rs, stage, body)
	if len(uncovered) > 0 {
		return nil, false
	}
	out := make(map[string]rules.Ref, len(covered))
	for id, ref := range covered {
		out[ruleStepID(id)] = ref
	}
	return out, true
}

// ruleStepID is the key of a step in phase.begin's rules map: its ID, or "cmd"
// for a command without one.
func ruleStepID(id string) string {
	if id == "" {
		return "cmd"
	}
	return id
}

// ruleBy is the audit.Authorization of a phase allow rules opened; each of its
// steps is stamped with its own rule (ruleStep).
func ruleBy() *audit.Authorization { return &audit.Authorization{Kind: authRule} }

// ruleStep is the audit.Authorization of a step a rule covers.
func ruleStep(ref rules.Ref) *audit.Authorization {
	return &audit.Authorization{Kind: authRule, Name: ref.String()}
}

// observeRule writes rule.observed for the rule object obj that ref names.
func observeRule(d denial, ref rules.Ref, obj ruleObject) error {
	if _, err := d.w.Append(d.ctx, audit.Draft{Type: audit.TypeRuleObserved, Actor: d.actor, Intent: d.intent, Stage: d.stage,
		SubjectHash: d.subject, Payload: ruleObserved{Scope: ref.Scope, Namespace: ref.Namespace, Name: ref.Name, UID: obj.uid,
			ResourceVersion: obj.resourceVersion, Generation: obj.generation}}); err != nil {
		return status.Errorf(codes.Unavailable, "record rule.observed: %v", err)
	}
	return nil
}

// observeAuthorization writes what authorizePhase found, before phase.begin:
// approval.observed for the approval, or for a rule phase one rule.observed
// per rule (observeRules), whose results it returns. An Auto phase observes nothing.
func observeAuthorization(d denial, layers policyLayers, approval *lastv1alpha1.IntentApproval,
	covered map[string]rules.Ref) (map[string]bool, map[string]string, error) {
	if approval != nil {
		return nil, nil, observeApproval(d, approval)
	}
	if covered != nil {
		return observeRules(d, layers, covered)
	}
	return nil, nil, nil
}

// observeRules writes one rule.observed for each distinct rule in covered, in
// layers' order, and returns the rule versions it named (ruleObject.key) and
// phase.begin's rules map.
func observeRules(d denial, layers policyLayers, covered map[string]rules.Ref) (map[string]bool, map[string]string, error) {
	used := map[rules.Ref]bool{}
	names := make(map[string]string, len(covered))
	for id, ref := range covered {
		used[ref] = true
		names[id] = ref.String()
	}
	seen := map[string]bool{}
	for _, r := range layers.rules {
		if !used[r.Ref] {
			continue
		}
		obj := layers.ruleObjects[r.Ref]
		if err := observeRule(d, r.Ref, obj); err != nil {
			return nil, nil, err
		}
		seen[obj.key()] = true
	}
	return seen, names, nil
}

// autoBy is the audit.Authorization of an Auto phase, or step, of stage.
func autoBy(stage lastv1alpha1.Stage) *audit.Authorization {
	return &audit.Authorization{Kind: authAuto, Name: string(policy.KeyFor(stage))}
}

// approvedBy is the audit.Authorization of an approval phase, or step.
func approvedBy(ns string, approval *lastv1alpha1.IntentApproval) *audit.Authorization {
	return &audit.Authorization{Kind: authApproval, Name: ns + "/" + approval.Name}
}

// observeApproval writes approval.observed for the approval that authorizes a
// phase (BeginPhase) or, after a fallback, its steps (recheckAuthorization).
func observeApproval(d denial, approval *lastv1alpha1.IntentApproval) error {
	if _, err := d.w.Append(d.ctx, audit.Draft{Type: audit.TypeApprovalObserved, Actor: d.actor, Intent: d.intent, Stage: d.stage,
		SubjectHash: d.subject, Payload: approvalObserved{Name: approval.Name, UID: string(approval.UID), ApprovedBy: approval.Spec.ApprovedBy,
			Decision: string(approval.Spec.Decision), Created: approval.CreationTimestamp.UTC().Format(time.RFC3339)}}); err != nil {
		return status.Errorf(codes.Unavailable, "record approval.observed: %v", err)
	}
	return nil
}

// withinAutoLimits reports whether Auto may authorize the stage's plan without
// an approval (design D9):
//   - Create, Update and Discovery: any plan or command that passed the checks;
//   - Remediate: every step runs exactly as some step of the executed plan ran,
//     references included (withinExecutedSteps);
//   - Delete: the steps are a permutation of the executed plan's teardown
//     commands, in resource order (withinTeardown); with no executed plan, never.
//
// An executed plan that fails its integrity check, or a command that cannot be
// encoded, is refused as Internal through d.
func withinAutoLimits(d denial, rec *LoadedRecord, stage lastv1alpha1.Stage, body plan.Body) (bool, error) {
	switch stage {
	case lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDiscovery:
		return true, nil
	case lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete:
	default:
		return false, nil
	}
	if rec.Executed == nil {
		return false, nil
	}
	executed, err := executedBody(d, rec.Executed)
	if err != nil {
		return false, err
	}
	if stage == lastv1alpha1.StageRemediate {
		return withinExecutedSteps(d, executed.Steps, body.Steps)
	}
	return withinTeardown(d, executed, body.Steps)
}

// withinExecutedSteps reports whether every step has the extended key
// (plan.ExtendedKeys) of some executed step: it runs the same command, and each
// of its ${steps…} references reads the output of the same command the
// executed step's did. A step may repeat, and steps may be left out or
// reordered, but none runs anything the approver of the executed plan did not
// see. A reference that cannot resolve is outside the limits.
func withinExecutedSteps(d denial, executed, steps []lastv1alpha1.Command) (bool, error) {
	have, err := plan.ExtendedKeys(executed)
	if err != nil {
		return false, d.deny(codes.Internal, "executed plan: %v", err)
	}
	keys, err := plan.ExtendedKeys(steps)
	if errors.Is(err, plan.ErrReference) {
		return false, nil
	}
	if err != nil {
		return false, d.deny(codes.Internal, "%v", err)
	}
	for _, k := range keys {
		if !slices.Contains(have, k) {
			return false, nil
		}
	}
	return true, nil
}

// withinTeardown reports whether steps are a permutation of the executed
// plan's teardown commands, gathered in resource order, by plan.CommandKey:
// each exactly once, unchanged but for its id. Teardown commands hold no
// ${steps…} reference (plan.Validate refuses one).
func withinTeardown(d denial, executed plan.Body, steps []lastv1alpha1.Command) (bool, error) {
	var teardown []lastv1alpha1.Command
	for _, r := range executed.Resources {
		teardown = append(teardown, r.Teardown...)
	}
	if len(steps) != len(teardown) {
		return false, nil
	}
	want := make(map[string]int, len(teardown))
	for i, c := range teardown {
		k, err := plan.CommandKey(c)
		if err != nil {
			return false, d.deny(codes.Internal, "executed teardown command %d cannot be encoded: %v", i+1, err)
		}
		want[k]++
	}
	for _, s := range steps {
		k, err := plan.CommandKey(s)
		if err != nil {
			return false, d.deny(codes.Internal, "step %s cannot be encoded: %v", s.ID, err)
		}
		if want[k] == 0 {
			return false, nil
		}
		want[k]--
	}
	return true, nil
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
	if err := g.finishEnded(ctx, w, actor, key, ns, name, string(intent.UID), stage, rec); err != nil {
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
	if err := g.finishEnding(ctx, w, actor, key, ns, name, string(intent.UID), stage); err != nil {
		return nil, err
	}
	// After the previous phase is finished: it may have become the executed plan.
	if err := checkExecuted(d, rec, stage, body); err != nil {
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
	eff, layers, err := g.readPolicy(ctx, ns)
	if invalid, ok := errors.AsType[errInvalidPolicy](err); ok {
		return nil, d.deny(codes.FailedPrecondition, "%v", invalid)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPlanPolicy(eff, body); err != nil {
		return nil, d.deny(codes.PermissionDenied, "%v", err)
	}
	auth, approval, covered, err := g.authorizePhase(ctx, d, layers, rec, &intent, stage, subject, body)
	if err != nil {
		return nil, err
	}

	ruleSeen, ruleNames, err := observeAuthorization(d, layers, approval, covered)
	if err != nil {
		return nil, err
	}
	begin, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseBegin, Actor: actor, Intent: key, Stage: string(stage),
		SubjectHash: subject, AuthorizedBy: auth, Payload: phaseBegin{Plan: string(canonical), Steps: len(body.Steps), Nonce: nonce,
			Rules: ruleNames}})
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
	g.phases.put(key, &phase{stage: stage, subject: subject, uid: string(intent.UID), body: body, auth: auth, auto: auth.Kind == authAuto,
		rule: auth.Kind == authRule, rules: covered, ruleSeen: ruleSeen, beginSeq: begin.Seq, began: began, results: plan.Results{}})
	return &gatewayv1.BeginPhaseResponse{BeginSeq: begin.Seq, Steps: int32(len(body.Steps))}, nil //nolint:gosec // bounded by MaxPlanBytes
}

// finishEnding finishes the cleanup of the Intent's succeeded Create or Update
// before a new phase opens. Such a phase is saved as the executed plan without
// its open phase, but stays in memory while stale ManagedResources are left to
// delete; a new phase would replace it and leave them behind. Its operator may
// have moved on (an EndPhase(failed) is refused as already succeeded), so
// BeginPhase does the work rather than wait for an EndPhase retry that may
// never come. A step-holder of the phase answers Aborted and a failed deletion
// Unavailable; the operator retries both. A Delete phase (stage) does not wait
// for the cleanup: the Intent's deletion garbage-collects its ManagedResources,
// and the record sweep is the backstop. The caller holds the Intent's lock,
// under which endSeq and outcome are written, and has checked that the record
// has no open phase.
func (g *Gateway) finishEnding(ctx context.Context, w *audit.Writer, actor, key, ns, name, uid string, stage lastv1alpha1.Stage) error {
	p := g.phases.get(key)
	if p == nil || p.endSeq == 0 || p.outcome != outcomeSucceeded || !replacesExecuted(p.stage) || p.uid != uid {
		return nil
	}
	if stage == lastv1alpha1.StageDelete {
		slog.Info("Left an ended phase's ManagedResource cleanup to the Intent's deletion", "intent", key, "subject", p.subject)
		g.phases.remove(key)
		return nil
	}
	if !p.mu.TryLock() {
		return status.Error(codes.Aborted, "the previous phase is still ending; retry")
	}
	defer p.mu.Unlock()
	if err := g.afterSave(ctx, w, actor, key, ns, name, p); err != nil {
		return status.Errorf(codes.Unavailable, "finish the cleanup of the previous %s (subject %s): %v", p.stage, p.subject, err)
	}
	g.phases.remove(key)
	return nil
}

// finishEnded finishes a phase of this process whose phase.end is written but
// whose record save failed: the record still holds it as open. Its operator may
// have abandoned it (an EndPhase(failed) is refused as already succeeded), and
// a restart would otherwise interrupt a phase the audit log says ended. So
// BeginPhase saves the record as EndPhase would have, and finishes what follows
// the save (finishEnding), which a Delete phase (stage) leaves out. A
// step-holder of the phase answers Aborted and a failed save Unavailable; the
// operator retries both. The caller holds the Intent's lock, under which endSeq
// and outcome are written.
func (g *Gateway) finishEnded(ctx context.Context, w *audit.Writer, actor, key, ns, name, uid string, stage lastv1alpha1.Stage,
	rec *LoadedRecord) error {
	p := g.phases.get(key)
	if rec.Open == nil || rec.Open.Epoch != g.epoch || p == nil || p.endSeq == 0 || p.uid != uid || p.beginSeq != rec.Open.BeginSeq {
		return nil
	}
	if !p.mu.TryLock() {
		return status.Error(codes.Aborted, "the previous phase is still ending; retry")
	}
	defer p.mu.Unlock()
	if err := g.saveEnded(ctx, rec, p); err != nil {
		return err
	}
	if stage != lastv1alpha1.StageDelete {
		if err := g.afterSave(ctx, w, actor, key, ns, name, p); err != nil {
			return status.Errorf(codes.Unavailable, "finish the previous phase (subject %s): %v", p.subject, err)
		}
	}
	g.phases.remove(key)
	return nil
}

// saveEnded saves the record of an ended phase: without the open phase, and,
// for a succeeded Create or Update, with the phase's plan as the executed one.
// The caller holds the Intent's lock and p.mu.
func (g *Gateway) saveEnded(ctx context.Context, rec *LoadedRecord, p *phase) error {
	rec.Open = nil
	if p.outcome == outcomeSucceeded && replacesExecuted(p.stage) {
		canonical, err := plan.Canonical(p.body)
		if err != nil {
			return status.Errorf(codes.Internal, "encode executed plan: %v", err)
		}
		rec.Executed = &ExecutedPlan{Stage: string(p.stage), PlanHash: p.subject, Plan: string(canonical), EndSeq: p.endSeq,
			BeginSeq: p.beginSeq, Began: formatBegan(p.began)}
	}
	if err := g.saveRecord(ctx, rec); err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
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
// caller already holds its mutex, nil when closeOpen should take it. A phase
// whose phase.end is already in the audit log (its record save failed before a
// restart) is closed as it ended, without a second phase.end (closeEnded).
func (g *Gateway) closeOpen(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord, held *phase) error {
	open := *rec.Open
	end, err := g.loggedEnd(ctx, w, key, rec, held)
	if err != nil {
		return status.Errorf(codes.Unavailable, "look for the phase's phase.end: %v", err)
	}
	if end != nil {
		return g.closeEnded(ctx, key, rec, held, *end)
	}
	if _, err := w.Append(ctx, audit.Draft{Type: audit.TypePhaseEnd, Actor: sandbox.GatewayActor, Intent: key, Stage: open.Stage,
		SubjectHash: open.SubjectHash, AuthorizedBy: open.Auth, Payload: phaseEnd{BeginSeq: open.BeginSeq, Outcome: outcomeInterrupted,
			Reason: "the gateway restarted or lost leadership during the phase"}}); err != nil {
		return status.Errorf(codes.Unavailable, "record phase.end: %v", err)
	}
	g.forgetPhase(key, held)
	rec.Open = nil
	if err := g.saveRecord(ctx, rec); err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	return nil
}

// maxEndScan bounds how many audit entries past the Intent's index closeOpen
// reads to find an open phase's phase.end.
const maxEndScan = 1000

// loggedPhaseEnd is a phase.end already in the audit log.
type loggedPhaseEnd struct {
	seq     uint64
	outcome string
}

// loggedEnd returns the phase.end the record's open phase already has in the
// audit log, or nil when it has none: EndPhase wrote it, then could not save
// the record. This process's phase knows it; otherwise the Intent's indexed
// phase.end entries after the phase.begin are read, then the entries written
// after the newest indexed one, which a restart may have kept from the index,
// up to the head and at most maxEndScan of them.
func (g *Gateway) loggedEnd(ctx context.Context, w *audit.Writer, key string, rec *LoadedRecord, held *phase) (*loggedPhaseEnd, error) {
	open := rec.Open
	p := held
	if p == nil {
		p = g.phases.get(key)
	}
	if p != nil && p.beginSeq == open.BeginSeq && p.endSeq != 0 {
		return &loggedPhaseEnd{seq: p.endSeq, outcome: p.outcome}, nil
	}
	match := func(e audit.Entry) *loggedPhaseEnd {
		var pe phaseEnd
		if e.Intent != key || e.Type != audit.TypePhaseEnd || json.Unmarshal(e.Payload, &pe) != nil || pe.BeginSeq != open.BeginSeq {
			return nil
		}
		return &loggedPhaseEnd{seq: e.Seq, outcome: pe.Outcome}
	}
	from := open.BeginSeq
	for _, ie := range rec.Index {
		from = max(from, ie.Seq)
		if ie.Seq <= open.BeginSeq || ie.Type != audit.TypePhaseEnd {
			continue
		}
		e, err := g.readEntry(ctx, ie.Seq)
		if err != nil {
			return nil, err
		}
		if m := match(e); m != nil {
			return m, nil
		}
	}
	head, _, ok := w.Head()
	if !ok {
		return nil, nil
	}
	to := min(head, from+maxEndScan)
	for seq := from + 1; seq <= to; seq++ {
		e, err := g.readEntry(ctx, seq)
		if errors.Is(err, audit.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if m := match(e); m != nil {
			return m, nil
		}
	}
	if to < head {
		slog.Warn("Gave up looking for the phase.end of an open phase", "intent", key, "beginSeq", open.BeginSeq, "scannedTo", to, "head", head)
	}
	return nil, nil
}

// closeEnded closes the record's open phase as its logged phase.end says,
// without writing another: a succeeded Create or Update becomes the executed
// plan, read from its phase.begin, and a succeeded Delete drops the record, as
// EndPhase would have done.
func (g *Gateway) closeEnded(ctx context.Context, key string, rec *LoadedRecord, held *phase, end loggedPhaseEnd) error {
	open := *rec.Open
	stage := lastv1alpha1.Stage(open.Stage)
	g.forgetPhase(key, held)
	if end.outcome == outcomeSucceeded && stage == lastv1alpha1.StageDelete {
		g.tornDown.Store(key, rec.IntentUID)
		if err := g.dropRecord(ctx, key); err != nil {
			return status.Errorf(codes.Unavailable, "%v", err)
		}
		slog.Info("Closed an ended phase", "intent", key, "beginSeq", open.BeginSeq, "outcome", end.outcome)
		return nil
	}
	rec.Open = nil
	if end.outcome == outcomeSucceeded && replacesExecuted(stage) {
		begin, err := g.readEntry(ctx, open.BeginSeq)
		if err != nil {
			return status.Errorf(codes.Unavailable, "read phase.begin %d: %v", open.BeginSeq, err)
		}
		var pb phaseBegin
		if err := json.Unmarshal(begin.Payload, &pb); err != nil || begin.Type != audit.TypePhaseBegin || begin.Intent != key {
			return status.Errorf(codes.Internal, "audit entry %d is not the phase.begin of %s", open.BeginSeq, key)
		}
		began, err := time.Parse(audit.TimeFormat, begin.TS)
		if err != nil {
			return status.Errorf(codes.Internal, "phase.begin %d: %v", open.BeginSeq, err)
		}
		rec.Executed = &ExecutedPlan{Stage: open.Stage, PlanHash: open.SubjectHash, Plan: pb.Plan, EndSeq: end.seq,
			BeginSeq: open.BeginSeq, Began: formatBegan(began)}
	}
	if err := g.saveRecord(ctx, rec); err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	slog.Info("Closed an ended phase", "intent", key, "beginSeq", open.BeginSeq, "outcome", end.outcome)
	return nil
}

// forgetPhase marks the key's in-memory phase closed and drops it; held is that
// phase when the caller holds its mutex.
func (g *Gateway) forgetPhase(key string, held *phase) {
	if held != nil {
		held.closed = true
	} else if p := g.phases.get(key); p != nil {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	}
	g.phases.remove(key)
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
	// The record is saved without the open phase before a succeeded Delete drops
	// it, so a failed deletion never leaves a phase for the next leader to interrupt.
	if err := g.saveEnded(ctx, rec, p); err != nil {
		return nil, err
	}
	if err := g.afterSave(ctx, w, actor, key, ns, name, p); err != nil {
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	}
	g.phases.remove(key)
	return &gatewayv1.EndPhaseResponse{EndSeq: p.endSeq}, nil
}

// afterSave finishes a succeeded phase once its record is saved without the open
// phase: a Delete drops the record, and a Create or Update, now the executed
// plan, deletes the ManagedResources it dropped; a Create replaces an executed
// plan when its operator could not read the audit log. On an error EndPhase keeps the phase,
// and the operator's retry saves again and repeats what is left.
func (g *Gateway) afterSave(ctx context.Context, w *audit.Writer, actor, key, ns, name string, p *phase) error {
	if p.outcome != outcomeSucceeded {
		return nil
	}
	switch p.stage {
	case lastv1alpha1.StageDelete:
		return g.dropRecord(ctx, key)
	case lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate:
		if p.stale == nil {
			p.stale = map[types.UID]staleDeletion{}
		}
		entry := audit.Draft{Actor: actor, Intent: key, Stage: string(p.stage), SubjectHash: p.subject, AuthorizedBy: p.auth}
		return g.deleteStaleManagedResources(ctx, w, entry, ns, name, types.UID(p.uid), p.body.Resources, p.stale)
	}
	return nil
}

// deleteStaleManagedResources deletes the ManagedResources a Create or Update
// dropped (ruling R1): those in namespace ns whose controller is the Intent of UID uid,
// named other than ManagedResourceName(intent, i) for the executed plan's kept
// resources. EndPhase(Update, succeeded) calls it, and the record sweep finishes
// a cleanup a restart cut short. The operator cannot delete them (spec §4.1):
// each deletion garbage-collects an output Secret, so each is audited as
// managedresource.deleted, from the entry template, once the delete succeeds. A
// ManagedResource already gone counts as deleted and writes no entry. First,
// the output Secrets the executed plan's resources declare move to the
// ManagedResources that now declare them (moveKeptOutputSecrets), so none is
// lost with a deleted one. The caller holds the Intent's lock. seen makes a
// retry repeat only what is left: a deletion whose entry could not be written
// is audited first, and one already audited is never deleted or audited again,
// even while it is still terminating.
func (g *Gateway) deleteStaleManagedResources(ctx context.Context, w *audit.Writer, entry audit.Draft, ns, intent string, uid types.UID,
	resources []lastv1alpha1.ManagedResourceSpec, seen map[types.UID]staleDeletion) error {
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

	if err := g.moveKeptOutputSecrets(ctx, w, entry, ns, intent, uid, resources); err != nil {
		return err
	}
	keep := make(map[string]bool, len(resources))
	for i := range resources {
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
		slog.Info("Deleted ManagedResource the executed plan dropped", "intent", entry.Intent, "name", mr.Name)
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

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
)

// Call log entries.
const (
	callLLM      = "CallLLM"
	beginPhase   = "BeginPhase"
	endSucceeded = "EndPhase:succeeded"
	endFailed    = "EndPhase:failed"
	runS1        = "RunCommand:s1"
	runS2        = "RunCommand:s2"
	runProbes    = "RunProbes"
	trueCmd      = "true"

	beginDiscovery   = "BeginPhase:Discovery"
	runDiscoveryStep = "RunCommand:discovery"
	queryAudit       = "QueryAudit"
)

// llmFunc answers one CallLLM request.
type llmFunc func(*gatewayv1.CallLLMRequest) (string, error)

// turns answers with the nth answer when the conversation holds n assistant
// messages, like the gateway's scripted provider.
func turns(answers ...string) llmFunc {
	return func(req *gatewayv1.CallLLMRequest) (string, error) {
		n := 0
		for _, m := range req.GetMessages() {
			if m.GetRole() == roleAssistant {
				n++
			}
		}
		if n >= len(answers) {
			return "", status.Error(codes.FailedPrecondition, "llm: scripted conversation has no more responses")
		}
		return answers[n], nil
	}
}

// script is how the fake gateway behaves for one Intent.
type script struct {
	llm      llmFunc
	stdout   map[string]string // step ID → stdout
	exit     map[string]int32  // step ID → exit code; a failing step's stderr is "boom"
	runErr   map[string]error  // step ID → RunCommand error
	runErrs  []error           // errors returned by the next RunCommand calls, in order, before runErr
	buildLog map[string]string // step ID → the log of a failed image build: no run starts
	endErrs  []error           // errors returned by the next EndPhase calls, in order
	loseEnd  bool              // the next EndPhase takes effect, but its answer is lost (Unavailable), once
	failing  map[string]bool   // probe ID → fails; other probes pass
	probeOut map[string]string // probe ID → the probe's stdout
	probeErr error             // RunProbes error
	onProbe  func()            // called during a successful RunProbes
	onEnd    func(bool)        // called with the outcome of each EndPhase that closes a phase, with f.mu held

	interrupt map[string]bool // step ID → the gateway restarts before it: the phase ends as interrupted, once

	beginErr error              // returned by BeginPhase once the call is recorded
	onBegin  func(stage string) // called as each BeginPhase begins, before it reads anything, without f.mu
	onRun    func(step string)  // called with f.mu held as each RunCommand of an open phase begins, before its re-check
	queryErr error              // returned by QueryAudit
	// queryHook, when set, is called with each QueryAudit call's types; an error it returns is the call's.
	queryHook      func(types []string) error
	auditTruncated bool // QueryAudit reports that the index dropped every entry, and serves none
}

// fakeGateway stands in for the gateway in envtest (spec §18). It enforces the
// phase rules the operator depends on (approval before BeginPhase, one open
// phase, calls only within it, probes only of an executed plan, Delete only for
// an Intent being deleted, Update and Remediate only of an executed plan,
// nothing after a succeeded Delete) and records every call per Intent. Its
// phase.begin and phase.end entries carry the gateway's payloads, so
// agent.ExecutedPlan reads them. A succeeded Update becomes the executed plan
// and deletes the ManagedResources it dropped, as the gateway does (ruling R1).
// A Remediate needs a nonce and the executed plan's resources unchanged, runs
// once per subject, and never becomes the executed plan (rulings R2, R3); every
// other stage refuses a nonce.
//
// It authorizes phases as the gateway does (rulings R2–R4): it resolves the
// stage's mode from the OperatorConfig and IntentPolicies in envtest, opens an
// Auto stage within D9's limits with no approval, refuses an Auto plan outside
// them with gatewayv1.MsgNeedsApproval, and re-checks what authorized the phase
// before every step.
type fakeGateway struct {
	gatewayv1.GatewayServiceClient // unimplemented RPCs panic

	c client.Client

	mu        sync.Mutex
	scripts   map[types.NamespacedName]*script
	log       map[types.NamespacedName][]string // "CallLLM", "BeginPhase", "RunCommand:s1", "EndPhase:failed", …
	llmReqs   map[types.NamespacedName][]*gatewayv1.CallLLMRequest
	argv      map[types.NamespacedName]map[string][]string          // step ID → argv sent
	outputs   map[types.NamespacedName]map[string]map[string]string // ManagedResource → values sent
	open      map[types.NamespacedName]string                       // subject of the open phase
	openStage map[types.NamespacedName]string                       // stage of the open phase
	openBegin map[types.NamespacedName]uint64                       // phase.begin sequence number of the open phase
	openUID   map[types.NamespacedName]types.UID                    // Intent UID the open phase belongs to
	openSteps map[types.NamespacedName]int                          // steps of the open phase that succeeded
	openKept  map[types.NamespacedName]int                          // resources of the open phase's plan
	tornDown  map[types.NamespacedName]types.UID                    // Intent UID whose Delete succeeded
	used      map[string]bool                                       // Discovery and Remediate subjects already opened: they are single-use
	executed  map[types.NamespacedName]string                       // hash of the last executed plan
	openBody  map[types.NamespacedName]plan.Body                    // plan of the open phase
	execBody  map[types.NamespacedName]plan.Body                    // the last executed plan, when a phase of this gateway executed it
	openAuto  map[types.NamespacedName]bool                         // the open phase was authorized by Auto
	auths     map[types.NamespacedName][]string                     // how each opened phase was authorized: "auto:<key>" or "approval"
	seq       uint64                                                // the last audit sequence number, across Intents
	audit     map[types.NamespacedName][]*gatewayv1.AuditEntry      // indexed entries, oldest first
}

func newFakeGateway(c client.Client) *fakeGateway {
	return &fakeGateway{c: c, scripts: map[types.NamespacedName]*script{}, log: map[types.NamespacedName][]string{},
		llmReqs: map[types.NamespacedName][]*gatewayv1.CallLLMRequest{}, argv: map[types.NamespacedName]map[string][]string{},
		outputs: map[types.NamespacedName]map[string]map[string]string{}, open: map[types.NamespacedName]string{},
		openStage: map[types.NamespacedName]string{}, openBegin: map[types.NamespacedName]uint64{},
		openUID: map[types.NamespacedName]types.UID{}, openSteps: map[types.NamespacedName]int{}, openKept: map[types.NamespacedName]int{},
		tornDown: map[types.NamespacedName]types.UID{}, used: map[string]bool{},
		executed: map[types.NamespacedName]string{}, audit: map[types.NamespacedName][]*gatewayv1.AuditEntry{},
		openBody: map[types.NamespacedName]plan.Body{}, execBody: map[types.NamespacedName]plan.Body{},
		openAuto: map[types.NamespacedName]bool{}, auths: map[types.NamespacedName][]string{}}
}

func keyOf(ref *gatewayv1.IntentRef) types.NamespacedName {
	return types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
}

func (f *fakeGateway) setScript(key types.NamespacedName, s *script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[key] = s
}

// calls returns the RPCs made for an Intent, in order.
func (f *fakeGateway) calls(key types.NamespacedName) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log[key])
}

// scriptFor and record are called with f.mu held.
func (f *fakeGateway) scriptFor(key types.NamespacedName) *script {
	if s := f.scripts[key]; s != nil {
		return s
	}
	return &script{}
}

func (f *fakeGateway) record(key types.NamespacedName, call string) {
	f.log[key] = append(f.log[key], call)
}

// fakeMessage is a message as the gateway's llm.request payload holds it.
type fakeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (f *fakeGateway) CallLLM(_ context.Context, in *gatewayv1.CallLLMRequest, _ ...grpc.CallOption) (*gatewayv1.CallLLMResponse, error) {
	key := keyOf(in.GetIntent())
	msgs := make([]fakeMessage, 0, len(in.GetMessages()))
	for _, m := range in.GetMessages() {
		msgs = append(msgs, fakeMessage{Role: m.GetRole(), Content: m.GetContent()})
	}
	f.mu.Lock()
	f.record(key, callLLM)
	f.llmReqs[key] = append(f.llmReqs[key], in)
	seq := f.appendAudit(key, "llm.request", "", "", map[string]any{"messages": msgs})
	llm := f.scriptFor(key).llm
	f.mu.Unlock()
	content, err := "", status.Error(codes.FailedPrecondition, "llm: no script for this intent")
	if llm != nil {
		content, err = llm(in)
	}
	payload := map[string]any{"requestSeq": seq, "content": content}
	if err != nil {
		payload["error"] = err.Error()
	}
	f.mu.Lock()
	f.appendAudit(key, "llm.response", "", "", payload)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &gatewayv1.CallLLMResponse{Content: content, RequestSeq: seq}, nil
}

// appendAudit adds an entry to the Intent's audit index, as the gateway would,
// and returns its sequence number. f.mu must be held.
func (f *fakeGateway) appendAudit(key types.NamespacedName, typ, stage, subject string, payload any) uint64 {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	f.seq++
	f.audit[key] = append(f.audit[key], &gatewayv1.AuditEntry{Seq: f.seq, Type: typ, Stage: stage, SubjectHash: subject, Payload: b})
	return f.seq
}

// auditOf returns the Intent's audit entries of one type, oldest first.
func (f *fakeGateway) auditOf(key types.NamespacedName, typ string) []*gatewayv1.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*gatewayv1.AuditEntry
	for _, e := range f.audit[key] {
		if e.GetType() == typ {
			out = append(out, e)
		}
	}
	return out
}

// approval reports whether an IntentApproval approves subject for the Intent's
// stage, and whether one denies it; a Deny wins.
func (f *fakeGateway) approval(ctx context.Context, key types.NamespacedName, stage, subject string) (approved, denied bool, err error) {
	var list lastv1alpha1.IntentApprovalList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return false, false, err
	}
	for _, a := range list.Items {
		if a.Spec.IntentRef.Name != key.Name || string(a.Spec.Stage) != stage || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return false, true, nil
		}
		approved = true
	}
	return approved, false, nil
}

// mode resolves the stage's mode for the Intent as the gateway does (ruling
// R1), from the OperatorConfig and the namespace's IntentPolicies in envtest.
func (f *fakeGateway) mode(ctx context.Context, intent *lastv1alpha1.Intent, stage string) (lastv1alpha1.Mode, error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := f.c.Get(ctx, client.ObjectKey{Name: operatorConfigName}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return "", err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := f.c.List(ctx, &policies, client.InNamespace(intent.Namespace)); err != nil {
		return "", err
	}
	return policy.Modes(op, policies.Items, intent.Spec.Modes).For(lastv1alpha1.Stage(stage)), nil
}

// withinAutoLimits applies design D9's limits as the gateway does (ruling R3):
// Create, Update and Discovery run anything; every Remediate step must have the
// extended key of an executed step; a Delete's steps must be a permutation of
// the executed plan's teardown commands. With no executed plan this fake knows,
// a Remediate or Delete is outside them. f.mu must be held.
func (f *fakeGateway) withinAutoLimits(key types.NamespacedName, stage string, body plan.Body) bool {
	switch lastv1alpha1.Stage(stage) {
	case lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDiscovery:
		return true
	case lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete:
	default:
		return false
	}
	executed, ok := f.execBody[key]
	if !ok {
		return false
	}
	if stage == string(lastv1alpha1.StageRemediate) {
		have, err := plan.ExtendedKeys(executed.Steps)
		if err != nil {
			panic(err) // the executed plan opened a phase here, so its references resolve
		}
		keys, err := plan.ExtendedKeys(body.Steps)
		if err != nil {
			return false
		}
		for _, k := range keys {
			if !slices.Contains(have, k) {
				return false
			}
		}
		return true
	}
	want := map[string]int{}
	n := 0
	for _, r := range executed.Resources {
		for _, c := range r.Teardown {
			k, err := plan.CommandKey(c)
			if err != nil {
				panic(err) // the executed plan opened a phase here, so it encodes
			}
			want[k]++
			n++
		}
	}
	if len(body.Steps) != n {
		return false
	}
	for _, s := range body.Steps {
		k, err := plan.CommandKey(s)
		if err != nil || want[k] == 0 {
			return false
		}
		want[k]--
	}
	return true
}

// recheck re-checks what authorized the open phase before a step, as the
// gateway does (ruling R4): a Deny refuses; an Auto phase passes while its stage
// is still Auto for the same Intent; otherwise an Approve must stand. f.mu must
// be held.
func (f *fakeGateway) recheck(ctx context.Context, key types.NamespacedName) error {
	stage, subject, auto := f.openStage[key], f.open[key], f.openAuto[key]
	withdrawn := "approval withdrawn"
	if auto {
		withdrawn = "auto authorization withdrawn"
	}
	var intent lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &intent); err != nil {
		return status.Errorf(codes.NotFound, "%s: intent not found", withdrawn)
	}
	approved, denied, err := f.approval(ctx, key, stage, subject)
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	if denied {
		return status.Errorf(codes.PermissionDenied, "%s%s: an approval denies this plan", gatewayv1.MsgDeniedPrefix, withdrawn)
	}
	if auto && intent.UID == f.openUID[key] {
		mode, err := f.mode(ctx, &intent, stage)
		if err != nil {
			return status.Error(codes.Unavailable, err.Error())
		}
		if mode == lastv1alpha1.ModeAuto {
			return nil
		}
	}
	switch {
	case approved:
		return nil
	case auto:
		return status.Errorf(codes.PermissionDenied, "%s: the %s stage is no longer Auto for this Intent, "+
			"and no IntentApproval approves this plan", withdrawn, stage)
	}
	return status.Errorf(codes.PermissionDenied, "%s: no IntentApproval approves this plan", withdrawn)
}

// authsOf returns how each phase the fake opened for the Intent was authorized, in order.
func (f *fakeGateway) authsOf(key types.NamespacedName) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.auths[key])
}

// pendingSubject checks a Discovery subject as the gateway does: it must be the
// Intent's pending command, hashed with its nonce.
func (f *fakeGateway) pendingSubject(ctx context.Context, key types.NamespacedName, subject string) error {
	var in lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &in); err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	pc := in.Status.PendingCommand
	if pc == nil {
		return status.Error(codes.FailedPrecondition, "intent has no pending discovery command")
	}
	hash, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
	if err != nil || pc.Hash != subject || hash != subject {
		return status.Error(codes.PermissionDenied, "the subject is not the pending discovery command")
	}
	return nil
}

// stageRule applies the gateway's stage rules: a Delete phase opens only for
// an Intent being deleted and only for a plan without resources, such an
// Intent opens no other phase but Discovery, and once its Delete succeeded it
// opens none. An Update or Remediate phase opens only for an Intent with an
// executed plan. f.mu must be held.
func (f *fakeGateway) stageRule(intent *lastv1alpha1.Intent, stage string) error {
	key, deleting := client.ObjectKeyFromObject(intent), intent.DeletionTimestamp != nil
	changes := stage == string(lastv1alpha1.StageUpdate) || stage == string(lastv1alpha1.StageRemediate)
	switch {
	case f.tornDown[key] == intent.UID:
		return status.Error(codes.FailedPrecondition, gatewayv1.MsgTornDown)
	case stage == string(lastv1alpha1.StageDelete) && !deleting:
		return status.Error(codes.FailedPrecondition, "intent is not being deleted")
	case stage == string(lastv1alpha1.StageDelete) && intent.Status.Plan != nil && len(intent.Status.Plan.Resources) > 0:
		return status.Error(codes.FailedPrecondition, "a Delete plan has no resources")
	case changes && f.executed[key] == "":
		return status.Error(codes.FailedPrecondition, "intent has no executed plan to "+strings.ToLower(stage))
	case stage != string(lastv1alpha1.StageDelete) && stage != string(lastv1alpha1.StageDiscovery) && deleting:
		return status.Error(codes.FailedPrecondition, "intent is being deleted; only a Delete or Discovery phase can open")
	}
	return nil
}

// nonceRule applies the gateway's nonce and Remediate rules to a plan: only a
// Remediate carries a nonce, and it must carry the executed plan's resources
// unchanged. f.mu must be held.
func (f *fakeGateway) nonceRule(key types.NamespacedName, stage string, body plan.Body) error {
	if stage != string(lastv1alpha1.StageRemediate) {
		if body.Nonce != "" {
			return status.Errorf(codes.InvalidArgument, "only a Remediate plan carries a nonce; this %s plan has one", stage)
		}
		return nil
	}
	if !plan.ValidNonce(body.Nonce) {
		return status.Error(codes.InvalidArgument, "a Remediate plan needs a nonce of 32 lowercase hex characters")
	}
	executed, ok := f.execBody[key]
	if !ok {
		return nil // executed by setExecuted, whose plan this fake does not know
	}
	same, err := plan.SameResources(body.Resources, executed.Resources)
	if err != nil || !same {
		return status.Error(codes.FailedPrecondition, "a Remediate plan must carry the executed plan's resources unchanged")
	}
	return nil
}

func (f *fakeGateway) BeginPhase(ctx context.Context, in *gatewayv1.BeginPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.BeginPhaseResponse, error) {
	key, stage, subject := keyOf(in.GetIntent()), in.GetStage(), in.GetSubjectHash()
	discovery := stage == string(lastv1alpha1.StageDiscovery)
	f.mu.Lock()
	onBegin := f.scriptFor(key).onBegin
	f.mu.Unlock()
	if onBegin != nil {
		onBegin(stage)
	}
	approved, denied, err := f.approval(ctx, key, stage, subject)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	var pendingErr error
	if discovery {
		pendingErr = f.pendingSubject(ctx, key, subject)
	}
	var intent lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &intent); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	mode, err := f.mode(ctx, &intent, stage)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	payload, kept, body := map[string]any{}, 0, plan.Body{}
	if p := intent.Status.Plan; !discovery && p != nil {
		kept, body = len(p.Resources), plan.FromStatus(p)
		canonical, err := plan.Canonical(body)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		payload["plan"], payload["steps"] = string(canonical), len(p.Steps)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if discovery {
		f.record(key, beginDiscovery)
	} else {
		f.record(key, beginPhase)
	}
	if err := f.scriptFor(key).beginErr; err != nil {
		return nil, err
	}
	// The gateway's order: the subject, then an open phase, then the rest.
	ruleErr := f.stageRule(&intent, stage)
	if ruleErr == nil && !discovery {
		ruleErr = f.nonceRule(key, stage, body)
	}
	switch {
	case pendingErr != nil:
		return nil, pendingErr
	case ruleErr != nil:
		return nil, ruleErr
	case f.open[key] != "":
		return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
	case f.used[subject] && discovery:
		return nil, status.Error(codes.FailedPrecondition, "this discovery command was already run")
	case f.used[subject]:
		return nil, status.Error(codes.FailedPrecondition, "this plan already opened a phase; a plan runs only once, so plan a different one")
	case denied:
		return nil, status.Error(codes.PermissionDenied, gatewayv1.MsgDeniedPrefix+"an approval denies this plan")
	}
	// The gateway's authorization order (ruling R2): Auto within D9's limits,
	// then an Approve, then NeedsApproval for an Auto stage.
	auto := mode == lastv1alpha1.ModeAuto && f.withinAutoLimits(key, stage, body)
	switch {
	case auto:
		f.auths[key] = append(f.auths[key], "auto:"+string(policy.KeyFor(lastv1alpha1.Stage(stage))))
	case approved:
		f.auths[key] = append(f.auths[key], "approval")
	case mode == lastv1alpha1.ModeAuto:
		return nil, status.Error(codes.PermissionDenied, gatewayv1.MsgNeedsApproval)
	case discovery:
		return nil, status.Error(codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"discovery command")
	default:
		return nil, status.Error(codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan")
	}
	f.open[key], f.openStage[key], f.openUID[key], f.openKept[key] = subject, stage, intent.UID, kept
	f.openAuto[key] = auto
	f.openBody[key] = body
	f.openBegin[key] = f.appendAudit(key, "phase.begin", stage, subject, payload)
	if discovery || stage == string(lastv1alpha1.StageRemediate) {
		f.used[subject] = true
	}
	return &gatewayv1.BeginPhaseResponse{}, nil
}

// noOpenPhase refuses a call that names no open phase, as the gateway does: as
// torn down for an Intent whose Delete succeeded. f.mu must not be held.
func (f *fakeGateway) noOpenPhase(ctx context.Context, key types.NamespacedName) error {
	f.mu.Lock()
	gone, ok := f.tornDown[key]
	f.mu.Unlock()
	var intent lastv1alpha1.Intent
	if ok && f.c.Get(ctx, key, &intent) == nil && intent.UID == gone {
		return status.Error(codes.FailedPrecondition, gatewayv1.MsgTornDown)
	}
	return status.Error(codes.FailedPrecondition, "no open phase for this plan")
}

func (f *fakeGateway) RunCommand(ctx context.Context, in *gatewayv1.RunCommandRequest, _ ...grpc.CallOption) (*gatewayv1.RunCommandResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "RunCommand:"+in.GetStepId())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		f.mu.Unlock()
		defer f.mu.Lock()
		return nil, f.noOpenPhase(ctx, key)
	}
	var c lastv1alpha1.Command
	if err := json.Unmarshal(in.GetCommandJson(), &c); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if f.argv[key] == nil {
		f.argv[key] = map[string][]string{}
	}
	f.argv[key][in.GetStepId()] = c.Run.Argv
	s := f.scriptFor(key)
	if s.onRun != nil {
		s.onRun(in.GetStepId())
	}
	if err := f.recheck(ctx, key); err != nil {
		return nil, err
	}
	if s.interrupt[in.GetStepId()] {
		// As a gateway restart does: the new leader closed the phase as interrupted.
		delete(s.interrupt, in.GetStepId())
		f.closePhase(key, "interrupted", "the gateway restarted")
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	if len(s.runErrs) > 0 {
		err := s.runErrs[0]
		s.runErrs = s.runErrs[1:]
		return nil, err
	}
	if err := s.runErr[in.GetStepId()]; err != nil {
		return nil, err
	}
	if log, ok := s.buildLog[in.GetStepId()]; ok {
		// As the gateway answers a failed build: no run, so no command.result entry.
		return &gatewayv1.RunCommandResponse{Result: &gatewayv1.CommandResult{ExitCode: -1,
			Error: "ImageBuildFailed: install step 1 exited 100", Stderr: log}}, nil
	}
	res := &gatewayv1.CommandResult{RunId: "run-" + in.GetStepId(), ExitCode: s.exit[in.GetStepId()], Stdout: s.stdout[in.GetStepId()]}
	if res.ExitCode != 0 {
		res.Stderr = "boom"
	}
	f.appendAudit(key, "command.result", f.openStage[key], in.GetSubjectHash(),
		map[string]any{"exitCode": res.ExitCode, "stdout": res.Stdout, "stderr": res.Stderr})
	if res.ExitCode == 0 {
		f.openSteps[key]++
	}
	return &gatewayv1.RunCommandResponse{Result: res}, nil
}

func (f *fakeGateway) WriteOutputSecret(ctx context.Context, in *gatewayv1.WriteOutputSecretRequest, _ ...grpc.CallOption) (*gatewayv1.WriteOutputSecretResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "WriteOutputSecret:"+in.GetManagedResource())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	name, err := f.writeOutputSecret(ctx, key, in.GetManagedResource(), in.GetValues())
	if err != nil {
		return nil, err
	}
	if f.outputs[key] == nil {
		f.outputs[key] = map[string]map[string]string{}
	}
	f.outputs[key][in.GetManagedResource()] = in.GetValues()
	return &gatewayv1.WriteOutputSecretResponse{SecretName: name}, nil
}

// writeOutputSecret writes the output Secret the ManagedResource names,
// controlled by it, as the gateway does; its data are the values as sent. An
// existing Secret anything else holds is refused, unless it moves (moves).
// f.mu must be held.
func (f *fakeGateway) writeOutputSecret(ctx context.Context, key types.NamespacedName, mrName string, values map[string]string) (string, error) {
	var mr lastv1alpha1.ManagedResource
	if err := f.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: mrName}, &mr); err != nil {
		return "", status.Error(codes.NotFound, err.Error())
	}
	if mr.Spec.OutputsSecretRef == nil {
		return "", status.Errorf(codes.InvalidArgument, "managed resource %s has no outputsSecretRef", mrName)
	}
	s := &corev1.Secret{}
	err := f.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: mr.Spec.OutputsSecretRef.Name}, s)
	switch {
	case apierrors.IsNotFound(err):
		s = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: mr.Spec.OutputsSecretRef.Name}}
	case err != nil:
		return "", status.Error(codes.Unavailable, err.Error())
	case !metav1.IsControlledBy(s, &mr) && !f.moves(ctx, key, s, &mr, f.openBody[key]):
		return "", status.Errorf(codes.PermissionDenied, "secret %s exists and is not %s's output secret", s.Name, mrName)
	}
	s.Data = map[string][]byte{}
	for k, v := range values {
		s.Data[k] = []byte(v)
	}
	s.OwnerReferences = []metav1.OwnerReference{controllerRef(&mr)}
	if s.ResourceVersion == "" {
		err = f.c.Create(ctx, s)
	} else {
		err = f.c.Update(ctx, s)
	}
	if err != nil {
		return "", status.Error(codes.Unavailable, err.Error())
	}
	return s.Name, nil
}

const kindManagedResource = "ManagedResource"

func controllerRef(mr *lastv1alpha1.ManagedResource) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: lastv1alpha1.GroupVersion.String(), Kind: kindManagedResource, Name: mr.Name, UID: mr.UID,
		Controller: new(true)}
}

// moves reports whether the Secret s, which mr does not control, moves to mr,
// as at the gateway: another ManagedResource mr's Intent controls holds it, and
// body declares it for mr's position.
func (f *fakeGateway) moves(ctx context.Context, key types.NamespacedName, s *corev1.Secret, mr *lastv1alpha1.ManagedResource, body plan.Body) bool {
	owner := metav1.GetControllerOf(s)
	if owner == nil || owner.Kind != kindManagedResource {
		return false
	}
	var holder lastv1alpha1.ManagedResource
	if err := f.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: owner.Name}, &holder); err != nil || holder.UID != owner.UID {
		return false
	}
	if c, mine := metav1.GetControllerOf(&holder), metav1.GetControllerOf(mr); c == nil || mine == nil || c.UID != mine.UID {
		return false
	}
	for i, r := range body.Resources {
		if lastv1alpha1.ManagedResourceName(key.Name, i) == mr.Name {
			return r.OutputsSecretRef != nil && r.OutputsSecretRef.Name == s.Name
		}
	}
	return false
}

// moveKeptSecrets moves each output Secret body declares to the ManagedResource
// that now declares it, as the gateway does before deleting stale ones. f.mu
// must be held.
func (f *fakeGateway) moveKeptSecrets(ctx context.Context, key types.NamespacedName, body plan.Body) error {
	for i, r := range body.Resources {
		if r.OutputsSecretRef == nil {
			continue
		}
		var mr lastv1alpha1.ManagedResource
		var s corev1.Secret
		if f.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: lastv1alpha1.ManagedResourceName(key.Name, i)}, &mr) != nil ||
			f.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: r.OutputsSecretRef.Name}, &s) != nil ||
			metav1.IsControlledBy(&s, &mr) || !f.moves(ctx, key, &s, &mr, body) {
			continue
		}
		s.OwnerReferences = []metav1.OwnerReference{controllerRef(&mr)}
		if err := f.c.Update(ctx, &s); err != nil {
			return status.Error(codes.Unavailable, err.Error())
		}
	}
	return nil
}

func (f *fakeGateway) EndPhase(ctx context.Context, in *gatewayv1.EndPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.EndPhaseResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	outcome := "failed"
	if in.GetSucceeded() {
		outcome = "succeeded"
	}
	f.record(key, "EndPhase:"+outcome)
	if s := f.scriptFor(key); len(s.endErrs) > 0 {
		err := s.endErrs[0]
		s.endErrs = s.endErrs[1:]
		return nil, err
	}
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		f.mu.Unlock()
		defer f.mu.Lock()
		return nil, f.noOpenPhase(ctx, key)
	}
	stage, uid, kept, body := f.openStage[key], f.openUID[key], f.openKept[key], f.openBody[key]
	f.closePhase(key, outcome, in.GetReason())
	if s := f.scriptFor(key); s.onEnd != nil {
		s.onEnd(in.GetSucceeded())
	}
	switch {
	case in.GetSucceeded() && stage == string(lastv1alpha1.StageDelete):
		// A succeeded Delete drops the record: no executed plan, and no phase opens again.
		delete(f.executed, key)
		delete(f.execBody, key)
		f.tornDown[key] = uid
	case in.GetSucceeded() && stage != string(lastv1alpha1.StageDiscovery) && stage != string(lastv1alpha1.StageRemediate):
		// A Discovery phase ran one inspection command, and a Remediate
		// repaired the executed plan: neither becomes the executed plan.
		f.executed[key], f.execBody[key] = in.GetSubjectHash(), body
	}
	if in.GetSucceeded() && (stage == string(lastv1alpha1.StageCreate) || stage == string(lastv1alpha1.StageUpdate)) {
		if err := f.moveKeptSecrets(ctx, key, body); err != nil {
			return nil, err
		}
		if err := f.deleteStale(ctx, key, uid, kept, stage, in.GetSubjectHash()); err != nil {
			return nil, err
		}
	}
	if s := f.scriptFor(key); s.loseEnd {
		s.loseEnd = false
		return nil, status.Error(codes.Unavailable, "the answer was lost")
	}
	return &gatewayv1.EndPhaseResponse{}, nil
}

// deleteStale deletes the ManagedResources a succeeded Create or Update dropped, those the
// Intent of uid controls named other than its first kept, and audits each as
// managedresource.deleted, as the gateway does: one already gone or terminating
// is skipped with no entry. Unlike the gateway, it does not retry a failed
// delete. f.mu must be held.
func (f *fakeGateway) deleteStale(ctx context.Context, key types.NamespacedName, uid types.UID, kept int, stage, subject string) error {
	var list lastv1alpha1.ManagedResourceList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	keep := map[string]bool{}
	for i := range kept {
		keep[lastv1alpha1.ManagedResourceName(key.Name, i)] = true
	}
	for i := range list.Items {
		mr := &list.Items[i]
		if owner := metav1.GetControllerOf(mr); owner == nil || owner.UID != uid || keep[mr.Name] || mr.DeletionTimestamp != nil {
			continue
		}
		err := f.c.Delete(ctx, mr, client.Preconditions{UID: &mr.UID})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return status.Error(codes.Unavailable, err.Error())
		}
		f.appendAudit(key, audit.TypeManagedResourceDeleted, stage, subject,
			map[string]any{"managedResource": mr.Name, "uid": string(mr.UID)})
	}
	return nil
}

// closePhase ends the Intent's open phase with its phase.end entry. f.mu must be held.
func (f *fakeGateway) closePhase(key types.NamespacedName, outcome, reason string) {
	f.appendAudit(key, "phase.end", f.openStage[key], f.open[key],
		map[string]any{"beginSeq": f.openBegin[key], "outcome": outcome, "reason": reason, "stepsRun": f.openSteps[key]})
	delete(f.open, key)
	delete(f.openSteps, key)
	delete(f.openStage, key)
	delete(f.openBegin, key)
	delete(f.openUID, key)
	delete(f.openKept, key)
	delete(f.openBody, key)
	delete(f.openAuto, key)
}

func (f *fakeGateway) RunProbes(ctx context.Context, in *gatewayv1.RunProbesRequest, _ ...grpc.CallOption) (*gatewayv1.RunProbesResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	f.record(key, runProbes)
	executed := f.executed[key]
	body, known := f.execBody[key]
	s := f.scriptFor(key)
	f.mu.Unlock()
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	if executed == "" {
		return nil, status.Error(codes.FailedPrecondition, "intent has no executed plan to probe")
	}
	var intent lastv1alpha1.Intent
	if err := f.c.Get(ctx, key, &intent); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	// The executed plan's probes, as the gateway runs them; the Intent's plan
	// stands in for one executed through setExecuted.
	resp := &gatewayv1.RunProbesResponse{PlanHash: executed}
	if !known && intent.Status.Plan != nil {
		body = plan.FromStatus(intent.Status.Plan)
	}
	for _, r := range body.Resources {
		for _, p := range r.Probes {
			o := &gatewayv1.ProbeOutcome{Id: p.ID, Passed: !s.failing[p.ID], Result: &gatewayv1.CommandResult{Stdout: s.probeOut[p.ID]}}
			if s.failing[p.ID] {
				o.Result.ExitCode = 1 // an assert that evaluates false: a result, and no error
			}
			resp.Outcomes = append(resp.Outcomes, o)
		}
	}
	if s.onProbe != nil {
		s.onProbe()
	}
	return resp, nil
}

// setOpen makes the gateway hold an open phase for subject, as if an earlier
// operator process had opened it.
func (f *fakeGateway) setOpen(key types.NamespacedName, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[key] = subject
}

// openPhase returns the subject of the Intent's open phase, or "" when none is open.
func (f *fakeGateway) openPhase(key types.NamespacedName) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[key]
}

// setExecuted makes the gateway's last executed plan for the Intent subject.
func (f *fakeGateway) setExecuted(key types.NamespacedName, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed[key] = subject
}

func (f *fakeGateway) argvOf(key types.NamespacedName, step string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.argv[key][step])
}

func (f *fakeGateway) outputsOf(key types.NamespacedName) map[string]map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outputs[key]
}

// lastRequest returns the last CallLLM request for an Intent.
func (f *fakeGateway) lastRequest(key types.NamespacedName) *gatewayv1.CallLLMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	reqs := f.llmReqs[key]
	if len(reqs) == 0 {
		return nil
	}
	return reqs[len(reqs)-1]
}

// updateScript changes an Intent's script in place.
func (f *fakeGateway) updateScript(key types.NamespacedName, change func(*script)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scripts[key] == nil {
		f.scripts[key] = &script{}
	}
	change(f.scripts[key])
}

func (f *fakeGateway) QueryAudit(_ context.Context, in *gatewayv1.QueryAuditRequest, _ ...grpc.CallOption) (*gatewayv1.QueryAuditResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(in.GetTypes(), []string{"phase.begin"}) && !slices.Equal(in.GetTypes(), []string{"phase.end"}) {
		// The used-plans read happens before every model turn, and the
		// executed-plan read before every teardown turn: not part of the call logs.
		f.record(key, queryAudit)
	}
	s := f.scriptFor(key)
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	if s.queryHook != nil {
		if err := s.queryHook(in.GetTypes()); err != nil {
			return nil, err
		}
	}
	limit := int(in.GetLimit())
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return nil, status.Error(codes.InvalidArgument, "limit must be 1..200")
	}
	resp := &gatewayv1.QueryAuditResponse{Truncated: s.auditTruncated}
	for _, e := range f.audit[key] {
		if s.auditTruncated || e.GetSeq() <= in.GetAfterSeq() || (len(in.GetTypes()) > 0 && !slices.Contains(in.GetTypes(), e.GetType())) {
			continue
		}
		if len(resp.Entries) == limit {
			resp.More = true
			break
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

// executedOf returns the hash of the Intent's last executed plan, or "".
func (f *fakeGateway) executedOf(key types.NamespacedName) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executed[key]
}

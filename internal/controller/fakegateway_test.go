package controller

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// Call log entries.
const (
	callLLM      = "CallLLM"
	beginPhase   = "BeginPhase"
	endSucceeded = "EndPhase:succeeded"
	endFailed    = "EndPhase:failed"
	runS1        = "RunCommand:s1"
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
	probeErr error             // RunProbes error
	onProbe  func()            // called during a successful RunProbes

	interrupt map[string]bool // step ID → the gateway restarts before it: the phase ends as interrupted, once

	beginErr error // returned by BeginPhase once the call is recorded
	queryErr error // returned by QueryAudit
	// queryHook, when set, is called with each QueryAudit call's types; an error it returns is the call's.
	queryHook      func(types []string) error
	auditTruncated bool // QueryAudit reports that the index dropped entries
}

// fakeGateway stands in for the gateway in envtest (spec §18). It enforces the
// phase rules the operator depends on (approval before BeginPhase, one open
// phase, calls only within it, probes only of an executed plan, Delete only for
// an Intent being deleted, nothing after a succeeded Delete) and records every
// call per Intent. Its phase.begin and phase.end entries carry the gateway's
// payloads, so agent.ExecutedPlan reads them.
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
	tornDown  map[types.NamespacedName]types.UID                    // Intent UID whose Delete succeeded
	used      map[string]bool                                       // Discovery subjects already opened: they are single-use
	executed  map[types.NamespacedName]string                       // hash of the last executed plan
	seq       uint64                                                // the last audit sequence number, across Intents
	audit     map[types.NamespacedName][]*gatewayv1.AuditEntry      // indexed entries, oldest first
}

func newFakeGateway(c client.Client) *fakeGateway {
	return &fakeGateway{c: c, scripts: map[types.NamespacedName]*script{}, log: map[types.NamespacedName][]string{},
		llmReqs: map[types.NamespacedName][]*gatewayv1.CallLLMRequest{}, argv: map[types.NamespacedName]map[string][]string{},
		outputs: map[types.NamespacedName]map[string]map[string]string{}, open: map[types.NamespacedName]string{},
		openStage: map[types.NamespacedName]string{}, openBegin: map[types.NamespacedName]uint64{},
		openUID: map[types.NamespacedName]types.UID{}, openSteps: map[types.NamespacedName]int{}, tornDown: map[types.NamespacedName]types.UID{}, used: map[string]bool{},
		executed: map[types.NamespacedName]string{}, audit: map[types.NamespacedName][]*gatewayv1.AuditEntry{}}
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

// approved reports whether an IntentApproval approves subject for the Intent's
// stage and none denies it.
func (f *fakeGateway) approved(ctx context.Context, key types.NamespacedName, stage, subject string) (bool, error) {
	var list lastv1alpha1.IntentApprovalList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return false, err
	}
	ok := false
	for _, a := range list.Items {
		if a.Spec.IntentRef.Name != key.Name || string(a.Spec.Stage) != stage || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return false, nil
		}
		ok = true
	}
	return ok, nil
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

// stageRule applies the gateway's Delete rules (Task 4): a Delete phase opens
// only for an Intent being deleted and only for a plan without resources, such
// an Intent opens no Create phase, and once its Delete succeeded it opens none.
func (f *fakeGateway) stageRule(intent *lastv1alpha1.Intent, stage string) error {
	deleting := intent.DeletionTimestamp != nil
	switch {
	case f.tornDown[client.ObjectKeyFromObject(intent)] == intent.UID:
		return status.Error(codes.FailedPrecondition, gatewayv1.MsgTornDown)
	case stage == string(lastv1alpha1.StageDelete) && !deleting:
		return status.Error(codes.FailedPrecondition, "intent is not being deleted")
	case stage == string(lastv1alpha1.StageDelete) && intent.Status.Plan != nil && len(intent.Status.Plan.Resources) > 0:
		return status.Error(codes.FailedPrecondition, "a Delete plan has no resources")
	case stage == string(lastv1alpha1.StageCreate) && deleting:
		return status.Error(codes.FailedPrecondition, "intent is being deleted; only a Delete or Discovery phase can open")
	}
	return nil
}

func (f *fakeGateway) BeginPhase(ctx context.Context, in *gatewayv1.BeginPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.BeginPhaseResponse, error) {
	key, stage, subject := keyOf(in.GetIntent()), in.GetStage(), in.GetSubjectHash()
	discovery := stage == string(lastv1alpha1.StageDiscovery)
	approved, err := f.approved(ctx, key, stage, subject)
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
	payload := map[string]any{}
	if p := intent.Status.Plan; !discovery && p != nil {
		canonical, err := plan.Canonical(plan.FromStatus(p))
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
	switch {
	case pendingErr != nil:
		return nil, pendingErr
	case ruleErr != nil:
		return nil, ruleErr
	case f.open[key] != "":
		return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
	case f.used[subject]:
		return nil, status.Error(codes.FailedPrecondition, "this discovery command was already run")
	case !approved:
		return nil, status.Error(codes.FailedPrecondition, "no IntentApproval approves this plan")
	}
	f.open[key], f.openStage[key], f.openUID[key] = subject, stage, intent.UID
	f.openBegin[key] = f.appendAudit(key, "phase.begin", stage, subject, payload)
	if discovery {
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

func (f *fakeGateway) WriteOutputSecret(_ context.Context, in *gatewayv1.WriteOutputSecretRequest, _ ...grpc.CallOption) (*gatewayv1.WriteOutputSecretResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "WriteOutputSecret:"+in.GetManagedResource())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	if f.outputs[key] == nil {
		f.outputs[key] = map[string]map[string]string{}
	}
	f.outputs[key][in.GetManagedResource()] = in.GetValues()
	return &gatewayv1.WriteOutputSecretResponse{SecretName: in.GetManagedResource() + "-outputs"}, nil
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
	stage, uid := f.openStage[key], f.openUID[key]
	f.closePhase(key, outcome, in.GetReason())
	switch {
	case in.GetSucceeded() && stage == string(lastv1alpha1.StageDelete):
		// A succeeded Delete drops the record: no executed plan, and no phase opens again.
		delete(f.executed, key)
		f.tornDown[key] = uid
	case in.GetSucceeded() && stage != string(lastv1alpha1.StageDiscovery):
		// A Discovery phase ran one inspection command; it never becomes the executed plan.
		f.executed[key] = in.GetSubjectHash()
	}
	if s := f.scriptFor(key); s.loseEnd {
		s.loseEnd = false
		return nil, status.Error(codes.Unavailable, "the answer was lost")
	}
	return &gatewayv1.EndPhaseResponse{}, nil
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
}

func (f *fakeGateway) RunProbes(ctx context.Context, in *gatewayv1.RunProbesRequest, _ ...grpc.CallOption) (*gatewayv1.RunProbesResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	f.record(key, runProbes)
	executed := f.executed[key]
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
	resp := &gatewayv1.RunProbesResponse{PlanHash: executed}
	if intent.Status.Plan != nil {
		for _, r := range intent.Status.Plan.Resources {
			for _, p := range r.Probes {
				o := &gatewayv1.ProbeOutcome{Id: p.ID, Passed: !s.failing[p.ID], Result: &gatewayv1.CommandResult{}}
				if s.failing[p.ID] {
					o.Result.ExitCode, o.Error = 1, "assertion failed"
				}
				resp.Outcomes = append(resp.Outcomes, o)
			}
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
		if e.GetSeq() <= in.GetAfterSeq() || (len(in.GetTypes()) > 0 && !slices.Contains(in.GetTypes(), e.GetType())) {
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

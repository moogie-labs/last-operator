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
)

// llmFunc answers one CallLLM request.
type llmFunc func(*gatewayv1.CallLLMRequest) (string, error)

// turns answers with the nth answer when the conversation holds n assistant
// messages, like the gateway's scripted provider.
func turns(answers ...string) llmFunc {
	return func(req *gatewayv1.CallLLMRequest) (string, error) {
		n := 0
		for _, m := range req.GetMessages() {
			if m.GetRole() == "assistant" {
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
	endErrs  []error           // errors returned by the next EndPhase calls, in order
	failing  map[string]bool   // probe ID → fails; other probes pass
	probeErr error             // RunProbes error
	onProbe  func()            // called during a successful RunProbes
}

// fakeGateway stands in for the gateway in envtest (spec §18). It enforces the
// phase rules the operator depends on (approval before BeginPhase, one open
// phase, calls only within it, probes only of an executed plan) and records
// every call per Intent.
type fakeGateway struct {
	gatewayv1.GatewayServiceClient // unimplemented RPCs panic

	c client.Client

	mu       sync.Mutex
	scripts  map[types.NamespacedName]*script
	log      map[types.NamespacedName][]string // "CallLLM", "BeginPhase", "RunCommand:s1", "EndPhase:failed", …
	llmReqs  map[types.NamespacedName][]*gatewayv1.CallLLMRequest
	argv     map[types.NamespacedName]map[string][]string          // step ID → argv sent
	outputs  map[types.NamespacedName]map[string]map[string]string // ManagedResource → values sent
	open     map[types.NamespacedName]string                       // subject of the open phase
	executed map[types.NamespacedName]string                       // hash of the last executed plan
	seq      uint64                                                // the last audit sequence number, across Intents
	audit    map[types.NamespacedName][]*gatewayv1.AuditEntry      // indexed entries, oldest first
}

func newFakeGateway(c client.Client) *fakeGateway {
	return &fakeGateway{c: c, scripts: map[types.NamespacedName]*script{}, log: map[types.NamespacedName][]string{},
		llmReqs: map[types.NamespacedName][]*gatewayv1.CallLLMRequest{}, argv: map[types.NamespacedName]map[string][]string{},
		outputs: map[types.NamespacedName]map[string]map[string]string{}, open: map[types.NamespacedName]string{},
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
// Create stage and none denies it.
func (f *fakeGateway) approved(ctx context.Context, key types.NamespacedName, subject string) (bool, error) {
	var list lastv1alpha1.IntentApprovalList
	if err := f.c.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
		return false, err
	}
	ok := false
	for _, a := range list.Items {
		if a.Spec.IntentRef.Name != key.Name || a.Spec.Stage != lastv1alpha1.StageCreate || a.Spec.SubjectHash != subject {
			continue
		}
		if a.Spec.Decision == lastv1alpha1.DecisionDeny {
			return false, nil
		}
		ok = true
	}
	return ok, nil
}

func (f *fakeGateway) BeginPhase(ctx context.Context, in *gatewayv1.BeginPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.BeginPhaseResponse, error) {
	key := keyOf(in.GetIntent())
	approved, err := f.approved(ctx, key, in.GetSubjectHash())
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "BeginPhase")
	switch {
	case f.open[key] != "":
		return nil, status.Error(codes.AlreadyExists, "a phase is already open for this intent")
	case !approved:
		return nil, status.Error(codes.FailedPrecondition, "no IntentApproval approves this plan")
	}
	f.open[key] = in.GetSubjectHash()
	return &gatewayv1.BeginPhaseResponse{}, nil
}

func (f *fakeGateway) RunCommand(_ context.Context, in *gatewayv1.RunCommandRequest, _ ...grpc.CallOption) (*gatewayv1.RunCommandResponse, error) {
	key := keyOf(in.GetIntent())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(key, "RunCommand:"+in.GetStepId())
	if f.open[key] == "" || f.open[key] != in.GetSubjectHash() {
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
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
	if err := s.runErr[in.GetStepId()]; err != nil {
		return nil, err
	}
	res := &gatewayv1.CommandResult{RunId: "run-" + in.GetStepId(), ExitCode: s.exit[in.GetStepId()], Stdout: s.stdout[in.GetStepId()]}
	if res.ExitCode != 0 {
		res.Stderr = "boom"
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

func (f *fakeGateway) EndPhase(_ context.Context, in *gatewayv1.EndPhaseRequest, _ ...grpc.CallOption) (*gatewayv1.EndPhaseResponse, error) {
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
		return nil, status.Error(codes.FailedPrecondition, "no open phase for this plan")
	}
	delete(f.open, key)
	if in.GetSucceeded() {
		f.executed[key] = in.GetSubjectHash()
	}
	return &gatewayv1.EndPhaseResponse{}, nil
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

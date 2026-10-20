package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
)

// noteRestarted tells the model why its session started over.
const noteRestarted = "The operator restarted and could not reload the earlier conversation, so planning starts over."

// sessionStore keeps planning conversations in memory (M3 spec §4.3). An entry
// serves only the Intent UID and session it was stored for.
type sessionStore struct {
	mu sync.Mutex
	m  map[types.NamespacedName]session
}

type session struct {
	uid  types.UID
	seq  int64
	conv agent.Conversation
}

func (s *sessionStore) get(in *lastv1alpha1.Intent) (agent.Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[client.ObjectKeyFromObject(in)]
	if !ok || e.uid != in.UID || e.seq != in.Status.PlanningSessionSeq {
		return nil, false
	}
	return e.conv, true
}

// put stores the Intent's conversation for its current session.
func (s *sessionStore) put(in *lastv1alpha1.Intent, conv agent.Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[types.NamespacedName]session{}
	}
	s.m[client.ObjectKeyFromObject(in)] = session{uid: in.UID, seq: in.Status.PlanningSessionSeq, conv: conv}
}

func (s *sessionStore) drop(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// conversation returns the Intent's planning session so far. fresh means a new
// session starts: the Intent has none yet, or its session can no longer be
// rebuilt. A session this process does not hold, after an operator restart or
// a leader change, is reloaded from the audit log (M3 spec §4.3). Each gateway
// call is bounded on its own.
func (r *IntentReconciler) conversation(ctx context.Context, in *lastv1alpha1.Intent) (conv agent.Conversation, fresh bool, err error) {
	if in.Status.PlanningSessionSeq == 0 {
		conv, err = r.newConversation(ctx, in, "")
		return conv, true, err
	}
	if conv, ok := r.sessions.get(in); ok {
		return conv, false, nil
	}
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	conv, err = agent.Reload(actx, r.Gateway, intentRef(in), uint64(in.Status.PlanningSessionSeq), recordedTurn(in))
	cancel()
	switch {
	case err == nil:
		logf.FromContext(ctx).Info("Reloaded planning conversation", "session", in.Status.PlanningSessionSeq, "messages", len(conv))
		r.sessions.put(in, conv)
		return conv, false, nil
	case errors.Is(err, agent.ErrNoReload) || terminal(err):
		logf.FromContext(ctx).Info("Could not reload planning conversation; starting a new session", "reason", err.Error())
		conv, err = r.newConversation(ctx, in, noteRestarted)
		return conv, true, err
	}
	return nil, false, err
}

// newConversation starts a session for the Intent's current spec, telling the
// model what the namespace's policy lets commands use. An Update session also
// shows the executed plan, read from the audit log; errNoExecutedPlan means it
// is no longer there. A Remediate session first confirms the drift with a
// probe run (remediateMessage); it has no note, since its first message
// carries everything the model needs.
func (r *IntentReconciler) newConversation(ctx context.Context, in *lastv1alpha1.Intent, note string) (agent.Conversation, error) {
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	defer cancel()
	env, err := r.environment(actx, in.Namespace)
	if err != nil {
		return nil, err
	}
	if planStage(in) == lastv1alpha1.StageRemediate {
		first, err := r.remediateMessage(ctx, in, env)
		if err != nil {
			return nil, err
		}
		return agent.NewConversation(first), nil
	}
	req := agent.Request{Namespace: in.Namespace, Name: in.Name, Text: in.Spec.Request, Answers: in.Spec.Answers,
		Feedback: replanFeedback(in), Note: note, Env: env}
	if planStage(in) == lastv1alpha1.StageUpdate {
		executed, _, found, err := agent.ExecutedPlan(actx, r.Gateway, intentRef(in))
		switch {
		case err != nil && status.Code(err) != codes.Unimplemented:
			return nil, err
		case err != nil || !found:
			return nil, errNoExecutedPlan
		}
		req.Executed = &executed
	}
	return agent.NewConversation(agent.FirstMessage(req)), nil
}

// errNoExecutedPlan marks an Update session whose executed plan the audit log
// no longer holds: the stage must be chosen again.
var errNoExecutedPlan = errors.New("the executed plan to update was not found")

// errBadPolicy marks a namespace whose effective policy cannot be resolved:
// the gateway would refuse every command, so planning fails.
var errBadPolicy = errors.New("the namespace's effective policy is invalid")

// environment returns what the namespace's effective policy lets commands use
// (M3 spec §4.4): the Providers that exist and the policy allows, the allowed
// egress, and whether scripts may run. The gateway enforces the policy; this
// only tells the model. It reads uncached, so a policy edited just before a
// session starts is current.
func (r *IntentReconciler) environment(ctx context.Context, ns string) (agent.Environment, error) {
	l, err := r.readLayers(ctx, ns)
	if err != nil {
		return agent.Environment{}, err
	}
	eff, err := policy.Resolve(l.op, l.policies)
	if err != nil {
		return agent.Environment{}, fmt.Errorf("%w: %v", errBadPolicy, err)
	}
	var providers lastv1alpha1.ProviderList
	if err := r.Reader.List(ctx, &providers); err != nil {
		return agent.Environment{}, err
	}
	slices.SortFunc(providers.Items, func(a, b lastv1alpha1.Provider) int { return strings.Compare(a.Name, b.Name) })
	env := agent.Environment{Egress: eff.Egress(), NoScripts: !eff.AllowScripts}
	for _, p := range providers.Items {
		if eff.AllowsProvider(p.Name) {
			env.Providers = append(env.Providers, agent.Provider{Name: p.Name, Description: p.Spec.Description})
		}
	}
	return env, nil
}

// layers are the policy layers a namespace's settings resolve across (main
// spec §6): the OperatorConfig, nil when there is none, and the namespace's
// IntentPolicies.
type layers struct {
	op       *lastv1alpha1.OperatorConfig
	policies []lastv1alpha1.IntentPolicy
}

// readLayers reads the namespace's policy layers uncached, so a policy edited
// just before is current.
func (r *IntentReconciler) readLayers(ctx context.Context, ns string) (layers, error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: operatorConfigName}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return layers{}, err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := r.Reader.List(ctx, &policies, client.InNamespace(ns)); err != nil {
		return layers{}, err
	}
	return layers{op: op, policies: policies.Items}, nil
}

// modes resolves the Intent's effective mode for each stage key across the
// layers and its spec.modes (main spec §6). The gateway resolves them itself
// and decides: the operator only shows them in status.effectiveModes and, for
// an Auto stage, does not wait for an approval before it asks the gateway.
func (l layers) modes(in *lastv1alpha1.Intent) policy.EffectiveModes {
	return policy.Modes(l.op, l.policies, in.Spec.Modes)
}

// pendingTurn returns the user turn the Intent's state owes the model (M3 spec
// §4.3), or "" when there is none: the answers to status.questions, or how the
// pending discovery command ended, which the Executed condition records.
func (r *IntentReconciler) pendingTurn(ctx context.Context, in *lastv1alpha1.Intent) (string, error) {
	if len(in.Status.Questions) > 0 {
		return agent.AnswersTurn(in.Status.Questions, in.Spec.Answers), nil
	}
	pc := in.Status.PendingCommand
	c := meta.FindStatusCondition(in.Status.Conditions, CondExecuted)
	if pc == nil || c == nil {
		return "", nil
	}
	switch c.Reason {
	case ReasonDiscoveryRan:
		turn, err := agent.DiscoveryResult(ctx, r.Gateway, intentRef(in), pc.Hash, uint64(in.Status.PlanningSessionSeq))
		if terminal(err) {
			return agent.ResultLostTurn(describe(err)), nil
		}
		return turn, err
	case ReasonDiscoveryDenied, ReasonDiscoveryFailed:
		return c.Message, nil
	}
	return "", nil
}

// recordedTurn admits only the model turn the Intent's status records: the
// questions in status.questions, or the command whose hash is the pending
// command's. A reloaded conversation must end with that turn, because
// pendingTurn answers it. With neither recorded nothing is admitted.
func recordedTurn(in *lastv1alpha1.Intent) func(agent.Turn) bool {
	if len(in.Status.Questions) > 0 {
		return func(t agent.Turn) bool {
			return t.Kind == agent.KindQuestions && reflect.DeepEqual(t.Questions, in.Status.Questions)
		}
	}
	if pc := in.Status.PendingCommand; pc != nil {
		return func(t agent.Turn) bool {
			if t.Kind != agent.KindCommand || t.Command == nil {
				return false
			}
			hash, _, err := plan.DiscoveryHash(*t.Command, pc.Nonce)
			return err == nil && hash == pc.Hash
		}
	}
	return func(agent.Turn) bool { return false }
}

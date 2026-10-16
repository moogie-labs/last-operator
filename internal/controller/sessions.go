package controller

import (
	"context"
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
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
// a leader change, is reloaded from the audit log (M3 spec §4.3).
func (r *IntentReconciler) conversation(ctx context.Context, in *lastv1alpha1.Intent) (conv agent.Conversation, fresh bool, err error) {
	if in.Status.PlanningSessionSeq == 0 {
		return newConversation(in, ""), true, nil
	}
	if conv, ok := r.sessions.get(in); ok {
		return conv, false, nil
	}
	conv, err = agent.Reload(ctx, r.Gateway, intentRef(in), uint64(in.Status.PlanningSessionSeq))
	switch {
	case err == nil:
		logf.FromContext(ctx).Info("Reloaded planning conversation", "session", in.Status.PlanningSessionSeq, "messages", len(conv))
		r.sessions.put(in, conv)
		return conv, false, nil
	case errors.Is(err, agent.ErrNoReload) || terminal(err):
		logf.FromContext(ctx).Info("Could not reload planning conversation; starting a new session", "reason", err.Error())
		return newConversation(in, noteRestarted), true, nil
	}
	return nil, false, err
}

// newConversation starts a session for the Intent's current spec.
func newConversation(in *lastv1alpha1.Intent, note string) agent.Conversation {
	return agent.NewConversation(agent.FirstMessage(agent.Request{Namespace: in.Namespace, Name: in.Name,
		Text: in.Spec.Request, Answers: in.Spec.Answers, Feedback: replanFeedback(in), Note: note}))
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

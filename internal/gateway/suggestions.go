package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	// suggestionDecisionsName is the ConfigMap, in the gateway namespace, that
	// maps the UID of each audited suggestion to the seq of its suggestion.decided
	// entry. It holds about 40 bytes a decision, of suggestions that still exist:
	// recordDecisions prunes the UIDs of deleted ones.
	suggestionDecisionsName      = "last-suggestion-decisions"
	suggestionDecisionsComponent = "suggestion-decisions"
	// suggestionDecisionInterval is how often the leader looks for new decisions.
	suggestionDecisionInterval = 30 * time.Second
	// suggestionListPage is the page size of the leader's suggestion list.
	suggestionListPage = 500
)

// suggestionDecided is the suggestion.decided audit payload.
type suggestionDecided struct {
	TargetScope     string          `json:"targetScope"`
	Namespace       string          `json:"namespace"`
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Trigger         string          `json:"trigger"`
	Decision        string          `json:"decision"`
	DecidedBy       string          `json:"decidedBy"`
	DecisionReason  string          `json:"decisionReason,omitempty"`
	Proposed        json.RawMessage `json:"proposed"`
	ProposedHash    string          `json:"proposedHash"`
}

// recordDecisions appends suggestion.decided for each suggestion a human has
// decided that the ConfigMap does not list yet, and records its UID after the
// entry is written. A failed write leaves the UID unrecorded, so the next run
// retries it. It lists the suggestions a page at a time and, once it has read
// every page, drops from the ConfigMap the UIDs of suggestions that no longer
// exist, before it records any new one.
func (g *Gateway) recordDecisions(ctx context.Context, w *audit.Writer) error {
	cms := g.kube.CoreV1().ConfigMaps(g.cfg.Namespace)
	cm, err := cms.Get(ctx, suggestionDecisionsName, metav1.GetOptions{})
	done := map[string]string{}
	switch {
	case apierrors.IsNotFound(err):
		cm = nil
	case err == nil:
		done = cm.Data
	default:
		return fmt.Errorf("gateway: load suggestion decisions: %w", err)
	}
	present := map[string]bool{}
	var todo []lastv1alpha1.CommandRuleSuggestion
	for cont := ""; ; {
		var list lastv1alpha1.CommandRuleSuggestionList
		if err := g.crd.List(ctx, &list, client.Limit(suggestionListPage), client.Continue(cont)); err != nil {
			return fmt.Errorf("gateway: list suggestions: %w", err)
		}
		for i := range list.Items {
			s := &list.Items[i]
			present[string(s.UID)] = true
			if s.Spec.Decision != lastv1alpha1.SuggestionAccepted && s.Spec.Decision != lastv1alpha1.SuggestionDenied {
				continue
			}
			if _, ok := done[string(s.UID)]; !ok {
				todo = append(todo, *s)
			}
		}
		if cont = list.Continue; cont == "" {
			break
		}
	}
	if cm, err = g.pruneDecisions(ctx, cm, present); err != nil {
		return err
	}
	for i := range todo {
		s := &todo[i]
		proposed, err := audit.Canonical(s.Spec.Proposed)
		if err != nil {
			return fmt.Errorf("gateway: encode suggestion %s/%s: %w", s.Namespace, s.Name, err)
		}
		sum := sha256.Sum256(proposed)
		e, err := w.Append(ctx, audit.Draft{Type: audit.TypeSuggestionDecided, Actor: sandbox.GatewayActor,
			Payload: suggestionDecided{TargetScope: string(s.Spec.TargetScope), Namespace: s.Namespace, Name: s.Name,
				UID: string(s.UID), ResourceVersion: s.ResourceVersion, Trigger: string(s.Spec.Trigger),
				Decision: string(s.Spec.Decision), DecidedBy: s.Spec.DecidedBy, DecisionReason: s.Spec.DecisionReason,
				Proposed: proposed, ProposedHash: "sha256:" + hex.EncodeToString(sum[:])}})
		if err != nil {
			return fmt.Errorf("gateway: record suggestion.decided for %s/%s: %w", s.Namespace, s.Name, err)
		}
		if cm, err = g.saveDecision(ctx, cm, string(s.UID), e.Seq); err != nil {
			return err
		}
	}
	return nil
}

// pruneDecisions drops from cm the UIDs not in present, the suggestions that
// still exist, and returns the saved ConfigMap; cm unchanged when there is
// nothing to drop.
func (g *Gateway) pruneDecisions(ctx context.Context, cm *corev1.ConfigMap, present map[string]bool) (*corev1.ConfigMap, error) {
	if cm == nil || !slices.ContainsFunc(slices.Collect(maps.Keys(cm.Data)), func(uid string) bool { return !present[uid] }) {
		return cm, nil
	}
	next := cm.DeepCopy()
	maps.DeleteFunc(next.Data, func(uid, _ string) bool { return !present[uid] })
	saved, err := g.kube.CoreV1().ConfigMaps(g.cfg.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	if err != nil {
		return cm, fmt.Errorf("gateway: prune suggestion decisions: %w", err)
	}
	return saved, nil
}

// saveDecision records uid -> seq, creating the ConfigMap if cm is nil, and
// returns the saved ConfigMap.
func (g *Gateway) saveDecision(ctx context.Context, cm *corev1.ConfigMap, uid string, seq uint64) (*corev1.ConfigMap, error) {
	cms := g.kube.CoreV1().ConfigMaps(g.cfg.Namespace)
	var saved *corev1.ConfigMap
	var err error
	if cm == nil {
		saved, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: suggestionDecisionsName, Namespace: g.cfg.Namespace,
				Labels: map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, labelComponent: suggestionDecisionsComponent}},
			Data: map[string]string{uid: strconv.FormatUint(seq, 10)},
		}, metav1.CreateOptions{})
	} else {
		next := cm.DeepCopy()
		if next.Data == nil {
			next.Data = map[string]string{}
		}
		next.Data[uid] = strconv.FormatUint(seq, 10)
		saved, err = cms.Update(ctx, next, metav1.UpdateOptions{})
	}
	if err != nil {
		slog.Error("Could not save suggestion decisions ConfigMap; the entry will be written again", "uid", uid, "err", err)
		return cm, fmt.Errorf("gateway: save suggestion decisions: %w", err)
	}
	return saved, nil
}

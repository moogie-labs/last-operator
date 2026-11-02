package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

// Event reasons of rule suggestions (M7 design §8).
const (
	ReasonSuggestionFailed     = "SuggestionFailed"
	ReasonSuggestionDropped    = "SuggestionDropped"
	ReasonSuggestionSuppressed = "SuggestionSuppressed"
	ReasonSuggestionLimit      = "SuggestionLimit"
	ReasonSuggestionCreated    = "SuggestionCreated"
)

const (
	// DefaultMaxPending is the most Pending suggestions a namespace holds before
	// the operator stops creating them (M7 design S6).
	DefaultMaxPending = 100
	// labelIntent names the Intent that prompted a suggestion.
	labelIntent = "last.moogie-labs.com/intent"
	// suggestTimeout bounds one suggest call. Its reply is a short list of
	// patterns, far smaller than a plan, so it gets less than agent.CallTimeout.
	suggestTimeout = 5 * time.Minute
	// suggestMaxTokens bounds a suggest reply.
	suggestMaxTokens = 8192
	// msgNoScript is the message of the gateway's scripted provider when it has
	// no script for a call (llm.ErrNoScript). For a suggest call that means no
	// proposal, not a failure (M7 design S10).
	msgNoScript = "llm: no script for this intent"
	// reasonCovered is suggest.Vet's reason for a proposal existing rules
	// already cover: nothing to suggest, and nothing worth an event.
	reasonCovered = "already covered by existing rules"
	// actionSuggest is the action of every suggestion event.
	actionSuggest = "Suggest"
)

// SuggestionCreator vets and creates the operator's rule suggestions, for every
// trigger (M7 design S5, S6, S8, §8). Client writes; Reader reads uncached, so a
// decision a human just made always counts.
type SuggestionCreator struct {
	Client   client.Client
	Reader   client.Reader
	Recorder events.EventRecorder
	// SystemNamespace holds Cluster-target suggestions.
	SystemNamespace string
	// MaxPending is the cap on Pending suggestions per namespace; 0 means DefaultMaxPending.
	MaxPending int
}

// Create vets p and creates it as a Pending CommandRuleSuggestion labelled with
// the Intent that prompted it. It returns the suggestion's name when it created
// it or found it already there (created tells which), and "" when p was
// dropped, skipped as already covered, suppressed by a Denied suggestion, or
// over the namespace's Pending cap. Each outcome but "already covered" emits its
// event on intent, which may be nil. The suppression check runs before the
// create, so a proposal identical to a Denied suggestion, which has its name,
// is reported as suppressed and never as found.
func (c *SuggestionCreator) Create(ctx context.Context, p suggest.Proposal, intent *lastv1alpha1.Intent) (name string, created bool, err error) {
	log := logf.FromContext(ctx)
	ns := p.Namespace
	if p.TargetScope == lastv1alpha1.RuleScopeCluster {
		ns = c.SystemNamespace
	}
	existing, err := c.readRules(ctx, p)
	if err != nil {
		return "", false, err
	}
	all, err := c.readSuggestions(ctx, p, ns)
	if err != nil {
		return "", false, err
	}
	var denied []suggest.Denied
	var deniedObjs []*lastv1alpha1.CommandRuleSuggestion
	pending := 0
	for i, s := range all {
		switch s.Spec.Decision {
		case lastv1alpha1.SuggestionDenied:
			denied = append(denied, suggest.Denied{Scope: s.Spec.TargetScope, Namespace: s.Namespace, Name: s.Name, Spec: s.Spec.Proposed})
			deniedObjs = append(deniedObjs, &all[i])
		case lastv1alpha1.SuggestionPending, "":
			if s.Namespace == ns {
				pending++
			}
		}
	}
	what := strings.Join(p.Spec.Match.Argv, " ")
	ok, reason, suppressedBy := suggest.Vet(p, existing, denied)
	switch {
	case ok:
	case suppressedBy != "":
		d := deniedObjs[slices.IndexFunc(denied, func(d suggest.Denied) bool { return d.Name == suppressedBy })]
		c.event(intent, d, corev1.EventTypeNormal, ReasonSuggestionSuppressed,
			"Did not suggest %q: it would let through everything the Denied suggestion %s/%s does", what, d.Namespace, d.Name)
		return "", false, nil
	case reason == reasonCovered:
		log.V(1).Info("Skipped a suggestion existing rules cover", "match", p.Spec.Match.Argv)
		return "", false, nil
	default:
		c.event(intent, nil, corev1.EventTypeNormal, ReasonSuggestionDropped, "Dropped the suggestion %q: %s", what, reason)
		return "", false, nil
	}
	if name, err = suggest.Name(p); err != nil {
		return "", false, err
	}
	for _, s := range all {
		if s.Namespace == ns && s.Name == name {
			return name, false, nil // the same proposal, from this or another trigger
		}
	}
	limit := c.MaxPending
	if limit <= 0 {
		limit = DefaultMaxPending
	}
	if pending >= limit {
		c.event(intent, nil, corev1.EventTypeWarning, ReasonSuggestionLimit,
			"Did not suggest %q: namespace %s already holds %d Pending suggestions", what, ns, limit)
		return "", false, nil
	}
	obj := &lastv1alpha1.CommandRuleSuggestion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: lastv1alpha1.CommandRuleSuggestionSpec{TargetScope: p.TargetScope, Trigger: p.Trigger, Rationale: p.Rationale,
			Proposed: p.Spec, Examples: p.Examples, Decision: lastv1alpha1.SuggestionPending},
	}
	if intent != nil {
		obj.Labels = map[string]string{labelIntent: intent.Name}
	}
	switch err := c.Client.Create(ctx, obj); {
	case apierrors.IsAlreadyExists(err):
		return name, false, nil
	case err != nil:
		return "", false, err
	}
	c.event(intent, obj, corev1.EventTypeNormal, ReasonSuggestionCreated, "Suggested the rule %s/%s for %q", ns, name, what)
	return name, true, nil
}

// readRules reads, uncached, the rules a proposal is checked against: every
// ClusterCommandRule and, for a Namespace target, its namespace's CommandRules.
func (c *SuggestionCreator) readRules(ctx context.Context, p suggest.Proposal) ([]rules.Rule, error) {
	var cluster lastv1alpha1.ClusterCommandRuleList
	if err := c.Reader.List(ctx, &cluster); err != nil {
		return nil, err
	}
	var local lastv1alpha1.CommandRuleList
	if p.TargetScope == lastv1alpha1.RuleScopeNamespace {
		if err := c.Reader.List(ctx, &local, client.InNamespace(p.Namespace)); err != nil {
			return nil, err
		}
	}
	rs, _ := rules.FromObjects(p.Namespace, cluster.Items, local.Items)
	return rs, nil
}

// readSuggestions reads, uncached, the suggestions a proposal is checked
// against: those of ns for a Namespace target, and every one for a Cluster
// target, which any denial counts against (S8).
func (c *SuggestionCreator) readSuggestions(ctx context.Context, p suggest.Proposal, ns string) ([]lastv1alpha1.CommandRuleSuggestion, error) {
	var list lastv1alpha1.CommandRuleSuggestionList
	var opts []client.ListOption
	if p.TargetScope != lastv1alpha1.RuleScopeCluster {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := c.Reader.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Ask makes one suggest call (M7 design S10) on behalf of ref for entries and
// returns the model's answers. A call the gateway's scripted provider has no
// script for returns no answers and no error: a missing script means no proposal.
func (c *SuggestionCreator) Ask(ctx context.Context, gw gatewayv1.GatewayServiceClient, ref *gatewayv1.IntentRef,
	entries []suggest.Entry) ([]suggest.Answer, error) {
	cctx, cancel := context.WithTimeout(ctx, suggestTimeout)
	defer cancel()
	resp, err := gw.CallLLM(cctx, &gatewayv1.CallLLMRequest{
		Intent:  ref,
		Purpose: gatewayv1.PurposeSuggest,
		Messages: []*gatewayv1.Message{
			{Role: "system", Content: suggest.SystemPrompt},
			{Role: "user", Content: suggest.Request(entries)},
		},
		MaxTokens:      suggestMaxTokens,
		ResponseSchema: suggest.Schema(),
	})
	switch {
	case noScript(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return suggest.Parse(resp.GetContent())
}

// noScript reports whether err is the gateway's answer to a call its scripted
// provider has no script for: FailedPrecondition with llm.ErrNoScript's message.
func noScript(err error) bool {
	s, ok := status.FromError(err)
	return err != nil && ok && s.Code() == codes.FailedPrecondition && strings.Contains(s.Message(), msgNoScript)
}

// event emits a suggestion event on intent, when there is one. related, the
// suggestion it is about, when there is one, also keeps the events about
// different suggestions apart: the recorder merges events that differ only in
// their note into one series.
func (c *SuggestionCreator) event(intent *lastv1alpha1.Intent, related *lastv1alpha1.CommandRuleSuggestion,
	kind, reason, format string, args ...any) {
	if intent == nil || c.Recorder == nil {
		return
	}
	var rel runtime.Object
	if related != nil {
		rel = related
	}
	c.Recorder.Eventf(intent, rel, kind, reason, actionSuggest, "%s", clip(fmt.Sprintf(format, args...)))
}

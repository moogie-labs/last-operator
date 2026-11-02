package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

// clusterSuggestionNamespace is where the operator keeps Cluster-target
// suggestions: its system namespace, by default (M7 design S1).
const clusterSuggestionNamespace = "last-system"

// opLabel marks text the operator wrote. The operator is untrusted (M7 design
// §2), so its text is printed under this label, each line behind "  | ", where it
// cannot pass for the CLI's own output.
const opLabel = "operator-proposed"

// The kinds of rule, and the label of a lint finding that does not refuse.
const (
	kindCommandRule        = "CommandRule"
	kindClusterCommandRule = "ClusterCommandRule"
	lintWarning            = "warning"
)

// op makes one piece of operator text safe to print on one line or in one table
// cell: terminal escapes and control characters stripped (suggest.Sanitize), and
// anything else that is not graphic, newlines and tabs included, written as
// \uXXXX (clean, then the tabs clean keeps), so it cannot add lines or cells.
func op(s string) string { return strings.ReplaceAll(clean(suggest.Sanitize(s)), "\t", `\u0009`) }

// opArgv is an operator-written argv, each element sanitized, quoted as a shell
// would need it.
func opArgv(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = suggest.Sanitize(a)
	}
	return shellWords(words)
}

// opPattern is an operator-written pattern, each element sanitized; an element
// that is empty or holds a space is quoted, so the elements stay apart.
func opPattern(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		w := op(a)
		if w == "" || strings.ContainsAny(w, " \t") {
			w = strconv.Quote(w)
		}
		words[i] = w
	}
	return strings.Join(words, " ")
}

// opLines prints multi-line operator text, each line behind "  | ".
func opLines(w io.Writer, s string) {
	for line := range strings.SplitSeq(strings.TrimRight(suggest.Sanitize(s), "\n"), "\n") {
		writef(w, "  | %s\n", clean(line))
	}
}

func opList[T ~string](xs []T) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = op(string(x))
	}
	return strings.Join(out, ",")
}

func isPending(s *lastv1alpha1.CommandRuleSuggestion) bool {
	return s.Spec.Decision == "" || s.Spec.Decision == lastv1alpha1.SuggestionPending
}

// suggestionRef names a suggestion for the user: its name in their namespace,
// else namespace/name.
func suggestionRef(s *lastv1alpha1.CommandRuleSuggestion, ns string) string {
	if s.Namespace == ns {
		return op(s.Name)
	}
	return op(s.Namespace) + "/" + op(s.Name)
}

// refusal says why the CLI refuses to accept a rule with spec, or "": a pattern
// that matches nothing or lets code vary (M7 design §2), from the CLI's own lint.
func refusal(spec lastv1alpha1.CommandRuleSpec) string {
	fs := rules.LintFindings(spec)
	for _, k := range []struct {
		kind rules.FindingKind
		what string
	}{
		{rules.KindNothing, "its pattern matches nothing"},
		{rules.KindCode, "the code it allows can vary"},
	} {
		if i := slices.IndexFunc(fs, func(f rules.Finding) bool { return f.Kind == k.kind }); i >= 0 {
			return k.what + " (" + op(fs[i].Message) + ")"
		}
	}
	return ""
}

func refusedError(s *lastv1alpha1.CommandRuleSuggestion, why string) error {
	return fmt.Errorf("refusing to accept suggestion %s/%s: %s; nothing was changed. "+
		"If you really want such a rule, write it by hand (check it with kubectl last rules lint)", op(s.Namespace), op(s.Name), why)
}

// coveringSuggestions lists the Pending suggestions whose rule would cover cmd at
// stage (M7 design S7): the Namespace-target ones in ns and the Cluster-target
// ones in the system namespace, by rules.Covers, never by label. A user who may
// not read the system namespace's suggestions gets the namespace's only.
func coveringSuggestions(ctx context.Context, k *Kube, ns string, stage lastv1alpha1.Stage,
	cmd lastv1alpha1.Command) ([]lastv1alpha1.CommandRuleSuggestion, error) {
	var local lastv1alpha1.CommandRuleSuggestionList
	if err := k.Client.List(ctx, &local, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("read the suggestions in %s: %w", ns, err)
	}
	// A suggestion in ns applies to it as a CommandRule there; in the system
	// namespace only a Cluster-target one applies elsewhere, as a ClusterCommandRule.
	var out []lastv1alpha1.CommandRuleSuggestion
	keep := func(items []lastv1alpha1.CommandRuleSuggestion, scopes ...lastv1alpha1.RuleScope) {
		for _, s := range items {
			if isPending(&s) && slices.Contains(scopes, s.Spec.TargetScope) && rules.Covers(s.Spec.Proposed, stage, cmd) {
				out = append(out, s)
			}
		}
	}
	if ns == clusterSuggestionNamespace {
		keep(local.Items, lastv1alpha1.RuleScopeNamespace, lastv1alpha1.RuleScopeCluster)
		return out, nil
	}
	keep(local.Items, lastv1alpha1.RuleScopeNamespace)
	var sys lastv1alpha1.CommandRuleSuggestionList
	if err := k.Client.List(ctx, &sys, client.InNamespace(clusterSuggestionNamespace)); err == nil {
		keep(sys.Items, lastv1alpha1.RuleScopeCluster)
	}
	return out, nil
}

// baseline is the rule the CLI's own g choice would create for one command, which
// a proposal is compared with (suggest.Broader).
type baseline struct {
	spec lastv1alpha1.CommandRuleSpec
	what string // the command it is for
	note string // what the comparison leaves out, if anything
}

// exampleBaseline is the g rule for a suggestion's first example, at its first
// stage, for a suggestion shown outside an approval. why says why there is none.
func exampleBaseline(s *lastv1alpha1.CommandRuleSuggestion) (g *baseline, why string) {
	if len(s.Spec.Examples) == 0 {
		return nil, "It has no examples, so it is not compared with the rule g would create."
	}
	if len(s.Spec.Proposed.Stages) == 0 {
		return nil, "It names no stage, so it is not compared with the rule g would create."
	}
	ex, stage := s.Spec.Examples[0], s.Spec.Proposed.Stages[0]
	base, ok := suggest.RuleSpecFor(plan.Body{Steps: []lastv1alpha1.Command{ex}}, ex, []lastv1alpha1.Stage{stage}, s.Spec.Proposed.Providers)
	if !ok {
		return nil, "No rule can hold its first example, so it is not compared with the rule g would create."
	}
	base.Match = rules.Generalize(ex)
	g = &baseline{spec: base, what: "its first example at stage " + op(string(stage))}
	if len(ex.Providers) == 0 {
		g.note = "The first example names no provider, so the providers are not compared."
	}
	return g, ""
}

// showSuggestion prints a suggestion as an approver reads it before deciding
// (M7 design §5): the rule accepting it creates, with spec as its rule; the CLI's
// own lint; the examples and whether the pattern matches each; every place it is
// broader than g; and the operator's description and rationale, labelled. It
// returns why the CLI refuses to accept spec, or "".
func showSuggestion(w io.Writer, s *lastv1alpha1.CommandRuleSuggestion, spec lastv1alpha1.CommandRuleSpec, g *baseline, noG string) string {
	writef(w, "Suggestion %s/%s: %s, trigger %s\n", op(s.Namespace), op(s.Name), op(string(decisionOf(s))), op(string(s.Spec.Trigger)))
	if !isPending(s) {
		writef(w, "Decided by %s", op(s.Spec.DecidedBy))
		if s.Spec.DecisionReason != "" {
			writef(w, ": %s", op(s.Spec.DecisionReason))
		}
		writeln(w)
	}
	kind, where := ruleTarget(s)
	writef(w, "Accepting it creates %s %s%s with this rule (%s):\n", kind, where, op(s.Name), opLabel)
	writef(w, "  match:     %s\n", opPattern(spec.Match.Argv))
	writef(w, "  stages:    %s\n", opList(spec.Stages))
	writef(w, "  providers: %s\n", opList(spec.Providers))
	if len(spec.Egress) > 0 {
		writef(w, "  egress:    %s\n", opList(spec.Egress))
	}
	for _, in := range spec.Install {
		writef(w, "  install:   %s\n", opPattern(in.Argv))
	}
	return showChecks(w, s, spec, g, noG)
}

// showChecks prints what the CLI works out for spec itself: its lint, the
// examples it matches, and where it is broader than g; then the operator's
// description and rationale. It returns the refusal, if any.
func showChecks(w io.Writer, s *lastv1alpha1.CommandRuleSuggestion, spec lastv1alpha1.CommandRuleSpec, g *baseline, noG string) string {
	writeln(w, "Lint (worked out by kubectl-last):")
	fs := rules.LintFindings(spec)
	if len(fs) == 0 {
		writeln(w, "  none")
	}
	for _, f := range fs {
		kind := lintWarning
		switch f.Kind {
		case rules.KindNothing:
			kind = "refused, matches nothing"
		case rules.KindCode:
			kind = "refused, code can vary"
		}
		writef(w, "  %s: %s\n", kind, op(f.Message))
	}
	if len(s.Spec.Examples) > 0 {
		writef(w, "Examples (%s), checked against the match by kubectl-last:\n", opLabel)
		for _, ex := range s.Spec.Examples {
			verdict := "does not match"
			if rules.Match(spec.Match.Argv, ex.Run.Argv) {
				verdict = "matches"
			}
			writef(w, "  %-16s | %s\n", verdict, opArgv(ex.Run.Argv))
		}
	}
	if g == nil {
		writeln(w, noG)
	} else {
		lines := suggest.Broader(spec, g.spec)
		if len(lines) == 0 {
			writef(w, "Nowhere broader than the rule g would create for %s (%s).\n", g.what, opPattern(g.spec.Match.Argv))
		} else {
			writef(w, "Broader than the rule g would create for %s (%s):\n", g.what, opPattern(g.spec.Match.Argv))
			for _, l := range lines {
				writef(w, "  - %s\n", op(l))
			}
		}
		if g.note != "" {
			writeln(w, g.note)
		}
	}
	if spec.Description != "" {
		writef(w, "Description (%s):\n", opLabel)
		opLines(w, spec.Description)
	}
	if s.Spec.Rationale != "" {
		writef(w, "Rationale (%s):\n", opLabel)
		opLines(w, s.Spec.Rationale)
	}
	return refusal(spec)
}

func decisionOf(s *lastv1alpha1.CommandRuleSuggestion) lastv1alpha1.SuggestionDecision {
	if s.Spec.Decision == "" {
		return lastv1alpha1.SuggestionPending
	}
	return s.Spec.Decision
}

// ruleTarget is the kind of rule accepting s creates, and the namespace prefix
// of its name.
func ruleTarget(s *lastv1alpha1.CommandRuleSuggestion) (kind, where string) {
	if s.Spec.TargetScope == lastv1alpha1.RuleScopeCluster {
		return kindClusterCommandRule, ""
	}
	return kindCommandRule, op(s.Namespace) + "/"
}

// acceptSuggestion creates s's rule with spec as the user, then records the
// decision (M7 design §5). The rule comes first, so a user who may not create it
// leaves the suggestion Pending; an existing rule with the same spec counts as
// created, so a second run after a failed decision finishes the job. made names
// the rule created, if one was.
func acceptSuggestion(ctx context.Context, k *Kube, w io.Writer, s *lastv1alpha1.CommandRuleSuggestion,
	spec lastv1alpha1.CommandRuleSpec, user, reason string) (made string, _ error) {
	spec.Source = &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginSuggestion,
		SuggestionRef: &lastv1alpha1.LocalRef{Name: s.Name}, CreatedBy: user}
	kind, where := ruleTarget(s)
	meta := metav1.ObjectMeta{Name: s.Name}
	var obj client.Object
	if s.Spec.TargetScope == lastv1alpha1.RuleScopeCluster {
		obj = &lastv1alpha1.ClusterCommandRule{ObjectMeta: meta, Spec: spec}
	} else {
		meta.Namespace = s.Namespace
		obj = &lastv1alpha1.CommandRule{ObjectMeta: meta, Spec: spec}
	}
	rule := kind + " " + where + op(s.Name)
	pending := fmt.Sprintf("suggestion %s/%s is still Pending", op(s.Namespace), op(s.Name))
	err := k.Client.Create(ctx, obj)
	switch {
	case err == nil:
		writef(w, "Created %s\n", rule)
		made = rule
	case apierrors.IsAlreadyExists(err):
		same, serr := sameSpec(ctx, k, obj, spec)
		switch {
		case serr != nil:
			return "", fmt.Errorf("read the existing %s: %w; %s", rule, serr, pending)
		case !same:
			return "", fmt.Errorf("%s already exists and its spec differs from the suggestion's; %s", rule, pending)
		}
		writef(w, "%s already exists\n", rule)
	default:
		return "", fmt.Errorf("create %s: %w; %s", rule, err, pending)
	}
	if err := decideSuggestion(ctx, k, w, s, lastv1alpha1.SuggestionAccepted, user, reason); err != nil {
		if _, ok := errors.AsType[*decidedOtherwise](err); ok {
			return made, fmt.Errorf("%w; %s stays in place: delete it if you agree with that decision", err, rule)
		}
		return made, fmt.Errorf("%w; %s is in place, and running kubectl last rules suggestions accept %s again records the decision",
			err, rule, op(s.Name))
	}
	return made, nil
}

// decidedOtherwise is a suggestion someone else decided the other way first.
type decidedOtherwise struct {
	s *lastv1alpha1.CommandRuleSuggestion
}

func (e *decidedOtherwise) Error() string {
	return fmt.Sprintf("suggestion %s/%s was %s by %s meanwhile", op(e.s.Namespace), op(e.s.Name),
		op(string(e.s.Spec.Decision)), op(e.s.Spec.DecidedBy))
}

// decideSuggestion records decision on s as user, with the resourceVersion read.
// On a conflict it reads s again: someone else's same decision counts as done
// and names them; theirs the other way is a *decidedOtherwise. A conflict on a
// suggestion still Pending is another change (its spec is immutable, S2), so it
// tries again.
func decideSuggestion(ctx context.Context, k *Kube, w io.Writer, s *lastv1alpha1.CommandRuleSuggestion,
	decision lastv1alpha1.SuggestionDecision, user, reason string) error {
	cur := s.DeepCopy()
	for range 3 {
		cur.Spec.Decision, cur.Spec.DecidedBy, cur.Spec.DecisionReason = decision, user, reason
		err := k.Client.Update(ctx, cur)
		if err == nil {
			writef(w, "%s suggestion %s/%s as %s\n", decision, op(s.Namespace), op(s.Name), user)
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("record the decision on suggestion %s/%s: %w", op(s.Namespace), op(s.Name), err)
		}
		var now lastv1alpha1.CommandRuleSuggestion
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(s), &now); err != nil {
			return fmt.Errorf("read suggestion %s/%s again: %w", op(s.Namespace), op(s.Name), err)
		}
		if !isPending(&now) {
			if now.Spec.Decision == decision {
				writef(w, "Suggestion %s/%s was already %s by %s\n", op(s.Namespace), op(s.Name), decision, op(now.Spec.DecidedBy))
				return nil
			}
			return &decidedOtherwise{s: &now}
		}
		cur = &now
	}
	return fmt.Errorf("record the decision on suggestion %s/%s: it kept changing", op(s.Namespace), op(s.Name))
}

func getSuggestion(ctx context.Context, k *Kube, name string) (*lastv1alpha1.CommandRuleSuggestion, error) {
	var s lastv1alpha1.CommandRuleSuggestion
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: name}, &s); err != nil {
		if apierrors.IsNotFound(err) && k.Namespace != clusterSuggestionNamespace {
			return nil, fmt.Errorf("%w (suggestions for ClusterCommandRules are in %s: add -n %s)", err,
				clusterSuggestionNamespace, clusterSuggestionNamespace)
		}
		return nil, err
	}
	return &s, nil
}

// runAccept is rules suggestions accept: show, optionally edit the match, confirm,
// then create the rule and record the decision.
func runAccept(ctx context.Context, d Deps, k *Kube, name string, edit, yes bool) error {
	s, err := getSuggestion(ctx, k, name)
	if err != nil {
		return err
	}
	switch decisionOf(s) {
	case lastv1alpha1.SuggestionPending:
	case lastv1alpha1.SuggestionAccepted:
		writef(d.Out, "Suggestion %s/%s was already Accepted by %s\n", op(s.Namespace), op(s.Name), op(s.Spec.DecidedBy))
		return nil
	default:
		return fmt.Errorf("suggestion %s/%s was %s by %s; nothing was changed", op(s.Namespace), op(s.Name),
			op(string(s.Spec.Decision)), op(s.Spec.DecidedBy))
	}
	spec := *s.Spec.Proposed.DeepCopy()
	spec.Source = nil
	g, noG := exampleBaseline(s)
	refused := showSuggestion(d.Out, s, spec, g, noG)
	var p *prompter
	if edit || !yes {
		p = newPrompter(d.In, d.Out)
	}
	reason := ""
	if edit {
		for {
			line, err := p.ask(ctx, "Press Enter to keep this match, or type an edited one (space-separated): ")
			if err != nil {
				return err
			}
			if line == "" {
				if refused == "" {
					break
				}
				writef(d.Out, "This match cannot be accepted: %s; edit it.\n", refused)
				continue
			}
			spec.Match = lastv1alpha1.ArgvPattern{Argv: strings.Fields(line)}
			writef(d.Out, "Edited match: %s\n", opPattern(spec.Match.Argv))
			refused = showChecks(d.Out, s, spec, g, noG)
		}
		if !slices.Equal(spec.Match.Argv, s.Spec.Proposed.Match.Argv) {
			reason = "accepted with an edited match: " + strings.Join(spec.Match.Argv, " ")
		}
	}
	if refused != "" {
		return refusedError(s, refused)
	}
	kind, where := ruleTarget(s)
	if !yes {
		line, err := p.ask(ctx, fmt.Sprintf("Create %s %s%s and accept the suggestion? [y/N]: ", kind, where, op(s.Name)))
		if err != nil {
			return err
		}
		if !isYes(line) {
			writeln(d.Out, "Nothing was changed.")
			return nil
		}
	}
	user, err := k.WhoAmI(ctx)
	if err != nil {
		return err
	}
	_, err = acceptSuggestion(ctx, k, d.Out, s, spec, user, reason)
	return err
}

func isYes(s string) bool { return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes") }

func runDeny(ctx context.Context, d Deps, k *Kube, name, reason string) error {
	s, err := getSuggestion(ctx, k, name)
	if err != nil {
		return err
	}
	switch decisionOf(s) {
	case lastv1alpha1.SuggestionPending:
	case lastv1alpha1.SuggestionDenied:
		writef(d.Out, "Suggestion %s/%s was already Denied by %s\n", op(s.Namespace), op(s.Name), op(s.Spec.DecidedBy))
		return nil
	default:
		return fmt.Errorf("suggestion %s/%s was %s by %s; nothing was changed", op(s.Namespace), op(s.Name),
			op(string(s.Spec.Decision)), op(s.Spec.DecidedBy))
	}
	user, err := k.WhoAmI(ctx)
	if err != nil {
		return err
	}
	return decideSuggestion(ctx, k, d.Out, s, lastv1alpha1.SuggestionDenied, user, reason)
}

// runListSuggestions prints the Pending suggestions (every one with all) of ns,
// or of every namespace, with the CLI's own lint count, never the operator's
// breadthWarnings.
func runListSuggestions(ctx context.Context, d Deps, k *Kube, allNamespaces, all bool) error {
	var list lastv1alpha1.CommandRuleSuggestionList
	var opts []client.ListOption
	if !allNamespaces {
		opts = append(opts, client.InNamespace(k.Namespace))
	}
	if err := k.Client.List(ctx, &list, opts...); err != nil {
		return fmt.Errorf("read the suggestions: %w", err)
	}
	items := slices.DeleteFunc(list.Items, func(s lastv1alpha1.CommandRuleSuggestion) bool { return !all && !isPending(&s) })
	slices.SortFunc(items, func(a, b lastv1alpha1.CommandRuleSuggestion) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	if len(items) == 0 {
		what, where := "pending suggestions", " in "+k.Namespace
		if all {
			what = "suggestions"
		}
		if allNamespaces {
			where = ""
		}
		writef(d.Out, "No %s%s.\n", what, where)
		return nil
	}
	tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
	var head []string
	if allNamespaces {
		head = append(head, "NAMESPACE")
	}
	head = append(head, "NAME", "SCOPE", "TRIGGER")
	if all {
		head = append(head, "DECISION")
	}
	head = append(head, "MATCH", "STAGES", "PROVIDERS", "AGE", "LINT")
	writeln(tw, strings.Join(head, "\t"))
	for i := range items {
		s := &items[i]
		var row []string
		if allNamespaces {
			row = append(row, op(s.Namespace))
		}
		row = append(row, op(s.Name), op(string(s.Spec.TargetScope)), op(string(s.Spec.Trigger)))
		if all {
			row = append(row, op(string(decisionOf(s))))
		}
		age := "-"
		if !s.CreationTimestamp.IsZero() {
			age = duration.HumanDuration(time.Since(s.CreationTimestamp.Time))
		}
		row = append(row, opPattern(s.Spec.Proposed.Match.Argv), opList(s.Spec.Proposed.Stages), opList(s.Spec.Proposed.Providers),
			age, lintSummary(s.Spec.Proposed))
		writeln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// lintSummary is the count of the CLI's own lint findings for spec, and whether
// it would refuse to accept it.
func lintSummary(spec lastv1alpha1.CommandRuleSpec) string {
	n := len(rules.LintFindings(spec))
	out := "none"
	switch {
	case n == 1:
		out = "1 warning"
	case n > 1:
		out = fmt.Sprintf("%d warnings", n)
	}
	if refusal(spec) != "" {
		out += ", refused"
	}
	return out
}

// offeredSuggestions is what approve's s choice offers for e: the covering
// suggestions (S7) the CLI would accept. It notes each one it does not offer.
func offeredSuggestions(ctx context.Context, k *Kube, w io.Writer, ns string, e entry) []lastv1alpha1.CommandRuleSuggestion {
	found, err := coveringSuggestions(ctx, k, ns, e.stage, e.cmd)
	if err != nil {
		writef(w, "Could not read the suggested rules, so none is offered: %v\n", err)
		return nil
	}
	var out []lastv1alpha1.CommandRuleSuggestion
	for _, s := range found {
		if why := refusal(s.Spec.Proposed); why != "" {
			writef(w, "Suggestion %s covers %s but is not offered: %s.\n", suggestionRef(&s, ns), clean(e.id), why)
			continue
		}
		out = append(out, s)
	}
	if len(out) > 0 {
		names := make([]string, len(out))
		for i := range out {
			names[i] = suggestionRef(&out[i], ns)
		}
		writef(w, "Suggested rules that cover %s: %s\n", clean(e.id), strings.Join(names, ", "))
	}
	return out
}

// chooseSuggestion puts approve's s choice to the approver: which suggestion,
// when several cover e, then the full proposal compared with g, and a
// confirmation. It returns nil when the approver picks none.
func chooseSuggestion(ctx context.Context, p *prompter, w io.Writer, ns string, e entry,
	offers []lastv1alpha1.CommandRuleSuggestion, g *baseline) (*choice, error) {
	pick := &offers[0]
	if len(offers) > 1 {
		for i := range offers {
			writef(w, "  %d. %s: %s\n", i+1, suggestionRef(&offers[i], ns), opPattern(offers[i].Spec.Proposed.Match.Argv))
		}
		for {
			line, err := p.ask(ctx, fmt.Sprintf("Which suggestion? [1-%d, Enter for none]: ", len(offers)))
			if err != nil {
				return nil, err
			}
			if line == "" {
				return nil, nil //nolint:nilnil // nil means none
			}
			if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(offers) {
				pick = &offers[n-1]
				break
			}
			writef(w, "Answer a number from 1 to %d.\n", len(offers))
		}
	}
	spec := *pick.Spec.Proposed.DeepCopy()
	spec.Source = nil
	showSuggestion(w, pick, spec, g, "")
	line, err := p.ask(ctx, fmt.Sprintf("Accept suggestion %s for %s? [y/N]: ", suggestionRef(pick, ns), clean(e.id)))
	if err != nil {
		return nil, err
	}
	if !isYes(line) {
		return nil, nil //nolint:nilnil // nil means none
	}
	return &choice{id: e.id, spec: spec, sugg: pick}, nil
}

// printCoveringSuggestions lists, for each uncovered entry, the Pending
// suggestions that would cover it (S7). It prints nothing when the user may not
// read suggestions.
func printCoveringSuggestions(ctx context.Context, w io.Writer, k *Kube, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, uncovered []string) {
	first := true
	for _, e := range entriesOf(in, stage) {
		if !slices.Contains(uncovered, e.id) {
			continue
		}
		found, err := coveringSuggestions(ctx, k, in.Namespace, e.stage, e.cmd)
		if err != nil {
			return
		}
		if len(found) == 0 {
			continue
		}
		if first {
			writeln(w, "Suggested rules that would cover them (read one with kubectl last rules suggestions show NAME):")
			first = false
		}
		names := make([]string, len(found))
		for i := range found {
			names[i] = suggestionRef(&found[i], in.Namespace)
		}
		writef(w, "  %s: %s\n", clean(e.id), strings.Join(names, ", "))
	}
}

func newSuggestionsCommand(d Deps, kf *KubeFlags) *cobra.Command {
	var allNamespaces, all bool
	cmd := &cobra.Command{
		Use:   "suggestions",
		Short: "List the rules the operator suggests, and accept or deny them",
		Long: "List the Pending CommandRuleSuggestions in the namespace (every one with --all), with kubectl-last's own " +
			"lint count. The operator is untrusted: read a suggestion with show before you accept it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			return runListSuggestions(cmd.Context(), d, k, allNamespaces, all)
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "list the suggestions of every namespace")
	cmd.Flags().BoolVar(&all, "all", false, "list decided suggestions too")

	show := &cobra.Command{
		Use:   "show NAME",
		Short: "Show a suggestion in full: its rule, lint, examples, and where it is broader than the generalized rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			s, err := getSuggestion(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			spec := *s.Spec.Proposed.DeepCopy()
			spec.Source = nil
			g, noG := exampleBaseline(s)
			if why := showSuggestion(d.Out, s, spec, g, noG); why != "" && isPending(s) {
				writef(d.Out, "kubectl-last will not accept it: %s.\n", why)
			}
			return nil
		},
	}

	var edit, yes bool
	accept := &cobra.Command{
		Use:   "accept NAME",
		Short: "Show a suggestion, then create its rule as you and mark it Accepted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			return runAccept(cmd.Context(), d, k, args[0], edit, yes)
		},
	}
	accept.Flags().BoolVar(&edit, "edit", false, "type an edited match before accepting; it is linted again")
	accept.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation (the suggestion is still printed in full)")

	var reason string
	deny := &cobra.Command{
		Use:   "deny NAME",
		Short: "Mark a suggestion Denied, so the operator never suggests it, or anything broader, again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			return runDeny(cmd.Context(), d, k, args[0], reason)
		},
	}
	deny.Flags().StringVar(&reason, "reason", "", "why (recorded as decisionReason)")

	cmd.AddCommand(show, accept, deny)
	return cmd
}

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/rules"
)

// labelIntent names the Intent whose approval created a rule.
const labelIntent = "last.moogie-labs.com/intent"

// stepRefSyntax is the reference to another step's output; a command that uses
// it has an argv no human has seen, so no rule can hold it (M6 design R3).
const stepRefSyntax = "${steps."

// Modes of --always-allow: a bare flag prompts, the others apply one choice to
// every uncovered command.
const (
	allowPrompt      = "prompt"
	allowExact       = "exact"
	allowGeneralized = "generalized"
)

// allowOpts are the flags of approve --always-allow.
type allowOpts struct {
	mode    string
	stages  []lastv1alpha1.Stage // replaces every rule's stages when set
	cluster bool
	// providers are given to a command that names none, when the plan names none either.
	providers []string
}

// entry is one command a plan or a discovery command asks to run, as
// rules.PlanCoverage names it.
type entry struct {
	id    string
	cmd   lastv1alpha1.Command
	stage lastv1alpha1.Stage // the stage its rule defaults to
}

// parseStages reads a comma list of stages and checks each against the Stage enum.
func parseStages(list string) ([]lastv1alpha1.Stage, error) {
	valid := []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery, lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate,
		lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete}
	var out []lastv1alpha1.Stage
	for s := range strings.SplitSeq(list, ",") {
		st := lastv1alpha1.Stage(strings.TrimSpace(s))
		if !slices.Contains(valid, st) {
			return nil, fmt.Errorf("--stages: %q is not a stage (want some of %v)", clean(s), valid)
		}
		if !slices.Contains(out, st) {
			out = append(out, st)
		}
	}
	return out, nil
}

// readRules reads the rules that apply in ns, as the user: every
// ClusterCommandRule and the namespace's CommandRules, in coverage order.
func readRules(ctx context.Context, k *Kube, ns string) ([]rules.Rule, error) {
	var cl lastv1alpha1.ClusterCommandRuleList
	if err := k.Client.List(ctx, &cl); err != nil {
		return nil, fmt.Errorf("read the ClusterCommandRules: %w", err)
	}
	var nl lastv1alpha1.CommandRuleList
	if err := k.Client.List(ctx, &nl, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("read the CommandRules: %w", err)
	}
	rs, _ := rules.FromObjects(ns, cl.Items, nl.Items)
	return rs, nil
}

// entriesOf lists every command the subject asks to run, in coverage order: the
// pending discovery command, or the plan's steps, then each resource's probes
// and teardown (a Create or Update plan only, as rules.PlanCoverage counts them).
func entriesOf(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) []entry {
	if stage == lastv1alpha1.StageDiscovery {
		pc := in.Status.PendingCommand
		cmd := pc.Command
		cmd.ID = plan.DiscoveryStepID
		return []entry{{id: plan.DiscoveryStepID, cmd: cmd, stage: stage}}
	}
	p := in.Status.Plan
	var out []entry
	for _, s := range p.Steps {
		out = append(out, entry{id: s.ID, cmd: s, stage: stage})
	}
	if stage != lastv1alpha1.StageCreate && stage != lastv1alpha1.StageUpdate {
		return out
	}
	for ri, r := range p.Resources {
		for _, pr := range r.Probes {
			id := "probe:" + pr.ID
			out = append(out, entry{id: id, cmd: pr.Command, stage: stage})
		}
		for i, c := range r.Teardown {
			id := fmt.Sprintf("teardown:%d.%d", ri, i)
			out = append(out, entry{id: id, cmd: c, stage: lastv1alpha1.StageDelete})
		}
	}
	return out
}

// coverageOf computes which of the subject's entries the rules cover, as the
// gateway does.
func coverageOf(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, rs []rules.Rule) (covered map[string]rules.Ref, uncovered []string) {
	if stage == lastv1alpha1.StageDiscovery {
		e := entriesOf(in, stage)[0]
		return rules.Coverage(rs, stage, []lastv1alpha1.Command{e.cmd})
	}
	return rules.PlanCoverage(rs, stage, plan.FromStatus(in.Status.Plan))
}

// unrulable says why no rule can hold cmd, or "" if one can.
func unrulable(cmd lastv1alpha1.Command) string {
	switch {
	case len(cmd.Run.Argv) == 0:
		return "it runs a script, and rules match argv only"
	case slices.ContainsFunc(cmd.Run.Argv, hasStepRefArg) || cmd.Image != nil &&
		slices.ContainsFunc(cmd.Image.Install, func(c lastv1alpha1.ArgvCommand) bool { return slices.ContainsFunc(c.Argv, hasStepRefArg) }):
		return "it uses " + stepRefSyntax + "...} references, which no rule covers"
	}
	return ""
}

func hasStepRefArg(a string) bool { return strings.Contains(a, stepRefSyntax) }

// ruleSpec is the rule for e matching pattern (M6 design R13): the stages given
// or e's own, e's providers and egress, and one exact pattern per image.install argv.
func ruleSpec(in *lastv1alpha1.Intent, e entry, pattern lastv1alpha1.ArgvPattern, stages []lastv1alpha1.Stage,
	providers []string) lastv1alpha1.CommandRuleSpec {
	if len(stages) == 0 {
		stages = []lastv1alpha1.Stage{e.stage}
	}
	spec := lastv1alpha1.CommandRuleSpec{
		Description: fmt.Sprintf("From the approval of Intent %s/%s (%s)", in.Namespace, in.Name, e.id),
		Stages:      slices.Clone(stages),
		Providers:   providers,
		Match:       pattern,
		Egress:      slices.Clone(e.cmd.Egress),
		Source:      &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginApproval},
	}
	if e.cmd.Image != nil {
		for _, ic := range e.cmd.Image.Install {
			spec.Install = append(spec.Install, rules.Exact(ic.Argv))
		}
	}
	return spec
}

// choice is a rule the approver chose for an entry.
type choice struct {
	id   string
	spec lastv1alpha1.CommandRuleSpec
}

// alwaysAllow walks the subject's uncovered commands and creates the rules the
// approver chooses, as the user. It creates nothing until every choice is made,
// and stops at the first rule it cannot create. p is nil unless opts.mode prompts.
func alwaysAllow(ctx context.Context, k *Kube, p *prompter, w io.Writer, in *lastv1alpha1.Intent,
	stage lastv1alpha1.Stage, opts allowOpts) (made []string, err error) {
	rs, err := readRules(ctx, k, in.Namespace)
	if err != nil {
		return nil, fmt.Errorf("compute what the rules cover: %w", err)
	}
	_, uncovered := coverageOf(in, stage, rs)
	if len(uncovered) == 0 {
		writeln(w, "Every command is already covered by an allow rule; there is nothing to add.")
		return nil, nil
	}
	all := entriesOf(in, stage)
	var chosen []choice
	for _, id := range uncovered {
		i := slices.IndexFunc(all, func(e entry) bool { return e.id == id })
		if i < 0 {
			continue
		}
		c, err := chooseRule(ctx, p, w, in, all, all[i], opts)
		if err != nil {
			return nil, err
		}
		if c != nil {
			if strings.HasPrefix(c.id, "teardown:") && !slices.Contains(c.spec.Stages, lastv1alpha1.StageDelete) {
				writef(w, "Note: the rule for %s does not list stage Delete, so it will not cover that teardown command.\n", clean(c.id))
			}
			chosen = append(chosen, *c)
		}
	}
	return createRules(ctx, k, w, in, chosen, opts.cluster)
}

// chooseRule puts one uncovered entry to the approver, or applies opts.mode to
// it. It returns nil for Just this once.
func chooseRule(ctx context.Context, p *prompter, w io.Writer, in *lastv1alpha1.Intent, all []entry, e entry,
	opts allowOpts) (*choice, error) {
	writef(w, "\nNot covered by an allow rule: %s\n", clean(e.id))
	renderCommand(w, "  ", e.cmd, false)
	if why := unrulable(e.cmd); why != "" {
		writef(w, "No rule for %s: %s; it is approved just this once.\n", clean(e.id), why)
		return nil, nil //nolint:nilnil // nil means no rule
	}
	providers := providersFor(e, all, opts)
	if len(providers) == 0 {
		writef(w, "No rule for %s: no command in this plan names a provider, and a rule needs at least one; "+
			"pass --providers p1,p2 to give it some. It is approved just this once.\n", clean(e.id))
		return nil, nil //nolint:nilnil // nil means no rule
	}
	if len(e.cmd.Files) > 0 {
		writef(w, "Warning: %s writes %s; this rule matches argv only; the files it runs can change.\n", clean(e.id),
			clean(strings.Join(slices.Sorted(maps.Keys(e.cmd.Files)), ", ")))
	}
	mode := opts.mode
	if mode == allowPrompt {
		for mode == allowPrompt {
			line, err := p.ask(ctx, fmt.Sprintf("Rule for %s? [e]xact, [g]eneralized, [o] just this once [e/g/O]: ", clean(e.id)))
			if err != nil {
				return nil, err
			}
			switch strings.ToLower(line) {
			case "e", "exact":
				mode = allowExact
			case "g", "generalized":
				mode = allowGeneralized
			case "", "o", "once", "just this once":
				return nil, nil //nolint:nilnil // nil means no rule
			default:
				writeln(w, "Answer e, g or o.")
			}
		}
	}
	var pattern lastv1alpha1.ArgvPattern
	if mode == allowExact {
		pattern = rules.Exact(e.cmd.Run.Argv)
	} else {
		// Always the CLI's own generalization of the entry's command: the operator's
		// status.plan.generalizations and pendingCommand.generalization are outside
		// the plan hash and the operator is untrusted (M6 design §10, I2).
		pattern = rules.Generalize(e.cmd)
	}
	for {
		spec := ruleSpec(in, e, pattern, opts.stages, providers)
		fs := showPattern(w, mode, all, spec)
		nothing := rules.MatchesNothing(fs)
		if p == nil || mode == allowExact {
			if nothing {
				return nil, fmt.Errorf("the %s pattern for %s matches nothing; nothing was created", mode, clean(e.id))
			}
			return &choice{id: e.id, spec: spec}, nil
		}
		line, err := p.ask(ctx, "Press Enter to accept, or type an edited pattern (space-separated): ")
		if err != nil {
			return nil, err
		}
		switch {
		case line == "" && nothing:
			writeln(w, "A pattern that matches nothing cannot be accepted; edit it.")
		case line == "":
			return &choice{id: e.id, spec: spec}, nil
		default:
			pattern = lastv1alpha1.ArgvPattern{Argv: strings.Fields(line)}
		}
	}
}

// providersFor is the providers of e's rule: e's own; else, for a command that
// names none, the sorted union of the providers of every command in the plan
// (a rule needs at least one); else opts.providers.
func providersFor(e entry, all []entry, opts allowOpts) []string {
	if len(e.cmd.Providers) > 0 {
		return slices.Clone(e.cmd.Providers)
	}
	var union []string
	for _, o := range all {
		for _, p := range o.cmd.Providers {
			if !slices.Contains(union, p) {
				union = append(union, p)
			}
		}
	}
	slices.Sort(union)
	if len(union) == 0 {
		return slices.Clone(opts.providers)
	}
	return union
}

// showPattern prints a candidate rule's pattern, its lint warnings, and the
// commands in the plan it matches.
func showPattern(w io.Writer, mode string, all []entry, spec lastv1alpha1.CommandRuleSpec) []rules.Finding {
	label := "Pattern"
	if mode == allowExact {
		label = "Exact pattern"
	}
	writef(w, "%s: %s\n", label, clean(strings.Join(spec.Match.Argv, " ")))
	fs := rules.LintFindings(spec)
	for _, f := range fs {
		kind := "warning"
		if f.Kind == rules.KindNothing {
			kind = "matches nothing"
		}
		writef(w, "  %s: %s\n", kind, clean(f.Message))
	}
	var matched []entry
	for _, e := range all {
		if rules.Match(spec.Match.Argv, e.cmd.Run.Argv) {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		writeln(w, "  It matches no command in this plan.")
		return fs
	}
	writeln(w, "  It matches these commands in this plan:")
	for _, e := range matched {
		writef(w, "    %s: %s\n", clean(e.id), shellWords(e.cmd.Run.Argv))
	}
	return fs
}

// ruleName is "<intent>-<entry>-<hash8>", DNS-1123: the entry is lowercased
// with ':' and '.' (and anything else a name cannot hold) as '-', and hash8 is
// the first 8 hex characters of the SHA-256 of the spec's canonical JSON.
func ruleName(intent, entryID string, spec lastv1alpha1.CommandRuleSpec) (string, error) {
	c, err := ruleSpecKey(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	prefix := dnsLabel(intent) + "-" + dnsLabel(entryID)
	if len(prefix) > 253-9 {
		prefix = strings.TrimRight(prefix[:253-9], "-")
	}
	return prefix + "-" + hex.EncodeToString(sum[:])[:8], nil
}

// ruleSpecKey is the canonical JSON of spec without its source, so two approvers
// who create the same rule get the same name and the same-spec check (M7 S4).
func ruleSpecKey(spec lastv1alpha1.CommandRuleSpec) ([]byte, error) {
	spec.Source = nil
	return audit.Canonical(spec)
}

func dnsLabel(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, s)
}

// createRules creates the chosen rules as the user, in order, and stops at the
// first it cannot create. A rule that already exists with the same spec counts
// as done; one with another spec is an error.
func createRules(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent, chosen []choice, cluster bool) (made []string, _ error) {
	fail := func(format string, a ...any) ([]string, error) {
		msg := fmt.Sprintf(format, a...)
		if len(made) == 0 {
			return nil, fmt.Errorf("%s; no rule was created and the Intent was not approved", msg)
		}
		return made, fmt.Errorf("%s; created before that: %s; the Intent was not approved", msg, strings.Join(made, ", "))
	}
	if len(chosen) == 0 {
		return nil, nil
	}
	user, err := k.WhoAmI(ctx)
	if err != nil {
		return fail("find out who you are: %v", err)
	}
	for _, c := range chosen {
		name, err := ruleName(in.Name, c.id, c.spec)
		if err != nil {
			return fail("name the rule for %s: %v", clean(c.id), err)
		}
		if c.spec.Source != nil {
			src := *c.spec.Source
			src.CreatedBy = user
			c.spec.Source = &src
		}
		meta := metav1.ObjectMeta{Name: name, Labels: map[string]string{labelIntent: in.Name}}
		var obj client.Object
		kind := "CommandRule"
		if cluster {
			kind = "ClusterCommandRule"
			obj = &lastv1alpha1.ClusterCommandRule{ObjectMeta: meta, Spec: c.spec}
		} else {
			meta.Namespace = in.Namespace
			obj = &lastv1alpha1.CommandRule{ObjectMeta: meta, Spec: c.spec}
		}
		err = k.Client.Create(ctx, obj)
		switch {
		case err == nil:
			writef(w, "Created %s %s\n", kind, name)
			made = append(made, kind+" "+name)
		case apierrors.IsAlreadyExists(err):
			same, serr := sameSpec(ctx, k, obj, c.spec)
			switch {
			case serr != nil:
				return fail("read the existing %s %s: %v", kind, name, serr)
			case !same:
				return fail("%s %s already exists and its spec differs from the rule chosen for %s", kind, name, clean(c.id))
			}
			writef(w, "%s %s already exists\n", kind, name)
		default:
			return fail("create %s %s: %v", kind, name, err)
		}
	}
	return made, nil
}

// sameSpec reports whether the object that already has obj's name holds spec.
func sameSpec(ctx context.Context, k *Kube, obj client.Object, spec lastv1alpha1.CommandRuleSpec) (bool, error) {
	var have lastv1alpha1.CommandRuleSpec
	switch o := obj.(type) {
	case *lastv1alpha1.ClusterCommandRule:
		var cur lastv1alpha1.ClusterCommandRule
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(o), &cur); err != nil {
			return false, err
		}
		have = cur.Spec
	case *lastv1alpha1.CommandRule:
		var cur lastv1alpha1.CommandRule
		if err := k.Client.Get(ctx, client.ObjectKeyFromObject(o), &cur); err != nil {
			return false, err
		}
		have = cur.Spec
	}
	a, err := ruleSpecKey(have)
	if err != nil {
		return false, err
	}
	b, err := ruleSpecKey(spec)
	return err == nil && string(a) == string(b), err
}

// printCoverage writes what the allow rules do for the plan shown: the entries
// no rule covers, from status.plan.uncoveredSteps, and, when the user can read
// the rules, the rule that covers each of the others.
func printCoverage(ctx context.Context, w io.Writer, k *Kube, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) {
	if stage == lastv1alpha1.StageDiscovery || in.Status.Plan == nil {
		return
	}
	if u := in.Status.Plan.UncoveredSteps; len(u) > 0 {
		writef(w, "Not covered by allow rules: %s\n", clean(strings.Join(u, ", ")))
	}
	rs, err := readRules(ctx, k, in.Namespace)
	if err != nil {
		return
	}
	covered, _ := coverageOf(in, stage, rs)
	first := true
	for _, e := range entriesOf(in, stage) {
		ref, ok := covered[e.id]
		if !ok {
			continue
		}
		if first {
			writeln(w, "Covered by allow rules:")
			first = false
		}
		writef(w, "  %s: %s\n", clean(e.id), clean(ref.String()))
	}
}

func newRulesCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{Use: "rules", Short: "Work with allow rules"}
	cmd.AddCommand(&cobra.Command{
		Use:   "lint FILE|-",
		Short: "Check CommandRule and ClusterCommandRule documents for patterns that are too broad or match nothing",
		Long: "Read one or more YAML documents of kind CommandRule or ClusterCommandRule from FILE (or stdin with -) and " +
			"print the lint warnings for each. Exits 0 when patterns are merely broad, and 1 when a pattern matches nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return lintRules(d, args[0]) },
	})
	return cmd
}

func lintRules(d Deps, path string) error {
	var r = d.In
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the user names the file
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	broken := 0
	for n := 1; ; n++ {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("document %d: %w", n, err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var tm struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &tm); err != nil {
			return fmt.Errorf("document %d: %w", n, err)
		}
		var spec lastv1alpha1.CommandRuleSpec
		switch tm.Kind {
		case "CommandRule":
			var o lastv1alpha1.CommandRule
			if err := json.Unmarshal(raw, &o); err != nil {
				return fmt.Errorf("document %d: %w", n, err)
			}
			spec = o.Spec
		case "ClusterCommandRule":
			var o lastv1alpha1.ClusterCommandRule
			if err := json.Unmarshal(raw, &o); err != nil {
				return fmt.Errorf("document %d: %w", n, err)
			}
			spec = o.Spec
		default:
			writef(d.Out, "document %d: unsupported kind %q (want CommandRule or ClusterCommandRule)\n", n, clean(tm.Kind))
			broken++
			continue
		}
		label := tm.Kind + " " + clean(tm.Metadata.Name)
		if tm.Metadata.Namespace != "" {
			label = tm.Kind + " " + clean(tm.Metadata.Namespace) + "/" + clean(tm.Metadata.Name)
		}
		fs := rules.LintFindings(spec)
		if len(fs) == 0 {
			writef(d.Out, "%s: ok\n", label)
			continue
		}
		writef(d.Out, "%s:\n", label)
		for _, f := range fs {
			kind := "warning"
			if f.Kind == rules.KindNothing {
				kind = "error"
			}
			writef(d.Out, "  %s: %s\n", kind, clean(f.Message))
		}
		if rules.MatchesNothing(fs) {
			broken++
		}
	}
	if broken > 0 {
		return fmt.Errorf("%d rule(s) have a pattern that matches nothing", broken)
	}
	return nil
}

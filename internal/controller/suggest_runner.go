package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

const (
	// DefaultDebounce is the shortest time between two RepeatedApproval scans
	// of one namespace (M7 design §4.2).
	DefaultDebounce = time.Minute
	// DefaultReviewInterval is how often PeriodicReview runs when the
	// OperatorConfig sets no suggestions.reviewInterval (M7 design §4.3).
	DefaultReviewInterval = 24 * time.Hour
	// defaultRepeatThreshold is suggestions.repeatThreshold's default.
	defaultRepeatThreshold = 3
	// maxExamples is the most examples a suggestion holds (its CRD's MaxItems).
	maxExamples = 20
	// maxInstall is the most install patterns a rule holds (its CRD's MaxItems).
	maxInstall = 32
	// auditPage is the page size of the runner's QueryAudit reads, the most
	// the gateway serves.
	auditPage = 200
	// maxCachedCommands is the most command.start entries the runner keeps of
	// one Intent, the newest. It is the size of the gateway's audit index of an
	// Intent (maxIndexEntries), which never serves more.
	maxCachedCommands = 200
	// runnerTick is how often the runner checks its queue and review timer
	// besides when Queue wakes it.
	runnerTick = 5 * time.Second
	// planTimeWorkers is how many PlanTime steps run at once.
	planTimeWorkers = 4
	// repeatEntry is the id every example of a RepeatedApproval group carries
	// in its suggest call: entries that share an id are one group (suggest.SystemPrompt).
	repeatEntry = "group"
	// authKindApproval is the authorized_by kind of a command an IntentApproval authorized.
	authKindApproval = "approval"
)

// auditedCommand is one command.start entry of an Intent's audit index.
type auditedCommand struct {
	seq    uint64
	stage  lastv1alpha1.Stage
	kind   string // authorized_by kind: approval, rule or auto
	by     string // authorized_by name: the IntentApproval for an approval
	cmd    lastv1alpha1.Command
	intent *lastv1alpha1.Intent
}

// intentAudit is what the runner holds of one Intent's audit index: the
// commands read so far and the high-water mark the next read starts after.
type intentAudit struct {
	uid  types.UID
	seq  uint64
	cmds []auditedCommand
}

// add appends c, a command newer than every one a holds, dropping the oldest
// beyond maxCachedCommands.
func (a *intentAudit) add(c auditedCommand) {
	a.cmds = append(a.cmds, c)
	if n := len(a.cmds); n > maxCachedCommands {
		a.cmds = slices.Clone(a.cmds[n-maxCachedCommands:]) // a fresh array, so the dropped commands go
	}
}

// repeatGroup is the manually approved commands of one RepeatedApproval group.
type repeatGroup struct {
	key       string
	class     lastv1alpha1.Stage
	providers []string
	cmds      []auditedCommand
	approvals map[string]bool
}

// planJob is a PlanTime step to run: the Intent, and the hash of the plan
// that waited for an approval when the step was queued.
type planJob struct {
	key  types.NamespacedName
	hash string
}

// SuggestRunner runs the three suggestion triggers (M7 design §4) on the
// leader. QueuePlanTime asks for the PlanTime step of a plan that waits for an
// approval, which runs on one of planTimeWorkers workers, away from the
// Intent's reconcile, so the suggest call never holds up an approval. Queue asks for a RepeatedApproval scan of
// a namespace, which runs at most once per Debounce; Review runs every
// suggestions.reviewInterval of the OperatorConfig (default 24h), with ±10%
// jitter, from a timer in memory that a restart starts again. Creates are
// idempotent (S6), so a scan or review run twice does no harm. Client reads
// Intents, CommandRules and the OperatorConfig; Creator vets and creates.
type SuggestRunner struct {
	Creator *SuggestionCreator
	Gateway gatewayv1.GatewayServiceClient
	Client  client.Client
	// Now is the clock scans debounce by and reviews are timed by; nil means time.Now.
	Now func() time.Time
	// Debounce is the shortest time between two scans of a namespace; 0 means DefaultDebounce.
	Debounce time.Duration

	once     sync.Once
	mu       sync.Mutex
	wake     chan struct{}
	queued   map[string]bool      // namespaces waiting for a scan
	lastScan map[string]time.Time // when each namespace was last scanned from the queue
	// planWake wakes the PlanTime workers; planQueue holds the steps waiting
	// to run, and planJobs every step queued or running, so one queued twice
	// runs once.
	planWake  chan struct{}
	planQueue []planJob
	planJobs  map[planJob]bool

	// work serializes scans and reviews, and guards audits and fired.
	work   sync.Mutex
	audits map[types.NamespacedName]*intentAudit
	// fired holds, per namespace and group key, how many distinct approvals
	// the group had when it was last proposed: a scan proposes it again only
	// once it has gained one. Review clears it.
	fired map[string]int

	// Seams for tests: the queue's polling period, the scan and review
	// Start runs, and the jitter source (in [0, 1)).
	tick     time.Duration
	scanFn   func(context.Context, string) error
	reviewFn func(context.Context) error
	jitter   func() float64
}

// NeedLeaderElection makes the runner run on the leader only.
func (s *SuggestRunner) NeedLeaderElection() bool { return true }

func (s *SuggestRunner) init() {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.wake, s.planWake = make(chan struct{}, 1), make(chan struct{}, 1)
		s.queued, s.lastScan, s.planJobs = map[string]bool{}, map[string]time.Time{}, map[planJob]bool{}
		s.audits, s.fired = map[types.NamespacedName]*intentAudit{}, map[string]int{}
		if s.tick <= 0 {
			s.tick = runnerTick
		}
		if s.scanFn == nil {
			s.scanFn = s.ScanRepeated
		}
		if s.reviewFn == nil {
			s.reviewFn = s.Review
		}
		if s.jitter == nil {
			s.jitter = rand.Float64 //nolint:gosec // jitter, not a secret
		}
	})
}

func (s *SuggestRunner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SuggestRunner) debounce() time.Duration {
	if s.Debounce > 0 {
		return s.Debounce
	}
	return DefaultDebounce
}

// Queue asks for a RepeatedApproval scan of namespace. It never blocks; scans
// queued within Debounce of the namespace's last one run together once it is
// up. A nil runner queues nothing.
func (s *SuggestRunner) Queue(namespace string) {
	if s == nil {
		return
	}
	s.init()
	s.mu.Lock()
	s.queued[namespace] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// isQueued reports whether namespace waits for a scan.
func (s *SuggestRunner) isQueued(namespace string) bool {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued[namespace]
}

// due takes the queued namespaces whose last scan is at least Debounce old,
// marking them scanned at now.
func (s *SuggestRunner) due(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for ns := range s.queued {
		if last, ok := s.lastScan[ns]; ok && now.Sub(last) < s.debounce() {
			continue
		}
		delete(s.queued, ns)
		s.lastScan[ns] = now
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}

// QueuePlanTime asks for the PlanTime step of the Intent's plan with hash. It
// never blocks. A step already queued or running for the same plan is not
// queued again. A nil runner queues nothing.
func (s *SuggestRunner) QueuePlanTime(key types.NamespacedName, hash string) {
	if s == nil {
		return
	}
	s.init()
	job := planJob{key: key, hash: hash}
	s.mu.Lock()
	if s.planJobs[job] {
		s.mu.Unlock()
		return
	}
	s.planJobs[job] = true
	s.planQueue = append(s.planQueue, job)
	s.mu.Unlock()
	s.wakePlanTime()
}

func (s *SuggestRunner) wakePlanTime() {
	select {
	case s.planWake <- struct{}{}:
	default:
	}
}

// nextPlanJob takes the oldest queued PlanTime step, waking another worker
// when more wait.
func (s *SuggestRunner) nextPlanJob() (planJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.planQueue) == 0 {
		return planJob{}, false
	}
	job := s.planQueue[0]
	s.planQueue = slices.Delete(s.planQueue, 0, 1)
	if len(s.planQueue) > 0 {
		s.wakePlanTime()
	}
	return job, true
}

// startPlanTime starts the PlanTime workers; they stop when ctx ends.
func (s *SuggestRunner) startPlanTime(ctx context.Context) {
	s.init()
	log := logf.FromContext(ctx).WithName("suggestions")
	for range planTimeWorkers {
		go func() {
			for {
				job, ok := s.nextPlanJob()
				if !ok {
					select {
					case <-ctx.Done():
						return
					case <-s.planWake:
					}
					continue
				}
				if err := s.planTime(ctx, job); err != nil && ctx.Err() == nil {
					// Not retried: suggestions are advice, and the plan still waits.
					log.Info("Could not finish the PlanTime suggestion step", "intent", job.key, "plan", job.hash, "reason", err.Error())
				}
				s.mu.Lock()
				delete(s.planJobs, job)
				s.mu.Unlock()
			}
		}()
	}
}

// Start runs the PlanTime workers, queued scans and the review timer until
// ctx ends.
func (s *SuggestRunner) Start(ctx context.Context) error {
	s.init()
	s.startPlanTime(ctx)
	log := logf.FromContext(ctx).WithName("suggestions")
	next := s.now().Add(s.reviewInterval(ctx))
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-t.C:
		}
		if now := s.now(); !now.Before(next) {
			if err := s.reviewFn(ctx); err != nil {
				log.Error(err, "Could not complete the periodic rule review")
			}
			next = s.now().Add(s.reviewInterval(ctx))
		}
		for _, ns := range s.due(s.now()) {
			if err := s.scanFn(ctx, ns); err != nil && ctx.Err() == nil {
				log.Info("Could not scan for repeated approvals; retrying after the debounce", "namespace", ns, "reason", err.Error())
				s.Queue(ns)
			}
		}
	}
}

// suggestionsConfig reads the OperatorConfig's suggestions settings; a
// missing OperatorConfig or section means the defaults.
func (s *SuggestRunner) suggestionsConfig(ctx context.Context) (lastv1alpha1.SuggestionsConfig, error) {
	op := &lastv1alpha1.OperatorConfig{}
	switch err := s.Client.Get(ctx, client.ObjectKey{Name: operatorConfigName}, op); {
	case apierrors.IsNotFound(err):
		return lastv1alpha1.SuggestionsConfig{}, nil
	case err != nil:
		return lastv1alpha1.SuggestionsConfig{}, err
	}
	if op.Spec.Suggestions == nil {
		return lastv1alpha1.SuggestionsConfig{}, nil
	}
	return *op.Spec.Suggestions, nil
}

// reviewInterval is the time to the next review: the OperatorConfig's
// suggestions.reviewInterval (default 24h, also when it can't be read) times
// a factor in [0.9, 1.1].
func (s *SuggestRunner) reviewInterval(ctx context.Context) time.Duration {
	s.init()
	d := DefaultReviewInterval
	cfg, err := s.suggestionsConfig(ctx)
	if err != nil {
		logf.FromContext(ctx).Info("Could not read the review interval; using the default", "reason", err.Error())
	} else if cfg.ReviewInterval != nil && cfg.ReviewInterval.Duration > 0 {
		d = cfg.ReviewInterval.Duration
	}
	return time.Duration(float64(d) * (0.9 + 0.2*s.jitter()))
}

// ScanRepeated is the RepeatedApproval scan of a namespace (M7 design §4.2). It
// reads the command.start entries of each of the namespace's Intents, keeps
// the commands an IntentApproval authorized that a rule could hold and no rule
// covers yet, groups them by rules.GroupKey, and proposes a rule for each group
// that holds commands from at least repeatThreshold distinct IntentApprovals
// and has gained one since it was last proposed.
func (s *SuggestRunner) ScanRepeated(ctx context.Context, namespace string) error {
	s.init()
	s.work.Lock()
	defer s.work.Unlock()
	return s.scanRepeated(ctx, namespace)
}

func (s *SuggestRunner) scanRepeated(ctx context.Context, namespace string) error {
	cfg, err := s.suggestionsConfig(ctx)
	if err != nil {
		return err
	}
	threshold := defaultRepeatThreshold
	if cfg.RepeatThreshold > 0 {
		threshold = int(cfg.RepeatThreshold)
	}
	cmds, err := s.readAudit(ctx, namespace)
	if err != nil {
		return err
	}
	rs, err := s.readRules(ctx, namespace)
	if err != nil {
		return err
	}
	groups := map[string]*repeatGroup{}
	for _, c := range cmds {
		if c.kind != authKindApproval || suggest.Unrulable(c.cmd) != "" ||
			slices.ContainsFunc(rs, func(r rules.Rule) bool { return rules.Covers(r.Spec, c.stage, c.cmd) }) {
			continue
		}
		key, ok := rules.GroupKey(c.stage, c.cmd)
		if !ok {
			continue
		}
		g := groups[key]
		if g == nil {
			g = &repeatGroup{key: key, class: stageClass(c.stage), providers: sortedSet(c.cmd.Providers), approvals: map[string]bool{}}
			groups[key] = g
		}
		g.cmds = append(g.cmds, c)
		g.approvals[c.by] = true
	}
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		g := groups[key]
		fired := namespace + "\x00" + key
		if n := len(g.approvals); n < threshold || n <= s.fired[fired] {
			continue
		}
		s.fired[fired] = len(g.approvals)
		if err := s.proposeGroup(ctx, namespace, g); err != nil {
			delete(s.fired, fired) // the retried scan proposes it again
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// proposeGroup proposes one rule for a group that reached the threshold. The
// examples are its 20 newest distinct commands, and the newest one's Intent is
// the one the suggest call is made for and the suggestion is labelled with.
// The model proposes one pattern for the whole group; the first answer that
// passes suggest.Vet's own checks is used, else rules.Merge of the examples.
// When neither gives a pattern there is no suggestion and SuggestionFailed. A
// create the API server refuses for good (finalCreateError) raises
// SuggestionFailed and returns nil, so the group is not proposed, nor the model
// asked, again until it gains an approval or a review; any other create error
// is returned, and the retried scan proposes the group again.
func (s *SuggestRunner) proposeGroup(ctx context.Context, namespace string, g *repeatGroup) error {
	log := logf.FromContext(ctx)
	examples, newest := newestExamples(g.cmds)
	if len(g.providers) == 0 {
		log.V(1).Info("Skipped a repeated-approval group whose commands name no provider", "namespace", namespace, "group", g.key)
		return nil
	}
	spec, ok := groupSpec(g, examples)
	if !ok {
		log.V(1).Info("Skipped a repeated-approval group with too many install commands", "namespace", namespace, "group", g.key)
		return nil
	}
	p := suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeNamespace, Namespace: namespace,
		Trigger: lastv1alpha1.TriggerRepeatedApproval, Spec: spec, Examples: examples}
	entries := make([]suggest.Entry, len(examples))
	for i, e := range examples {
		entries[i] = suggest.Entry{ID: repeatEntry, Stage: g.class, Argv: e.Run.Argv}
	}
	answers, err := s.Creator.Ask(ctx, s.Gateway, intentRef(newest), entries)
	if err != nil {
		log.Info("Could not get a rule suggestion for repeated approvals", "namespace", namespace, "reason", err.Error())
		s.Creator.event(newest, nil, corev1.EventTypeWarning, ReasonSuggestionFailed,
			"The suggest call for commands approved in %d IntentApprovals failed: %s", len(g.approvals), describe(err))
	}
	chosen := false
	for _, a := range answers {
		if a.Entry != repeatEntry {
			continue
		}
		c := p
		c.Spec.Match = lastv1alpha1.ArgvPattern{Argv: a.Argv}
		c.Rationale = a.Rationale
		if ok, _, _ := suggest.Vet(c, nil, nil); ok {
			p, chosen = c, true
			break
		}
	}
	if !chosen {
		argvs := make([][]string, len(examples))
		for i, e := range examples {
			argvs[i] = e.Run.Argv
		}
		merged, ok := rules.Merge(argvs)
		if !ok {
			s.Creator.event(newest, nil, corev1.EventTypeWarning, ReasonSuggestionFailed,
				"Found no pattern for %q, approved in %d IntentApprovals: the model gave none that matches every example, and the examples do not merge",
				strings.Join(examples[0].Run.Argv, " "), len(g.approvals))
			return nil
		}
		p.Spec.Match = lastv1alpha1.ArgvPattern{Argv: merged}
		p.Rationale = fmt.Sprintf("Commands like these were approved by hand in %d IntentApprovals; this pattern merges the %d newest.",
			len(g.approvals), len(examples))
	}
	switch _, _, err := s.Creator.Create(ctx, p, newest); {
	case err != nil && finalCreateError(err):
		log.Info("Could not create a rule suggestion for repeated approvals", "namespace", namespace, "group", g.key,
			"reason", err.Error())
		s.Creator.event(newest, nil, corev1.EventTypeWarning, ReasonSuggestionFailed,
			"Could not create the suggestion %q for commands approved in %d IntentApprovals: %s",
			strings.Join(p.Spec.Match.Argv, " "), len(g.approvals), err.Error())
	case err != nil:
		return fmt.Errorf("creating a repeated-approval suggestion: %w", err)
	}
	return nil
}

// groupSpec is the rule for a group, without its match: the group's stage
// class and providers, the union of its examples' egress, and an exact
// install pattern per distinct install argv. ok is false over maxInstall.
func groupSpec(g *repeatGroup, examples []lastv1alpha1.Command) (lastv1alpha1.CommandRuleSpec, bool) {
	spec := lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{g.class}, Providers: slices.Clone(g.providers)}
	for _, e := range examples {
		for _, eg := range e.Egress {
			if !slices.Contains(spec.Egress, eg) {
				spec.Egress = append(spec.Egress, eg)
			}
		}
		if e.Image == nil {
			continue
		}
		for _, ic := range e.Image.Install {
			p := rules.Exact(ic.Argv)
			if !slices.ContainsFunc(spec.Install, func(x lastv1alpha1.ArgvPattern) bool { return slices.Equal(x.Argv, p.Argv) }) {
				spec.Install = append(spec.Install, p)
			}
		}
	}
	slices.Sort(spec.Egress)
	return spec, len(spec.Install) <= maxInstall
}

// newestExamples returns up to maxExamples distinct commands of cmds, newest
// first, and the Intent of the newest. cmds is not empty.
func newestExamples(cmds []auditedCommand) ([]lastv1alpha1.Command, *lastv1alpha1.Intent) {
	sorted := slices.SortedFunc(slices.Values(cmds), func(a, b auditedCommand) int { return cmp.Compare(b.seq, a.seq) })
	var out []lastv1alpha1.Command
	seen := map[string]bool{}
	for _, c := range sorted {
		b, err := json.Marshal(c.cmd)
		if err != nil || seen[string(b)] {
			continue
		}
		seen[string(b)] = true
		if out = append(out, c.cmd); len(out) == maxExamples {
			break
		}
	}
	return out, sorted[0].intent
}

// readAudit brings the runner's copy of the audit of each of the namespace's
// Intents up to date and returns their commands. Each Intent is read with
// QueryAudit from its high-water mark on, a page of auditPage at a time; an
// Intent recreated under the same name is read again from the start, and the
// copies of Intents that are gone, including one the gateway no longer finds,
// are dropped (their approvals stop counting).
func (s *SuggestRunner) readAudit(ctx context.Context, namespace string) ([]auditedCommand, error) {
	var list lastv1alpha1.IntentList
	if err := s.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	present := map[types.NamespacedName]bool{}
	var out []auditedCommand
	for i := range list.Items {
		in := &list.Items[i]
		key := types.NamespacedName{Namespace: in.Namespace, Name: in.Name}
		present[key] = true
		a := s.audits[key]
		if a == nil || a.uid != in.UID {
			a = &intentAudit{uid: in.UID}
			s.audits[key] = a
		}
		switch err := s.readIntentAudit(ctx, in, a); {
		case status.Code(err) == codes.NotFound:
			delete(present, key) // deleted since the list
			continue
		case err != nil:
			return nil, fmt.Errorf("reading the audit of Intent %s: %w", key, err)
		}
		for j := range a.cmds {
			a.cmds[j].intent = in
		}
		out = append(out, a.cmds...)
	}
	for key := range s.audits {
		if key.Namespace == namespace && !present[key] {
			delete(s.audits, key)
		}
	}
	return out, nil
}

// readIntentAudit appends the Intent's command.start entries after a.seq to a,
// without their files (exampleOf), keeping its newest maxCachedCommands.
func (s *SuggestRunner) readIntentAudit(ctx context.Context, in *lastv1alpha1.Intent, a *intentAudit) error {
	for {
		qctx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
		resp, err := s.Gateway.QueryAudit(qctx, &gatewayv1.QueryAuditRequest{Intent: intentRef(in),
			Types: []string{audit.TypeCommandStart}, AfterSeq: a.seq, Limit: auditPage})
		cancel()
		if err != nil {
			return err
		}
		for _, e := range resp.GetEntries() {
			a.seq = max(a.seq, e.GetSeq())
			var payload struct {
				Command lastv1alpha1.Command `json:"command"`
			}
			if json.Unmarshal(e.GetPayload(), &payload) != nil || e.GetAuthorizedBy() == nil {
				continue
			}
			a.add(auditedCommand{seq: e.GetSeq(), stage: lastv1alpha1.Stage(e.GetStage()),
				kind: e.GetAuthorizedBy().GetKind(), by: e.GetAuthorizedBy().GetName(), cmd: exampleOf(payload.Command)})
		}
		if !resp.GetMore() || len(resp.GetEntries()) == 0 {
			return nil
		}
	}
}

// readRules reads the rules that apply in namespace: every ClusterCommandRule
// and the namespace's CommandRules.
func (s *SuggestRunner) readRules(ctx context.Context, namespace string) ([]rules.Rule, error) {
	var cluster lastv1alpha1.ClusterCommandRuleList
	if err := s.Client.List(ctx, &cluster); err != nil {
		return nil, err
	}
	var local lastv1alpha1.CommandRuleList
	if err := s.Client.List(ctx, &local, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	rs, _ := rules.FromObjects(namespace, cluster.Items, local.Items)
	return rs, nil
}

// Review is PeriodicReview (M7 design §4.3). In each namespace that has
// Intents or CommandRules it runs the full RepeatedApproval scan, which also
// lets every group be proposed again, and consolidates the namespace's
// CommandRules; then it proposes Cluster rules for CommandRules that are
// identical in two or more namespaces. A failure in one namespace or group
// does not stop the others; Review returns them all. It first drops what the
// runner holds of Intents and namespaces that are gone.
func (s *SuggestRunner) Review(ctx context.Context) error {
	s.init()
	s.work.Lock()
	defer s.work.Unlock()
	var intents lastv1alpha1.IntentList
	if err := s.Client.List(ctx, &intents); err != nil {
		return err
	}
	var crs lastv1alpha1.CommandRuleList
	if err := s.Client.List(ctx, &crs); err != nil {
		return err
	}
	namespaces := map[string]bool{}
	live := map[types.NamespacedName]types.UID{}
	for _, in := range intents.Items {
		namespaces[in.Namespace] = true
		live[types.NamespacedName{Namespace: in.Namespace, Name: in.Name}] = in.UID
	}
	byNamespace := map[string][]lastv1alpha1.CommandRule{}
	for _, r := range crs.Items {
		namespaces[r.Namespace] = true
		byNamespace[r.Namespace] = append(byNamespace[r.Namespace], r)
	}
	s.prune(live, namespaces)
	clear(s.fired)
	var errs []error
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		if err := s.scanRepeated(ctx, ns); err != nil {
			errs = append(errs, fmt.Errorf("scanning namespace %s: %w", ns, err))
		}
		if err := s.consolidate(ctx, ns, byNamespace[ns]); err != nil {
			errs = append(errs, fmt.Errorf("consolidating the rules of namespace %s: %w", ns, err))
		}
	}
	if err := s.promote(ctx, crs.Items); err != nil {
		errs = append(errs, fmt.Errorf("promoting rules to cluster scope: %w", err))
	}
	return errors.Join(errs...)
}

// prune drops the runner's copy of the audit of each Intent not in live (by
// name and UID), and the last scan time of each namespace not in namespaces.
// work must be held.
func (s *SuggestRunner) prune(live map[types.NamespacedName]types.UID, namespaces map[string]bool) {
	for key, a := range s.audits {
		if uid, ok := live[key]; !ok || uid != a.uid {
			delete(s.audits, key)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ns := range s.lastScan {
		if !namespaces[ns] {
			delete(s.lastScan, ns)
		}
	}
}

// consolidate proposes one rule for each set of two or more of the
// namespace's CommandRules that share argv[0], pattern length, stages,
// providers, egress and install, merged with rules.MergePatterns (no LLM). The
// proposal must contain each merged rule's match; its examples are audited
// commands the rules covered.
func (s *SuggestRunner) consolidate(ctx context.Context, namespace string, crs []lastv1alpha1.CommandRule) error {
	sets := map[string][]lastv1alpha1.CommandRule{}
	for _, r := range crs {
		if k, ok := consolidationKey(r.Spec); ok {
			sets[k] = append(sets[k], r)
		}
	}
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(sets)) {
		set := sets[k]
		if len(set) < 2 {
			continue
		}
		slices.SortFunc(set, func(a, b lastv1alpha1.CommandRule) int { return cmp.Compare(a.Name, b.Name) })
		patterns := make([][]string, len(set))
		names := make([]string, len(set))
		specs := make([]lastv1alpha1.CommandRuleSpec, len(set))
		for i, r := range set {
			patterns[i], names[i], specs[i] = r.Spec.Match.Argv, r.Name, r.Spec
		}
		merged, ok := rules.MergePatterns(patterns)
		if !ok {
			logf.FromContext(ctx).V(1).Info("Did not consolidate CommandRules that do not merge", "namespace", namespace, "rules", names)
			continue
		}
		spec := normalSpec(set[0].Spec)
		spec.Match = lastv1alpha1.ArgvPattern{Argv: merged}
		examples, intent := s.coveredExamples([]string{namespace}, specs)
		p := suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeNamespace, Namespace: namespace,
			Trigger: lastv1alpha1.TriggerPeriodicReview, Spec: spec, Examples: examples, Contains: patterns,
			Rationale: nameList("Consolidates the CommandRules ", names, ", which differ only in values.")}
		if _, _, err := s.Creator.Create(ctx, p, intent); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// promote proposes one Cluster rule, as a suggestion in the system namespace,
// for each CommandRule spec (canonical JSON, ignoring source) that two or more
// namespaces hold.
func (s *SuggestRunner) promote(ctx context.Context, crs []lastv1alpha1.CommandRule) error {
	type holders struct {
		spec       lastv1alpha1.CommandRuleSpec
		names      []string
		namespaces []string
	}
	byKey := map[string]*holders{}
	for _, r := range crs {
		spec := normalSpec(r.Spec)
		b, err := json.Marshal(spec)
		if err != nil {
			continue // unreachable: strings only
		}
		h := byKey[string(b)]
		if h == nil {
			h = &holders{spec: spec}
			byKey[string(b)] = h
		}
		h.names = append(h.names, r.Namespace+"/"+r.Name)
		if !slices.Contains(h.namespaces, r.Namespace) {
			h.namespaces = append(h.namespaces, r.Namespace)
		}
	}
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		h := byKey[k]
		if len(h.namespaces) < 2 {
			continue
		}
		slices.Sort(h.names)
		examples, intent := s.coveredExamples(h.namespaces, []lastv1alpha1.CommandRuleSpec{h.spec})
		p := suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeCluster, Trigger: lastv1alpha1.TriggerPeriodicReview,
			Spec: h.spec, Examples: examples, Contains: [][]string{h.spec.Match.Argv},
			Rationale: nameList("The CommandRules ", h.names, " are identical; one ClusterCommandRule would cover every namespace.")}
		if _, _, err := s.Creator.Create(ctx, p, intent); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// consolidationKey is what CommandRules must share to be consolidated:
// argv[0], pattern length, stages, providers, egress and install. ok is false
// for a rule with an empty match.
func consolidationKey(spec lastv1alpha1.CommandRuleSpec) (string, bool) {
	if len(spec.Match.Argv) == 0 {
		return "", false
	}
	n := normalSpec(spec)
	b, err := json.Marshal(struct {
		Argv0     string                     `json:"argv0"`
		Len       int                        `json:"len"`
		Stages    []lastv1alpha1.Stage       `json:"stages"`
		Providers []string                   `json:"providers"`
		Egress    []string                   `json:"egress"`
		Install   []lastv1alpha1.ArgvPattern `json:"install"`
	}{spec.Match.Argv[0], len(spec.Match.Argv), n.Stages, n.Providers, n.Egress, n.Install})
	if err != nil {
		return "", false // unreachable: strings only
	}
	return string(b), true
}

// normalSpec is spec without source or description, with its stages,
// providers and egress sorted and deduplicated: the canonical form rules are
// compared and proposed in.
func normalSpec(spec lastv1alpha1.CommandRuleSpec) lastv1alpha1.CommandRuleSpec {
	out := lastv1alpha1.CommandRuleSpec{
		Stages:    sortedSet(spec.Stages),
		Providers: sortedSet(spec.Providers),
		Match:     lastv1alpha1.ArgvPattern{Argv: slices.Clone(spec.Match.Argv)},
		Install:   slices.Clone(spec.Install),
	}
	if len(spec.Egress) > 0 {
		out.Egress = sortedSet(spec.Egress)
	}
	return out
}

// coveredExamples returns up to maxExamples distinct audited commands, newest
// first, from the runner's copy of the audit of the given namespaces, that one
// of specs covers at the stage they ran at, and the Intent of the newest (nil
// when there is none).
func (s *SuggestRunner) coveredExamples(namespaces []string, specs []lastv1alpha1.CommandRuleSpec) ([]lastv1alpha1.Command, *lastv1alpha1.Intent) {
	var cmds []auditedCommand
	for key, a := range s.audits {
		if !slices.Contains(namespaces, key.Namespace) {
			continue
		}
		for _, c := range a.cmds {
			if slices.ContainsFunc(specs, func(sp lastv1alpha1.CommandRuleSpec) bool { return rules.Covers(sp, c.stage, c.cmd) }) {
				cmds = append(cmds, c)
			}
		}
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	return newestExamples(cmds)
}

// nameList is prefix, names joined with ", ", and suffix, within
// suggest.MaxRationale characters: names that don't fit are counted instead.
func nameList(prefix string, names []string, suffix string) string {
	for n := len(names); n > 0; n-- {
		list := strings.Join(names[:n], ", ")
		if n < len(names) {
			list += fmt.Sprintf(" and %d more", len(names)-n)
		}
		if s := prefix + list + suffix; len([]rune(s)) <= suggest.MaxRationale {
			return s
		}
	}
	return clip(fmt.Sprintf("%s%d rules%s", prefix, len(names), suffix))
}

// stageClass is the stage a rule for a command run at stage lists: Create for
// Update too, since Create and Update are one class (M6 design R15).
func stageClass(stage lastv1alpha1.Stage) lastv1alpha1.Stage {
	if stage == lastv1alpha1.StageUpdate {
		return lastv1alpha1.StageCreate
	}
	return stage
}

// sortedSet returns the sorted distinct elements of xs, never nil, so a
// proposal's name does not depend on nil versus empty.
func sortedSet[T cmp.Ordered](xs []T) []T {
	out := append([]T{}, xs...)
	slices.Sort(out)
	return slices.Compact(out)
}

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

const (
	systemNamespace = "last-system"
	argPods         = "pods"
	anyValue        = "[!-]*"
	authByRule      = "rule:pods"
)

// podsCmd is a "kubectl get pods <name>" step under the kubernetes provider.
func podsCmd(name string, extra ...string) lastv1alpha1.Command {
	return k8sStep("s1", append([]string{argKubectl, argGet, argPods, name}, extra...)...)
}

// approvedBy is the authorized_by of a command an IntentApproval in ns authorized.
func approvedBy(ns, approval string) string { return "approval:" + ns + "/" + approval }

// newRunner returns a SuggestRunner with its own fake gateway, so the audit and
// suggest calls of other specs' Intents never reach it.
func newRunner() (*SuggestRunner, *fakeGateway) {
	f := newFakeGateway(k8sClient)
	return &SuggestRunner{Creator: suiteRunner.Creator, Gateway: f, Client: k8sClient, Debounce: time.Minute}, f
}

// createIntents creates an Intent per name in ns; with no planning script they
// fail, which leaves them, and their audit, in place.
func createIntents(ns string, names ...string) {
	for _, n := range names {
		createIntent(types.NamespacedName{Namespace: ns, Name: n}, "run "+n)
	}
}

// setSuggestionsConfig creates the OperatorConfig with spec.suggestions cfg; it
// goes when the spec ends.
func setSuggestionsConfig(cfg lastv1alpha1.SuggestionsConfig) {
	op := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: operatorConfigName},
		Spec: lastv1alpha1.OperatorConfigSpec{Suggestions: &cfg}}
	Expect(k8sClient.Create(ctx, op)).To(Succeed())
	DeferCleanup(func() { Expect(k8sClient.Delete(ctx, op)).To(Succeed()) })
}

// refusingClient fails each suggestion create refuse returns an error for.
type refusingClient struct {
	client.Client
	refuse func(*lastv1alpha1.CommandRuleSuggestion) error
}

func (c refusingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if s, ok := obj.(*lastv1alpha1.CommandRuleSuggestion); ok {
		if err := c.refuse(s); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

// refusingCreator is the suite's SuggestionCreator with suggestion creates
// that refuse fails.
func refusingCreator(refuse func(*lastv1alpha1.CommandRuleSuggestion) error) *SuggestionCreator {
	c := *suiteRunner.Creator
	c.Client = refusingClient{Client: c.Client, refuse: refuse}
	return &c
}

// testClock is a clock specs move by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// counter counts calls per key.
type counter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *counter) inc(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[key]++
}

func (c *counter) get(key string) func() int {
	return func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.n[key]
	}
}

var _ = Describe("RepeatedApproval suggestions", func() {
	var (
		ns     string
		runner *SuggestRunner
		f      *fakeGateway
	)
	create, update := lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate
	podsAny := []string{argKubectl, argGet, argPods, anyValue}
	podsAnySpec := lastv1alpha1.CommandRuleSpec{Stages: stages(create), Providers: []string{providerK8s},
		Match: lastv1alpha1.ArgvPattern{Argv: podsAny}}
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }

	BeforeEach(func() {
		ns = newNamespace()
		runner, f = newRunner()
	})

	It("suggests after 3 distinct approvals of a group and not after 2", func() {
		createIntents(ns, "a", "b", "c")
		web, api, db := podsCmd("web"), podsCmd("api"), podsCmd("db")
		f.seedCommand(key("a"), create, web, approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), update, api, approvedBy(ns, "appr-b"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty())
		Expect(f.suggestRequests(key("b"))).To(BeEmpty())

		f.seedCommand(key("c"), create, db, approvedBy(ns, "appr-c"))
		f.setSuggestScript(key("c"), suggestReply(suggest.Answer{Entry: repeatEntry, Argv: podsAny, Rationale: "any pod"}))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())

		name := suggestionName(ns, podsAnySpec)
		got := getSuggestion(ns, name)
		Expect(got.Spec.TargetScope).To(Equal(lastv1alpha1.RuleScopeNamespace))
		Expect(got.Spec.Trigger).To(Equal(lastv1alpha1.TriggerRepeatedApproval))
		Expect(got.Spec.Rationale).To(Equal("any pod"))
		Expect(got.Spec.Proposed).To(Equal(podsAnySpec))
		Expect(got.Spec.Examples).To(Equal([]lastv1alpha1.Command{db, api, web}), "newest first")
		Expect(got.Labels).To(HaveKeyWithValue(labelIntent, "c"))
		Expect(suggestionsIn(ns)).To(HaveLen(1))
		Eventually(func() []string { return eventNotes(key("c"), ReasonSuggestionCreated) }).Should(HaveLen(1))
	})

	It("counts the same IntentApproval once, however many commands it ran", func() {
		createIntents(ns, "a", "b")
		for _, n := range []string{"web", "api", "db", "x"} {
			f.seedCommand(key("a"), create, podsCmd(n), approvedBy(ns, "appr-a"))
		}
		f.seedCommand(key("b"), create, podsCmd("y"), approvedBy(ns, "appr-b"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty())
		Expect(f.suggestRequests(key("b"))).To(BeEmpty())

		By("a second approval of the same Intent counts")
		f.seedCommand(key("b"), create, podsCmd("z"), approvedBy(ns, "appr-c"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(ConsistOf(HaveField("ObjectMeta.Name", suggestionName(ns, podsAnySpec))))
	})

	It("ignores rule- and Auto-authorized commands, and commands a rule covers", func() {
		createRule(ns, "pods-web", stages(create), argKubectl, argGet, argPods, "web")
		createIntents(ns, "a", "b", "c", "d", "e", "f")
		api, y, z := podsCmd("api"), podsCmd("y"), podsCmd("z")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, api, approvedBy(ns, "appr-b"))
		f.seedCommand(key("c"), create, podsCmd("db"), authByRule)
		f.seedCommand(key("d"), create, podsCmd("x"), "auto:create")
		f.seedCommand(key("e"), create, y, approvedBy(ns, "appr-e"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty())

		f.seedCommand(key("f"), create, z, approvedBy(ns, "appr-f"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		got := getSuggestion(ns, suggestionName(ns, podsAnySpec))
		Expect(got.Spec.Examples).To(Equal([]lastv1alpha1.Command{z, y, api}))
	})

	It("honours repeatThreshold from OperatorConfig", func() {
		createIntents(ns, "a", "b")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty(), "the default threshold is 3")

		setSuggestionsConfig(lastv1alpha1.SuggestionsConfig{RepeatThreshold: 2})
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(ConsistOf(HaveField("ObjectMeta.Name", suggestionName(ns, podsAnySpec))))
	})

	It("falls back to Merge when the LLM gives no answer, and makes no suggestion when neither works, with SuggestionFailed", func() {
		createIntents(ns, "a", "b", "c")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		f.seedCommand(key("c"), create, podsCmd("db"), approvedBy(ns, "appr-c"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(f.suggestRequests(key("c"))).To(HaveLen(1), "asked, with no script: no answer")
		got := getSuggestion(ns, suggestionName(ns, podsAnySpec))
		Expect(got.Spec.Rationale).To(ContainSubstring("3 IntentApprovals"))

		By("an answer that misses an example, and examples Merge can't join")
		ns2 := newNamespace()
		k2 := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns2, Name: name} }
		createIntents(ns2, "a", "b", "c")
		f.seedCommand(k2("a"), create, podsCmd("web"), approvedBy(ns2, "appr-a"))
		f.seedCommand(k2("b"), create, podsCmd("web", "-o", "json"), approvedBy(ns2, "appr-b"))
		f.seedCommand(k2("c"), create, podsCmd("db"), approvedBy(ns2, "appr-c"))
		f.setSuggestScript(k2("c"), suggestReply(suggest.Answer{Entry: repeatEntry, Argv: podsCmd("db").Run.Argv}))
		Expect(runner.ScanRepeated(ctx, ns2)).To(Succeed())
		Expect(suggestionsIn(ns2)).To(BeEmpty())
		Eventually(func() []string { return eventNotes(k2("c"), ReasonSuggestionFailed) }).Should(
			ConsistOf(ContainSubstring("no pattern")))
	})

	It("the LLM call uses the newest example Intent", func() {
		createIntents(ns, "a", "b", "c")
		web, api, db := podsCmd("web"), podsCmd("api"), podsCmd("db")
		f.seedCommand(key("c"), create, db, approvedBy(ns, "appr-c"))
		f.seedCommand(key("a"), create, web, approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, api, approvedBy(ns, "appr-b"))
		f.setSuggestScript(key("b"), suggestReply(suggest.Answer{Entry: repeatEntry, Argv: podsAny}))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())

		Expect(f.suggestRequests(key("a"))).To(BeEmpty())
		Expect(f.suggestRequests(key("c"))).To(BeEmpty())
		reqs := f.suggestRequests(key("b"))
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].GetPurpose()).To(Equal(gatewayv1.PurposeSuggest))
		Expect(reqs[0].GetMessages()[0].GetContent()).To(Equal(suggest.SystemPrompt))
		Expect(reqs[0].GetMessages()[1].GetContent()).To(Equal(suggest.Request([]suggest.Entry{
			{ID: repeatEntry, Stage: create, Argv: api.Run.Argv},
			{ID: repeatEntry, Stage: create, Argv: web.Run.Argv},
			{ID: repeatEntry, Stage: create, Argv: db.Run.Argv},
		})))
		Expect(getSuggestion(ns, suggestionName(ns, podsAnySpec)).Labels).To(HaveKeyWithValue(labelIntent, "b"))
	})

	It("keeps the 20 newest commands as examples, reading every page of the audit", func() {
		createIntents(ns, "a", "b", "c")
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		f.seedCommand(key("c"), create, podsCmd("db"), approvedBy(ns, "appr-c"))
		var newest []lastv1alpha1.Command
		for i := range 230 {
			c := podsCmd(fmt.Sprintf("n%03d", i))
			f.seedCommand(key("a"), create, c, approvedBy(ns, "appr-a"))
			newest = append([]lastv1alpha1.Command{c}, newest...)
		}
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		got := getSuggestion(ns, suggestionName(ns, podsAnySpec))
		Expect(got.Spec.Examples).To(Equal(newest[:20]))
	})

	It("does not ask again for a group until it gains an approval", func() {
		createIntents(ns, "a", "b", "c", "d")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		f.seedCommand(key("c"), create, podsCmd("db"), approvedBy(ns, "appr-c"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(f.suggestRequests(key("c"))).To(HaveLen(1))

		f.seedCommand(key("d"), create, podsCmd("x"), approvedBy(ns, "appr-d"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(f.suggestRequests(key("d"))).To(HaveLen(1))
	})

	It("skips an Intent the gateway no longer finds", func() {
		createIntents(ns, "a", "b", "c", "gone")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		f.seedCommand(key("c"), create, podsCmd("db"), approvedBy(ns, "appr-c"))
		f.updateScript(key("gone"), func(s *script) { s.queryErr = status.Error(codes.NotFound, "intent not found") })
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(ConsistOf(HaveField("ObjectMeta.Name", suggestionName(ns, podsAnySpec))))

		By("any other QueryAudit failure fails the scan")
		f.updateScript(key("gone"), func(s *script) { s.queryErr = status.Error(codes.Unavailable, "down") })
		Expect(runner.ScanRepeated(ctx, ns)).To(MatchError(ContainSubstring("down")))
	})

	It("a create refused for good does not stop the namespace's other groups, and its group is not asked again", func() {
		createIntents(ns, "a", "b", "c", "d", "e", "f")
		for _, n := range []string{"a", "b", "c"} {
			f.seedCommand(key(n), create, podsCmd("web-"+n), approvedBy(ns, "appr-"+n))
		}
		for _, n := range []string{"d", "e", "f"} {
			f.seedCommand(key(n), create, k8sStep("s1", argKubectl, argGet, "svc", "web-"+n), approvedBy(ns, "appr-"+n))
		}
		runner.Creator = refusingCreator(func(s *lastv1alpha1.CommandRuleSuggestion) error {
			if slices.Contains(s.Spec.Proposed.Match.Argv, argPods) {
				return apierrors.NewRequestEntityTooLargeError("limit is 3145728")
			}
			return nil
		})
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		svcAny := lastv1alpha1.CommandRuleSpec{Stages: stages(create), Providers: []string{providerK8s},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{argKubectl, argGet, "svc", anyValue}}}
		Expect(suggestionsIn(ns)).To(ConsistOf(HaveField("ObjectMeta.Name", suggestionName(ns, svcAny))))
		Eventually(func() []string { return eventNotes(key("c"), ReasonSuggestionFailed) }).Should(
			ConsistOf(ContainSubstring("Could not create the suggestion")))

		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(f.suggestRequests(key("c"))).To(HaveLen(1), "no second suggest call for the refused group")
		Expect(f.suggestRequests(key("f"))).To(HaveLen(1))
	})

	It("a create that fails for now does not stop the namespace's other groups, and its group is proposed again", func() {
		createIntents(ns, "a", "b", "c", "d", "e", "f")
		for _, n := range []string{"a", "b", "c"} {
			f.seedCommand(key(n), create, podsCmd("web-"+n), approvedBy(ns, "appr-"+n))
		}
		for _, n := range []string{"d", "e", "f"} {
			f.seedCommand(key(n), create, k8sStep("s1", argKubectl, argGet, "svc", "web-"+n), approvedBy(ns, "appr-"+n))
		}
		runner.Creator = refusingCreator(func(s *lastv1alpha1.CommandRuleSuggestion) error {
			if slices.Contains(s.Spec.Proposed.Match.Argv, argPods) {
				return apierrors.NewServiceUnavailable("etcd is down")
			}
			return nil
		})
		Expect(runner.ScanRepeated(ctx, ns)).To(MatchError(ContainSubstring("etcd is down")))
		Expect(suggestionsIn(ns)).To(HaveLen(1), "the svc group's suggestion")

		runner.Creator = suiteRunner.Creator
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(suggestionsIn(ns)).To(HaveLen(2))
		Expect(f.suggestRequests(key("c"))).To(HaveLen(2))
		Expect(f.suggestRequests(key("f"))).To(HaveLen(1))
	})

	It("suggests for an Intent whose name is too long for a label value, without the label", func() {
		long := strings.Repeat("x", 70)
		createIntents(ns, "a", "b", long)
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		f.seedCommand(key("b"), create, podsCmd("api"), approvedBy(ns, "appr-b"))
		f.seedCommand(key(long), create, podsCmd("db"), approvedBy(ns, "appr-c"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		got := getSuggestion(ns, suggestionName(ns, podsAnySpec))
		Expect(got.Labels).NotTo(HaveKey(labelIntent))
		Expect(f.suggestRequests(key(long))).To(HaveLen(1))
	})

	It("keeps no files or reason, in its audit copy or in examples", func() {
		createIntents(ns, "a", "b", "c")
		var plain []lastv1alpha1.Command
		for _, n := range []string{"a", "b", "c"} {
			c := podsCmd(n)
			plain = append([]lastv1alpha1.Command{c}, plain...)
			c.Reason = "look at " + n
			c.Files = map[string]string{"/work/big": strings.Repeat("x", 4096)}
			f.seedCommand(key(n), create, c, approvedBy(ns, "appr-"+n))
		}
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(getSuggestion(ns, suggestionName(ns, podsAnySpec)).Spec.Examples).To(Equal(plain))
		for _, n := range []string{"a", "b", "c"} {
			for _, c := range runner.audits[key(n)].cmds {
				Expect(c.cmd.Files).To(BeNil())
				Expect(c.cmd.Reason).To(BeEmpty())
			}
		}
	})

	It("keeps only the newest 200 commands of an Intent", func() {
		createIntents(ns, "a")
		var last uint64
		for i := range 230 {
			f.seedCommand(key("a"), create, podsCmd(fmt.Sprintf("n%03d", i)), approvedBy(ns, "appr-a"))
		}
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		cmds := runner.audits[key("a")].cmds
		Expect(cmds).To(HaveLen(maxCachedCommands))
		Expect(cmds[len(cmds)-1].cmd.Run.Argv).To(Equal(podsCmd("n229").Run.Argv))
		Expect(cmds[0].cmd.Run.Argv).To(Equal(podsCmd("n030").Run.Argv))
		last = runner.audits[key("a")].seq

		f.seedCommand(key("a"), create, podsCmd("n230"), approvedBy(ns, "appr-a"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		cmds = runner.audits[key("a")].cmds
		Expect(cmds).To(HaveLen(maxCachedCommands))
		Expect(cmds[0].cmd.Run.Argv).To(Equal(podsCmd("n031").Run.Argv))
		Expect(runner.audits[key("a")].seq).To(BeNumerically(">", last))
	})

	It("Review drops the audit copy of Intents that are gone, and the scan times of namespaces that are", func() {
		createIntents(ns, "a")
		f.seedCommand(key("a"), create, podsCmd("web"), approvedBy(ns, "appr-a"))
		Expect(runner.ScanRepeated(ctx, ns)).To(Succeed())
		Expect(runner.audits).To(HaveKey(key("a")))
		runner.Queue("gone-namespace")
		Expect(runner.due(time.Now())).To(ContainElement("gone-namespace"))

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := getIntent(key("a"))
			controllerutil.RemoveFinalizer(cur, lastv1alpha1.FinalizerTeardown)
			return k8sClient.Update(ctx, cur)
		})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getIntent(key("a")))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key("a"), &lastv1alpha1.Intent{}))
		}).Should(BeTrue())

		Expect(runner.Review(ctx)).To(Succeed())
		Expect(runner.audits).NotTo(HaveKey(key("a")))
		Expect(runner.lastScan).NotTo(HaveKey("gone-namespace"))
	})

	It("queues a scan of the namespace after a successful EndPhase", func() {
		k := types.NamespacedName{Namespace: ns, Name: intentName}
		gw.setScript(k, &script{llm: turns(answer([]lastv1alpha1.Command{argvStep("s1", trueCmd)}))})
		createIntent(k, "run web")
		Eventually(phase(k)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(suiteRunner.isQueued(ns)).To(BeFalse())
		approve(k, lastv1alpha1.DecisionApprove)
		Eventually(phase(k)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(suiteRunner.isQueued(ns)).To(BeTrue())
	})

	It("Queue debounces scans to one per minute per namespace", func() {
		clock := &testClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
		scans := &counter{}
		runner.Now = clock.Now
		runner.tick = 10 * time.Millisecond
		runner.scanFn = func(_ context.Context, ns string) error { scans.inc(ns); return nil }
		runner.reviewFn = func(context.Context) error { return nil }
		rctx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(runner.Start(rctx)).To(Succeed())
		}()

		runner.Queue("ns-a")
		Eventually(scans.get("ns-a")).Should(Equal(1))
		runner.Queue("ns-a")
		runner.Queue("ns-a")
		runner.Queue("ns-b")
		Eventually(scans.get("ns-b")).Should(Equal(1), "per namespace")
		Consistently(scans.get("ns-a"), 300*time.Millisecond).Should(Equal(1))

		clock.Add(59 * time.Second)
		Consistently(scans.get("ns-a"), 200*time.Millisecond).Should(Equal(1))
		clock.Add(2 * time.Second)
		Eventually(scans.get("ns-a")).Should(Equal(2), "the queued scans run once the minute is up")
		Consistently(scans.get("ns-a"), 200*time.Millisecond).Should(Equal(2), "as one scan")
	})
})

var _ = Describe("PeriodicReview suggestions", func() {
	var (
		runner *SuggestRunner
		f      *fakeGateway
	)
	create := lastv1alpha1.StageCreate

	BeforeEach(func() {
		runner, f = newRunner()
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: systemNamespace}})
		if !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("Review merges two CommandRules that differ in one position into a suggestion that contains both", func() {
		ns := newNamespace()
		createRule(ns, "pods-web", stages(create), argKubectl, argGet, argPods, "web")
		createRule(ns, "pods-api", stages(create), argKubectl, argGet, argPods, "api")
		a := types.NamespacedName{Namespace: ns, Name: "a"}
		createIntents(ns, "a")
		web := podsCmd("web")
		f.seedCommand(a, create, web, authByRule)
		Expect(runner.Review(ctx)).To(Succeed())

		merged := lastv1alpha1.CommandRuleSpec{Stages: stages(create), Providers: []string{providerK8s},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{argKubectl, argGet, argPods, anyValue}}}
		got := getSuggestion(ns, suggestionName(ns, merged))
		Expect(got.Spec.Trigger).To(Equal(lastv1alpha1.TriggerPeriodicReview))
		Expect(got.Spec.TargetScope).To(Equal(lastv1alpha1.RuleScopeNamespace))
		Expect(got.Spec.Proposed).To(Equal(merged))
		Expect(got.Spec.Rationale).To(And(ContainSubstring("pods-web"), ContainSubstring("pods-api")))
		Expect(got.Spec.Examples).To(Equal([]lastv1alpha1.Command{web}), "audited commands the rules covered")
		Expect(got.Labels).To(HaveKeyWithValue(labelIntent, "a"))
		Expect(suggestionsIn(ns)).To(HaveLen(1))
		Expect(f.suggestRequests(a)).To(BeEmpty(), "no LLM")
	})

	It("Review does not merge rules that differ at a StartsCode position", func() {
		ns := newNamespace()
		createRule(ns, "ssh-1", stages(create), "ssh", "host1", "uptime")
		createRule(ns, "ssh-2", stages(create), "ssh", "host2", "uptime")
		createRule(ns, "exec-1", stages(create), argKubectl, "exec", "web", "--", "date")
		createRule(ns, "exec-2", stages(create), argKubectl, "exec", "web", "--", "id")
		Expect(runner.Review(ctx)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty())
	})

	It("Review does not consolidate a rule for a destructive verb with one for another verb", func() {
		ns := newNamespace()
		del := lastv1alpha1.StageDelete
		createRule(ns, "delete-web", stages(del), argKubectl, "delete", argPods, "web")
		createRule(ns, "get-api", stages(del), argKubectl, argGet, argPods, "api")
		Expect(runner.Review(ctx)).To(Succeed())
		Expect(suggestionsIn(ns)).To(BeEmpty())
	})

	It("Review proposes a Cluster suggestion in last-system for identical rules in two namespaces", func() {
		ns1, ns2 := newNamespace(), newNamespace()
		argv := []string{"helm", "status", ns1}
		createRule(ns1, "helm-status", stages(create), argv...)
		createRule(ns2, "status", stages(create), argv...)
		Expect(runner.Review(ctx)).To(Succeed())

		spec := ruleSpec(stages(create), argv...)
		name, err := suggest.Name(suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeCluster, Spec: spec})
		Expect(err).NotTo(HaveOccurred())
		got := getSuggestion(systemNamespace, name)
		Expect(got.Spec.TargetScope).To(Equal(lastv1alpha1.RuleScopeCluster))
		Expect(got.Spec.Trigger).To(Equal(lastv1alpha1.TriggerPeriodicReview))
		Expect(got.Spec.Proposed).To(Equal(spec))
		Expect(got.Spec.Rationale).To(And(ContainSubstring(ns1+"/helm-status"), ContainSubstring(ns2+"/status")))
		Expect(suggestionsIn(ns1)).To(BeEmpty())
		Expect(suggestionsIn(ns2)).To(BeEmpty())
	})

	It("names the Denied Namespace suggestion, by namespace and name, that suppresses a Cluster proposal", func() {
		ns1, ns2 := newNamespace(), newNamespace()
		argv := []string{"helm", "history", ns1}
		createRule(ns1, "history", stages(create), argv...)
		createRule(ns2, "history", stages(create), argv...)
		a := types.NamespacedName{Namespace: ns1, Name: "a"}
		createIntents(ns1, "a")
		f.seedCommand(a, create, k8sStep("s1", argv...), authByRule)

		// Two Denied suggestions share a name; the namespace listed first does
		// not suppress the proposal, the second does.
		first := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-a-"}}
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		second := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-b-"}}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		createSuggestion(first.Name, "denied-helm", lastv1alpha1.TriggerPlanTime, lastv1alpha1.SuggestionDenied,
			ruleSpec(stages(create), "helm", "list"))
		createSuggestion(second.Name, "denied-helm", lastv1alpha1.TriggerPlanTime, lastv1alpha1.SuggestionDenied,
			ruleSpec(stages(create), argv...))
		Expect(runner.Review(ctx)).To(Succeed())

		Eventually(func() []string { return eventNotes(a, ReasonSuggestionSuppressed) }).Should(
			ConsistOf(ContainSubstring(second.Name + "/denied-helm")))
		spec := ruleSpec(stages(create), argv...)
		name, err := suggest.Name(suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeCluster, Spec: spec})
		Expect(err).NotTo(HaveOccurred())
		var s lastv1alpha1.CommandRuleSuggestion
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: systemNamespace, Name: name}, &s))).To(BeTrue())
	})

	It("Start runs Review every reviewInterval from OperatorConfig, with ±10% jitter", func() {
		setSuggestionsConfig(lastv1alpha1.SuggestionsConfig{ReviewInterval: &metav1.Duration{Duration: 2 * time.Hour}})
		clock := &testClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
		reviews := &counter{}
		runner.Now = clock.Now
		runner.tick = 10 * time.Millisecond
		runner.jitter = func() float64 { return 1 } // the most: +10%
		runner.reviewFn = func(context.Context) error { reviews.inc(""); return nil }
		rctx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(runner.Start(rctx)).To(Succeed())
		}()

		Consistently(reviews.get(""), 200*time.Millisecond).Should(BeZero())
		clock.Add(2*time.Hour + 11*time.Minute)
		Consistently(reviews.get(""), 200*time.Millisecond).Should(BeZero(), "2h plus 10% is 2h12m")
		clock.Add(2 * time.Minute)
		Eventually(reviews.get("")).Should(Equal(1))
		clock.Add(2*time.Hour + 13*time.Minute)
		Eventually(reviews.get("")).Should(Equal(2))
	})

	It("defaults the review interval to 24h, jittered by up to 10% either way", func() {
		runner.jitter = func() float64 { return 0 }
		Expect(runner.reviewInterval(ctx)).To(Equal(time.Duration(0.9 * float64(24*time.Hour))))
		runner.jitter = func() float64 { return 1 }
		Expect(runner.reviewInterval(ctx)).To(Equal(time.Duration(1.1 * float64(24*time.Hour))))
	})
})

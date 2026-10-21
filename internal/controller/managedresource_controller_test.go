package controller

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// These specs call Reconcile directly with a controlled clock; the suite's
// manager runs only the Intent controller.
var _ = Describe("ManagedResource probe schedule", func() {
	var (
		key  types.NamespacedName
		r    *ManagedResourceReconciler
		now  time.Time
		last time.Time // the Verifying run's start, as recorded on the ManagedResource
	)

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
		r = &ManagedResourceReconciler{Client: k8sClient, Reader: k8sClient, Scheme: scheme.Scheme, Gateway: gw,
			Now: func() time.Time { return now }, Jitter: func() float64 { return 0 }}
		resource := lastv1alpha1.ManagedResourceSpec{Probes: []lastv1alpha1.Probe{{ID: "p1",
			Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "true"}}}
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{argvStep("s1", "true")}, resource))})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		last = getMR(key.Namespace, "web-0").Status.LastProbeTime.Time
	})

	reconcileAt := func(t time.Time) ctrl.Result {
		now = t
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	probeCalls := func() int {
		n := 0
		for _, c := range gw.calls(key) {
			if c == runProbes {
				n++
			}
		}
		return n
	}

	scheduledInterval := func() time.Duration {
		sched, ok := r.schedule(key)
		Expect(ok).To(BeTrue())
		return sched.interval
	}

	It("runs the probes when they are due and not before", func() {
		before := probeCalls()
		Expect(reconcileAt(last.Add(30 * time.Second)).RequeueAfter).To(Equal(30 * time.Second))
		Expect(probeCalls()).To(Equal(before))

		// A new Intent is probed at the fast interval; the next run is 2m out.
		Expect(reconcileAt(last.Add(time.Minute)).RequeueAfter).To(Equal(2 * time.Minute))
		Expect(probeCalls()).To(Equal(before + 1))
		mr := getMR(key.Namespace, "web-0")
		Expect(mr.Status.LastProbeTime.Time).To(BeTemporally("~", last.Add(time.Minute), time.Second))
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "passed"))).To(Equal(1.0))
	})

	It("probes a Ready Intent at the fast interval, then less often", func() {
		at := last
		wait := reconcileAt(at).RequeueAfter // first sight: the first run is due a fast interval after the last probe
		Expect(wait).To(Equal(time.Minute))
		gaps := make([]time.Duration, 0, 5)
		for range 5 {
			at = at.Add(wait)
			res := reconcileAt(at)
			wait = res.RequeueAfter
			gaps = append(gaps, res.RequeueAfter)
		}
		Expect(gaps).To(Equal([]time.Duration{2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}))
		Expect(testutil.ToFloat64(probeInterval.WithLabelValues(key.Namespace, key.Name))).To(Equal(300.0))
	})

	It("probes fast again after a failure", func() {
		reconcileAt(last.Add(time.Minute))
		gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
		Expect(reconcileAt(last.Add(3 * time.Minute)).RequeueAfter).To(Equal(time.Minute))
		Expect(testutil.ToFloat64(probeInterval.WithLabelValues(key.Namespace, key.Name))).To(Equal(60.0))
	})

	It("probes fast again when a passed run could not be recorded", func() {
		reconcileAt(last.Add(time.Minute)) // passed: the interval doubles to 2m
		Expect(scheduledInterval()).To(Equal(2 * time.Minute))
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		r.Client = interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*lastv1alpha1.ManagedResource); ok {
					return errors.New("status update refused")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		})
		now = last.Add(3 * time.Minute)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		Expect(scheduledInterval()).To(Equal(time.Minute))
	})

	It("probes fast again when the plan changes", func() {
		reconcileAt(last.Add(time.Minute))
		reconcileAt(last.Add(3 * time.Minute)) // the interval is now 4m
		Expect(scheduledInterval()).To(Equal(4 * time.Minute))
		Eventually(func() error {
			var in lastv1alpha1.Intent
			if err := k8sClient.Get(ctx, key, &in); err != nil {
				return err
			}
			in.Status.Plan.Hash = "changed"
			return k8sClient.Status().Update(ctx, &in)
		}).Should(Succeed())
		// The first run of a new plan is due a fast interval after the newest probe.
		Expect(reconcileAt(last.Add(3*time.Minute + 30*time.Second)).RequeueAfter).To(Equal(30 * time.Second))
		Expect(scheduledInterval()).To(Equal(time.Minute))
	})

	It("forgets the interval gauge when the Intent is no longer probed", func() {
		reconcileAt(last.Add(time.Minute))
		Expect(testutil.CollectAndCount(probeInterval)).To(BeNumerically(">=", 1))
		r.forget(key)
		Expect(testutil.ToFloat64(probeInterval.WithLabelValues(key.Namespace, key.Name))).To(Equal(0.0))
	})

	It("records drift when a scheduled run fails", func() {
		gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
		reconcileAt(last.Add(time.Minute))
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())
		Eventually(func() []string { return eventReasons(key, corev1.EventTypeNormal) }).Should(
			ContainElement(string(lastv1alpha1.PhaseDrifted)))
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "failed"))).To(Equal(1.0))
	})

	It("counts the runs that fell due while a run was in flight as skipped", func() {
		start := last.Add(time.Minute)
		gw.updateScript(key, func(s *script) { s.onProbe = func() { now = start.Add(5 * time.Minute) } })
		// The next run was due at start+2m and start+4m while this one took 5 minutes; the next is at start+6m.
		Expect(reconcileAt(start).RequeueAfter).To(Equal(time.Minute))
		Expect(testutil.ToFloat64(probeRunsSkipped.WithLabelValues(key.Namespace, key.Name))).To(Equal(2.0))
	})

	It("skips a run when the gateway already has one in flight", func() {
		gw.updateScript(key, func(s *script) {
			s.probeErr = status.Error(codes.Aborted, "a probe run for this intent is already in flight")
		})
		Expect(reconcileAt(last.Add(time.Minute)).RequeueAfter).To(Equal(time.Minute))
		Expect(testutil.ToFloat64(probeRunsSkipped.WithLabelValues(key.Namespace, key.Name))).To(Equal(1.0))
		Expect(getMR(key.Namespace, "web-0").Status.LastProbeTime.Time).To(BeTemporally("~", last, time.Second))
	})

	It("does not probe an Intent that is not Ready", func() {
		other := types.NamespacedName{Namespace: key.Namespace, Name: "unplanned"}
		createIntent(other, "no script")
		Eventually(phase(other)).Should(Equal(lastv1alpha1.PhaseFailed))
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: other})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{}))
		Expect(gw.calls(other)).NotTo(ContainElement(runProbes))
	})
})

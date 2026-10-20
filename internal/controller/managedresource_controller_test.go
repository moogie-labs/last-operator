package controller

import (
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
			Now: func() time.Time { return now }}
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

	It("runs the probes when they are due and not before", func() {
		before := probeCalls()
		Expect(reconcileAt(last.Add(time.Minute)).RequeueAfter).To(Equal(4 * time.Minute))
		Expect(probeCalls()).To(Equal(before))

		Expect(reconcileAt(last.Add(5 * time.Minute)).RequeueAfter).To(Equal(5 * time.Minute))
		Expect(probeCalls()).To(Equal(before + 1))
		mr := getMR(key.Namespace, "web-0")
		Expect(mr.Status.LastProbeTime.Time).To(BeTemporally("~", last.Add(5*time.Minute), time.Second))
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "passed"))).To(Equal(1.0))
	})

	It("records drift when a scheduled run fails", func() {
		gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
		reconcileAt(last.Add(5 * time.Minute))
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())
		Eventually(func() []string { return eventReasons(key, corev1.EventTypeNormal) }).Should(
			ContainElement(string(lastv1alpha1.PhaseDrifted)))
		Expect(testutil.ToFloat64(probeRuns.WithLabelValues(key.Namespace, key.Name, "failed"))).To(Equal(1.0))
	})

	It("counts the runs that fell due while a run was in flight as skipped", func() {
		start := last.Add(5 * time.Minute)
		gw.updateScript(key, func(s *script) { s.onProbe = func() { now = start.Add(11 * time.Minute) } })
		// Runs were due at start+5m and start+10m while this one took 11 minutes; the next is at start+15m.
		Expect(reconcileAt(start).RequeueAfter).To(Equal(4 * time.Minute))
		Expect(testutil.ToFloat64(probeRunsSkipped.WithLabelValues(key.Namespace, key.Name))).To(Equal(2.0))
	})

	It("skips a run when the gateway already has one in flight", func() {
		gw.updateScript(key, func(s *script) {
			s.probeErr = status.Error(codes.Aborted, "a probe run for this intent is already in flight")
		})
		Expect(reconcileAt(last.Add(5 * time.Minute)).RequeueAfter).To(Equal(5 * time.Minute))
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

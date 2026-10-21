package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// intentLabels labels the per-Intent probe metrics.
var intentLabels = []string{"namespace", "intent"}

var (
	probeRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_total",
		Help: "Scheduled probe runs, by result (passed, failed, error).",
	}, append(intentLabels[:2:2], "result"))
	probeRunsSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_skipped_total",
		Help: "Scheduled probe runs skipped because the previous run was still in flight (spec §10.1).",
	}, intentLabels)
	probeInterval = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "last_operator_last_probe_interval_seconds",
		Help: "The probe interval an Intent's next run was scheduled at, before jitter (spec §10.1).",
	}, intentLabels)
)

func init() {
	metrics.Registry.MustRegister(probeRuns, probeRunsSkipped, probeInterval)
}

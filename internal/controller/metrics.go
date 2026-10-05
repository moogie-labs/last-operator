package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	probeRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_total",
		Help: "Scheduled probe runs, by result (passed, failed, error).",
	}, []string{"namespace", "intent", "result"})
	probeRunsSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "last_operator_probe_runs_skipped_total",
		Help: "Scheduled probe runs skipped because the previous run was still in flight (spec §10.1).",
	}, []string{"namespace", "intent"})
)

func init() {
	metrics.Registry.MustRegister(probeRuns, probeRunsSkipped)
}

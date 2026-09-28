package gateway

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/moogie-labs/last-operator/internal/audit"
)

type Metrics struct {
	registry *prometheus.Registry
	writes   *prometheus.CounterVec
	failures prometheus.Counter
	seq      prometheus.Gauge
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_audit_writes_total", Help: "Audit entries written, by type.",
		}, []string{"type"}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "last_audit_write_failures_total", Help: "Audit writes that failed and broke the writer.",
		}),
		seq: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "last_audit_chain_seq", Help: "Sequence number of the last audit entry written.",
		}),
	}
	m.registry.MustRegister(m.writes, m.failures, m.seq)
	return m
}

func (m *Metrics) ObserveWrite(e audit.Entry) {
	m.writes.WithLabelValues(e.Type).Inc()
	m.seq.Set(float64(e.Seq))
}

func (m *Metrics) ObserveFailure(error) { m.failures.Inc() }

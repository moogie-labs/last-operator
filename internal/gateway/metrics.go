package gateway

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
)

type Metrics struct {
	registry *prometheus.Registry
	writes   *prometheus.CounterVec
	failures prometheus.Counter
	seq      prometheus.Gauge

	llmCalls  *prometheus.CounterVec
	llmTokens *prometheus.CounterVec
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
		llmCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_llm_calls_total", Help: "LLM calls by provider, model and result (ok or error).",
		}, []string{"provider", "model", "result"}),
		llmTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "last_llm_tokens_total", Help: "LLM tokens by provider, model and kind (input, output, cache_read, cache_creation).",
		}, []string{"provider", "model", "kind"}),
	}
	m.registry.MustRegister(m.writes, m.failures, m.seq, m.llmCalls, m.llmTokens)
	return m
}

func (m *Metrics) ObserveWrite(e audit.Entry) {
	m.writes.WithLabelValues(e.Type).Inc()
	m.seq.Set(float64(e.Seq))
}

func (m *Metrics) ObserveFailure(error) { m.failures.Inc() }

// ObserveLLM counts one provider call and the tokens it used (spec §16).
func (m *Metrics) ObserveLLM(provider, model string, r llm.Response, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.llmCalls.WithLabelValues(provider, model, result).Inc()
	for kind, n := range map[string]int{"input": r.InputTokens, "output": r.OutputTokens,
		"cache_read": r.CacheReadTokens, "cache_creation": r.CacheCreationTokens} {
		if n > 0 {
			m.llmTokens.WithLabelValues(provider, model, kind).Add(float64(n))
		}
	}
}

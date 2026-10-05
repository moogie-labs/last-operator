package policy

import (
	"time"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// DefaultProbeInterval is the shipped steady-state probe interval (spec §6).
	DefaultProbeInterval = 5 * time.Minute
	// DefaultMinProbeInterval is the shipped floor for every probe interval (spec §10.1).
	DefaultMinProbeInterval = time.Minute
)

// ProbeInterval returns the fixed probe interval for a namespace: the shortest
// probes.interval set by OperatorConfig.spec.defaults or any IntentPolicy
// (DefaultProbeInterval when none sets one), never below the cluster floor
// OperatorConfig.spec.probes.minInterval. Adaptive intervals arrive in M5.
func ProbeInterval(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) time.Duration {
	floor := DefaultMinProbeInterval
	if op != nil && op.Spec.Probes != nil && op.Spec.Probes.MinInterval != nil && op.Spec.Probes.MinInterval.Duration > 0 {
		floor = op.Spec.Probes.MinInterval.Duration
	}
	var interval time.Duration
	consider := func(p *lastv1alpha1.ProbePolicy) {
		if p == nil || p.Interval == nil || p.Interval.Duration <= 0 {
			return
		}
		if interval == 0 || p.Interval.Duration < interval {
			interval = p.Interval.Duration
		}
	}
	if op != nil && op.Spec.Defaults != nil {
		consider(op.Spec.Defaults.Probes)
	}
	for i := range policies {
		consider(policies[i].Spec.Probes)
	}
	if interval == 0 {
		interval = DefaultProbeInterval
	}
	return max(interval, floor)
}

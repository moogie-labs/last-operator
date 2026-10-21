package policy

import (
	"time"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// DefaultProbeInterval is the shipped steady-state probe interval (spec §6).
	DefaultProbeInterval = 5 * time.Minute
	// DefaultFastProbeInterval is the shipped probe interval after a change or drift (spec §6).
	DefaultFastProbeInterval = time.Minute
	// DefaultMinProbeInterval is the shipped floor for every probe interval (spec §10.1).
	DefaultMinProbeInterval = time.Minute
)

// ProbeIntervals returns the fast and steady probe intervals for a namespace
// (spec §10.1). fast is the shortest probes.fastInterval set by
// OperatorConfig.spec.defaults or any IntentPolicy (DefaultFastProbeInterval
// when none sets one); steady is the shortest probes.interval set
// (DefaultProbeInterval when none sets one). Neither is below the cluster floor
// OperatorConfig.spec.probes.minInterval, and fast is never above steady.
func ProbeIntervals(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) (fast, steady time.Duration) {
	floor := DefaultMinProbeInterval
	if op != nil && op.Spec.Probes != nil && op.Spec.Probes.MinInterval != nil && op.Spec.Probes.MinInterval.Duration > 0 {
		floor = op.Spec.Probes.MinInterval.Duration
	}
	consider := func(p *lastv1alpha1.ProbePolicy) {
		if p == nil {
			return
		}
		if d := p.FastInterval; d != nil && d.Duration > 0 && (fast == 0 || d.Duration < fast) {
			fast = d.Duration
		}
		if d := p.Interval; d != nil && d.Duration > 0 && (steady == 0 || d.Duration < steady) {
			steady = d.Duration
		}
	}
	if op != nil && op.Spec.Defaults != nil {
		consider(op.Spec.Defaults.Probes)
	}
	for i := range policies {
		consider(policies[i].Spec.Probes)
	}
	if fast == 0 {
		fast = DefaultFastProbeInterval
	}
	if steady == 0 {
		steady = DefaultProbeInterval
	}
	steady = max(steady, floor)
	fast = min(max(fast, floor), steady)
	return fast, steady
}

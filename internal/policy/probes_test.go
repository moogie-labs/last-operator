package policy_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
)

func probes(d time.Duration) *lastv1alpha1.ProbePolicy {
	return &lastv1alpha1.ProbePolicy{Interval: &metav1.Duration{Duration: d}}
}

func opWith(defaults *lastv1alpha1.ProbePolicy, minInterval time.Duration) *lastv1alpha1.OperatorConfig {
	op := &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{Probes: defaults}}}
	if minInterval > 0 {
		op.Spec.Probes = &lastv1alpha1.ProbeLimits{MinInterval: &metav1.Duration{Duration: minInterval}}
	}
	return op
}

func nsPolicy(p *lastv1alpha1.ProbePolicy) lastv1alpha1.IntentPolicy {
	return lastv1alpha1.IntentPolicy{Spec: lastv1alpha1.IntentPolicySpec{Probes: p}}
}

func fastProbes(fast, steady time.Duration) *lastv1alpha1.ProbePolicy {
	p := &lastv1alpha1.ProbePolicy{}
	if fast > 0 {
		p.FastInterval = &metav1.Duration{Duration: fast}
	}
	if steady > 0 {
		p.Interval = &metav1.Duration{Duration: steady}
	}
	return p
}

func TestProbeIntervalsDefaultsAndFloor(t *testing.T) {
	m := time.Minute
	cases := []struct {
		name         string
		op           *lastv1alpha1.OperatorConfig
		policies     []lastv1alpha1.IntentPolicy
		fast, steady time.Duration
	}{
		{"no config", nil, nil, m, 5 * m},
		{"cluster default", opWith(probes(3*m), 0), nil, m, 3 * m},
		{"policy shortens", opWith(probes(3*m), 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(2 * m))}, m, 2 * m},
		{"shortest policy wins", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(4 * m)), nsPolicy(probes(90 * time.Second))}, m, 90 * time.Second},
		{"policy cannot go below the default floor", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(10 * time.Second))}, m, m},
		{"cluster floor lifts both", opWith(fastProbes(90*time.Second, 2*m), 3*m), nil, 3 * m, 3 * m},
		{"floor above the default", opWith(nil, 10*m), nil, 10 * m, 10 * m},
		{"fast interval set", opWith(fastProbes(2*m, 0), 0), nil, 2 * m, 5 * m},
		{"shortest fast interval wins", opWith(fastProbes(3*m, 0), 0), []lastv1alpha1.IntentPolicy{nsPolicy(fastProbes(2*m, 0))}, 2 * m, 5 * m},
		{"fast above steady is clamped to steady", opWith(fastProbes(20*m, 0), 0), nil, 5 * m, 5 * m},
		{"unset and zero intervals are ignored", opWith(&lastv1alpha1.ProbePolicy{}, 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(0)), nsPolicy(nil)}, m, 5 * m},
	}
	for _, tc := range cases {
		fast, steady := policy.ProbeIntervals(tc.op, tc.policies)
		if fast != tc.fast || steady != tc.steady {
			t.Errorf("%s: ProbeIntervals = %v, %v; want %v, %v", tc.name, fast, steady, tc.fast, tc.steady)
		}
	}
}

func TestProbeFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   *lastv1alpha1.OperatorConfig
		want time.Duration
	}{
		{"no OperatorConfig", nil, policy.DefaultMinProbeInterval},
		{"no floor set", opWith(nil, 0), policy.DefaultMinProbeInterval},
		{"floor set", opWith(nil, 3*time.Minute), 3 * time.Minute},
	} {
		if got := policy.ProbeFloor(tc.op); got != tc.want {
			t.Errorf("%s: ProbeFloor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

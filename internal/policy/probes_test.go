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

func TestProbeInterval(t *testing.T) {
	cases := []struct {
		name     string
		op       *lastv1alpha1.OperatorConfig
		policies []lastv1alpha1.IntentPolicy
		want     time.Duration
	}{
		{"no config", nil, nil, 5 * time.Minute},
		{"cluster default", opWith(probes(3*time.Minute), 0), nil, 3 * time.Minute},
		{"policy shortens", opWith(probes(3*time.Minute), 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(2 * time.Minute))}, 2 * time.Minute},
		{"shortest policy wins", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(4 * time.Minute)), nsPolicy(probes(90 * time.Second))}, 90 * time.Second},
		{"policy cannot go below the default floor", nil, []lastv1alpha1.IntentPolicy{nsPolicy(probes(10 * time.Second))}, time.Minute},
		{"cluster floor applies", opWith(probes(2*time.Minute), 3*time.Minute), nil, 3 * time.Minute},
		{"floor above the default", opWith(nil, 10*time.Minute), nil, 10 * time.Minute},
		{"unset and zero intervals are ignored", opWith(&lastv1alpha1.ProbePolicy{}, 0), []lastv1alpha1.IntentPolicy{nsPolicy(probes(0)), nsPolicy(nil)}, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := policy.ProbeInterval(tc.op, tc.policies); got != tc.want {
			t.Errorf("%s: ProbeInterval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

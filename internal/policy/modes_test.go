package policy_test

import (
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
)

const (
	auto   = lastv1alpha1.ModeAuto
	manual = lastv1alpha1.ModeManual
)

// modeLayer builds a policy layer that allows Auto on Create and sets the given Create mode.
func modeLayer(allow bool, mode lastv1alpha1.Mode) lastv1alpha1.IntentPolicySpec {
	return lastv1alpha1.IntentPolicySpec{
		AllowAutoModes: &lastv1alpha1.StageFlags{Create: allow},
		Modes:          &lastv1alpha1.StageModes{Create: mode},
	}
}

func opModes(d lastv1alpha1.IntentPolicySpec) *lastv1alpha1.OperatorConfig {
	return &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &d}}
}

func ipol(s lastv1alpha1.IntentPolicySpec) lastv1alpha1.IntentPolicy {
	return lastv1alpha1.IntentPolicy{Spec: s}
}

func TestModesDefaultToManual(t *testing.T) {
	for _, op := range []*lastv1alpha1.OperatorConfig{nil, {}} {
		m := policy.Modes(op, nil, &lastv1alpha1.StageModes{Create: auto})
		if got := m.Status(); *got != (lastv1alpha1.StageModes{Discovery: manual, Create: manual, Reconcile: manual, Delete: manual}) {
			t.Fatalf("Status() = %+v, want all Manual", *got)
		}
	}
}

func TestAutoNeedsEveryLayerToAllowIt(t *testing.T) {
	op := opModes(modeLayer(true, auto))
	pols := []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, "")), ipol(lastv1alpha1.IntentPolicySpec{})}
	if got := policy.Modes(op, pols, &lastv1alpha1.StageModes{Create: auto}).Create; got != manual {
		t.Fatalf("Create = %q, want Manual when one policy does not allow Auto", got)
	}
	if got := policy.Modes(op, pols[:1], &lastv1alpha1.StageModes{Create: auto}).Create; got != auto {
		t.Fatalf("Create = %q, want Auto when every layer allows it", got)
	}
	if got := policy.Modes(opModes(modeLayer(false, auto)), nil, &lastv1alpha1.StageModes{Create: auto}).Create; got != manual {
		t.Fatalf("Create = %q, want Manual when the defaults do not allow Auto", got)
	}
}

func TestAnyPolicyManualWins(t *testing.T) {
	op := opModes(modeLayer(true, auto))
	pols := []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, auto)), ipol(modeLayer(true, manual))}
	if got := policy.Modes(op, pols, nil).Create; got != manual {
		t.Fatalf("Create = %q, want Manual", got)
	}
	if got := policy.Modes(op, pols[:1], nil).Create; got != auto {
		t.Fatalf("Create = %q, want Auto from the policies' Auto", got)
	}
}

func TestTheIntentCanLoosenOnlyWhenAllowed(t *testing.T) {
	loosen := &lastv1alpha1.StageModes{Create: auto}
	if got := policy.Modes(opModes(modeLayer(true, manual)), nil, loosen).Create; got != auto {
		t.Fatalf("Create = %q, want Auto when allowed", got)
	}
	if got := policy.Modes(opModes(modeLayer(false, manual)), nil, loosen).Create; got != manual {
		t.Fatalf("Create = %q, want Manual when not allowed", got)
	}
}

func TestAnIntentManualAlwaysWins(t *testing.T) {
	op := opModes(modeLayer(true, auto))
	if got := policy.Modes(op, nil, &lastv1alpha1.StageModes{Create: manual}).Create; got != manual {
		t.Fatalf("Create = %q, want Manual", got)
	}
}

func TestUpdateUsesTheCreateKey(t *testing.T) {
	m := policy.Modes(opModes(modeLayer(true, auto)), nil, nil)
	if m.For(lastv1alpha1.StageUpdate) != auto || m.For(lastv1alpha1.StageCreate) != auto {
		t.Fatalf("Update/Create = %q/%q, want Auto", m.For(lastv1alpha1.StageUpdate), m.For(lastv1alpha1.StageCreate))
	}
	if m.For(lastv1alpha1.StageRemediate) != manual || m.For(lastv1alpha1.Stage("Bogus")) != manual {
		t.Fatal("other and unknown stages must be Manual")
	}
	if policy.KeyFor(lastv1alpha1.StageUpdate) != policy.KeyCreate || policy.KeyFor(lastv1alpha1.StageRemediate) != policy.KeyReconcile ||
		policy.KeyFor(lastv1alpha1.StageDiscovery) != policy.KeyDiscovery || policy.KeyFor(lastv1alpha1.StageDelete) != policy.KeyDelete ||
		policy.KeyFor("Bogus") != "" {
		t.Fatal("KeyFor mapping is wrong")
	}
}

func TestModesIgnorePolicyOrder(t *testing.T) {
	op := opModes(modeLayer(true, auto))
	a, b, c := ipol(modeLayer(true, auto)), ipol(modeLayer(true, manual)), ipol(modeLayer(true, ""))
	perms := [][]lastv1alpha1.IntentPolicy{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}}
	for _, p := range perms {
		if got := policy.Modes(op, p, &lastv1alpha1.StageModes{Create: auto}).Create; got != manual {
			t.Fatalf("Create = %q for order %v, want Manual", got, p)
		}
	}
}

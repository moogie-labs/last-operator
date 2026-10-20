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

func TestAnIntentPolicyManualIsFinal(t *testing.T) {
	op := opModes(modeLayer(true, auto))
	pols := []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, manual))}
	if got := policy.Modes(op, pols, &lastv1alpha1.StageModes{Create: auto}).Create; got != manual {
		t.Fatalf("Create = %q, want Manual: an IntentPolicy's Manual beats the Intent's Auto", got)
	}
}

// setKey returns StageModes with only the given key set to m.
func setKey(key policy.ModeKey, m lastv1alpha1.Mode) *lastv1alpha1.StageModes {
	s := &lastv1alpha1.StageModes{}
	switch key {
	case policy.KeyDiscovery:
		s.Discovery = m
	case policy.KeyCreate:
		s.Create = m
	case policy.KeyReconcile:
		s.Reconcile = m
	case policy.KeyDelete:
		s.Delete = m
	}
	return s
}

func allowKey(key policy.ModeKey) *lastv1alpha1.StageFlags {
	f := &lastv1alpha1.StageFlags{}
	switch key {
	case policy.KeyDiscovery:
		f.Discovery = true
	case policy.KeyCreate:
		f.Create = true
	case policy.KeyReconcile:
		f.Reconcile = true
	case policy.KeyDelete:
		f.Delete = true
	}
	return f
}

func autoKeys(m policy.EffectiveModes) map[policy.ModeKey]bool {
	return map[policy.ModeKey]bool{
		policy.KeyDiscovery: m.Discovery == auto, policy.KeyCreate: m.Create == auto,
		policy.KeyReconcile: m.Reconcile == auto, policy.KeyDelete: m.Delete == auto,
	}
}

func TestModesPerKey(t *testing.T) {
	for _, key := range []policy.ModeKey{policy.KeyDiscovery, policy.KeyCreate, policy.KeyReconcile, policy.KeyDelete} {
		defaults := lastv1alpha1.IntentPolicySpec{AllowAutoModes: allowKey(key)}
		cases := map[string]policy.EffectiveModes{
			"intent":   policy.Modes(opModes(defaults), nil, setKey(key, auto)),
			"defaults": policy.Modes(opModes(lastv1alpha1.IntentPolicySpec{AllowAutoModes: allowKey(key), Modes: setKey(key, auto)}), nil, nil),
			"policy": policy.Modes(opModes(defaults), []lastv1alpha1.IntentPolicy{
				ipol(lastv1alpha1.IntentPolicySpec{AllowAutoModes: allowKey(key), Modes: setKey(key, auto)})}, nil),
		}
		for name, m := range cases {
			for k, isAuto := range autoKeys(m) {
				if isAuto != (k == key) {
					t.Errorf("key %s via %s: %s auto = %v", key, name, k, isAuto)
				}
			}
			if m.For(map[policy.ModeKey]lastv1alpha1.Stage{
				policy.KeyDiscovery: lastv1alpha1.StageDiscovery, policy.KeyCreate: lastv1alpha1.StageCreate,
				policy.KeyReconcile: lastv1alpha1.StageRemediate, policy.KeyDelete: lastv1alpha1.StageDelete,
			}[key]) != auto {
				t.Errorf("key %s via %s: For is not Auto", key, name)
			}
		}
	}
}

func TestAnUnknownModeIsUnset(t *testing.T) {
	bogus := &lastv1alpha1.StageModes{Create: "Bogus"}
	// In the Intent, with the policy layers on Auto: treated as unset, so the policies' Auto stands.
	if got := policy.Modes(opModes(modeLayer(true, auto)), nil, bogus).Create; got != auto {
		t.Fatalf("Create = %q, want Auto: a bogus Intent mode is unset", got)
	}
	// In the Intent, with nothing setting Auto: Manual, not Auto.
	if got := policy.Modes(opModes(modeLayer(true, "")), nil, bogus).Create; got != manual {
		t.Fatalf("Create = %q, want Manual", got)
	}
	// In a policy: it neither forces Manual nor counts as Auto.
	pols := []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, "Bogus"))}
	if got := policy.Modes(opModes(modeLayer(true, auto)), pols, nil).Create; got != auto {
		t.Fatalf("Create = %q, want Auto: a bogus policy mode is unset", got)
	}
	if got := policy.Modes(opModes(modeLayer(true, "")), pols, nil).Create; got != manual {
		t.Fatalf("Create = %q, want Manual: a bogus policy mode is not Auto", got)
	}
}

func TestAutoFromThePoliciesWhenTheIntentSetsNothing(t *testing.T) {
	fromPolicy := policy.Modes(opModes(modeLayer(true, "")), []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, auto))}, nil)
	if fromPolicy.Create != auto {
		t.Fatalf("Create = %q, want Auto from a policy", fromPolicy.Create)
	}
	fromDefaults := policy.Modes(opModes(modeLayer(true, auto)), []lastv1alpha1.IntentPolicy{ipol(modeLayer(true, ""))}, &lastv1alpha1.StageModes{})
	if fromDefaults.Create != auto {
		t.Fatalf("Create = %q, want Auto from the defaults", fromDefaults.Create)
	}
}

func TestModesIgnorePolicyOrderWhenAuto(t *testing.T) {
	op := opModes(modeLayer(true, ""))
	a, b, c := ipol(modeLayer(true, auto)), ipol(modeLayer(true, "")), ipol(modeLayer(true, auto))
	perms := [][]lastv1alpha1.IntentPolicy{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}}
	for _, p := range perms {
		if got := policy.Modes(op, p, nil).Create; got != auto {
			t.Fatalf("Create = %q for order %v, want Auto", got, p)
		}
	}
}

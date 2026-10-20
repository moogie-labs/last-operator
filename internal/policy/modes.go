package policy

import lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"

// ModeKey names a stage key of the Auto/Manual modes. Create covers the Create
// and Update stages, and Reconcile covers Remediate.
type ModeKey string

// The four mode keys.
const (
	KeyDiscovery ModeKey = "discovery"
	KeyCreate    ModeKey = "create"
	KeyReconcile ModeKey = "reconcile"
	KeyDelete    ModeKey = "delete"
)

// KeyFor maps a stage to its mode key; an unknown stage maps to "".
func KeyFor(stage lastv1alpha1.Stage) ModeKey {
	switch stage {
	case lastv1alpha1.StageDiscovery:
		return KeyDiscovery
	case lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate:
		return KeyCreate
	case lastv1alpha1.StageRemediate:
		return KeyReconcile
	case lastv1alpha1.StageDelete:
		return KeyDelete
	}
	return ""
}

// EffectiveModes is the resolved Auto/Manual mode of each stage key.
type EffectiveModes struct {
	Discovery, Create, Reconcile, Delete lastv1alpha1.Mode
}

// For returns the mode of a stage; an unknown stage is Manual.
func (m EffectiveModes) For(stage lastv1alpha1.Stage) lastv1alpha1.Mode {
	switch KeyFor(stage) {
	case KeyDiscovery:
		return m.Discovery
	case KeyCreate:
		return m.Create
	case KeyReconcile:
		return m.Reconcile
	case KeyDelete:
		return m.Delete
	}
	return lastv1alpha1.ModeManual
}

// Status returns the modes as the Intent's status.effectiveModes, every key set.
func (m EffectiveModes) Status() *lastv1alpha1.StageModes {
	return &lastv1alpha1.StageModes{Discovery: m.Discovery, Create: m.Create, Reconcile: m.Reconcile, Delete: m.Delete}
}

// Modes resolves each stage key's mode across OperatorConfig.spec.defaults,
// every IntentPolicy in the namespace and the Intent. The result does not depend
// on the order of the policies. For a key:
//   - Auto is allowed only when every layer sets allowAutoModes for it; a missing
//     defaults layer or allowAutoModes does not allow it.
//   - The policies' mode is Manual if any IntentPolicy sets Manual, else Auto if
//     the defaults or any IntentPolicy set Auto, else Manual.
//   - The mode is Auto only when Auto is allowed and the Intent sets Auto, or the
//     Intent sets nothing and the policies' mode is Auto. An Intent's Manual wins.
func Modes(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy, intent *lastv1alpha1.StageModes) EffectiveModes {
	var defaults lastv1alpha1.IntentPolicySpec
	if op != nil && op.Spec.Defaults != nil {
		defaults = *op.Spec.Defaults
	}
	specs := make([]lastv1alpha1.IntentPolicySpec, 0, len(policies))
	for _, p := range policies {
		specs = append(specs, p.Spec)
	}
	pick := func(key ModeKey) lastv1alpha1.Mode {
		allowed := flag(defaults.AllowAutoModes, key)
		policyAuto := modeOf(defaults.Modes, key) == lastv1alpha1.ModeAuto
		for _, s := range specs {
			allowed = allowed && flag(s.AllowAutoModes, key)
			switch modeOf(s.Modes, key) {
			case lastv1alpha1.ModeManual:
				return lastv1alpha1.ModeManual
			case lastv1alpha1.ModeAuto:
				policyAuto = true
			}
		}
		if !allowed {
			return lastv1alpha1.ModeManual
		}
		switch modeOf(intent, key) {
		case lastv1alpha1.ModeAuto:
			return lastv1alpha1.ModeAuto
		case lastv1alpha1.ModeManual:
			return lastv1alpha1.ModeManual
		}
		if policyAuto {
			return lastv1alpha1.ModeAuto
		}
		return lastv1alpha1.ModeManual
	}
	return EffectiveModes{
		Discovery: pick(KeyDiscovery),
		Create:    pick(KeyCreate),
		Reconcile: pick(KeyReconcile),
		Delete:    pick(KeyDelete),
	}
}

// flag reports whether f sets the key; nil allows nothing.
func flag(f *lastv1alpha1.StageFlags, key ModeKey) bool {
	if f == nil {
		return false
	}
	switch key {
	case KeyDiscovery:
		return f.Discovery
	case KeyCreate:
		return f.Create
	case KeyReconcile:
		return f.Reconcile
	case KeyDelete:
		return f.Delete
	}
	return false
}

// modeOf returns the key's mode in m, or "" when it is unset or not Auto or Manual.
func modeOf(m *lastv1alpha1.StageModes, key ModeKey) lastv1alpha1.Mode {
	if m == nil {
		return ""
	}
	var v lastv1alpha1.Mode
	switch key {
	case KeyDiscovery:
		v = m.Discovery
	case KeyCreate:
		v = m.Create
	case KeyReconcile:
		v = m.Reconcile
	case KeyDelete:
		v = m.Delete
	}
	if v == lastv1alpha1.ModeAuto || v == lastv1alpha1.ModeManual {
		return v
	}
	return ""
}

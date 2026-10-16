// Package policy resolves the layered sandbox policy the gateway enforces (spec §6).
package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// ErrDenied reports a command the effective policy does not allow.
var ErrDenied = errors.New("policy: denied")

// Effective is the resolved policy for one namespace. Each list layer narrows the
// layers above it; a command must satisfy every layer.
type Effective struct {
	AllowScripts   bool
	providerLayers [][]string
	egressLayers   [][]sandbox.EgressRule
}

// Resolve layers OperatorConfig.spec.defaults and every IntentPolicy in the
// namespace. The cluster layer always applies, and an unset list there allows
// nothing (the shipped default); an IntentPolicy only adds the lists it sets.
func Resolve(op *lastv1alpha1.OperatorConfig, policies []lastv1alpha1.IntentPolicy) (Effective, error) {
	e := Effective{AllowScripts: true}
	var defaults lastv1alpha1.IntentPolicySpec
	if op != nil && op.Spec.Defaults != nil {
		defaults = *op.Spec.Defaults
	}
	if err := e.addLayer(defaults, true); err != nil {
		return Effective{}, err
	}
	for _, p := range policies {
		if err := e.addLayer(p.Spec, false); err != nil {
			return Effective{}, fmt.Errorf("intentpolicy %s: %w", p.Name, err)
		}
	}
	return e, nil
}

func (e *Effective) addLayer(s lastv1alpha1.IntentPolicySpec, always bool) error {
	if s.Providers != nil || always {
		e.providerLayers = append(e.providerLayers, s.Providers)
	}
	if s.Egress != nil || always {
		var patterns []string
		if s.Egress != nil {
			patterns = s.Egress.Allowed
		}
		rules, err := parseAllowed(patterns)
		if err != nil {
			return err
		}
		e.egressLayers = append(e.egressLayers, rules)
	}
	if s.AllowScripts != nil && !*s.AllowScripts {
		e.AllowScripts = false
	}
	return nil
}

func parseAllowed(patterns []string) ([]sandbox.EgressRule, error) {
	rules := make([]sandbox.EgressRule, 0, len(patterns))
	for _, p := range patterns {
		r, err := sandbox.ParseEgress(p)
		if errors.Is(err, sandbox.ErrHostEgressUnsupported) {
			continue // host patterns need Cilium (spec §11.1); in M2b they allow nothing
		}
		if err != nil {
			return nil, fmt.Errorf("policy: egress pattern %q: %w", p, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// AllowsProvider reports whether every layer lists the provider.
func (e Effective) AllowsProvider(name string) bool {
	for _, layer := range e.providerLayers {
		if !slices.Contains(layer, name) {
			return false
		}
	}
	return true
}

// AllowsEgress reports whether, in every layer, some allowed rule covers r. The
// comparison uses parsed prefixes, never strings, so spellings such as
// ::ffff:0:0/96 (which normalizes to 0.0.0.0/0) cannot slip through.
func (e Effective) AllowsEgress(r sandbox.EgressRule) bool {
	for _, layer := range e.egressLayers {
		if !slices.ContainsFunc(layer, func(a sandbox.EgressRule) bool { return covers(a, r) }) {
			return false
		}
	}
	return true
}

// Egress returns each layer's allowed destinations, as CIDR:port. A
// destination must be allowed by every layer (AllowsEgress).
func (e Effective) Egress() [][]string {
	out := make([][]string, 0, len(e.egressLayers))
	for _, layer := range e.egressLayers {
		rules := make([]string, 0, len(layer))
		for _, r := range layer {
			rules = append(rules, r.String())
		}
		out = append(out, rules)
	}
	return out
}

func covers(allowed, r sandbox.EgressRule) bool {
	pa, errA := netip.ParsePrefix(allowed.CIDR)
	pr, errR := netip.ParsePrefix(r.CIDR)
	return errA == nil && errR == nil && allowed.Port == r.Port &&
		pa.Bits() <= pr.Bits() && pa.Contains(pr.Addr())
}

// CheckCommand returns an ErrDenied error when c uses anything the policy forbids.
func (e Effective) CheckCommand(c lastv1alpha1.Command) error {
	if c.Image != nil {
		return fmt.Errorf("%w: image.install commands are not supported until image builds (M4)", ErrDenied)
	}
	if c.Run.Script != "" && !e.AllowScripts {
		return fmt.Errorf("%w: scripts are not allowed by policy", ErrDenied)
	}
	for _, p := range c.Providers {
		if !e.AllowsProvider(p) {
			return fmt.Errorf("%w: provider %q is not allowed", ErrDenied, p)
		}
	}
	for _, s := range c.Egress {
		r, err := sandbox.ParseEgress(s)
		if err != nil {
			return fmt.Errorf("%w: egress %q: %v", ErrDenied, s, err)
		}
		if !e.AllowsEgress(r) {
			return fmt.Errorf("%w: egress %s is not allowed", ErrDenied, r)
		}
	}
	return nil
}

package v1alpha1_test

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func ruleSpec(argv ...string) lastv1alpha1.CommandRuleSpec {
	return lastv1alpha1.CommandRuleSpec{
		Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
		Providers: []string{"aws-prod"},
		Match:     lastv1alpha1.ArgvPattern{Argv: argv},
	}
}

func TestCommandRuleAcceptsWildcards(t *testing.T) {
	obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "ec2", "describe-*", "re:^--region=[a-z0-9-]+$", "...")}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestCommandRuleRequiresLiteralBinary(t *testing.T) {
	for _, first := range []string{"*", "aw?", "[a]ws", "re:^aws$", "..."} {
		obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec(first, "ec2")}
		expectInvalid(t, k8sClient.Create(ctx, obj), "argv[0] must be a literal")
	}
}

func TestCommandRuleEllipsisOnlyLast(t *testing.T) {
	obj := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "...", "describe")}
	expectInvalid(t, k8sClient.Create(ctx, obj), "only allowed in the last position")

	twice := &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: ruleSpec("aws", "...", "...")}
	expectInvalid(t, k8sClient.Create(ctx, twice), "only allowed in the last position")
}

func TestCommandRuleRequiresStagesAndProviders(t *testing.T) {
	noStages := ruleSpec("aws")
	noStages.Stages = nil
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: noStages}), "spec.stages")

	noProviders := ruleSpec("aws")
	noProviders.Providers = nil
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: noProviders}), "spec.providers")
}

func TestCommandRuleValidatesInstallPatterns(t *testing.T) {
	spec := ruleSpec("aws", "...")
	spec.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{"*", "install"}}}
	expectInvalid(t, k8sClient.Create(ctx, &lastv1alpha1.CommandRule{ObjectMeta: meta(), Spec: spec}), "argv[0] must be a literal")
}

func TestClusterCommandRuleSharesValidation(t *testing.T) {
	obj := &lastv1alpha1.ClusterCommandRule{ObjectMeta: clusterMeta(), Spec: ruleSpec("...")}
	expectInvalid(t, k8sClient.Create(ctx, obj), "argv[0] must be a literal")

	ok := &lastv1alpha1.ClusterCommandRule{ObjectMeta: clusterMeta(), Spec: ruleSpec("kubectl", "get", "...")}
	if err := k8sClient.Create(ctx, ok); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ok) })
}

func suggestion() *lastv1alpha1.CommandRuleSuggestion {
	return &lastv1alpha1.CommandRuleSuggestion{ObjectMeta: meta(), Spec: lastv1alpha1.CommandRuleSuggestionSpec{
		TargetScope: lastv1alpha1.RuleScopeNamespace,
		Trigger:     lastv1alpha1.TriggerPlanTime,
		Rationale:   "lets drift remediation run unattended",
		Proposed:    ruleSpec("kubectl", "get", "..."),
	}}
}

func TestSuggestionDefaultsToPending(t *testing.T) {
	obj := suggestion()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &lastv1alpha1.CommandRuleSuggestion{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Fatalf("decision = %q, want Pending", got.Spec.Decision)
	}
}

func TestSuggestionDecisionRequiresDecidedBy(t *testing.T) {
	obj := suggestion()
	obj.Spec.Decision = lastv1alpha1.SuggestionDenied
	expectInvalid(t, k8sClient.Create(ctx, obj), "decidedBy is required once decided")
}

func TestSuggestionDecisionCannotChangeOnceDecided(t *testing.T) {
	obj := suggestion()
	obj.Spec.Decision = lastv1alpha1.SuggestionDenied
	obj.Spec.DecidedBy = "alice@example.com"
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	obj.Spec.Decision = lastv1alpha1.SuggestionAccepted
	expectInvalid(t, k8sClient.Update(ctx, obj), "decision cannot change once decided")
}

//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

func TestContains(t *testing.T) {
	tests := []struct {
		name          string
		broad, narrow []string
		want          rules.Verdict
	}{
		{"equal literals", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "pods"}, rules.Yes},
		{"different literals", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "nodes"}, rules.No},
		{"different argv[0]", []string{"kubectl", "get"}, []string{"oc", "get"}, rules.No},
		{"literal in star", []string{"kubectl", "get", "*"}, []string{"kubectl", "get", "pods"}, rules.Yes},
		{"literal in value slot", []string{"kubectl", "get", "[!-]*"}, []string{"kubectl", "get", "pods"}, rules.Yes},
		{"flag literal not in value slot", []string{"kubectl", "get", "[!-]*"}, []string{"kubectl", "get", "-x"}, rules.No},
		{"escaped literal decided by Match", []string{"tool", `a\*b`}, []string{"tool", `a\*b`}, rules.Yes},
		{"escaped literal is not the glob", []string{"tool", "a*b"}, []string{"tool", `a\*`}, rules.No},
		{"literal in glob", []string{"aws", "ec2", "describe-*"}, []string{"aws", "ec2", "describe-instances"}, rules.Yes},
		{"literal not in glob", []string{"aws", "ec2", "describe-*"}, []string{"aws", "ec2", "run-instances"}, rules.No},
		{"literal in regex", []string{"tool", "re:v[0-9]+"}, []string{"tool", "v12"}, rules.Yes},
		{"glob in star", []string{"aws", "ec2", "*"}, []string{"aws", "ec2", "describe-*"}, rules.Yes},
		{"identical globs", []string{"aws", "ec2", "describe-*"}, []string{"aws", "ec2", "describe-*"}, rules.Yes},
		{"glob that cannot start with a dash in value slot", []string{"aws", "ec2", "[!-]*"}, []string{"aws", "ec2", "describe-*"}, rules.Yes},
		{"class without a dash in value slot", []string{"tool", "[!-]*"}, []string{"tool", "[a-z]*"}, rules.Yes},
		{"escaped first character in value slot", []string{"tool", "[!-]*"}, []string{"tool", `\*x*`}, rules.Yes},
		{"star in value slot is undecided", []string{"tool", "[!-]*"}, []string{"tool", "*"}, rules.Unknown},
		{"escaped dash not in value slot", []string{"tool", "[!-]*"}, []string{"tool", `\-*`}, rules.No},
		{"regex in value slot is undecided", []string{"tool", "[!-]*"}, []string{"tool", "re:a.*"}, rules.Unknown},
		{"regex in star", []string{"tool", "*"}, []string{"tool", "re:a.*"}, rules.Yes},
		{"star in regex is undecided", []string{"tool", "re:.*"}, []string{"tool", "*"}, rules.Unknown},
		{"value slot not in a literal", []string{"kubectl", "apply", "-f", "web.yaml"}, []string{"kubectl", "apply", "-f", "[!-]*"}, rules.No},
		{"question mark not in a literal", []string{"tool", "ab"}, []string{"tool", "a?"}, rules.No},
		{"star not in a one-string class", []string{"tool", "[w]eb"}, []string{"tool", "w*"}, rules.No},
		{"escaped star and class are no wildcard", []string{"tool", "ab"}, []string{"tool", `[ab]\*`}, rules.No},
		{"class in a literal is undecided", []string{"tool", "a"}, []string{"tool", "[ab]"}, rules.Unknown},
		{"glob in another glob is undecided", []string{"aws", "ec2", "describe-*"}, []string{"aws", "ec2", "describe-i*"}, rules.Unknown},
		{"value slot not in a prefix glob", []string{"kubectl", "get", "pods", "web-*"}, []string{"kubectl", "get", "pods", "[!-]*"}, rules.No},
		{"prefix glob in value slot", []string{"kubectl", "get", "pods", "[!-]*"}, []string{"kubectl", "get", "pods", "web-*"}, rules.Yes},
		{"negated class witness not in a literal", []string{"tool", "a"}, []string{"tool", "[!a]"}, rules.No},
		{"class witness the broad glob matches is undecided", []string{"tool", "[a-c]x"}, []string{"tool", "[ab]?"}, rules.Unknown},
		{"narrow rest not in a position", []string{"aws", "ec2", "*"}, []string{"aws", "ec2", "..."}, rules.No},
		{"narrow rest in broad rest", []string{"aws", "ec2", "..."}, []string{"aws", "ec2", "..."}, rules.Yes},
		{"broad rest covers a longer tail", []string{"aws", "..."}, []string{"aws", "ec2", "describe-instances", "--region", "x"}, rules.Yes},
		{"broad rest covers no arguments", []string{"aws", "..."}, []string{"aws"}, rules.Yes},
		{"broad rest needs its fixed positions", []string{"aws", "ec2", "..."}, []string{"aws"}, rules.No},
		{"broad rest with a differing fixed position", []string{"aws", "ec2", "..."}, []string{"aws", "s3", "ls"}, rules.No},
		{"different lengths", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "pods", "web"}, rules.No},
		{"different lengths shorter", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get"}, rules.No},
		{"spec example", []string{"aws", "ec2", "..."}, []string{"aws", "ec2", "describe-*", "..."}, rules.Yes},
		{"spec example reversed", []string{"aws", "ec2", "describe-*", "..."}, []string{"aws", "ec2", "..."}, rules.No},
		{"spec example broader", []string{"aws", "..."}, []string{"aws", "ec2", "..."}, rules.Yes},
		{"No wins over Unknown", []string{"tool", "re:.*", "a"}, []string{"tool", "*", "b"}, rules.No},
		{"broad matches nothing is undecided", []string{"k*", "get"}, []string{"kubectl", "get"}, rules.Unknown},
		{"narrow matches nothing is undecided", []string{"kubectl", "..."}, []string{"kubectl", "...", "get"}, rules.Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules.Contains(tt.broad, tt.narrow); got != tt.want {
				t.Errorf("Contains(%q, %q) = %v, want %v", tt.broad, tt.narrow, got, tt.want)
			}
		})
	}
}

func TestSubsumes(t *testing.T) {
	create, update, del := lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDelete
	spec := func(argv ...string) lastv1alpha1.CommandRuleSpec {
		return lastv1alpha1.CommandRuleSpec{
			Match:     lastv1alpha1.ArgvPattern{Argv: argv},
			Stages:    []lastv1alpha1.Stage{create},
			Providers: []string{"aws"},
		}
	}
	with := func(s lastv1alpha1.CommandRuleSpec, f func(*lastv1alpha1.CommandRuleSpec)) lastv1alpha1.CommandRuleSpec {
		f(&s)
		return s
	}
	broad := spec("aws", "ec2", "...")
	narrow := spec("aws", "ec2", "describe-*", "...")
	install := func(argvs ...[]string) []lastv1alpha1.ArgvPattern {
		out := make([]lastv1alpha1.ArgvPattern, len(argvs))
		for i, a := range argvs {
			out[i] = lastv1alpha1.ArgvPattern{Argv: a}
		}
		return out
	}

	tests := []struct {
		name string
		n, d lastv1alpha1.CommandRuleSpec
		want rules.Verdict
	}{
		{"equal", broad, broad, rules.Yes},
		{"broader match", broad, narrow, rules.Yes},
		{"narrower match", narrow, broad, rules.No},
		{"match only", spec("aws", "s3", "..."), broad, rules.No},
		{"stages only", broad, with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Stages = []lastv1alpha1.Stage{create, del} }), rules.No},
		{"Create in d with Update in n", with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Stages = []lastv1alpha1.Stage{update} }), broad, rules.Yes},
		{"extra stages in n", with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Stages = []lastv1alpha1.Stage{create, del} }), broad, rules.Yes},
		{"providers only", broad, with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Providers = []string{"aws", "gcp"} }), rules.No},
		{"extra providers in n", with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Providers = []string{"aws", "gcp"} }), broad, rules.Yes},
		{"egress only", broad, with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.0.0.0/8:443"} }), rules.No},
		{"egress covered",
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.0.0.0/8:443"} }),
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.1.0.0/16:443"} }), rules.Yes},
		{"egress on another port",
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.0.0.0/8:443"} }),
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.1.0.0/16:80"} }), rules.No},
		{"egress split across entries is undecided",
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.0.0.0/9:443", "10.128.0.0/9:443"} }),
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"10.0.0.0/8:443"} }), rules.Unknown},
		{"host egress in d allows nothing",
			broad, with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Egress = []string{"example.com:443"} }), rules.Yes},
		{"install only", broad, with(broad, func(s *lastv1alpha1.CommandRuleSpec) {
			s.Install = install([]string{"pip", "install", "boto3"})
		}), rules.No},
		{"install contained in one of n's",
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) {
				s.Install = install([]string{"apt-get", "install", "jq"}, []string{"pip", "install", "[!-]*"})
			}),
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Install = install([]string{"pip", "install", "boto3"}) }), rules.Yes},
		{"install undecided",
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Install = install([]string{"pip", "install", "re:.*"}) }),
			with(broad, func(s *lastv1alpha1.CommandRuleSpec) { s.Install = install([]string{"pip", "install", "*"}) }), rules.Unknown},
		{"Unknown match", spec("tool", "re:.*"), spec("tool", "*"), rules.Unknown},
		{"a denied value slot does not suppress a prefix glob",
			spec("kubectl", "get", "pods", "web-*"), spec("kubectl", "get", "pods", "[!-]*"), rules.No},
		{"a denied prefix glob suppresses a value slot",
			spec("kubectl", "get", "pods", "[!-]*"), spec("kubectl", "get", "pods", "web-*"), rules.Yes},
		{"No dimension wins over Unknown match",
			spec("tool", "re:.*"), with(spec("tool", "*"), func(s *lastv1alpha1.CommandRuleSpec) { s.Providers = []string{"gcp"} }), rules.No},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules.Subsumes(tt.n, tt.d); got != tt.want {
				t.Errorf("Subsumes = %v, want %v", got, tt.want)
			}
		})
	}
}

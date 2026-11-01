//nolint:goconst // table-driven test data repeats argv words
package suggest_test

import (
	"regexp"
	"testing"

	"github.com/moogie-labs/last-operator/internal/suggest"
)

func TestNameIsStableAndScoped(t *testing.T) {
	p := proposal([]string{"aws", "s3", "ls"}, []string{"aws", "s3", "ls"})
	a, err := suggest.Name(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := suggest.Name(p)
	if a != b {
		t.Errorf("same input gave %q and %q", a, b)
	}
	if !regexp.MustCompile(`^sugg-[0-9a-f]{10}$`).MatchString(a) {
		t.Errorf("name %q has the wrong format", a)
	}
	q := p
	q.Namespace = "team-y"
	if c, _ := suggest.Name(q); c == a {
		t.Error("a different namespace gave the same name")
	}
	q = p
	q.Spec.Match.Argv = []string{"aws", "s3", "cp"}
	if c, _ := suggest.Name(q); c == a {
		t.Error("a different match gave the same name")
	}
	// rationale, trigger and examples are not part of the identity
	q = p
	q.Rationale = "other"
	q.Examples = nil
	if c, _ := suggest.Name(q); c != a {
		t.Error("rationale or examples changed the name")
	}
}

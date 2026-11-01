//nolint:goconst // table-driven test data repeats argv words
package suggest_test

import (
	"encoding/json"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

func TestParse(t *testing.T) {
	got, err := suggest.Parse(`{"suggestions":[{"entry":"c1","argv":["aws","s3","ls"],"rationale":"r","extra":1}],"other":2}`)
	if err != nil || len(got) != 1 || got[0].Entry != "c1" || got[0].Rationale != "r" || strings.Join(got[0].Argv, " ") != "aws s3 ls" {
		t.Errorf("Parse = %+v, %v", got, err)
	}
	if _, err := suggest.Parse("not json"); err == nil {
		t.Error("non-JSON parsed")
	}
	if got, err := suggest.Parse(`{"suggestions":[]}`); err != nil || len(got) != 0 {
		t.Errorf("empty list: %+v, %v", got, err)
	}
}

func TestSchemaHasNoRegexKeywords(t *testing.T) {
	s := suggest.Schema()
	if len(s) >= 1024 {
		t.Errorf("schema is %d bytes", len(s))
	}
	var v any
	if err := json.Unmarshal(s, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "pattern" || k == "format" {
					t.Errorf("schema has the keyword %q", k)
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
}

func TestRequestAndPrompt(t *testing.T) {
	r := suggest.Request([]suggest.Entry{{ID: "c1", Stage: lastv1alpha1.StageDiscovery, Argv: []string{"aws", "s3", "ls"}}})
	if !strings.Contains(r, "c1") || !strings.Contains(r, "Discovery") || !strings.Contains(r, `"s3"`) {
		t.Errorf("Request = %q", r)
	}
	for _, w := range []string{"argv[0]", "--", "delete", "..."} {
		if !strings.Contains(suggest.SystemPrompt, w) {
			t.Errorf("SystemPrompt does not mention %q", w)
		}
	}
}

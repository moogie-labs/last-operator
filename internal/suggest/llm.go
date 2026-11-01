package suggest

import (
	"encoding/json"
	"fmt"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// SystemPrompt is the system prompt for a suggest call (S10).
const SystemPrompt = `You propose allow-rule match patterns for shell commands that keep needing human approval. Each entry has an id, a stage and an argv. For each entry you can usefully generalize, reply with one pattern that matches that entry's argv, and a short rationale. Skip entries that should stay exact.

Pattern syntax: one string per argv position, matched position by position.
- A plain string matches itself exactly.
- Globs match within one argument: * (any characters), ? (one character), [abc] (a set), [!-]* (anything that does not start with a dash).
- "re:<RE2>" matches one whole argument against a regular expression.
- "..." as the last position matches any remaining arguments.
- Escape a literal glob character with a backslash.

Rules you must follow:
- argv[0] must stay a literal, the same as the entry's argv[0].
- Code must stay literal: interpreter code (bash -c, python -c, node -e), a command runner's command (env, xargs, sudo, docker or kubectl exec or run) and everything after "--".
- Never put a wildcard (a glob, a regex or "...") after a destructive verb such as delete, rm, destroy, drop, purge or terminate-*.
- Prefer [!-]* for values, so a slot cannot match a flag.
- Give one pattern per entry, and it must match that entry's argv.
- Keep rationales under 500 characters, in plain text.

Reply only with JSON of the form {"suggestions":[{"entry":"<id>","argv":["..."],"rationale":"..."}]}. An empty list is fine.`

const schema = `{"type":"object","additionalProperties":false,"required":["suggestions"],"properties":{"suggestions":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["entry","argv","rationale"],"properties":{"entry":{"type":"string"},"argv":{"type":"array","items":{"type":"string"}},"rationale":{"type":"string"}}}}}}`

// Schema is the JSON schema of a suggest reply: {suggestions: [{entry, argv,
// rationale}]}, with no regex keywords.
func Schema() []byte { return []byte(schema) }

// Entry is one command the operator asks the model to generalize.
type Entry struct {
	ID    string
	Stage lastv1alpha1.Stage
	Argv  []string
}

// Answer is one suggestion from the model: a match pattern for an entry.
type Answer struct {
	Entry     string
	Argv      []string
	Rationale string
}

// Request renders the user message for a suggest call.
func Request(entries []Entry) string {
	type e struct {
		ID    string   `json:"id"`
		Stage string   `json:"stage"`
		Argv  []string `json:"argv"`
	}
	out := make([]e, len(entries))
	for i, x := range entries {
		out[i] = e{x.ID, string(x.Stage), x.Argv}
	}
	b, _ := json.Marshal(out) //nolint:errchkjson // strings and string slices always marshal
	return "Entries needing approval:\n" + string(b)
}

// Parse decodes a suggest reply. Unknown fields are ignored.
func Parse(content string) ([]Answer, error) {
	var r struct {
		Suggestions []struct {
			Entry     string   `json:"entry"`
			Argv      []string `json:"argv"`
			Rationale string   `json:"rationale"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		return nil, fmt.Errorf("parsing the suggest reply: %w", err)
	}
	out := make([]Answer, len(r.Suggestions))
	for i, s := range r.Suggestions {
		out[i] = Answer{Entry: s.Entry, Argv: s.Argv, Rationale: s.Rationale}
	}
	return out, nil
}

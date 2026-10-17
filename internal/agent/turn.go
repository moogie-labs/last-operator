package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// Kind is what a turn does (M3 spec §4.1).
type Kind string

// Turn kinds.
const (
	KindPlan      Kind = "plan"
	KindQuestions Kind = "questions"
	KindCommand   Kind = "command"
)

// keywordType is the JSON Schema keyword naming a node's type.
const keywordType = "type"

const (
	// MaxQuestions bounds one questions turn.
	MaxQuestions = 5
	// NonceBytes is the size of a discovery nonce before hex encoding: 128 bits.
	NonceBytes = 16
	// maxQuestionsBytes bounds a questions turn's JSON, so status.questions stays small.
	maxQuestionsBytes = 16 << 10
	// maxReasonBytes bounds a discovery command's reason.
	maxReasonBytes = 4 << 10
)

// Turn is one valid model answer.
type Turn struct {
	Kind Kind
	// Plan is set for KindPlan, hash included.
	Plan *lastv1alpha1.Plan
	// Questions is set for KindQuestions.
	Questions []lastv1alpha1.Question
	// Command is set for KindCommand, with id plan.DiscoveryStepID; Reason says
	// why the model wants it run.
	Command *lastv1alpha1.Command
	Reason  string
}

// The wire format is the JSON the model writes. Structured outputs (OpenAI
// strict mode, Anthropic's schema subset) need every object closed and every
// property required, so maps become arrays of pairs, free-form objects become
// JSON strings, and a field a turn does not use is empty: "", [], or null for
// command. Decoding is lenient about missing fields and null, for models that
// do not enforce the schema.
type wireTurn struct {
	Kind      Kind           `json:"kind"`
	Summary   string         `json:"summary"`
	Steps     []wireCommand  `json:"steps"`
	Resources []wireResource `json:"resources"`
	Questions []wireQuestion `json:"questions"`
	Reason    string         `json:"reason"`
	Command   *wireCommand   `json:"command"`
}

type wireCommand struct {
	ID        string     `json:"id"`
	Reason    string     `json:"reason"`
	Argv      []string   `json:"argv"`
	Script    string     `json:"script"`
	Files     []wireFile `json:"files"`
	Providers []string   `json:"providers"`
	Egress    []string   `json:"egress"`
	Timeout   string     `json:"timeout"`
}

type wireFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type wireResource struct {
	Description   string        `json:"description"`
	Identity      string        `json:"identity"`
	Desired       string        `json:"desired"`
	Probes        []wireProbe   `json:"probes"`
	OutputsSecret string        `json:"outputsSecret"`
	Outputs       []wireOutput  `json:"outputs"`
	Teardown      []wireCommand `json:"teardown"`
}

type wireProbe struct {
	ID      string      `json:"id"`
	Command wireCommand `json:"command"`
	Assert  string      `json:"assert"`
}

type wireOutput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type wireQuestion struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

// ParseTurn reads a model answer: one turn object, optionally inside a ```json
// fence. It checks everything the gateway and the CRD schema would refuse, so a
// bad answer becomes a correction turn rather than a rejected status write.
func ParseTurn(content string) (Turn, error) {
	var w wireTurn
	if err := decodeOne(content, &w, "turn"); err != nil {
		return Turn{}, err
	}
	switch w.Kind {
	case KindPlan:
		return parsePlan(w)
	case KindQuestions:
		return parseQuestions(w)
	case KindCommand:
		return parseCommand(w)
	}
	return Turn{}, fmt.Errorf("kind %q is not one of plan, questions and command", w.Kind)
}

func parsePlan(w wireTurn) (Turn, error) {
	if len(w.Questions) > 0 || w.Reason != "" || w.Command != nil {
		return Turn{}, errors.New(`a plan turn leaves "questions", "reason" and "command" empty`)
	}
	var steps []lastv1alpha1.Command
	for _, s := range w.Steps {
		c, err := s.command("step " + s.ID)
		if err != nil {
			return Turn{}, err
		}
		steps = append(steps, c)
	}
	var resources []lastv1alpha1.ManagedResourceSpec
	for i, r := range w.Resources {
		spec, err := r.spec(i)
		if err != nil {
			return Turn{}, err
		}
		resources = append(resources, spec)
	}
	p, err := checkPlan(w.Summary, steps, resources)
	if err != nil {
		return Turn{}, err
	}
	return Turn{Kind: KindPlan, Plan: p}, nil
}

func parseQuestions(w wireTurn) (Turn, error) {
	if w.Summary != "" || len(w.Steps) > 0 || len(w.Resources) > 0 || w.Reason != "" || w.Command != nil {
		return Turn{}, errors.New(`a questions turn leaves everything but "questions" empty`)
	}
	if len(w.Questions) == 0 || len(w.Questions) > MaxQuestions {
		return Turn{}, fmt.Errorf("a questions turn asks 1 to %d questions, not %d", MaxQuestions, len(w.Questions))
	}
	seen := map[string]bool{}
	qs := make([]lastv1alpha1.Question, 0, len(w.Questions))
	for i, q := range w.Questions {
		id := strings.TrimSpace(q.ID) // spec.answers names it; "q1" and "q1 " would look alike
		switch {
		case id == "":
			return Turn{}, fmt.Errorf("question %d has no id", i)
		case seen[id]:
			return Turn{}, fmt.Errorf("duplicate question id %q", id)
		case strings.TrimSpace(q.Text) == "":
			return Turn{}, fmt.Errorf("question %q has no text", id)
		case slices.ContainsFunc(q.Options, func(o string) bool { return strings.TrimSpace(o) == "" }):
			return Turn{}, fmt.Errorf("question %q has an empty option", id)
		}
		seen[id] = true
		qs = append(qs, lastv1alpha1.Question{ID: id, Text: q.Text, Options: nilIfEmpty(q.Options)})
	}
	if b, err := json.Marshal(qs); err != nil || len(b) > maxQuestionsBytes {
		return Turn{}, fmt.Errorf("the questions are %d bytes; the limit is %d", len(b), maxQuestionsBytes)
	}
	return Turn{Kind: KindQuestions, Questions: qs}, nil
}

func parseCommand(w wireTurn) (Turn, error) {
	const where = "the discovery command"
	switch {
	case w.Summary != "" || len(w.Steps) > 0 || len(w.Resources) > 0 || len(w.Questions) > 0:
		return Turn{}, errors.New(`a command turn leaves everything but "reason" and "command" empty`)
	case w.Command == nil:
		return Turn{}, errors.New("a command turn needs a command")
	case strings.TrimSpace(w.Reason) == "":
		return Turn{}, errors.New("a command turn needs a reason: the approver reads it")
	case len(w.Reason) > maxReasonBytes:
		return Turn{}, fmt.Errorf("the reason is %d bytes; the limit is %d", len(w.Reason), maxReasonBytes)
	}
	c, err := w.Command.command(where)
	if err != nil {
		return Turn{}, err
	}
	c.ID = plan.DiscoveryStepID
	if err := checkCommand(where, c); err != nil {
		return Turn{}, err
	}
	for _, a := range c.Run.Argv {
		if strings.Contains(a, "${steps.") {
			return Turn{}, fmt.Errorf("%s uses %q; a discovery command runs alone and cannot use ${steps…} references", where, a)
		}
	}
	// Every nonce has the same length, so a placeholder gives the exact size.
	_, size, err := plan.DiscoveryHash(c, strings.Repeat("0", 2*NonceBytes))
	if err != nil {
		return Turn{}, err
	}
	if size > plan.MaxPendingCommandBytes {
		return Turn{}, fmt.Errorf("%s is %d bytes; the limit is %d", where, size, plan.MaxPendingCommandBytes)
	}
	return Turn{Kind: KindCommand, Command: &c, Reason: w.Reason}, nil
}

// command converts a wire command. It leaves the run.argv and run.script rule
// to callers, for the commands the CRD schema checks (checkCommand).
func (c wireCommand) command(where string) (lastv1alpha1.Command, error) {
	cmd := lastv1alpha1.Command{ID: c.ID, Reason: c.Reason,
		Run:       lastv1alpha1.CommandRun{Argv: nilIfEmpty(c.Argv), Script: c.Script},
		Providers: nilIfEmpty(c.Providers), Egress: nilIfEmpty(c.Egress)}
	for _, f := range c.Files {
		if _, dup := cmd.Files[f.Path]; dup {
			return cmd, fmt.Errorf("%s: file %q is listed twice", where, f.Path)
		}
		if cmd.Files == nil {
			cmd.Files = map[string]string{}
		}
		cmd.Files[f.Path] = f.Content
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return cmd, fmt.Errorf("%s: timeout %q is not a duration such as \"5m\"", where, c.Timeout)
		}
		cmd.Timeout = &metav1.Duration{Duration: d}
	}
	return cmd, nil
}

func (r wireResource) spec(i int) (lastv1alpha1.ManagedResourceSpec, error) {
	spec := lastv1alpha1.ManagedResourceSpec{Description: r.Description}
	var err error
	if spec.Identity, err = rawObject(r.Identity); err != nil {
		return spec, fmt.Errorf("resource %d identity: %w", i, err)
	}
	if spec.Desired, err = rawObject(r.Desired); err != nil {
		return spec, fmt.Errorf("resource %d desired: %w", i, err)
	}
	for _, p := range r.Probes {
		c, err := p.Command.command("probe " + p.ID)
		if err != nil {
			return spec, err
		}
		spec.Probes = append(spec.Probes, lastv1alpha1.Probe{ID: p.ID, Command: c, Assert: p.Assert})
	}
	if r.OutputsSecret != "" {
		spec.OutputsSecretRef = &lastv1alpha1.LocalRef{Name: r.OutputsSecret}
	}
	for _, o := range r.Outputs {
		if _, dup := spec.Outputs[o.Key]; dup {
			return spec, fmt.Errorf("resource %d output %q is listed twice", i, o.Key)
		}
		if spec.Outputs == nil {
			spec.Outputs = map[string]string{}
		}
		spec.Outputs[o.Key] = o.Value
	}
	for _, t := range r.Teardown {
		c, err := t.command("teardown command " + t.ID)
		if err != nil {
			return spec, err
		}
		spec.Teardown = append(spec.Teardown, c)
	}
	return spec, nil
}

// rawObject decodes a free-form field: "" or a JSON object written as a string.
func rawObject(s string) (*runtime.RawExtension, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, errors.New("must be a JSON object written as a string, or empty")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return nil, err
	}
	return &runtime.RawExtension{Raw: buf.Bytes()}, nil
}

// Encode writes t in the wire format. Tests and scripted conversations use it;
// ParseTurn(Encode(t)) gives t back, with a plan's hash recomputed.
func Encode(t Turn) (string, error) {
	w := wireTurn{Kind: t.Kind, Steps: []wireCommand{}, Resources: []wireResource{}, Questions: []wireQuestion{}}
	switch t.Kind {
	case KindPlan:
		if t.Plan == nil {
			return "", errors.New("agent: a plan turn needs a plan")
		}
		w.Summary = t.Plan.Summary
		for _, s := range t.Plan.Steps {
			c, err := commandToWire(s)
			if err != nil {
				return "", err
			}
			w.Steps = append(w.Steps, c)
		}
		for _, r := range t.Plan.Resources {
			wr, err := resourceToWire(r)
			if err != nil {
				return "", err
			}
			w.Resources = append(w.Resources, wr)
		}
	case KindQuestions:
		for _, q := range t.Questions {
			w.Questions = append(w.Questions, wireQuestion{ID: q.ID, Text: q.Text, Options: nonNil(q.Options)})
		}
	case KindCommand:
		if t.Command == nil {
			return "", errors.New("agent: a command turn needs a command")
		}
		c, err := commandToWire(*t.Command)
		if err != nil {
			return "", err
		}
		w.Reason, w.Command = t.Reason, &c
	default:
		return "", fmt.Errorf("agent: unknown turn kind %q", t.Kind)
	}
	b, err := json.Marshal(w)
	return string(b), err
}

func commandToWire(c lastv1alpha1.Command) (wireCommand, error) {
	if c.Image != nil {
		return wireCommand{}, errors.New("agent: image commands have no wire form")
	}
	w := wireCommand{ID: c.ID, Reason: c.Reason, Argv: nonNil(c.Run.Argv), Script: c.Run.Script, Files: []wireFile{},
		Providers: nonNil(c.Providers), Egress: nonNil(c.Egress)}
	for _, p := range slices.Sorted(maps.Keys(c.Files)) {
		w.Files = append(w.Files, wireFile{Path: p, Content: c.Files[p]})
	}
	if c.Timeout != nil {
		w.Timeout = c.Timeout.Duration.String()
	}
	return w, nil
}

func resourceToWire(r lastv1alpha1.ManagedResourceSpec) (wireResource, error) {
	if r.IntentRef != nil {
		return wireResource{}, errors.New("agent: intentRef has no wire form")
	}
	w := wireResource{Description: r.Description, Probes: []wireProbe{}, Outputs: []wireOutput{}, Teardown: []wireCommand{}}
	if r.Identity != nil {
		w.Identity = string(r.Identity.Raw)
	}
	if r.Desired != nil {
		w.Desired = string(r.Desired.Raw)
	}
	for _, p := range r.Probes {
		c, err := commandToWire(p.Command)
		if err != nil {
			return wireResource{}, err
		}
		w.Probes = append(w.Probes, wireProbe{ID: p.ID, Command: c, Assert: p.Assert})
	}
	if r.OutputsSecretRef != nil {
		w.OutputsSecret = r.OutputsSecretRef.Name
	}
	for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
		w.Outputs = append(w.Outputs, wireOutput{Key: k, Value: r.Outputs[k]})
	}
	for _, t := range r.Teardown {
		c, err := commandToWire(t)
		if err != nil {
			return wireResource{}, err
		}
		w.Teardown = append(w.Teardown, c)
	}
	return w, nil
}

// Schema returns the JSON Schema of a turn, sent as CallLLM's response_schema.
// It stays inside what OpenAI strict mode and Anthropic structured outputs
// accept: every object closed, every property required, no formats or bounds.
// ParseTurn checks the rest.
func Schema() []byte { return slices.Clone(schemaJSON) }

var schemaJSON = buildSchema()

// buildSchema defines each shared object (command, probe, resource) once in
// $defs and references it. Written out in full at every use, the command alone
// appeared four times, and Anthropic refused the schema: "The compiled grammar
// is too large" (hack/schemaprobe compares the shapes).
func buildSchema() []byte {
	const cmd = "command" // the $defs name of a command, and the field naming one
	str := map[string]any{keywordType: "string"}
	strs := array(str)
	ref := func(name string) map[string]any { return map[string]any{"$ref": "#/$defs/" + name} }
	command := object(map[string]any{
		"id": str, "reason": str, "argv": strs, "script": str,
		"files":     array(object(map[string]any{"path": str, "content": str})),
		"providers": strs, "egress": strs, "timeout": str,
	})
	probe := object(map[string]any{"id": str, cmd: ref(cmd), "assert": str})
	resource := object(map[string]any{
		"description": str, "identity": str, "desired": str, "probes": array(ref("probe")), "outputsSecret": str,
		"outputs":  array(object(map[string]any{"key": str, "value": str})),
		"teardown": array(ref(cmd)),
	})
	question := object(map[string]any{"id": str, "text": str, "options": strs})
	turn := object(map[string]any{
		"kind":      map[string]any{keywordType: "string", "enum": []Kind{KindPlan, KindQuestions, KindCommand}},
		"summary":   str,
		"steps":     array(ref(cmd)),
		"resources": array(ref("resource")),
		"questions": array(question),
		"reason":    str,
		cmd:         map[string]any{"anyOf": []any{ref(cmd), map[string]any{keywordType: "null"}}},
	})
	turn["$defs"] = map[string]any{cmd: command, "probe": probe, "resource": resource}
	b, err := json.Marshal(turn)
	if err != nil {
		panic(err)
	}
	return b
}

// object is a closed JSON Schema object whose every property is required.
func object(props map[string]any) map[string]any {
	return map[string]any{keywordType: "object", "properties": props,
		"required": slices.Sorted(maps.Keys(props)), "additionalProperties": false}
}

func array(items any) map[string]any { return map[string]any{keywordType: "array", "items": items} }

// NewNonce returns a fresh discovery nonce: NonceBytes random bytes, hex-encoded.
func NewNonce() (string, error) {
	b := make([]byte, NonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

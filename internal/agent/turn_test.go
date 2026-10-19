package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// fullPlan uses every field the wire format carries.
var fullPlan = lastv1alpha1.Plan{Summary: "nginx", Steps: []lastv1alpha1.Command{
	{ID: "apply", Reason: "create it", Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "apply", "-f", "/work/a.yaml"}},
		Files:     map[string]string{"/work/a.yaml": "kind: Deployment", "/work/b.yaml": "kind: Service"},
		Providers: []string{"k8s"}, Egress: []string{"10.0.0.1:443"}, Timeout: &metav1.Duration{Duration: 2 * time.Minute}},
	{ID: "svc", Run: lastv1alpha1.CommandRun{Script: "kubectl get svc web -o json"}},
}, Resources: []lastv1alpha1.ManagedResourceSpec{{
	Description: webName,
	Identity:    &runtime.RawExtension{Raw: []byte(`{"kind":"Deployment","name":"web"}`)},
	Desired:     &runtime.RawExtension{Raw: []byte(`{"replicas":2}`)},
	Probes: []lastv1alpha1.Probe{{ID: "ready",
		Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"true"}}}, Assert: "result.exitCode == 0"}},
	OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
	Outputs:          map[string]string{"ip": "${steps.svc.json.spec.clusterIP}"},
	Teardown:         []lastv1alpha1.Command{{ID: "t1", Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "delete", "deploy", webName}}}},
}}}

const aptGet = "apt-get"

const oneOfArgvScript = "exactly one of run.argv and run.script"

const (
	kubectlBin      = "kubectl"
	webName         = "web"
	unknownFieldMsg = "unknown field"
	limitMsg        = "the limit is"
)

func encode(t *testing.T, turn Turn) string {
	t.Helper()
	s, err := Encode(turn)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEncodeAndParseTurnRoundTripAPlan(t *testing.T) {
	got, err := ParseTurn(encode(t, Turn{Kind: KindPlan, Plan: &fullPlan}))
	if err != nil {
		t.Fatalf("ParseTurn = %v", err)
	}
	want, err := plan.Hash(plan.FromStatus(&fullPlan))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindPlan || got.Plan.Hash != want {
		t.Fatalf("turn = %+v, want a plan hashed %s", got, want)
	}
	got.Plan.Hash = ""
	if !reflect.DeepEqual(*got.Plan, fullPlan) {
		t.Errorf("plan = %+v\nwant %+v", *got.Plan, fullPlan)
	}
}

func TestParseTurnReadsQuestionsAndCommands(t *testing.T) {
	qs := []lastv1alpha1.Question{{ID: "q1", Text: "Which namespace?", Options: []string{"default", webName}}, {ID: "q2", Text: "Public?"}}
	got, err := ParseTurn(encode(t, Turn{Kind: KindQuestions, Questions: qs}))
	if err != nil || got.Kind != KindQuestions || !reflect.DeepEqual(got.Questions, qs) {
		t.Fatalf("questions turn = %+v, %v", got, err)
	}

	cmd := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "get", "ns"}}, Providers: []string{"k8s"}}
	got, err = ParseTurn(encode(t, Turn{Kind: KindCommand, Command: &cmd, Reason: "see what exists"}))
	if err != nil || got.Kind != KindCommand || got.Reason != "see what exists" {
		t.Fatalf("command turn = %+v, %v", got, err)
	}
	want := cmd
	want.ID = plan.DiscoveryStepID
	if !reflect.DeepEqual(*got.Command, want) {
		t.Errorf("command = %+v, want %+v", *got.Command, want)
	}
}

func TestParseTurnTrimsQuestionIDs(t *testing.T) {
	got, err := ParseTurn(`{"kind":"questions","questions":[{"id":" q1 ","text":"Which?"}]}`)
	if err != nil || len(got.Questions) != 1 || got.Questions[0].ID != "q1" {
		t.Fatalf("questions turn = %+v, %v; want id \"q1\"", got, err)
	}
}

func TestParseTurnAcceptsFencesAndWhitespace(t *testing.T) {
	turn := encode(t, Turn{Kind: KindQuestions, Questions: []lastv1alpha1.Question{{ID: "q1", Text: "Which?"}}})
	for name, content := range map[string]string{
		"fenced":        "```json\n" + turn + "\n```",
		"bare fence":    "```\n" + turn + "\n```",
		"surrounded ws": "\n  " + turn + "  \n",
	} {
		if _, err := ParseTurn(content); err != nil {
			t.Errorf("%s: ParseTurn = %v", name, err)
		}
	}
}

// A model without enforced structured output may leave fields out or send null.
func TestParseTurnAcceptsOmittedAndNullFields(t *testing.T) {
	for name, content := range map[string]string{
		"omitted":   `{"kind":"plan","steps":[{"id":"s1","argv":["true"]}]}`,
		"nulls":     `{"kind":"plan","summary":null,"steps":[{"id":"s1","argv":["true"],"files":null}],"resources":null,"questions":null,"command":null}`,
		"questions": `{"kind":"questions","questions":[{"id":"q1","text":"Which?"}]}`,
		"command":   `{"kind":"command","reason":"look","command":{"argv":["kubectl","get","ns"]}}`,
	} {
		if _, err := ParseTurn(content); err != nil {
			t.Errorf("%s: ParseTurn = %v", name, err)
		}
	}
}

func TestParseTurnRefusesMalformedTurns(t *testing.T) {
	step := `{"id":"s1","argv":["true"]}`
	planTurn := func(rest string) string { return `{"kind":"plan","steps":[` + step + `]` + rest + `}` }
	resource := func(fields string) string { return planTurn(`,"resources":[{` + fields + `}]`) }
	big := strings.Repeat("x", MaxPlanBytes)
	file := strings.Repeat("y", 70<<10)
	sixQuestions := make([]string, 0, 6)
	for i := range 6 {
		sixQuestions = append(sixQuestions, fmt.Sprintf(`{"id":"q%d","text":"?"}`, i))
	}
	cases := map[string]struct{ content, want string }{
		"prose":             {"Here is my turn: " + planTurn(""), "not a turn object"},
		"trailing text":     {planTurn("") + " hope this helps", "text after the turn object"},
		unknownFieldMsg:     {planTurn(`,"extra":1`), unknownFieldMsg},
		"unknown kind":      {`{"kind":"answer"}`, `kind "answer" is not one of`},
		"plan and command":  {planTurn(`,"command":{"argv":["true"]}`), "a plan turn leaves"},
		"no steps":          {`{"kind":"plan"}`, "at least one step"},
		"argv and script":   {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"script":"b"}]}`, oneOfArgvScript},
		"neither":           {`{"kind":"plan","steps":[{"id":"s1"}]}`, oneOfArgvScript},
		"duplicate file":    {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"files":[{"path":"/w/a"},{"path":"/w/a"}]}]}`, "listed twice"},
		"bad timeout":       {`{"kind":"plan","steps":[{"id":"s1","argv":["a"],"timeout":"soon"}]}`, "is not a duration"},
		"probe without id":  {resource(`"probes":[{"command":{"argv":["true"]},"assert":"true"}]`), "probe 0 of resource 0 has no id"},
		"probe no assert":   {resource(`"probes":[{"id":"p","command":{"argv":["true"]}}]`), `probe "p" has no assert`},
		"probe bad command": {resource(`"probes":[{"id":"p","command":{},"assert":"true"}]`), oneOfArgvScript},
		"teardown both":     {resource(`"teardown":[{"id":"t1","argv":["a"],"script":"b"}]`), oneOfArgvScript},
		"teardown ref":      {resource(`"teardown":[{"id":"t1","argv":["kubectl","delete","ns","${steps.s1.stdout}"]}]`), "write the value itself"},
		"identity array":    {resource(`"identity":"[1]"`), "JSON object"},
		"desired garbage":   {resource(`"desired":"{nope"`), "JSON object"},
		"literal output":    {resource(`"outputsSecret":"o","outputs":[{"key":"k","value":"hunter2"}]`), "not a single step reference"},
		"duplicate output":  {resource(`"outputsSecret":"o","outputs":[{"key":"k","value":"${steps.s1.stdout}"},{"key":"k","value":"${steps.s1.stdout}"}]`), "listed twice"},
		"too large":         {`{"kind":"plan","steps":[{"id":"s1","argv":["echo","` + big + `"]}]}`, limitMsg},
		"no questions":      {`{"kind":"questions","questions":[]}`, "1 to 5 questions"},
		"six questions":     {`{"kind":"questions","questions":[` + strings.Join(sixQuestions, ",") + `]}`, "1 to 5 questions"},
		"question no id":    {`{"kind":"questions","questions":[{"id":" ","text":"?"}]}`, "question 0 has no id"},
		"question repeated": {`{"kind":"questions","questions":[{"id":"q1","text":"?"},{"id":"q1","text":"?"}]}`, `duplicate question id "q1"`},
		"question respaced": {`{"kind":"questions","questions":[{"id":"q1","text":"?"},{"id":"q1 ","text":"?"}]}`, `duplicate question id "q1"`},
		"question no text":  {`{"kind":"questions","questions":[{"id":"q1","text":""}]}`, `question "q1" has no text`},
		"empty option":      {`{"kind":"questions","questions":[{"id":"q1","text":"?","options":["a",""]}]}`, `question "q1" has an empty option`},
		"questions + steps": {`{"kind":"questions","questions":[{"id":"q1","text":"?"}],"steps":[` + step + `]}`, "a questions turn leaves"},
		"no command":        {`{"kind":"command","reason":"look"}`, "needs a command"},
		"no reason":         {`{"kind":"command","command":{"argv":["true"]}}`, "needs a reason"},
		"step reference":    {`{"kind":"command","reason":"look","command":{"argv":["echo","${steps.s1.stdout}"]}}`, "${steps"},
		"command both":      {`{"kind":"command","reason":"look","command":{"argv":["a"],"script":"b"}}`, oneOfArgvScript},
		"command too large": {`{"kind":"command","reason":"look","command":{"argv":["cat","/w/f"],"files":[{"path":"/w/f","content":"` + file + `"}]}}`, limitMsg},
		"command + summary": {`{"kind":"command","summary":"s","reason":"look","command":{"argv":["true"]}}`, "a command turn leaves"},
	}
	for name, tc := range cases {
		_, err := ParseTurn(tc.content)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ParseTurn = %v, want an error containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats an output value", name)
		}
	}
}

// The schema must stay inside what OpenAI strict mode and Anthropic structured
// outputs accept, and must describe exactly the wire types ParseTurn decodes.
func TestSchemaIsStrictAndMatchesTheWireFormat(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	delete(root, "$defs")
	checkSchema(t, "turn", root, reflect.TypeFor[wireTurn](), defs)
	if kinds := root["properties"].(map[string]any)["kind"].(map[string]any)["enum"]; !reflect.DeepEqual(kinds,
		[]any{"plan", "questions", "command"}) {
		t.Errorf("kind enum = %v", kinds)
	}
	s := Schema()
	s[0] = 'x'
	if Schema()[0] == 'x' {
		t.Error("Schema returns shared storage")
	}
}

// Anthropic refuses a schema whose compiled grammar is too large, and the
// command object written out four times was too large (HTTP 400 "The compiled
// grammar is too large"; hack/schemaprobe). Each shared object is defined once
// in $defs and referenced.
func TestSchemaDefinesEachSharedObjectOnce(t *testing.T) {
	s := string(Schema())
	for _, field := range []string{"argv", "assert", "outputsSecret"} {
		if n := strings.Count(s, `"`+field+`":{`); n != 1 {
			t.Errorf("property %q is defined %d times in the schema, want once", field, n)
		}
	}
}

var allowedKeywords = []string{"type", "properties", "required", "additionalProperties", "items", "enum", "anyOf", "$ref"}

// checkSchema walks node against the wire type typ, resolving "#/$defs/<name>"
// references in defs.
func checkSchema(t *testing.T, path string, node map[string]any, typ reflect.Type, defs map[string]any) {
	t.Helper()
	for k := range node {
		if !slices.Contains(allowedKeywords, k) {
			t.Errorf("%s: keyword %q", path, k)
		}
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if ref, ok := node["$ref"].(string); ok {
		def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok || len(node) != 1 {
			t.Fatalf("%s: $ref %q does not resolve, or has siblings", path, ref)
		}
		checkSchema(t, path, def, typ, defs)
		return
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		if len(anyOf) != 2 || anyOf[1].(map[string]any)["type"] != "null" {
			t.Fatalf("%s: anyOf is not <type> or null", path)
		}
		checkSchema(t, path, anyOf[0].(map[string]any), typ, defs)
		return
	}
	switch node["type"] {
	case "object":
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties is not false", path)
		}
		props := node["properties"].(map[string]any)
		required := node["required"].([]any)
		if len(required) != len(props) || typ.NumField() != len(props) {
			t.Errorf("%s: %d properties, %d required, %d wire fields", path, len(props), len(required), typ.NumField())
		}
		for field := range typ.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			p, ok := props[name].(map[string]any)
			if !ok || !slices.Contains(required, any(name)) {
				t.Errorf("%s: property %q is missing or not required", path, name)
				continue
			}
			checkSchema(t, path+"."+name, p, field.Type, defs)
		}
	case "array":
		if typ.Kind() != reflect.Slice {
			t.Errorf("%s: array for %s", path, typ)
			return
		}
		checkSchema(t, path+"[]", node["items"].(map[string]any), typ.Elem(), defs)
	case "string":
		if typ.Kind() != reflect.String {
			t.Errorf("%s: string for %s", path, typ)
		}
	default:
		t.Errorf("%s: type %v", path, node["type"])
	}
}

func TestNewNonceIs128RandomBits(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) || a == b {
		t.Fatalf("nonces %q and %q", a, b)
	}
}

func TestSchemaCommandHasInstall(t *testing.T) {
	var root struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(Schema(), &root); err != nil {
		t.Fatal(err)
	}
	const want = `{"items":{"items":{"type":"string"},"type":"array"},"type":"array"}`
	if got := string(root.Defs["command"].Properties["install"]); got != want {
		t.Errorf("command.install = %s, want an array of arrays of strings", got)
	}
}

func TestParseTurnMapsInstallToAnImage(t *testing.T) {
	parse := func(install string) (Turn, error) {
		return ParseTurn(`{"kind":"plan","steps":[{"id":"s1","argv":["true"],"install":` + install + `}]}`)
	}
	got, err := parse(`[]`)
	if err != nil || got.Plan.Steps[0].Image != nil {
		t.Fatalf("empty install = %+v, %v; want no image", got, err)
	}
	got, err = parse(`[["apt-get","install","-y","awscli=2.9.19-1"]]`)
	want := &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{aptGet, "install", "-y", "awscli=2.9.19-1"}}}}
	if err != nil || !reflect.DeepEqual(got.Plan.Steps[0].Image, want) {
		t.Fatalf("install = %+v, %v; want %+v", got, err, want)
	}
	if _, err = parse(`[[]]`); err == nil || !strings.Contains(err.Error(), "step s1") {
		t.Errorf("empty install argv: err = %v, want one naming step s1", err)
	}
}

func TestCommandToWireRoundTripsAnImage(t *testing.T) {
	c := lastv1alpha1.Command{ID: "s1", Run: lastv1alpha1.CommandRun{Argv: []string{"aws", "s3", "ls"}},
		Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{aptGet, "update"}}, {Argv: []string{aptGet, "install", "-y", "awscli=2.9.19-1"}}}}}
	back, err := commandToWire(c).command("step s1")
	if err != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip = %+v, %v; want %+v", back, err, c)
	}
}

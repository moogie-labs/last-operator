package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	keySecret     = "key"
)

func llmConfig(providerType lastv1alpha1.LLMProviderType) (*lastv1alpha1.OperatorConfig, *lastv1alpha1.LLMProvider) {
	op := allowAll()
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: "fixture"}
	lp := &lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, Spec: lastv1alpha1.LLMProviderSpec{Type: providerType, Model: "m"}}
	if providerType == lastv1alpha1.LLMScripted {
		lp.Spec.Scripted = &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: "llm-script"}}
	} else {
		lp.Spec.SecretRef = &lastv1alpha1.LocalRef{Name: keySecret}
	}
	return op, lp
}

func newLLMEnv(t *testing.T, providerType lastv1alpha1.LLMProviderType) *testEnv {
	t.Helper()
	op, lp := llmConfig(providerType)
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if _, err := env.kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-script", Namespace: ns},
		Data:       map[string]string{"team-x.web": `["the plan"]`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *testEnv) callLLM(msgs ...*gatewayv1.Message) (*gatewayv1.CallLLMResponse, error) {
	return e.g.callLLM(e.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), Messages: msgs, MaxTokens: 1024})
}

func TestCallLLMAuditsRequestAndResponse(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	resp, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "run nginx"})
	if err != nil || resp.GetContent() != "the plan" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
	es := env.entries()
	req, res := es[len(es)-2], es[len(es)-1]
	if req.Type != audit.TypeLLMRequest || res.Type != audit.TypeLLMResponse || req.Actor != testOperator || req.Intent != envKey {
		t.Fatalf("entries = %s, %s", req.Type, res.Type)
	}
	var rp llmResponsePayload
	if err := json.Unmarshal(res.Payload, &rp); err != nil || rp.RequestSeq != req.Seq || rp.Content != "the plan" {
		t.Fatalf("llm.response = %s", res.Payload)
	}
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleAssistant}, &gatewayv1.Message{Role: roleUser}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("exhausted script = %v, want FailedPrecondition", err)
	}
	var last llmResponsePayload
	_ = json.Unmarshal(env.last().Payload, &last)
	if env.last().Type != audit.TypeLLMResponse || last.Error == "" {
		t.Fatal("a failed call must still record llm.response with the error")
	}
}

func TestCallLLMRefusals(t *testing.T) {
	user := &gatewayv1.Message{Role: roleUser, Content: "x"}
	if _, err := newLLMEnv(t, lastv1alpha1.LLMAnthropic).callLLM(user); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("anthropic without its key Secret = %v, want FailedPrecondition", err)
	}
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	if _, err := env.callLLM(&gatewayv1.Message{Role: "tool", Content: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad role = %v", err)
	}
	if _, err := env.callLLM(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no messages = %v", err)
	}
	env.g.writer.Store(nil)
	if _, err := env.callLLM(user); status.Code(err) != codes.Unavailable {
		t.Fatalf("no writer = %v", err)
	}
	noLLM := newTestEnv(t, intentWithPlan(t, twoSteps()), allowAll())
	if _, err := noLLM.callLLM(user); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no provider configured = %v", err)
	}
}

const answerSchema = `{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"],"additionalProperties":false}`

func TestCallLLMAuditsTheSchemaDigestAndReturnsTheRequestSeq(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	resp, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), MaxTokens: 1024, ResponseSchema: []byte(answerSchema),
		Messages: []*gatewayv1.Message{{Role: roleUser, Content: "run nginx"}}})
	if err != nil {
		t.Fatal(err)
	}
	es := env.entries()
	req := es[len(es)-2]
	if req.Type != audit.TypeLLMRequest || resp.GetRequestSeq() != req.Seq {
		t.Fatalf("request_seq = %d, want the llm.request seq %d", resp.GetRequestSeq(), req.Seq)
	}
	var p llmRequestPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(answerSchema))
	if p.SchemaSHA256 != hex.EncodeToString(sum[:]) || p.SchemaBytes != len(answerSchema) {
		t.Fatalf("llm.request schema digest = %q (%d bytes)", p.SchemaSHA256, p.SchemaBytes)
	}
	if strings.Contains(string(req.Payload), `"additionalProperties"`) {
		t.Fatal("the full schema was written to the audit log")
	}
}

func TestCallLLMRefusesABadSchemaBeforeAuditing(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	before := len(env.entries())
	for name, schema := range map[string][]byte{
		"not json": []byte("{"),
		"too big":  []byte(`"` + strings.Repeat("x", maxSchemaBytes) + `"`),
	} {
		_, err := env.g.callLLM(env.ctx, &gatewayv1.CallLLMRequest{Intent: ref(), ResponseSchema: schema,
			Messages: []*gatewayv1.Message{{Role: roleUser, Content: "x"}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if len(env.entries()) != before {
		t.Fatal("a refused request was audited")
	}
}

func TestLLMStatusMapsAdapterErrors(t *testing.T) {
	cases := map[error]codes.Code{
		fmt.Errorf("%w (HTTP 401)", llm.ErrAuth):          codes.FailedPrecondition,
		fmt.Errorf("%w (HTTP 404)", llm.ErrModel):         codes.FailedPrecondition,
		fmt.Errorf("%w (category cyber)", llm.ErrRefused): codes.FailedPrecondition,
		llm.ErrNoScript:        codes.FailedPrecondition,
		llm.ErrScriptExhausted: codes.FailedPrecondition,
		fmt.Errorf("%w: bad schema", llm.ErrBadRequest): codes.InvalidArgument,
		fmt.Errorf("%w (HTTP 529)", llm.ErrTransient):   codes.Unavailable,
		errors.New("connection reset"):                  codes.Unavailable,
	}
	for err, want := range cases {
		if got := status.Code(llmStatus(err)); got != want {
			t.Errorf("llmStatus(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestCallLLMCountsCallsAndTokens(t *testing.T) {
	env := newLLMEnv(t, lastv1alpha1.LLMScripted)
	if _, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(env.g.metrics.llmCalls.WithLabelValues("fixture", "m", "ok")); got != 1 {
		t.Fatalf("llm calls = %v, want 1", got)
	}
}

// anthropicEnv is an env whose LLMProvider is anthropic at a fake API that checks
// the key and answers with text.
func anthropicEnv(t *testing.T, storedKey string) *testEnv {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Api-Key") != "sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"real answer\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null,\"stop_details\":null},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	op, lp := llmConfig(lastv1alpha1.LLMAnthropic)
	lp.Spec.Endpoint = srv.URL
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if storedKey != "" {
		if _, err := env.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: keySecret, Namespace: ns},
			Data:       map[string][]byte{llmAPIKeyField: []byte(storedKey)},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func TestCallLLMAnthropicUsesTheKeyFromTheSecretTrimmed(t *testing.T) {
	env := anthropicEnv(t, "  sk-test\n")
	resp, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if err != nil || resp.GetContent() != "real answer" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
}

func TestCallLLMAnthropicKeyProblemsAreFailedPreconditionWithoutTheKey(t *testing.T) {
	missing := anthropicEnv(t, "")
	_, err := missing.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Secret "+keySecret+" not found") ||
		!strings.Contains(err.Error(), `"`+llmAPIKeyField+`"`) {
		t.Fatalf("missing Secret = %v; want FailedPrecondition naming Secret %q and its %q key", err, keySecret, llmAPIKeyField)
	}
	blank := anthropicEnv(t, " \n")
	_, err = blank.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Secret "+keySecret+` has no "`+llmAPIKeyField+`" key`) {
		t.Fatalf("blank apiKey = %v; want FailedPrecondition naming Secret %q and its %q key", err, keySecret, llmAPIKeyField)
	}
	wrong := anthropicEnv(t, "sk-wrong-value")
	_, err = wrong.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "sk-wrong-value") {
		t.Fatalf("rejected key = %v; want FailedPrecondition that never repeats the key", err)
	}
	for _, e := range wrong.entries() {
		if strings.Contains(string(e.Payload), "sk-wrong-value") {
			t.Fatalf("entry %s carries the API key", e.Type)
		}
	}
}
func TestCallLLMOpenAICompatibleUsesItsEndpointAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-local" || r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"local answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	op, lp := llmConfig(lastv1alpha1.LLMOpenAICompatible)
	lp.Spec.Endpoint = srv.URL + "/v1"
	env := newTestEnv(t, intentWithPlan(t, twoSteps()), op, lp)
	if _, err := env.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keySecret, Namespace: ns}, Data: map[string][]byte{llmAPIKeyField: []byte("sk-local")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	resp, err := env.callLLM(&gatewayv1.Message{Role: roleUser, Content: "x"})
	if err != nil || resp.GetContent() != "local answer" {
		t.Fatalf("CallLLM = %v, %v", resp, err)
	}
}

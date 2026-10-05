package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

const roleUser = "user"

func llmConfig(providerType lastv1alpha1.LLMProviderType) (*lastv1alpha1.OperatorConfig, *lastv1alpha1.LLMProvider) {
	op := allowAll()
	op.Spec.LLMProviderRef = &lastv1alpha1.LocalRef{Name: "fixture"}
	lp := &lastv1alpha1.LLMProvider{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, Spec: lastv1alpha1.LLMProviderSpec{Type: providerType, Model: "m"}}
	if providerType == lastv1alpha1.LLMScripted {
		lp.Spec.Scripted = &lastv1alpha1.ScriptedLLM{ConfigMapRef: lastv1alpha1.LocalRef{Name: "llm-script"}}
	} else {
		lp.Spec.SecretRef = &lastv1alpha1.LocalRef{Name: "key"}
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
	if _, err := env.callLLM(&gatewayv1.Message{Role: "assistant"}, &gatewayv1.Message{Role: roleUser}); status.Code(err) != codes.FailedPrecondition {
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
	if _, err := newLLMEnv(t, lastv1alpha1.LLMAnthropic).callLLM(user); status.Code(err) != codes.Unimplemented {
		t.Fatalf("anthropic = %v, want Unimplemented", err)
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

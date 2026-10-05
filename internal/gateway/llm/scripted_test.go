package llm_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/gateway/llm"
)

const (
	scriptKey     = "team-x.web"
	intentRef     = "team-x/web"
	roleAssistant = "assistant"
	roleUser      = "user"
	gwNS          = "last-gateway"
	cmName        = "llm-script"
)

func script(data map[string]string) *fake.Clientset {
	return fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: gwNS}, Data: data})
}

func TestScriptedAnswersByAssistantTurnCount(t *testing.T) {
	s := llm.NewScripted(script(map[string]string{scriptKey: `["plan A","plan B"]`}), gwNS, cmName)
	ctx := context.Background()
	r, err := s.Complete(ctx, llm.Request{Intent: intentRef, Messages: []llm.Message{{Role: roleUser, Content: "hi"}}})
	if err != nil || r.Content != "plan A" {
		t.Fatalf("first = %q, %v", r.Content, err)
	}
	r, err = s.Complete(ctx, llm.Request{Intent: intentRef, Messages: []llm.Message{
		{Role: roleUser, Content: "hi"}, {Role: roleAssistant, Content: "plan A"}, {Role: roleUser, Content: "again"}}})
	if err != nil || r.Content != "plan B" {
		t.Fatalf("second = %q, %v", r.Content, err)
	}
	_, err = s.Complete(ctx, llm.Request{Intent: intentRef, Messages: []llm.Message{
		{Role: roleAssistant}, {Role: roleAssistant}}})
	if !errors.Is(err, llm.ErrScriptExhausted) {
		t.Fatalf("exhausted = %v", err)
	}
}

func TestScriptedErrors(t *testing.T) {
	ctx := context.Background()
	req := llm.Request{Intent: intentRef, Messages: []llm.Message{{Role: roleUser}}}
	if _, err := llm.NewScripted(script(map[string]string{}), gwNS, cmName).Complete(ctx, req); !errors.Is(err, llm.ErrNoScript) {
		t.Fatalf("missing key = %v", err)
	}
	if _, err := llm.NewScripted(script(map[string]string{scriptKey: `{}`}), gwNS, cmName).Complete(ctx, req); err == nil {
		t.Fatal("a non-array script must be an error")
	}
	if _, err := llm.NewScripted(fake.NewClientset(), gwNS, cmName).Complete(ctx, req); err == nil {
		t.Fatal("a missing ConfigMap must be an error")
	}
	if llm.ScriptKey(intentRef) != scriptKey {
		t.Fatal("ScriptKey")
	}
}

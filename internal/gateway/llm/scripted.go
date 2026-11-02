package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// ErrNoScript means the script ConfigMap has no entry for the intent.
	ErrNoScript = errors.New("llm: no script for this intent")
	// ErrScriptExhausted means the conversation already used every scripted response.
	ErrScriptExhausted = errors.New("llm: scripted conversation has no more responses")
)

// Scripted replays fixed responses for tests and demos (spec §18). The ConfigMap
// maps ScriptKey(intent) to a JSON array of strings; a conversation that already
// holds n assistant messages gets response n.
type Scripted struct {
	kube      kubernetes.Interface
	namespace string
	configMap string
}

func NewScripted(kube kubernetes.Interface, namespace, configMap string) *Scripted {
	return &Scripted{kube: kube, namespace: namespace, configMap: configMap}
}

const (
	// PurposeSuggest is Request.Purpose for a rule suggestion.
	PurposeSuggest = "suggest"
	// SuggestScriptPrefix precedes the script key of a suggest call.
	SuggestScriptPrefix = "_suggest."
)

// ScriptKey turns "<namespace>/<name>" into a valid ConfigMap key.
func ScriptKey(intent string) string { return strings.Replace(intent, "/", ".", 1) }

func (s *Scripted) Complete(ctx context.Context, req Request) (Response, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.configMap, metav1.GetOptions{})
	if err != nil {
		return Response{}, fmt.Errorf("llm: read script %s/%s: %w", s.namespace, s.configMap, err)
	}
	key := ScriptKey(req.Intent)
	if req.Purpose == PurposeSuggest {
		key = SuggestScriptPrefix + key
	}
	raw, ok := cm.Data[key]
	if !ok {
		return Response{}, fmt.Errorf("%w: %s", ErrNoScript, key)
	}
	var responses []string
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return Response{}, fmt.Errorf("llm: script %s is not a JSON array of strings: %w", key, err)
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	if n >= len(responses) {
		return Response{}, fmt.Errorf("%w (%d of %d used)", ErrScriptExhausted, n, len(responses))
	}
	return Response{Content: responses[n]}, nil
}

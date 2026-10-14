package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type openAISeen struct {
	mu   sync.Mutex
	path string
	auth string
	body map[string]any
}

func fakeOpenAI(t *testing.T, statusCode int, body string) (*httptest.Server, *openAISeen) {
	t.Helper()
	seen := &openAISeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.path, seen.auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.Unmarshal(raw, &seen.body)
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

const openAIAnswer = `{"choices":[{"message":{"role":"assistant","content":"{\"kind\":\"plan\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`

func TestOpenAISendsChatCompletionsWithAJSONSchema(t *testing.T) {
	srv, seen := fakeOpenAI(t, http.StatusOK, openAIAnswer)
	o := NewOpenAI(OpenAIConfig{APIKey: testKey, Model: "gpt-x", Endpoint: srv.URL + "/v1"})
	resp, err := o.Complete(context.Background(), conversation)
	if err != nil || resp.Content != `{"kind":"plan"}` || resp.InputTokens != 20 || resp.OutputTokens != 5 {
		t.Fatalf("Complete = %+v, %v", resp, err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.path != "/v1/chat/completions" || seen.auth != "Bearer "+testKey {
		t.Fatalf("path %s auth %q", seen.path, seen.auth)
	}
	b := seen.body
	rf, _ := b["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if b["model"] != "gpt-x" || b["max_completion_tokens"] != float64(1000) || b["max_tokens"] != nil ||
		rf["type"] != "json_schema" || js["strict"] != true || js["name"] != "answer" || js["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("body = %v", b)
	}
	msgs, _ := b["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestOpenAICompatibleSendsMaxTokensAndNoFormatWithoutASchema(t *testing.T) {
	srv, seen := fakeOpenAI(t, http.StatusOK, openAIAnswer)
	req := conversation
	req.Schema = nil
	o := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "llama", Endpoint: srv.URL + "/v1/", Compatible: true})
	if _, err := o.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.path != "/v1/chat/completions" || seen.body["max_tokens"] != float64(1000) ||
		seen.body["max_completion_tokens"] != nil || seen.body["response_format"] != nil {
		t.Fatalf("path %s body %v", seen.path, seen.body)
	}
}

func TestOpenAIReportsARefusalAndAnEmptyAnswer(t *testing.T) {
	srv, _ := fakeOpenAI(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`)
	_, err := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "m", Endpoint: srv.URL}).Complete(context.Background(), conversation)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("refusal = %v, want ErrRefused", err)
	}
	empty, _ := fakeOpenAI(t, http.StatusOK, `{"choices":[]}`)
	if _, err := NewOpenAI(OpenAIConfig{APIKey: "k", Model: "m", Endpoint: empty.URL}).Complete(context.Background(), conversation); !errors.Is(err, ErrTransient) {
		t.Fatalf("no choices = %v, want ErrTransient", err)
	}
}

func TestOpenAIMapsHTTPErrorsWithoutTheKey(t *testing.T) {
	cases := map[int]error{401: ErrAuth, 404: ErrModel, 400: ErrBadRequest, 408: ErrTransient, 429: ErrTransient, 503: ErrTransient}
	for code, want := range cases {
		srv, _ := fakeOpenAI(t, code, `{"error":{"message":"Incorrect API key provided: sk-sec***ret"}}`)
		_, err := NewOpenAI(OpenAIConfig{APIKey: testKey, Model: "m", Endpoint: srv.URL}).Complete(context.Background(), conversation)
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", code, err, want)
		}
		if code == 401 && err != nil && strings.Contains(err.Error(), "sk-sec") {
			t.Errorf("HTTP 401: the error quotes the provider's key hint: %v", err)
		}
	}
}

func TestOpenAIDefaultsToTheOpenAIEndpoint(t *testing.T) {
	if o := NewOpenAI(OpenAIConfig{}); o.cfg.Endpoint != DefaultOpenAIEndpoint {
		t.Fatalf("endpoint = %q", o.cfg.Endpoint)
	}
}

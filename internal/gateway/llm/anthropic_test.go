package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sse renders server-sent events: each pair is an event name and its JSON data.
func sse(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", pairs[i], pairs[i+1])
	}
	return b.String()
}

// textStream is a streamed answer with the given text, stop reason, and stop details.
func textStream(text, stopReason, stopDetails string) string {
	quoted, _ := json.Marshal(text)
	return sse(
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(quoted)+`}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null,"stop_details":`+stopDetails+`},"usage":{"output_tokens":9}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
}

const (
	testKey   = "secret-key"
	testModel = "claude-opus-5-5"
)

type captured struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   map[string]any
}

// fakeAnthropic answers every request with status and body, recording the request.
func fakeAnthropic(t *testing.T, statusCode int, body string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path, c.header = r.URL.Path, r.Header.Clone()
		_ = json.Unmarshal(raw, &c.body)
		c.mu.Unlock()
		if statusCode == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func anthropicFor(srv *httptest.Server, effort string) *Anthropic {
	return NewAnthropic(AnthropicConfig{APIKey: testKey, Model: testModel, Effort: effort,
		BaseURL: srv.URL, DisableRetries: true})
}

var conversation = Request{Intent: "team-x/web", MaxTokens: 1000, Schema: []byte(`{"type":"object"}`), Messages: []Message{
	{Role: "system", Content: "You plan."}, {Role: "user", Content: "run nginx"},
	{Role: "assistant", Content: "{}"}, {Role: "user", Content: "Answer again."}}}

func TestAnthropicSendsTheRequestTheSpecRequires(t *testing.T) {
	srv, c := fakeAnthropic(t, http.StatusOK, textStream(`{"kind":"plan"}`, "end_turn", "null"))
	resp, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"kind":"plan"}` || resp.InputTokens != 12 || resp.OutputTokens != 9 ||
		resp.CacheReadTokens != 100 || resp.CacheCreationTokens != 7 {
		t.Fatalf("response = %+v", resp)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path != "/v1/messages" || c.header.Get("X-Api-Key") != testKey ||
		!strings.Contains(c.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("path %s, headers %v", c.path, c.header)
	}
	b := c.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] != float64(1000) || b["stream"] != true || b["fallbacks"] != "default" {
		t.Fatalf("body = %v", b)
	}
	oc, _ := b["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if oc["effort"] != DefaultEffort || format["type"] != "json_schema" || format["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("output_config = %v", oc)
	}
	system, _ := b["system"].([]any)
	last, _ := system[len(system)-1].(map[string]any)
	if cc, _ := last["cache_control"].(map[string]any); last["text"] != "You plan." || cc["type"] != "ephemeral" {
		t.Fatalf("system = %v", system)
	}
	msgs, _ := b["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles = %v (the system message must not be sent as a turn)", roles)
	}
}

func TestAnthropicHonoursEffortAndOmitsAnAbsentSchema(t *testing.T) {
	srv, c := fakeAnthropic(t, http.StatusOK, textStream("hi", "end_turn", "null"))
	req := conversation
	req.Schema, req.MaxTokens = nil, 0
	if _, err := anthropicFor(srv, "medium").Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oc, _ := c.body["output_config"].(map[string]any)
	if _, ok := oc["format"]; ok || oc["effort"] != "medium" || c.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Fatalf("body = %v", c.body)
	}
}

func TestAnthropicReturnsPartialContentAtMaxTokens(t *testing.T) {
	srv, _ := fakeAnthropic(t, http.StatusOK, textStream(`{"kind":"pl`, "max_tokens", "null"))
	resp, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if err != nil || resp.Content != `{"kind":"pl` {
		t.Fatalf("Complete = %+v, %v; want the partial content and no error", resp, err)
	}
}

func TestAnthropicReportsARefusal(t *testing.T) {
	srv, _ := fakeAnthropic(t, http.StatusOK, textStream("", "refusal", `{"type":"refusal","category":"cyber","explanation":"x"}`))
	_, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cyber") {
		t.Fatalf("Complete = %v, want ErrRefused naming the category", err)
	}
}

// A server-side fallback marks where the declining model's output ends with a
// fallback block (a start and stop, no deltas); the fallback model's text follows.
func TestAnthropicJoinsTheTextOnEitherSideOfAFallback(t *testing.T) {
	text := func(i int, s string) string {
		return sse(
			"content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, i),
			"content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, i, s),
			"content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	stream := sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}`) +
		text(0, `{"kind":"comm`) +
		sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-sonnet-5-5"},"trigger":{"type":"refusal","category":"cyber"}}}`,
			"content_block_stop", `{"type":"content_block_stop","index":1}`) +
		text(2, `and"}`) +
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
			"message_stop", `{"type":"message_stop"}`)
	srv, _ := fakeAnthropic(t, http.StatusOK, stream)
	resp, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if err != nil || resp.Content != `{"kind":"command"}` {
		t.Fatalf("Complete = %+v, %v; want the two texts joined in order and no error", resp, err)
	}
}

func TestAnthropicMapsHTTPErrors(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"x","message":"nope"}}`
	cases := map[int]error{401: ErrAuth, 403: ErrAuth, 404: ErrModel, 400: ErrBadRequest, 408: ErrTransient, 429: ErrTransient, 500: ErrTransient, 529: ErrTransient}
	for code, want := range cases {
		srv, _ := fakeAnthropic(t, code, errBody)
		_, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", code, err, want)
		}
		if err != nil && strings.Contains(err.Error(), testKey) {
			t.Errorf("HTTP %d: the API key leaked into the error", code)
		}
	}
}

func TestAnthropicErrorsNeverCarryTheEndpointsCredentials(t *testing.T) {
	srv, _ := fakeAnthropic(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`)
	proxied := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	a := NewAnthropic(AnthropicConfig{APIKey: testKey, Model: testModel, BaseURL: proxied, DisableRetries: true})
	_, err := a.Complete(context.Background(), conversation)
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "max_tokens: too large") {
		t.Fatalf("Complete = %v, want ErrBadRequest with the API's message", err)
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("Complete = %v; the endpoint leaked into the error", err)
	}
}

func TestAnthropicTreatsAMidStreamErrorEventAsTransient(t *testing.T) {
	body := sse(
		"message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	)
	srv, _ := fakeAnthropic(t, http.StatusOK, body)
	_, err := anthropicFor(srv, "").Complete(context.Background(), conversation)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Complete = %v, want ErrTransient", err)
	}
}

func TestAnthropicIgnoresAmbientCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	srv, c := fakeAnthropic(t, http.StatusOK, textStream("hi", "end_turn", "null"))
	if _, err := anthropicFor(srv, "").Complete(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == "" || c.header.Get("Authorization") != "" || c.header.Get("X-Api-Key") != testKey {
		t.Fatalf("path %q, headers %v", c.path, c.header)
	}
}

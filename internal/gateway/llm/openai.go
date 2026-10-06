package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenAIEndpoint is the OpenAI API base URL.
const DefaultOpenAIEndpoint = "https://api.openai.com/v1"

const (
	openAITimeout       = 10 * time.Minute
	maxOpenAIErrorBytes = 4 << 10
	maxOpenAIBodyBytes  = 32 << 20
)

// OpenAIConfig configures the Chat Completions adapter, used for both the
// "openai" and "openai-compatible" provider types. APIKey is a credential: it
// is never logged or placed in an error.
type OpenAIConfig struct {
	APIKey   string
	Model    string
	Endpoint string // API base URL; empty means DefaultOpenAIEndpoint
	// Compatible sends max_tokens, which self-hosted servers expect, instead of
	// OpenAI's max_completion_tokens.
	Compatible bool
	HTTPClient *http.Client
}

// OpenAI calls an OpenAI-style Chat Completions API. It does not retry: a
// transient failure is ErrTransient, and the operator retries with backoff.
type OpenAI struct {
	cfg    OpenAIConfig
	client *http.Client
}

// NewOpenAI builds the adapter.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultOpenAIEndpoint
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: openAITimeout}
	}
	return &OpenAI{cfg: cfg, client: c}
}

type openAIRequest struct {
	Model               string                `json:"model"`
	Messages            []Message             `json:"messages"`
	MaxCompletionTokens int                   `json:"max_completion_tokens,omitempty"`
	MaxTokens           int                   `json:"max_tokens,omitempty"`
	ResponseFormat      *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete sends one Chat Completions request and returns the first choice.
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	body := openAIRequest{Model: o.cfg.Model, Messages: req.Messages}
	if o.cfg.Compatible {
		body.MaxTokens = maxTokens
	} else {
		body.MaxCompletionTokens = maxTokens
	}
	if len(req.Schema) > 0 {
		body.ResponseFormat = &openAIResponseFormat{Type: "json_schema",
			JSONSchema: openAIJSONSchema{Name: "answer", Schema: json.RawMessage(req.Schema), Strict: true}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request: %v", ErrBadRequest, err)
	}
	url := strings.TrimSuffix(o.cfg.Endpoint, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	res, err := o.client.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, maxOpenAIErrorBytes))
		return Response{}, classifyHTTP(res.StatusCode, strings.TrimSpace(string(detail)))
	}
	var out openAIResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxOpenAIBodyBytes)).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrTransient, err)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: the response has no choices", ErrTransient)
	}
	msg := out.Choices[0].Message
	if msg.Refusal != "" {
		return Response{}, fmt.Errorf("%w: %s", ErrRefused, msg.Refusal)
	}
	return Response{Content: msg.Content, InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}, nil
}

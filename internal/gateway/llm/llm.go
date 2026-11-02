// Package llm is the gateway's provider-neutral LLM interface and its adapters (spec §12).
package llm

import (
	"context"
	"errors"
)

// Message is one conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a provider-neutral completion request.
type Request struct {
	Intent    string // "<namespace>/<name>"
	Messages  []Message
	MaxTokens int
	// Schema, when set, is a JSON Schema the answer must match. Adapters that
	// support structured output enforce it; the caller validates the answer anyway.
	Schema []byte
	// Purpose is "" or "turn" for a planning turn, "suggest" for a rule suggestion.
	Purpose string
}

// Response is the model's reply.
type Response struct {
	Content             string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// Provider completes conversations; adapters translate for each vendor.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Errors adapters wrap (with %w) so the gateway can map them to gRPC codes.
// Messages never include credentials.
var (
	ErrAuth       = errors.New("llm: the provider rejected the credentials")
	ErrModel      = errors.New("llm: the provider does not know the model")
	ErrBadRequest = errors.New("llm: the provider rejected the request")
	ErrRefused    = errors.New("llm: the model declined")
	ErrTransient  = errors.New("llm: the provider is unavailable")
)

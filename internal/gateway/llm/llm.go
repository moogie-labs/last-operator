// Package llm is the gateway's provider-neutral LLM interface and its adapters (spec §12).
package llm

import "context"

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
}

// Response is the model's reply.
type Response struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

// Provider completes conversations; adapters translate for each vendor.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

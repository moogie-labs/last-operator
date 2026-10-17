// Command schemaprobe asks Anthropic whether the operator's turn schema
// (agent.Schema) compiles as structured output. Anthropic refuses a schema whose
// compiled grammar is too large, which no offline test can show, so run this
// after changing the schema. It sends one tiny request at effort low through
// the gateway's own adapter. The key comes from ANTHROPIC_API_KEY and is never
// printed.
//
//	ANTHROPIC_API_KEY=... go run ./hack/schemaprobe
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
)

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "Set ANTHROPIC_API_KEY.")
		os.Exit(1)
	}
	model := os.Getenv("DEMO_MODEL")
	if model == "" {
		model = "claude-opus-5-5"
	}
	a := llm.NewAnthropic(llm.AnthropicConfig{APIKey: key, Model: model, Effort: "low"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	schema := agent.Schema()
	const ask = `Reply with kind "questions" and one question: "Which namespace?". Leave every other field empty.`
	resp, err := a.Complete(ctx, llm.Request{Intent: "probe/probe", MaxTokens: 4096, Schema: schema,
		Messages: []llm.Message{
			{Role: "system", Content: "Answer with one JSON object that matches the schema."},
			{Role: "user", Content: ask},
		}})
	if err != nil {
		fmt.Printf("REJECTED (%d-byte schema): %v\n", len(schema), err)
		os.Exit(1)
	}
	if _, err := agent.ParseTurn(resp.Content); err != nil {
		fmt.Printf("ACCEPTED (%d-byte schema), but the answer is not a valid turn: %v\n", len(schema), err)
		os.Exit(1)
	}
	fmt.Printf("ACCEPTED (%d-byte schema); the answer parses as a turn\n", len(schema))
}

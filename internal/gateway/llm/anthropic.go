package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

const (
	// DefaultEffort is the reasoning effort when the LLMProvider sets none:
	// planning is intelligence-sensitive (M3 spec §3.2).
	DefaultEffort = "high"
	// DefaultMaxTokens caps an answer when the request sets no limit.
	DefaultMaxTokens = 16000
)

// AnthropicConfig configures the anthropic adapter. APIKey is a credential:
// it is never logged or placed in an error.
type AnthropicConfig struct {
	APIKey string
	Model  string
	Effort string // empty means DefaultEffort
	// BaseURL overrides the API endpoint (a proxy, or tests); empty means the SDK default.
	BaseURL    string
	HTTPClient *http.Client
	// DisableRetries turns off the SDK's retries (tests).
	DisableRetries bool
}

// Anthropic calls Claude through the Messages API with structured output,
// prompt caching, and the server-side refusal fallback.
type Anthropic struct {
	client anthropic.Client
	model  string
	effort string
}

// NewAnthropic builds the adapter.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	retries := 2
	if cfg.DisableRetries {
		retries = 0
	}
	// WithoutEnvironmentDefaults first: the SDK would otherwise read ANTHROPIC_* credentials,
	// base URL and profiles from the environment, and the key must come only from the Secret.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(retries)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	effort := cfg.Effort
	if effort == "" {
		effort = DefaultEffort
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: cfg.Model, effort: effort}
}

// Complete streams one answer and returns its text. A refusal is ErrRefused;
// an answer cut off at max_tokens is returned as it is, for the caller to judge.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	params := anthropic.BetaMessageNewParams{
		Model:        a.model,
		MaxTokens:    int64(maxTokens),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks:    anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(a.effort)},
	}
	if len(req.Schema) > 0 {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: json.RawMessage(req.Schema)}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			params.System = append(params.System, anthropic.BetaTextBlockParam{Text: m.Content})
		case "assistant":
			params.Messages = append(params.Messages, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)}})
		default:
			params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(m.Content)))
		}
	}
	// The system prompt is long and stable across turns: cache everything up to its end.
	if n := len(params.System); n > 0 {
		params.System[n-1].CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}

	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Response{}, fmt.Errorf("%w: %v", ErrTransient, err)
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, classifyAnthropic(err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		category := string(msg.StopDetails.Category)
		if category == "" {
			category = "unspecified"
		}
		return Response{}, fmt.Errorf("%w (category %s)", ErrRefused, category)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	u := msg.Usage
	return Response{Content: text.String(), InputTokens: int(u.InputTokens), OutputTokens: int(u.OutputTokens),
		CacheReadTokens: int(u.CacheReadInputTokens), CacheCreationTokens: int(u.CacheCreationInputTokens)}, nil
}

// maxAPIErrorMessageBytes bounds the API error message carried into an error.
const maxAPIErrorMessageBytes = 1 << 10

// classifyAnthropic wraps an SDK error in the llm error the gateway maps to a
// gRPC code. The API's error text names the problem, never the key.
func classifyAnthropic(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%w: %v", ErrTransient, err)
	}
	if apiErr.StatusCode < http.StatusBadRequest {
		// An error event inside a 200 stream (for example overloaded_error).
		return fmt.Errorf("%w: the stream reported an error", ErrTransient)
	}
	// Never apiErr.Error(): it quotes the request URL, and an endpoint's user-info
	// (a proxy credential) would reach the error and the audit log.
	return classifyHTTP(apiErr.StatusCode, apiErrorMessage(apiErr.RawJSON()))
}

// apiErrorMessage returns error.message from an API error body, bounded, or ""
// when the body is not the API's error envelope.
func apiErrorMessage(body string) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return ""
	}
	msg := envelope.Error.Message
	if len(msg) > maxAPIErrorMessageBytes {
		cut := maxAPIErrorMessageBytes
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…(truncated)"
	}
	return strings.ToValidUTF8(msg, "")
}

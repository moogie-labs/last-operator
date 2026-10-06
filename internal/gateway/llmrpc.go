package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

type llmRequestPayload struct {
	Provider  string        `json:"provider"`
	Model     string        `json:"model"`
	Messages  []llm.Message `json:"messages"`
	MaxTokens int           `json:"maxTokens,omitempty"`
	// The response schema is the same on every turn; the log keeps its digest only.
	SchemaSHA256 string `json:"schemaSha256,omitempty"`
	SchemaBytes  int    `json:"schemaBytes,omitempty"`
}

type llmResponsePayload struct {
	RequestSeq          uint64 `json:"requestSeq"`
	Content             string `json:"content"`
	InputTokens         int    `json:"inputTokens"`
	OutputTokens        int    `json:"outputTokens"`
	CacheReadTokens     int    `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int    `json:"cacheCreationTokens,omitempty"`
	Error               string `json:"error,omitempty"`
}

// maxSchemaBytes bounds CallLLMRequest.response_schema.
const maxSchemaBytes = 64 << 10

var llmRoles = map[string]bool{"system": true, "user": true, "assistant": true}

// llmAPIKeyField is the Secret key holding an LLMProvider's API key.
const llmAPIKeyField = "apiKey"

// llmAPIKey reads an LLMProvider's API key from its Secret in the gateway
// namespace. Errors name the Secret, never the value.
func (g *Gateway) llmAPIKey(ctx context.Context, lp *lastv1alpha1.LLMProvider) (string, error) {
	if lp.Spec.SecretRef == nil {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no secretRef", lp.Name)
	}
	s, err := g.kube.CoreV1().Secrets(g.cfg.Namespace).Get(ctx, lp.Spec.SecretRef.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s: Secret %s not found", lp.Name, lp.Spec.SecretRef.Name)
	}
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "read LLMProvider %s key Secret: %v", lp.Name, err)
	}
	key := strings.TrimSpace(string(s.Data[llmAPIKeyField]))
	if key == "" {
		return "", status.Errorf(codes.FailedPrecondition, "LLMProvider %s: Secret %s has no %q key", lp.Name, lp.Spec.SecretRef.Name, llmAPIKeyField)
	}
	return key, nil
}

func (g *Gateway) llmProvider(ctx context.Context, lp *lastv1alpha1.LLMProvider) (llm.Provider, error) {
	switch lp.Spec.Type {
	case lastv1alpha1.LLMScripted:
		if lp.Spec.Scripted == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no scripted config", lp.Name)
		}
		return llm.NewScripted(g.kube, g.cfg.Namespace, lp.Spec.Scripted.ConfigMapRef.Name), nil
	case lastv1alpha1.LLMAnthropic:
		key, err := g.llmAPIKey(ctx, lp)
		if err != nil {
			return nil, err
		}
		return llm.NewAnthropic(llm.AnthropicConfig{APIKey: key, Model: lp.Spec.Model, Effort: string(lp.Spec.Effort),
			BaseURL: lp.Spec.Endpoint}), nil
	case lastv1alpha1.LLMOpenAI, lastv1alpha1.LLMOpenAICompatible:
		key, err := g.llmAPIKey(ctx, lp)
		if err != nil {
			return nil, err
		}
		return llm.NewOpenAI(llm.OpenAIConfig{APIKey: key, Model: lp.Spec.Model, Endpoint: lp.Spec.Endpoint,
			Compatible: lp.Spec.Type == lastv1alpha1.LLMOpenAICompatible}), nil
	default:
		return nil, status.Errorf(codes.Unimplemented, "LLM provider type %q is not supported yet", lp.Spec.Type)
	}
}

func (g *Gateway) callLLM(ctx context.Context, req *gatewayv1.CallLLMRequest) (*gatewayv1.CallLLMResponse, error) {
	w, release, err := g.beginRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	ns, name, key, err := intentKey(req.GetIntent())
	if err != nil {
		return nil, err
	}
	if len(req.GetMessages()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "messages are required")
	}
	msgs := make([]llm.Message, 0, len(req.GetMessages()))
	for _, m := range req.GetMessages() {
		if !llmRoles[m.GetRole()] {
			return nil, status.Errorf(codes.InvalidArgument, "role %q is not system, user, or assistant", m.GetRole())
		}
		msgs = append(msgs, llm.Message{Role: m.GetRole(), Content: m.GetContent()})
	}
	schema := req.GetResponseSchema()
	if len(schema) > maxSchemaBytes {
		return nil, status.Errorf(codes.InvalidArgument, "response_schema is %d bytes; the limit is %d", len(schema), maxSchemaBytes)
	}
	if len(schema) > 0 && !json.Valid(schema) {
		return nil, status.Error(codes.InvalidArgument, "response_schema is not JSON")
	}
	var intent lastv1alpha1.Intent
	if err := g.crd.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &intent); err != nil {
		return nil, notFoundOr(err, "intent")
	}
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return nil, err
	}
	if op == nil || op.Spec.LLMProviderRef == nil {
		return nil, status.Error(codes.FailedPrecondition, "no LLM provider configured in OperatorConfig")
	}
	var lp lastv1alpha1.LLMProvider
	if err := g.crd.Get(ctx, client.ObjectKey{Name: op.Spec.LLMProviderRef.Name}, &lp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s not found", op.Spec.LLMProviderRef.Name)
		}
		return nil, status.Errorf(codes.Unavailable, "read LLMProvider: %v", err)
	}
	provider, err := g.llmProvider(ctx, &lp)
	if err != nil {
		return nil, err
	}
	actor := CallerFrom(ctx)
	lreq := llm.Request{Intent: key, Messages: msgs, MaxTokens: int(req.GetMaxTokens()), Schema: schema}
	reqPayload := llmRequestPayload{Provider: lp.Name, Model: lp.Spec.Model, Messages: msgs, MaxTokens: lreq.MaxTokens}
	if len(schema) > 0 {
		sum := sha256.Sum256(schema)
		reqPayload.SchemaSHA256, reqPayload.SchemaBytes = hex.EncodeToString(sum[:]), len(schema)
	}
	var resp llm.Response
	var requestSeq uint64
	err = w.Do(ctx,
		audit.Draft{Type: audit.TypeLLMRequest, Actor: actor, Intent: key, Payload: reqPayload},
		func(ctx context.Context) (any, error) {
			r, err := provider.Complete(ctx, lreq)
			resp = r
			return r, err
		},
		func(before audit.Entry, _ any, actionErr error) audit.Draft {
			requestSeq = before.Seq
			p := llmResponsePayload{RequestSeq: before.Seq, Content: resp.Content, InputTokens: resp.InputTokens,
				OutputTokens: resp.OutputTokens, CacheReadTokens: resp.CacheReadTokens, CacheCreationTokens: resp.CacheCreationTokens}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			return audit.Draft{Type: audit.TypeLLMResponse, Actor: actor, Intent: key, Payload: p}
		})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		g.metrics.ObserveLLM(lp.Name, lp.Spec.Model, resp, err)
	}
	switch {
	case err == nil:
		return &gatewayv1.CallLLMResponse{Content: resp.Content, InputTokens: int32(resp.InputTokens), //nolint:gosec // token counts fit
			OutputTokens: int32(resp.OutputTokens), RequestSeq: requestSeq}, nil //nolint:gosec // token counts fit
	case errors.Is(err, audit.ErrAuditUnavailable):
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	default:
		return nil, llmStatus(err)
	}
}

// llmStatus maps an adapter error to a gRPC status the operator classifies:
// errors retrying cannot fix are FailedPrecondition or InvalidArgument;
// everything else is Unavailable and retried with backoff.
func llmStatus(err error) error {
	switch {
	case errors.Is(err, llm.ErrNoScript), errors.Is(err, llm.ErrScriptExhausted),
		errors.Is(err, llm.ErrAuth), errors.Is(err, llm.ErrModel), errors.Is(err, llm.ErrRefused):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	case errors.Is(err, llm.ErrBadRequest):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	default:
		return status.Errorf(codes.Unavailable, "LLM call failed: %v", err)
	}
}

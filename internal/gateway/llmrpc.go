package gateway

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
}

type llmResponsePayload struct {
	RequestSeq   uint64 `json:"requestSeq"`
	Content      string `json:"content"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	Error        string `json:"error,omitempty"`
}

var llmRoles = map[string]bool{"system": true, "user": true, "assistant": true}

func (g *Gateway) llmProvider(lp *lastv1alpha1.LLMProvider) (llm.Provider, error) {
	switch lp.Spec.Type {
	case lastv1alpha1.LLMScripted:
		if lp.Spec.Scripted == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "LLMProvider %s has no scripted config", lp.Name)
		}
		return llm.NewScripted(g.kube, g.cfg.Namespace, lp.Spec.Scripted.ConfigMapRef.Name), nil
	default:
		return nil, status.Errorf(codes.Unimplemented, "LLM provider type %q arrives with the real adapters (M3)", lp.Spec.Type)
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
	provider, err := g.llmProvider(&lp)
	if err != nil {
		return nil, err
	}
	actor := CallerFrom(ctx)
	lreq := llm.Request{Intent: key, Messages: msgs, MaxTokens: int(req.GetMaxTokens())}
	var resp llm.Response
	err = w.Do(ctx,
		audit.Draft{Type: audit.TypeLLMRequest, Actor: actor, Intent: key,
			Payload: llmRequestPayload{Provider: lp.Name, Model: lp.Spec.Model, Messages: msgs, MaxTokens: lreq.MaxTokens}},
		func(ctx context.Context) (any, error) {
			r, err := provider.Complete(ctx, lreq)
			resp = r
			return r, err
		},
		func(before audit.Entry, _ any, actionErr error) audit.Draft {
			p := llmResponsePayload{RequestSeq: before.Seq, Content: resp.Content, InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens}
			if actionErr != nil {
				p.Error = actionErr.Error()
			}
			return audit.Draft{Type: audit.TypeLLMResponse, Actor: actor, Intent: key, Payload: p}
		})
	switch {
	case err == nil:
		return &gatewayv1.CallLLMResponse{Content: resp.Content, InputTokens: int32(resp.InputTokens), OutputTokens: int32(resp.OutputTokens)}, nil //nolint:gosec // token counts fit
	case errors.Is(err, audit.ErrAuditUnavailable):
		return nil, status.Errorf(codes.Unavailable, "%v", err)
	case errors.Is(err, llm.ErrNoScript), errors.Is(err, llm.ErrScriptExhausted):
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return nil, status.Errorf(codes.Unavailable, "LLM call failed: %v", err)
	}
}

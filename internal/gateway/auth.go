package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GRPCAudience is the audience the operator's projected token must carry.
const GRPCAudience = "last-gateway"

// Authenticator admits only the operator's ServiceAccount (spec §4.2).
type Authenticator struct {
	kube     kubernetes.Interface
	audience string
	allowed  string
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]time.Time // hash of an accepted token → cache expiry
}

func NewAuthenticator(kube kubernetes.Interface, audience, allowed string) *Authenticator {
	return &Authenticator{kube: kube, audience: audience, allowed: allowed, ttl: 30 * time.Second,
		now: time.Now, cache: map[[32]byte]time.Time{}}
}

// Authenticate validates token with the TokenReview API and returns the caller.
// Failures are never cached; an unreachable API server fails closed.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if exp, ok := a.cache[key]; ok && now.Before(exp) {
		a.mu.Unlock()
		return a.allowed, nil
	}
	a.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := a.kube.AuthenticationV1().TokenReviews().Create(rctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("Could not review token", "err", err)
		return "", status.Error(codes.Unavailable, "token review failed")
	}
	switch {
	case !res.Status.Authenticated:
		slog.Warn("Rejected gRPC caller", "reason", "token not authenticated")
		return "", status.Error(codes.Unauthenticated, "token not authenticated")
	case res.Status.Error != "":
		slog.Warn("Rejected gRPC caller", "reason", "token review reported an error")
		return "", status.Error(codes.Unauthenticated, "token not authenticated")
	case !slices.Contains(res.Status.Audiences, a.audience):
		slog.Warn("Rejected gRPC caller", "reason", "wrong audience")
		return "", status.Error(codes.Unauthenticated, "token audience is not "+a.audience)
	case res.Status.User.Username != a.allowed:
		slog.Warn("Rejected gRPC caller", "user", res.Status.User.Username)
		return "", status.Errorf(codes.PermissionDenied, "caller %s is not allowed", res.Status.User.Username)
	}
	expiry := now.Add(a.ttl)
	if exp, ok := tokenExpiry(token); ok {
		if !exp.After(now) {
			return a.allowed, nil // already expired by its own claim: never cache
		}
		if exp.Before(expiry) {
			expiry = exp
		}
	}
	a.mu.Lock()
	for k, exp := range a.cache {
		if !now.Before(exp) {
			delete(a.cache, k)
		}
	}
	a.cache[key] = expiry
	a.mu.Unlock()
	return a.allowed, nil
}

// tokenExpiry reads the unverified "exp" claim of a JWT. It only ever shortens a
// cache entry; TokenReview alone decides whether the token is valid.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0), true
}

type callerKey struct{}

// CallerFrom returns the authenticated caller stored by the interceptor.
func CallerFrom(ctx context.Context) string {
	s, _ := ctx.Value(callerKey{}).(string)
	return s
}

// UnaryInterceptor authenticates every call from its "authorization: Bearer" metadata.
func (a *Authenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		user, err := a.Authenticate(ctx, bearerToken(ctx))
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, callerKey{}, user), req)
	}
}

// StreamInterceptor authenticates every streaming call the same way.
func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		user, err := a.Authenticate(ss.Context(), bearerToken(ss.Context()))
		if err != nil {
			return err
		}
		return handler(srv, &callerStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), callerKey{}, user)})
	}
}

type callerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (c *callerStream) Context() context.Context { return c.ctx }

func bearerToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) != 1 {
		return ""
	}
	if scheme, token, ok := strings.Cut(vals[0], " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

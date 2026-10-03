package gateway

import (
	"context"
	"crypto/sha256"
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

	res, err := a.kube.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", status.Error(codes.Unavailable, "token review failed")
	}
	switch {
	case !res.Status.Authenticated:
		return "", status.Error(codes.Unauthenticated, "token not authenticated")
	case !slices.Contains(res.Status.Audiences, a.audience):
		return "", status.Error(codes.Unauthenticated, "token audience is not "+a.audience)
	case res.Status.User.Username != a.allowed:
		return "", status.Errorf(codes.PermissionDenied, "caller %s is not allowed", res.Status.User.Username)
	}
	a.mu.Lock()
	for k, exp := range a.cache {
		if !now.Before(exp) {
			delete(a.cache, k)
		}
	}
	a.cache[key] = now.Add(a.ttl)
	a.mu.Unlock()
	return a.allowed, nil
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

func bearerToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

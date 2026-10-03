package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	tokOther    = "other"
	tokWrongAud = "wrong-audience"
	tokBoom     = "boom"
)

const operatorID = "system:serviceaccount:last-system:last-operator"

// reviewingClient answers TokenReviews by token value and counts the calls.
func reviewingClient(calls *int) *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*calls++
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		switch tr.Spec.Token {
		case "good":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{GRPCAudience}}
		case tokOther:
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "system:serviceaccount:last-system:default"}, Audiences: []string{GRPCAudience}}
		case tokWrongAud:
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{"https://kubernetes.default.svc"}}
		case "no-audiences":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}}
		case "authed-with-error":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Error: "partial", User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{GRPCAudience}}
		case tokBoom:
			return true, nil, errors.New("apiserver unavailable")
		default:
			if strings.Contains(tr.Spec.Token, ".") { // JWT-shaped test tokens are valid
				tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{GRPCAudience}}
				break
			}
			tr.Status = authnv1.TokenReviewStatus{Authenticated: false, Error: "invalid token"}
		}
		return true, tr, nil
	})
	return kube
}

func codeOf(err error) codes.Code { return status.Code(err) }

func TestAuthenticateAcceptsTheOperatorAndCaches(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	for range 2 {
		user, err := a.Authenticate(context.Background(), "good")
		if err != nil || user != operatorID {
			t.Fatalf("user=%q err=%v", user, err)
		}
	}
	if calls != 1 {
		t.Fatalf("TokenReview calls = %d, want 1 (cached)", calls)
	}
	now = now.Add(31 * time.Second)
	if _, err := a.Authenticate(context.Background(), "good"); err != nil || calls != 2 {
		t.Fatalf("after expiry: err=%v calls=%d", err, calls)
	}
}

func TestAuthenticateRejectsAndNeverCachesFailures(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	cases := map[string]codes.Code{
		"":                  codes.Unauthenticated,
		"bad":               codes.Unauthenticated,
		"wrong-audience":    codes.Unauthenticated,
		"other":             codes.PermissionDenied,
		"boom":              codes.Unavailable,
		"no-audiences":      codes.Unauthenticated,
		"authed-with-error": codes.Unauthenticated,
	}
	for token, want := range cases {
		for range 2 {
			if _, err := a.Authenticate(context.Background(), token); codeOf(err) != want {
				t.Fatalf("token %q: code %v, want %v (err %v)", token, codeOf(err), want, err)
			}
		}
	}
	if calls != 12 { // 6 non-empty tokens × 2 attempts, nothing cached
		t.Fatalf("TokenReview calls = %d, want 12", calls)
	}
}

func jwtWithExp(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}

func TestCacheNeverOutlivesTokenExpiry(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	tok := jwtWithExp(now.Add(5 * time.Second))
	for range 2 {
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	now = now.Add(6 * time.Second)
	if _, err := a.Authenticate(context.Background(), tok); err != nil || calls != 2 {
		t.Fatalf("after exp: err=%v calls=%d, want a re-review", err, calls)
	}
}

func TestExpiredOrUnparseableExpDoesNotBreakCaching(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	past := jwtWithExp(now.Add(-time.Minute))
	for range 2 {
		if _, err := a.Authenticate(context.Background(), past); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("past-exp token calls = %d, want 2 (not cached)", calls)
	}
	calls = 0
	for _, tok := range []string{"a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + ".c"} {
		for range 2 {
			if _, err := a.Authenticate(context.Background(), tok); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("no-exp tokens calls = %d, want 2 (cached under the 30s TTL)", calls)
	}
}

func TestDenialsAreLoggedWithoutTheToken(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	for _, tok := range []string{tokOther, "bad", tokWrongAud, tokBoom} {
		_, _ = a.Authenticate(context.Background(), tok)
	}
	out := buf.String()
	if !strings.Contains(out, "system:serviceaccount:last-system:default") {
		t.Errorf("deny did not log the username:\n%s", out)
	}
	for _, want := range []string{"Rejected gRPC caller", "Could not review token"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	for line := range strings.SplitSeq(out, "\n") {
		for _, tok := range []string{tokOther, tokWrongAud, tokBoom} {
			if strings.Contains(line, "token="+tok) || strings.Contains(line, "Bearer "+tok) {
				t.Errorf("token leaked in log line: %s", line)
			}
		}
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

func TestStreamInterceptorAuthenticates(t *testing.T) {
	calls := 0
	a := NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID)
	run := func(md metadata.MD) (string, error) {
		var caller string
		ctx := metadata.NewIncomingContext(context.Background(), md)
		err := a.StreamInterceptor()(nil, fakeStream{ctx: ctx}, &grpc.StreamServerInfo{}, func(_ any, ss grpc.ServerStream) error {
			caller = CallerFrom(ss.Context())
			return nil
		})
		return caller, err
	}
	if _, err := run(metadata.MD{}); codeOf(err) != codes.Unauthenticated {
		t.Fatalf("no token: %v", err)
	}
	caller, err := run(metadata.Pairs("authorization", "Bearer good"))
	if err != nil || caller != operatorID {
		t.Fatalf("caller=%q err=%v", caller, err)
	}
}

func TestGRPCServerRequiresAnAuthenticator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("GRPCServer(nil) did not panic")
		}
	}()
	testGateway(t, nil).GRPCServer(nil)
}

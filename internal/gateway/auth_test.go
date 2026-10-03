package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		case "other":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "system:serviceaccount:last-system:default"}, Audiences: []string{GRPCAudience}}
		case "wrong-audience":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: operatorID}, Audiences: []string{"https://kubernetes.default.svc"}}
		case "boom":
			return true, nil, errors.New("apiserver unavailable")
		default:
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
		"":               codes.Unauthenticated,
		"bad":            codes.Unauthenticated,
		"wrong-audience": codes.Unauthenticated,
		"other":          codes.PermissionDenied,
		"boom":           codes.Unavailable,
	}
	for token, want := range cases {
		for range 2 {
			if _, err := a.Authenticate(context.Background(), token); codeOf(err) != want {
				t.Fatalf("token %q: code %v, want %v (err %v)", token, codeOf(err), want, err)
			}
		}
	}
	if calls != 8 { // 4 non-empty tokens × 2 attempts, nothing cached
		t.Fatalf("TokenReview calls = %d, want 8", calls)
	}
}

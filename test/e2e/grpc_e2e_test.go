//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func TestGatewayGRPCAuth(t *testing.T) {
	kubectl(t, "rollout", "status", "-n", "last-gateway", "deploy/last-gateway", "--timeout=5m")
	addr := portForward(t, "last-gateway", "svc/last-gateway", "9090")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := gatewayv1.NewGatewayServiceClient(conn)
	ping := func(token string) (*gatewayv1.PingResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		return client.Ping(ctx, &gatewayv1.PingRequest{})
	}
	operatorToken := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--audience", "last-gateway", "--duration", "10m")

	t.Run("operator token is accepted", func(t *testing.T) {
		resp, err := ping(operatorToken)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCaller() != "system:serviceaccount:last-system:last-operator" || !resp.GetAuditReady() {
			t.Fatalf("resp = %+v", resp)
		}
	})
	t.Run("other callers are refused", func(t *testing.T) {
		otherSA := kubectl(t, "create", "token", "default", "-n", "last-system", "--audience", "last-gateway", "--duration", "10m")
		wrongAudience := kubectl(t, "create", "token", "last-operator", "-n", "last-system", "--duration", "10m")
		cases := map[string]codes.Code{
			"":            codes.Unauthenticated,
			"not-a-token": codes.Unauthenticated,
			wrongAudience: codes.Unauthenticated,
			otherSA:       codes.PermissionDenied,
		}
		for token, want := range cases {
			if _, err := ping(token); status.Code(err) != want {
				t.Errorf("token %.12q…: code %v, want %v (%v)", token, status.Code(err), want, err)
			}
		}
	})
}

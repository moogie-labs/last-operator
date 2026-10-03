package gateway

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func pingClient(t *testing.T) gatewayv1.GatewayServiceClient {
	t.Helper()
	calls := 0
	g := testGateway(t, audittest.NewMemSink())
	srv := g.GRPCServer(NewAuthenticator(reviewingClient(&calls), GRPCAudience, operatorID))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gatewayv1.NewGatewayServiceClient(conn)
}

func TestPingReturnsTheAuthenticatedCaller(t *testing.T) {
	c := pingClient(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer good")
	resp, err := c.Ping(ctx, &gatewayv1.PingRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCaller() != operatorID || resp.GetChainId() != testChainID || resp.GetVersion() != Version || resp.GetAuditReady() {
		t.Fatalf("resp = %+v (audit_ready must be false: no writer yet)", resp)
	}
}

func TestPingRequiresABearerToken(t *testing.T) {
	c := pingClient(t)
	for _, md := range [][]string{nil, {"authorization", "good"}, {"authorization", "Basic good"}} {
		ctx := context.Background()
		if md != nil {
			ctx = metadata.AppendToOutgoingContext(ctx, md...)
		}
		if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("metadata %v: code %v", md, codeOf(err))
		}
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer other")
	if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.PermissionDenied {
		t.Fatalf("other SA: code %v", codeOf(err))
	}
}

func TestPingBearerSchemeIsCaseInsensitive(t *testing.T) {
	c := pingClient(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "bearer good")
	if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestPingRejectsMultipleAuthorizationValues(t *testing.T) {
	c := pingClient(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer good", "authorization", "Bearer good")
	if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.Unauthenticated {
		t.Fatalf("code %v", codeOf(err))
	}
}

func TestPingFailsClosedWhenTokenReviewErrors(t *testing.T) {
	c := pingClient(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer boom")
	if _, err := c.Ping(ctx, &gatewayv1.PingRequest{}); codeOf(err) != codes.Unavailable {
		t.Fatalf("code %v", codeOf(err))
	}
}

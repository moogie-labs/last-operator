package gatewayclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// recorder is a gateway that remembers the authorization header of each call.
type recorder struct {
	gatewayv1.UnimplementedGatewayServiceServer
	mu     sync.Mutex
	tokens []string
}

func (r *recorder) Ping(ctx context.Context, _ *gatewayv1.PingRequest) (*gatewayv1.PingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, strings.Join(md.Get("authorization"), ","))
	return &gatewayv1.PingResponse{}, nil
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

func dialBuf(t *testing.T, tokenPath string) (gatewayv1.GatewayServiceClient, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	gatewayv1.RegisterGatewayServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", append(dialOptions(tokenPath),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gatewayv1.NewGatewayServiceClient(conn), rec
}

func ping(c gatewayv1.GatewayServiceClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Ping(ctx, &gatewayv1.PingRequest{})
	return err
}

func TestTokenIsReadOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	c, rec := dialBuf(t, path)
	for _, tok := range []string{"tok-1\n", "tok-2"} {
		if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ping(c); err != nil {
			t.Fatal(err)
		}
	}
	got := rec.seen()
	if len(got) != 2 || got[0] != "Bearer tok-1" || got[1] != "Bearer tok-2" {
		t.Fatalf("authorization headers = %q, want the rotated token on each call", got)
	}
}

func TestMissingOrEmptyTokenFailsTheCall(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "nope"), "empty": empty} {
		c, rec := dialBuf(t, path)
		if err := ping(c); err == nil {
			t.Errorf("%s token: Ping succeeded", name)
		}
		if n := len(rec.seen()); n != 0 {
			t.Errorf("%s token: the gateway saw %d calls", name, n)
		}
	}
}

// Package gatewayclient connects the operator to the gateway (spec §4.2).
package gatewayclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// TokenFile authenticates every call with the projected ServiceAccount token at
// Path (audience last-gateway). The file is read on every call because the
// kubelet rotates it.
type TokenFile struct{ Path string }

func (t TokenFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: read token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return nil, errors.New("gatewayclient: token file is empty")
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity is false: v1 has no TLS on the gRPC hop, and the
// token check is the authentication boundary (spec §4.2).
func (TokenFile) RequireTransportSecurity() bool { return false }

func dialOptions(tokenPath string) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(TokenFile{Path: tokenPath}),
		// The gateway's keepalive policy rejects pings more often than every 30s.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: time.Minute, Timeout: 20 * time.Second}),
	}
}

// Dial returns a connection to the gateway at addr ("host:port"). It does not
// block; calls fail with Unavailable while the gateway is unreachable.
func Dial(addr, tokenPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, dialOptions(tokenPath)...)
}

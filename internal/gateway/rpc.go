package gateway

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

type rpcServer struct {
	gatewayv1.UnimplementedGatewayServiceServer
	g *Gateway
}

func (s *rpcServer) Ping(ctx context.Context, _ *gatewayv1.PingRequest) (*gatewayv1.PingResponse, error) {
	w := s.g.writer.Load()
	return &gatewayv1.PingResponse{
		Caller: CallerFrom(ctx), Version: Version, ChainId: s.g.cfg.ChainID, AuditReady: w != nil && !w.Broken(),
	}, nil
}

// GRPCServer builds the gateway's gRPC server; every call is authenticated.
func (g *Gateway) GRPCServer(auth *Authenticator) *grpc.Server {
	if auth == nil {
		panic("gateway: GRPCServer requires an Authenticator")
	}
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
		grpc.MaxRecvMsgSize(1<<20),
		grpc.MaxConcurrentStreams(100),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second}),
		grpc.ConnectionTimeout(10*time.Second),
	)
	gatewayv1.RegisterGatewayServiceServer(srv, &rpcServer{g: g})
	return srv
}

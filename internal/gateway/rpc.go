package gateway

import (
	"context"

	"google.golang.org/grpc"

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
	srv := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryInterceptor()))
	gatewayv1.RegisterGatewayServiceServer(srv, &rpcServer{g: g})
	return srv
}

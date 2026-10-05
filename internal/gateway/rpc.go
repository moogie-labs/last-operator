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

func (s *rpcServer) BeginPhase(ctx context.Context, req *gatewayv1.BeginPhaseRequest) (*gatewayv1.BeginPhaseResponse, error) {
	return s.g.beginPhase(ctx, req)
}

func (s *rpcServer) EndPhase(ctx context.Context, req *gatewayv1.EndPhaseRequest) (*gatewayv1.EndPhaseResponse, error) {
	return s.g.endPhase(ctx, req)
}

func (s *rpcServer) RunCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	return s.g.runCommand(ctx, req)
}

func (s *rpcServer) RunProbes(ctx context.Context, req *gatewayv1.RunProbesRequest) (*gatewayv1.RunProbesResponse, error) {
	return s.g.runProbes(ctx, req)
}

func (s *rpcServer) WriteOutputSecret(ctx context.Context, req *gatewayv1.WriteOutputSecretRequest) (*gatewayv1.WriteOutputSecretResponse, error) {
	return s.g.writeOutputSecret(ctx, req)
}

func (s *rpcServer) CallLLM(ctx context.Context, req *gatewayv1.CallLLMRequest) (*gatewayv1.CallLLMResponse, error) {
	return s.g.callLLM(ctx, req)
}

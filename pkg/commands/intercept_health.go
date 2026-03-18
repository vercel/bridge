package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/vercel/bridge/pkg/grpcutil"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// interceptServer exposes a gRPC server for the intercept process with a
// health check endpoint that reports whether the interceptor is initialized.
type interceptServer struct {
	healthpb.UnimplementedHealthServer

	server *grpc.Server
	ready  bool
}

func newInterceptServer(addr string) (*interceptServer, error) {
	s := &interceptServer{}
	s.server = grpcutil.NewServer()
	healthpb.RegisterHealthServer(s.server, s)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	slog.Info("Intercept server listening", "addr", lis.Addr().String())

	go s.server.Serve(lis)
	return s, nil
}

// SetReady marks the intercept as ready.
func (s *interceptServer) SetReady() {
	s.ready = true
}

// Stop gracefully stops the gRPC server.
func (s *interceptServer) Stop() {
	s.server.GracefulStop()
}

// Check implements the gRPC health check by reporting the interceptor's
// own readiness state.
func (s *interceptServer) Check(_ context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if !s.ready {
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_NOT_SERVING,
		}, nil
	}

	return &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_SERVING,
	}, nil
}

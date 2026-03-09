package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// interceptServer exposes a gRPC server for the intercept process with a
// health check endpoint that verifies connectivity to the bridge proxy server.
type interceptServer struct {
	healthpb.UnimplementedHealthServer

	server    *grpc.Server
	ready     bool
	proxyConn *grpc.ClientConn
}

func newInterceptServer(addr string, proxyConn *grpc.ClientConn) (*interceptServer, error) {
	s := &interceptServer{
		proxyConn: proxyConn,
	}
	s.server = grpc.NewServer()
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

// Check implements the gRPC health check by verifying connectivity to
// the bridge proxy server.
func (s *interceptServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if !s.ready {
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_NOT_SERVING,
		}, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := healthpb.NewHealthClient(s.proxyConn)
	resp, err := client.Check(checkCtx, &healthpb.HealthCheckRequest{})
	if err != nil {
		msg := fmt.Sprintf("proxy server: %v", err)
		slog.Warn("Health check failed", "error", msg)
		return nil, status.Errorf(codes.Unavailable, msg)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		msg := fmt.Sprintf("proxy server: status %s", resp.GetStatus())
		slog.Warn("Health check failed", "error", msg)
		return nil, status.Errorf(codes.Unavailable, msg)
	}

	return &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_SERVING,
	}, nil
}

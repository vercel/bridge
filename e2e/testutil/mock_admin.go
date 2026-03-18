package testutil

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/mocks/bridgev1mocks"
)

// mockAdminServer wraps a mockery-generated mock so it satisfies the gRPC
// AdministratorServiceServer interface (which requires the Unimplemented embed).
type mockAdminServer struct {
	bridgev1.UnimplementedAdministratorServiceServer
	mock *bridgev1mocks.AdministratorServiceServer
}

func (s *mockAdminServer) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	return s.mock.CreateBridge(ctx, req)
}

func (s *mockAdminServer) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	return s.mock.ListBridges(ctx, req)
}

func (s *mockAdminServer) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	return s.mock.DeleteBridge(ctx, req)
}

// MockAdminGRPCServer is a real gRPC server backed by a mockery mock.
type MockAdminGRPCServer struct {
	Mock   *bridgev1mocks.AdministratorServiceServer
	server *grpc.Server
	Addr   string
}

// StartMockAdminServer starts a gRPC server with a mockery-generated
// AdministratorServiceServer on a random local port.
func StartMockAdminServer(m *bridgev1mocks.AdministratorServiceServer) (*MockAdminGRPCServer, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	grpcServer := grpc.NewServer()
	bridgev1.RegisterAdministratorServiceServer(grpcServer, &mockAdminServer{mock: m})

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSrv)

	go grpcServer.Serve(lis)

	return &MockAdminGRPCServer{
		Mock:   m,
		server: grpcServer,
		Addr:   lis.Addr().String(),
	}, nil
}

// Stop gracefully shuts down the gRPC server.
func (s *MockAdminGRPCServer) Stop() {
	s.server.GracefulStop()
}

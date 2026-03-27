package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// ForwarderGRPCServer serves BridgeProxyService over gRPC, forwarding all
// calls to an upstream via the Forwarder.
type ForwarderGRPCServer struct {
	bridgev1.UnimplementedBridgeProxyServiceServer
	forwarder *Forwarder
	server    *grpc.Server
	addr      string
}

// NewForwarderGRPCServer creates a gRPC server backed by a Forwarder.
func NewForwarderGRPCServer(addr string, fwd *Forwarder) *ForwarderGRPCServer {
	s := &ForwarderGRPCServer{
		forwarder: fwd,
		addr:      addr,
	}
	s.server = grpcutil.NewServer()
	bridgev1.RegisterBridgeProxyServiceServer(s.server, s)
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(s.server, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return s
}

func (s *ForwarderGRPCServer) Start() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	slog.Info("Server proxy (gRPC) listening", "addr", s.addr)
	return s.server.Serve(lis)
}

func (s *ForwarderGRPCServer) Shutdown(_ context.Context) {
	slog.Info("shutting down server proxy (gRPC)")
	s.server.GracefulStop()
	s.forwarder.Close()
}

func (s *ForwarderGRPCServer) ResolveDNSQuery(ctx context.Context, req *bridgev1.ProxyResolveDNSRequest) (*bridgev1.ProxyResolveDNSResponse, error) {
	resp, err := s.forwarder.ResolveDNSQuery(ctx, req)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return resp, nil
}

func (s *ForwarderGRPCServer) GetMetadata(ctx context.Context, req *bridgev1.GetMetadataRequest) (*bridgev1.GetMetadataResponse, error) {
	return s.forwarder.GetMetadata(ctx, req)
}

func (s *ForwarderGRPCServer) CopyFiles(ctx context.Context, req *bridgev1.CopyFilesRequest) (*bridgev1.CopyFilesResponse, error) {
	return s.forwarder.CopyFiles(ctx, req)
}

func (s *ForwarderGRPCServer) TunnelNetwork(stream grpc.BidiStreamingServer[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	s.forwarder.HandleTunnelStream(stream.Context(), tunnel.NewStaticStream(stream))
	return nil
}

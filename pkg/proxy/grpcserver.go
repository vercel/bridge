package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/tunnel"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// egressDialTimeout caps how long the bridge server waits when dialing an
// egress destination on behalf of the tunnel client.
const egressDialTimeout = 10 * time.Second

// ListenPort describes a port the proxy should listen on for ingress traffic.
type ListenPort struct {
	Port     int
	Protocol string // "tcp" or "udp"
}

// ParseListenPort parses a string like "8080/tcp", "9090/udp", or "8080" (defaults to tcp).
func ParseListenPort(s string) (ListenPort, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "/", 2)
	port, err := strconv.Atoi(parts[0])
	if err != nil {
		return ListenPort{}, fmt.Errorf("invalid port %q: %w", parts[0], err)
	}
	proto := "tcp"
	if len(parts) == 2 {
		proto = strings.ToLower(parts[1])
		if proto != "tcp" && proto != "udp" {
			return ListenPort{}, fmt.Errorf("unsupported protocol %q (use tcp or udp)", proto)
		}
	}
	return ListenPort{Port: port, Protocol: proto}, nil
}

// systemEnvPrefixes lists env var prefixes that are filtered out of GetMetadata
// responses because they are injected by the container runtime, not the app config.
var systemEnvPrefixes = []string{"BRIDGE_"}

// systemEnvVars lists exact env var names filtered out of GetMetadata responses.
var systemEnvVars = map[string]bool{
	"PATH": true, "HOME": true, "HOSTNAME": true, "TERM": true,
	"SHELL": true, "USER": true, "PWD": true, "SHLVL": true,
	"LANG": true, "GODEBUG": true,
}

// isSystemEnvVar returns true if the key is a system/runtime env var that
// should not be forwarded to the devcontainer.
func isSystemEnvVar(key string) bool {
	if systemEnvVars[key] {
		return true
	}
	for _, prefix := range systemEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Well-known paths where the administrator mounts the CA Secret into the proxy pod.
const (
	caCertPath = "/etc/bridge/tls/ca.crt"
	caKeyPath  = "/etc/bridge/tls/ca.key"
)

// readFileCopy reads a single file and returns a FileCopy proto message.
func readFileCopy(path string, info os.FileInfo) *bridgev1.FileCopy {
	fc := &bridgev1.FileCopy{
		Path:    path,
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime().Unix(),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fc.Error = err.Error()
		return fc
	}
	fc.Content = data
	return fc
}

// GRPCServer implements the BridgeProxyService gRPC server.
type GRPCServer struct {
	bridgev1.UnimplementedBridgeProxyServiceServer
	*Service
	server *grpc.Server
	addr   string
}

// NewGRPCServer creates a new gRPC proxy server.
func NewGRPCServer(addr string, listenPorts []ListenPort, facades []*bridgev1.ServerFacade) *GRPCServer {
	s := &GRPCServer{
		Service: NewService(listenPorts, facades),
		addr:    addr,
	}

	s.server = grpcutil.NewServer()
	bridgev1.RegisterBridgeProxyServiceServer(s.server, s)
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(s.server, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return s
}

// Start starts the gRPC server and ingress listeners.
func (s *GRPCServer) Start() error {
	s.Service.StartIngressListeners()

	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	slog.Info("gRPC proxy server listening", "addr", s.addr)
	return s.server.Serve(lis)
}

// Shutdown gracefully stops the gRPC server.
func (s *GRPCServer) Shutdown(_ context.Context) {
	slog.Info("shutting down gRPC proxy server")
	s.server.GracefulStop()
}

// ResolveDNSQuery delegates to the shared service, converting errors to gRPC status.
func (s *GRPCServer) ResolveDNSQuery(ctx context.Context, req *bridgev1.ProxyResolveDNSRequest) (*bridgev1.ProxyResolveDNSResponse, error) {
	resp, err := s.Service.ResolveDNSQuery(ctx, req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return resp, nil
}

// GetMetadata delegates to the shared service.
func (s *GRPCServer) GetMetadata(ctx context.Context, req *bridgev1.GetMetadataRequest) (*bridgev1.GetMetadataResponse, error) {
	return s.Service.GetMetadata(ctx, req)
}

// CopyFiles delegates to the shared service.
func (s *GRPCServer) CopyFiles(ctx context.Context, req *bridgev1.CopyFilesRequest) (*bridgev1.CopyFilesResponse, error) {
	return s.Service.CopyFiles(ctx, req)
}

// TunnelNetwork handles the single bidirectional tunnel stream.
func (s *GRPCServer) TunnelNetwork(stream grpc.BidiStreamingServer[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	s.Service.HandleTunnelStream(stream.Context(), tunnel.NewStaticStream(stream))
	return nil
}

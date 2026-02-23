package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/vercel/bridge/pkg/plumbing"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

// GRPCServer implements the BridgeProxyService gRPC server.
type GRPCServer struct {
	bridgev1.UnimplementedBridgeProxyServiceServer
	server      *grpc.Server
	addr        string
	listenPorts []ListenPort

	tunnelMu sync.Mutex
	tunnel   plumbing.Tunnel
}

// NewGRPCServer creates a new gRPC proxy server.
func NewGRPCServer(addr string, listenPorts []ListenPort) *GRPCServer {
	s := &GRPCServer{
		addr:        addr,
		listenPorts: listenPorts,
	}
	s.server = grpc.NewServer()
	bridgev1.RegisterBridgeProxyServiceServer(s.server, s)
	return s
}

// Start starts the gRPC server and ingress listeners.
func (s *GRPCServer) Start() error {
	s.startIngressListeners()

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

// ResolveDNSQuery resolves a hostname using the system DNS resolver.
func (s *GRPCServer) ResolveDNSQuery(ctx context.Context, req *bridgev1.ProxyResolveDNSRequest) (*bridgev1.ProxyResolveDNSResponse, error) {
	hostname := req.GetHostname()
	if hostname == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}

	slog.Debug("resolving DNS query", "hostname", hostname)

	addrs, err := net.DefaultResolver.LookupHost(ctx, hostname)
	if err != nil {
		slog.Debug("DNS resolution failed", "hostname", hostname, "error", err)
		return &bridgev1.ProxyResolveDNSResponse{
			Error: err.Error(),
		}, nil
	}

	var ipv4 []string
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
			ipv4 = append(ipv4, addr)
		}
	}

	slog.Debug("DNS resolved", "hostname", hostname, "addresses", ipv4)
	return &bridgev1.ProxyResolveDNSResponse{
		Addresses: ipv4,
	}, nil
}

// TunnelNetwork handles the single bidirectional tunnel stream. All egress and
// ingress traffic is multiplexed over this stream using connection IDs.
func (s *GRPCServer) TunnelNetwork(stream grpc.BidiStreamingServer[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	slog.Info("Tunnel connected")

	tun := plumbing.NewTunnel(&net.Dialer{}, stream)

	s.tunnelMu.Lock()
	s.tunnel = tun
	s.tunnelMu.Unlock()

	tun.Start(stream.Context())

	defer func() {
		s.tunnelMu.Lock()
		if s.tunnel == tun {
			s.tunnel = nil
		}
		s.tunnelMu.Unlock()
	}()

	<-tun.Done()
	slog.Info("Tunnel stream closed")
	return nil
}

// startIngressListeners opens a TCP listener for each configured listen port.
func (s *GRPCServer) startIngressListeners() {
	for _, lp := range s.listenPorts {
		if lp.Protocol != "tcp" {
			slog.Warn("Ingress UDP listeners not yet supported, skipping", "port", lp.Port)
			continue
		}
		addr := fmt.Sprintf(":%d", lp.Port)
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("Failed to start ingress listener", "addr", addr, "error", err)
			continue
		}
		slog.Info("Ingress listener started", "addr", addr)
		go s.acceptIngressConns(lis)
	}
}

func (s *GRPCServer) acceptIngressConns(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			slog.Debug("Ingress accept error", "error", err)
			return
		}

		s.tunnelMu.Lock()
		tun := s.tunnel
		s.tunnelMu.Unlock()

		if tun == nil {
			slog.Debug("Ingress connection dropped, no tunnel connected", "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}

		tun.AddConn(conn, "")
	}
}

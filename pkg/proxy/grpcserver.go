package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/mitm"
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

// GRPCServer implements the BridgeProxyService gRPC server.
type GRPCServer struct {
	bridgev1.UnimplementedBridgeProxyServiceServer
	server      *grpc.Server
	addr        string
	listenPorts []ListenPort

	tunnelMu sync.Mutex
	tunnel   tunnel.Tunnel

	// CA cert and key for TLS mock interception, read once at startup.
	caCert []byte
	caKey  []byte

	hijacker mitm.Hijacker
}

// NewGRPCServer creates a new gRPC proxy server.
func NewGRPCServer(addr string, listenPorts []ListenPort, facades []*bridgev1.ServerFacade) *GRPCServer {
	s := &GRPCServer{
		addr:        addr,
		listenPorts: listenPorts,
	}

	// Read the CA cert and key once at startup.
	if cert, err := os.ReadFile(caCertPath); err == nil {
		s.caCert = cert
		slog.Info("Loaded CA certificate", "path", caCertPath)
	}
	if key, err := os.ReadFile(caKeyPath); err == nil {
		s.caKey = key
		slog.Info("Loaded CA key", "path", caKeyPath)
	}

	// Create facade hijacker if specs are provided.
	if len(facades) > 0 {
		h, err := mitm.NewFacadeHijacker(facades, s.caCert, s.caKey)
		if err != nil {
			slog.Error("Failed to compile server facade specs", "error", err)
		} else {
			s.hijacker = h
			slog.Info("Loaded server facade specs", "count", len(facades))
		}
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

// GetMetadata returns metadata about the bridge proxy, including environment
// variables that were set on the pod by the administrator.
func (s *GRPCServer) GetMetadata(_ context.Context, _ *bridgev1.GetMetadataRequest) (*bridgev1.GetMetadataResponse, error) {
	envVars := make(map[string]string)
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		if !isSystemEnvVar(k) {
			envVars[k] = v
		}
	}

	return &bridgev1.GetMetadataResponse{
		EnvVars: envVars,
		CaCert:  s.caCert,
		CaKey:   s.caKey,
	}, nil
}

// CopyFiles reads the requested files and directories from the local filesystem
// and returns their contents along with metadata (permissions, modification time).
// Directories are walked recursively.
func (s *GRPCServer) CopyFiles(_ context.Context, req *bridgev1.CopyFilesRequest) (*bridgev1.CopyFilesResponse, error) {
	var files []*bridgev1.FileCopy
	for _, p := range req.GetPaths() {
		info, err := os.Stat(p)
		if err != nil {
			files = append(files, &bridgev1.FileCopy{Path: p, Error: err.Error()})
			continue
		}

		if !info.IsDir() {
			files = append(files, readFileCopy(p, info))
			continue
		}

		// Walk directory recursively, only emitting regular files.
		err = filepath.Walk(p, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				files = append(files, &bridgev1.FileCopy{Path: path, Error: walkErr.Error()})
				return nil
			}
			if fi.IsDir() {
				return nil
			}
			files = append(files, readFileCopy(path, fi))
			return nil
		})
		if err != nil {
			files = append(files, &bridgev1.FileCopy{Path: p, Error: err.Error()})
		}
	}
	return &bridgev1.CopyFilesResponse{Files: files}, nil
}

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

// TunnelNetwork handles the single bidirectional tunnel stream. All egress and
// ingress traffic is multiplexed over this stream using connection IDs.
func (s *GRPCServer) TunnelNetwork(stream grpc.BidiStreamingServer[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	slog.Info("Tunnel connected")

	var opts []tunnel.Option
	if s.hijacker != nil {
		opts = append(opts, tunnel.WithHijacker(s.hijacker))
	}
	tun := tunnel.New(&net.Dialer{Timeout: egressDialTimeout}, stream, opts...)

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

		tun.AddConn(conn, "", "")
	}
}

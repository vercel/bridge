package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/mitm"
	"github.com/vercel/bridge/pkg/tunnel"
)

// Service contains the protocol-agnostic business logic shared by both the
// gRPC and HTTP bridge proxy servers.
type Service struct {
	listenPorts []ListenPort

	tunnelMu sync.Mutex
	tunnel   tunnel.Tunnel

	caCert   []byte
	caKey    []byte
	hijacker mitm.Hijacker
}

// NewService creates a new Service, loading CA certificates and compiling
// server facade specs.
func NewService(listenPorts []ListenPort, facades []*bridgev1.ServerFacade) *Service {
	s := &Service{
		listenPorts: listenPorts,
	}

	if cert, err := os.ReadFile(caCertPath); err == nil {
		s.caCert = cert
		slog.Info("Loaded CA certificate", "path", caCertPath)
	}
	if key, err := os.ReadFile(caKeyPath); err == nil {
		s.caKey = key
		slog.Info("Loaded CA key", "path", caKeyPath)
	}

	if len(facades) > 0 {
		h, err := mitm.NewFacadeHijacker(facades, s.caCert, s.caKey)
		if err != nil {
			slog.Error("Failed to compile server facade specs", "error", err)
		} else {
			s.hijacker = h
			slog.Info("Loaded server facade specs", "count", len(facades))
		}
	}

	return s
}

// TunnelOpts returns tunnel options for the current service configuration.
func (s *Service) TunnelOpts() []tunnel.Option {
	var opts []tunnel.Option
	if s.hijacker != nil {
		opts = append(opts, tunnel.WithHijacker(s.hijacker))
	}
	return opts
}

// HandleTunnelStream manages a bidirectional tunnel stream, setting it as the
// active tunnel and blocking until it closes. Works with both gRPC and
// WebSocket streams since both satisfy tunnel.Stream.
func (s *Service) HandleTunnelStream(ctx context.Context, stream tunnel.Stream) {
	slog.Info("Tunnel connected")

	tun := tunnel.New(&net.Dialer{Timeout: egressDialTimeout}, stream, s.TunnelOpts()...)

	s.tunnelMu.Lock()
	s.tunnel = tun
	s.tunnelMu.Unlock()

	tun.Start(ctx)

	defer func() {
		s.tunnelMu.Lock()
		if s.tunnel == tun {
			s.tunnel = nil
		}
		s.tunnelMu.Unlock()
	}()

	<-tun.Done()
	slog.Info("Tunnel stream closed")
}

// ResolveDNSQuery resolves a hostname using the system DNS resolver.
func (s *Service) ResolveDNSQuery(ctx context.Context, req *bridgev1.ProxyResolveDNSRequest) (*bridgev1.ProxyResolveDNSResponse, error) {
	hostname := req.GetHostname()
	if hostname == "" {
		return nil, fmt.Errorf("hostname is required")
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

// GetMetadata returns environment variables and CA certificate/key.
func (s *Service) GetMetadata(_ context.Context, _ *bridgev1.GetMetadataRequest) (*bridgev1.GetMetadataResponse, error) {
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

// CopyFiles reads files and directories from the local filesystem.
func (s *Service) CopyFiles(_ context.Context, req *bridgev1.CopyFilesRequest) (*bridgev1.CopyFilesResponse, error) {
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

// StartIngressListeners opens a TCP listener for each configured listen port.
func (s *Service) StartIngressListeners() {
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
		go s.AcceptIngressConns(lis)
	}
}

// AcceptIngressConns accepts connections on a listener and adds them to the
// active tunnel.
func (s *Service) AcceptIngressConns(lis net.Listener) {
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

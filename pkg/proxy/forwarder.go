package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	bridgedns "github.com/vercel/bridge/pkg/dns"
	"github.com/vercel/bridge/pkg/tunnel"
)

// Forwarder implements the BridgeProxyService by forwarding all calls to an
// upstream bridge server. It is used by the server-proxy command to sit
// between the interceptor and the real bridge server.
type Forwarder struct {
	upstreamAddr     string
	upstreamProtocol string // "grpc" or "http"

	mu       sync.Mutex
	upstream ProxyClient
}

// NewForwarder creates a forwarder that connects to the given upstream.
func NewForwarder(upstreamAddr, upstreamProtocol string) (*Forwarder, error) {
	f := &Forwarder{
		upstreamAddr:     upstreamAddr,
		upstreamProtocol: upstreamProtocol,
	}
	if err := f.connect(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Forwarder) connect() error {
	var client ProxyClient
	var err error
	switch f.upstreamProtocol {
	case "grpc":
		client, err = NewGRPCProxyClient(f.upstreamAddr)
	case "http":
		client = NewHTTPProxyClient(f.upstreamAddr)
	default:
		return fmt.Errorf("unsupported upstream protocol %q", f.upstreamProtocol)
	}
	if err != nil {
		return fmt.Errorf("connect to upstream: %w", err)
	}

	f.mu.Lock()
	if f.upstream != nil {
		f.upstream.Close()
	}
	f.upstream = client
	f.mu.Unlock()
	return nil
}

func (f *Forwarder) client() ProxyClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upstream
}

// Close closes the upstream connection.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upstream != nil {
		return f.upstream.Close()
	}
	return nil
}

// ResolveDNSQuery forwards to the upstream.
func (f *Forwarder) ResolveDNSQuery(ctx context.Context, req *bridgev1.ProxyResolveDNSRequest) (*bridgev1.ProxyResolveDNSResponse, error) {
	result, err := f.client().ResolveDNS(ctx, req.GetHostname())
	if err != nil {
		return nil, err
	}
	return &bridgev1.ProxyResolveDNSResponse{
		Addresses: result.Addresses,
		Error:     result.Error,
	}, nil
}

// GetMetadata forwards to the upstream.
func (f *Forwarder) GetMetadata(ctx context.Context, req *bridgev1.GetMetadataRequest) (*bridgev1.GetMetadataResponse, error) {
	return f.client().GetMetadata(ctx)
}

// CopyFiles forwards to the upstream.
func (f *Forwarder) CopyFiles(ctx context.Context, req *bridgev1.CopyFilesRequest) (*bridgev1.CopyFilesResponse, error) {
	return f.client().CopyFiles(ctx, req.GetPaths())
}

// ResolveDNS implements the dns resolver interface for direct use.
func (f *Forwarder) ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error) {
	return f.client().ResolveDNS(ctx, hostname)
}

// HandleTunnelStream bridges a client tunnel stream to the upstream tunnel.
// If the upstream stream fails, it reconnects and resumes forwarding.
func (f *Forwarder) HandleTunnelStream(ctx context.Context, clientStream tunnel.Stream) {
	for {
		upstreamStream, err := f.client().OpenTunnel(ctx)
		if err != nil {
			slog.Error("Failed to open upstream tunnel", "error", err)
			// Reconnect the client and retry.
			if reconnErr := f.connect(); reconnErr != nil {
				slog.Error("Failed to reconnect upstream", "error", reconnErr)
				return
			}
			continue
		}

		slog.Info("Upstream tunnel connected, bridging streams")
		err = bridgeStreams(ctx, clientStream, upstreamStream)
		if err == nil || ctx.Err() != nil {
			return
		}

		slog.Warn("Upstream tunnel stream failed, reconnecting", "error", err)
		if reconnErr := f.connect(); reconnErr != nil {
			slog.Error("Failed to reconnect upstream", "error", reconnErr)
			return
		}
	}
}

// bridgeStreams forwards messages bidirectionally between two tunnel streams.
// Returns when either stream errors or the context is cancelled.
func bridgeStreams(ctx context.Context, a, b tunnel.Stream) error {
	errCh := make(chan error, 2)

	// a → b
	go func() {
		for {
			msg, err := a.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("recv from client: %w", err)
				}
				return
			}
			if err := b.Send(msg); err != nil {
				errCh <- fmt.Errorf("send to upstream: %w", err)
				return
			}
		}
	}()

	// b → a
	go func() {
		for {
			msg, err := b.Recv()
			if err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- fmt.Errorf("recv from upstream: %w", err)
				}
				return
			}
			if err := a.Send(msg); err != nil {
				errCh <- fmt.Errorf("send to client: %w", err)
				return
			}
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return nil
	}
}

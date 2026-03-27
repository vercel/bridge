package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	bridgedns "github.com/vercel/bridge/pkg/dns"
	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc"
)

// ProxyClient abstracts the bridge proxy operations needed by the intercept command.
type ProxyClient interface {
	GetMetadata(ctx context.Context) (*bridgev1.GetMetadataResponse, error)
	CopyFiles(ctx context.Context, paths []string) (*bridgev1.CopyFilesResponse, error)
	ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error)
	OpenTunnel(ctx context.Context) (tunnel.Stream, error)
	Close() error
}

// grpcProxyClient implements ProxyClient over gRPC.
type grpcProxyClient struct {
	conn   *grpc.ClientConn
	client bridgev1.BridgeProxyServiceClient
}

// NewGRPCProxyClient creates a ProxyClient backed by a gRPC connection.
func NewGRPCProxyClient(serverAddr string, dialOpts ...grpc.DialOption) (ProxyClient, error) {
	conn, err := grpcutil.NewClient(serverAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}
	return &grpcProxyClient{
		conn:   conn,
		client: bridgev1.NewBridgeProxyServiceClient(conn),
	}, nil
}

func (c *grpcProxyClient) GetMetadata(ctx context.Context) (*bridgev1.GetMetadataResponse, error) {
	return c.client.GetMetadata(ctx, &bridgev1.GetMetadataRequest{})
}

func (c *grpcProxyClient) CopyFiles(ctx context.Context, paths []string) (*bridgev1.CopyFilesResponse, error) {
	return c.client.CopyFiles(ctx, &bridgev1.CopyFilesRequest{Paths: paths})
}

func (c *grpcProxyClient) ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error) {
	resp, err := c.client.ResolveDNSQuery(ctx, &bridgev1.ProxyResolveDNSRequest{Hostname: hostname})
	if err != nil {
		return nil, fmt.Errorf("ResolveDNSQuery RPC: %w", err)
	}
	return &bridgedns.DNSResolveResult{
		Addresses: resp.GetAddresses(),
		Error:     resp.GetError(),
	}, nil
}

func (c *grpcProxyClient) OpenTunnel(ctx context.Context) (tunnel.Stream, error) {
	stream, err := c.client.TunnelNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("TunnelNetwork RPC: %w", err)
	}
	return tunnel.NewStaticStream(stream), nil
}

func (c *grpcProxyClient) Close() error {
	return c.conn.Close()
}

// httpProxyClient implements ProxyClient over the dispatcher HTTP control
// endpoints plus the existing WebSocket tunnel path.
type httpProxyClient struct {
	baseURL      string
	client       *http.Client
	tunnelConnCh <-chan *websocket.Conn
}

// NewHTTPProxyClient creates a ProxyClient backed by dispatcher HTTP control
// endpoints and an inbound WebSocket tunnel stream.
func NewHTTPProxyClient(baseURL string, tunnelConnCh <-chan *websocket.Conn) ProxyClient {
	return &httpProxyClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       http.DefaultClient,
		tunnelConnCh: tunnelConnCh,
	}
}

func (c *httpProxyClient) GetMetadata(ctx context.Context) (*bridgev1.GetMetadataResponse, error) {
	return &bridgev1.GetMetadataResponse{}, nil
}

func (c *httpProxyClient) CopyFiles(ctx context.Context, paths []string) (*bridgev1.CopyFilesResponse, error) {
	return &bridgev1.CopyFilesResponse{}, nil
}

func (c *httpProxyClient) ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error) {
	return &bridgedns.DNSResolveResult{
		Addresses: nil,
		Error:     "",
	}, nil
}

func (c *httpProxyClient) OpenTunnel(ctx context.Context) (tunnel.Stream, error) {
	connectCtx, cancel := context.WithTimeout(ctx, dispatcherConnectTimeout)
	defer cancel()

	stream := tunnel.NewWakeableWSStream(c.tunnelConnCh, c.wakeTunnel)
	if err := stream.Refresh(connectCtx); err != nil {
		return nil, err
	}
	return stream, nil
}

func (c *httpProxyClient) Close() error {
	return nil
}

const (
	dispatcherConnectTimeout = 10 * time.Second
)

func (c *httpProxyClient) wakeTunnel(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/__tunnel/connect", nil)
	if err != nil {
		return fmt.Errorf("build wake request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("wake dispatcher tunnel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("wake dispatcher tunnel: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

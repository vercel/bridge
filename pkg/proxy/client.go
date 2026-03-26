package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/gorilla/websocket"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/api/go/bridge/v1/bridgev1connect"
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
	return stream, nil
}

func (c *grpcProxyClient) Close() error {
	return c.conn.Close()
}

// httpProxyClient implements ProxyClient over HTTP (Connect RPC + WebSocket).
type httpProxyClient struct {
	baseURL string
	client  bridgev1connect.BridgeProxyServiceClient
}

// NewHTTPProxyClient creates a ProxyClient backed by Connect RPC for unary
// calls and WebSocket for the tunnel stream.
func NewHTTPProxyClient(baseURL string) ProxyClient {
	return &httpProxyClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  bridgev1connect.NewBridgeProxyServiceClient(http.DefaultClient, baseURL),
	}
}

func (c *httpProxyClient) GetMetadata(ctx context.Context) (*bridgev1.GetMetadataResponse, error) {
	resp, err := c.client.GetMetadata(ctx, connect.NewRequest(&bridgev1.GetMetadataRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *httpProxyClient) CopyFiles(ctx context.Context, paths []string) (*bridgev1.CopyFilesResponse, error) {
	resp, err := c.client.CopyFiles(ctx, connect.NewRequest(&bridgev1.CopyFilesRequest{Paths: paths}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *httpProxyClient) ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error) {
	resp, err := c.client.ResolveDNSQuery(ctx, connect.NewRequest(&bridgev1.ProxyResolveDNSRequest{Hostname: hostname}))
	if err != nil {
		return nil, fmt.Errorf("ResolveDNSQuery: %w", err)
	}
	return &bridgedns.DNSResolveResult{
		Addresses: resp.Msg.GetAddresses(),
		Error:     resp.Msg.GetError(),
	}, nil
}

func (c *httpProxyClient) OpenTunnel(ctx context.Context) (tunnel.Stream, error) {
	// Derive WebSocket URL from the HTTP base URL.
	wsURL := c.baseURL + "/bridge.v1.BridgeProxyService/TunnelNetwork"
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	return tunnel.NewWSStream(conn), nil
}

func (c *httpProxyClient) Close() error {
	return nil
}

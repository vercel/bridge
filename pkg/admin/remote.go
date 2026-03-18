package admin

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/k8s/k8spf"
)

// Client implements Service by calling the in-cluster administrator via gRPC.
type Client struct {
	conn    *grpc.ClientConn
	client  bridgev1.AdministratorServiceClient
	builder *k8spf.Builder
}

// NewClient creates a Client that connects to the administrator gRPC
// server at the given address (e.g. "k8spf:///administrator.bridge:9090").
func NewClient(addr string) (*Client, error) {
	builder := k8spf.NewBuilder(k8spf.BuilderConfig{})
	conn, err := grpcutil.NewClient(addr,
		append(builder.DialOptions(),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(16<<20)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to administrator: %w", err)
	}
	return &Client{
		conn:    conn,
		client:  bridgev1.NewAdministratorServiceClient(conn),
		builder: builder,
	}, nil
}

func (r *Client) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	resp, err := r.client.CreateBridge(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

func (r *Client) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	resp, err := r.client.ListBridges(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

func (r *Client) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	resp, err := r.client.DeleteBridge(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

// Conn returns the underlying gRPC client connection.
func (r *Client) Conn() *grpc.ClientConn {
	return r.conn
}

// Close releases the gRPC connection and the underlying port-forward pool.
func (r *Client) Close() error {
	if r.builder != nil {
		r.builder.Close()
	}
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// userError extracts a clean message from a gRPC status error.
func userError(err error) error {
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s", s.Message())
	}
	return err
}

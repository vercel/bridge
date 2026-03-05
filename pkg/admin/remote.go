package admin

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/k8s/k8spf"
)

// remoteAdmin implements Service by calling the in-cluster administrator via gRPC.
type remoteAdmin struct {
	conn    *grpc.ClientConn
	client  bridgev1.AdministratorServiceClient
	builder *k8spf.Builder
}

// NewClient creates a remote Service that connects to the administrator gRPC
// server at the given address (e.g. "k8spf:///administrator.bridge:9090").
func NewClient(addr string) (Service, error) {
	builder := k8spf.NewBuilder(k8spf.BuilderConfig{})
	conn, err := grpc.NewClient(addr,
		append(builder.DialOptions(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(16<<20)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to administrator: %w", err)
	}
	return &remoteAdmin{
		conn:    conn,
		client:  bridgev1.NewAdministratorServiceClient(conn),
		builder: builder,
	}, nil
}

func (r *remoteAdmin) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	resp, err := r.client.CreateBridge(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

func (r *remoteAdmin) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	resp, err := r.client.ListBridges(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

func (r *remoteAdmin) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	resp, err := r.client.DeleteBridge(ctx, req)
	if err != nil {
		return nil, userError(err)
	}
	return resp, nil
}

// Close releases the gRPC connection and the underlying port-forward pool.
func (r *remoteAdmin) Close() error {
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

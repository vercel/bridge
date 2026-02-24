package admin

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/k8s/k8spf"
)

// remoteAdmin implements Admin by calling the in-cluster administrator via gRPC.
type remoteAdmin struct {
	conn   *grpc.ClientConn
	client pb.AdministratorServiceClient
}

// NewRemote creates a remote Admin that connects to the administrator gRPC
// server at the given address (e.g. "k8spf:///administrator.bridge:9090").
func NewRemote(addr string) (Admin, error) {
	builder := k8spf.NewBuilder(k8spf.BuilderConfig{})
	conn, err := grpc.NewClient(addr,
		append(builder.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to administrator: %w", err)
	}
	return &remoteAdmin{
		conn:   conn,
		client: pb.NewAdministratorServiceClient(conn),
	}, nil
}

func (r *remoteAdmin) CreateBridge(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	resp, err := r.client.CreateBridge(ctx, &pb.CreateBridgeRequest{
		DeviceId:         req.DeviceID,
		SourceDeployment: req.SourceDeployment,
		SourceNamespace:  req.SourceNamespace,
		Force:            req.Force,
	})
	if err != nil {
		return nil, userError(err)
	}
	return &CreateResponse{
		Namespace:      resp.Namespace,
		PodName:        resp.PodName,
		Port:           resp.Port,
		DeploymentName: resp.DeploymentName,
		EnvVars:        resp.EnvVars,
	}, nil
}

func (r *remoteAdmin) ListBridges(ctx context.Context, deviceID string) ([]*BridgeInfo, error) {
	resp, err := r.client.ListBridges(ctx, &pb.ListBridgesRequest{
		DeviceId: deviceID,
	})
	if err != nil {
		return nil, userError(err)
	}
	bridges := make([]*BridgeInfo, len(resp.Bridges))
	for i, b := range resp.Bridges {
		bridges[i] = &BridgeInfo{
			DeviceID:         b.DeviceId,
			SourceDeployment: b.SourceDeployment,
			SourceNamespace:  b.SourceNamespace,
			Namespace:        b.Namespace,
			CreatedAt:        b.CreatedAt,
			Status:           b.Status,
		}
	}
	return bridges, nil
}

func (r *remoteAdmin) Close() error {
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

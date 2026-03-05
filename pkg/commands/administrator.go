package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/admin"
	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/resources"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Administrator returns the CLI command for the bridge administrator server.
func Administrator() *cli.Command {
	return &cli.Command{
		Name:   "administrator",
		Usage:  "Start the bridge administrator gRPC server",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Usage:   "Address to bind the gRPC server to",
				Value:   ":9090",
				Sources: cli.EnvVars("BRIDGE_ADMINISTRATOR_ADDR"),
			},
			&cli.StringFlag{
				Name:    "proxy-image",
				Usage:   "Bridge proxy container image",
				Value:   "ghcr.io/vercel/bridge-cli:latest",
				Sources: cli.EnvVars("BRIDGE_PROXY_IMAGE"),
			},
			&cli.StringFlag{
				Name:    "service-account",
				Usage:   "ServiceAccount name for RBAC binding",
				Value:   "administrator",
				Sources: cli.EnvVars("SERVICE_ACCOUNT_NAME"),
			},
			&cli.StringFlag{
				Name:    "namespace",
				Usage:   "Namespace where the administrator is running",
				Value:   "bridge",
				Sources: cli.EnvVars("POD_NAMESPACE"),
			},
			&cli.StringFlag{
				Name:    "kube-context",
				Usage:   "Override the kubectl context (out-of-cluster only)",
				Sources: cli.EnvVars("BRIDGE_KUBE_CONTEXT"),
			},
			&cli.StringFlag{
				Name:    "kube-namespace",
				Usage:   "Override the default kubeconfig namespace (out-of-cluster only)",
				Sources: cli.EnvVars("BRIDGE_KUBE_NAMESPACE"),
			},
		},
		Action: runAdministrator,
	}
}

func runAdministrator(ctx context.Context, c *cli.Command) error {
	addr := c.String("addr")

	kubeCfg := kube.Config{
		Context:   c.String("kube-context"),
		Namespace: c.String("kube-namespace"),
	}
	restCfg, err := kube.RestConfig(kubeCfg)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("failed to create dynamic kubernetes client: %w", err)
	}

	localAdm := admin.NewLocalFromClient(clientset, dynClient, restCfg, admin.LocalConfig{
		ServiceAccountName:      c.String("service-account"),
		ServiceAccountNamespace: c.String("namespace"),
	})

	srv := grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20))
	bridgev1.RegisterAdministratorServiceServer(srv, &administratorServer{admin: localAdm})

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Administrator server starting", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(lis)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("Shutting down administrator server...")
		srv.GracefulStop()
		return nil
	}
}

// administratorServer is a thin gRPC wrapper that delegates to an Service implementation.
type administratorServer struct {
	bridgev1.UnimplementedAdministratorServiceServer
	admin admin.Service
}

func (s *administratorServer) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	resp, err := s.admin.CreateBridge(ctx, req)
	if err != nil {
		slog.Error("CreateBridge failed", "device_id", req.DeviceId, "error", err)
		return nil, grpcError(err)
	}
	return resp, nil
}

func (s *administratorServer) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	resp, err := s.admin.ListBridges(ctx, req)
	if err != nil {
		slog.Error("ListBridges failed", "device_id", req.DeviceId, "error", err)
		return nil, grpcError(err)
	}
	return resp, nil
}

func (s *administratorServer) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	resp, err := s.admin.DeleteBridge(ctx, req)
	if err != nil {
		slog.Error("DeleteBridge failed", "device_id", req.DeviceId, "name", req.Name, "error", err)
		return nil, grpcError(err)
	}
	return resp, nil
}

// grpcError converts an error from the Service implementation to an appropriate
// gRPC status error.
func grpcError(err error) error {
	var notFound *resources.DeploymentNotFoundError
	if errors.As(err, &notFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

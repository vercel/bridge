package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/except"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/k8s/namespace"
	"github.com/vercel/bridge/pkg/k8s/resources"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
				Sources: cli.EnvVars("ADMINISTRATOR_ADDR"),
			},
			&cli.StringFlag{
				Name:    "proxy-image",
				Usage:   "Bridge proxy container image",
				Value:   "ghcr.io/vercel/bridge-cli:edge",
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
				Sources: cli.EnvVars("KUBE_CONTEXT"),
			},
			&cli.StringFlag{
				Name:    "kube-namespace",
				Usage:   "Override the default kubeconfig namespace (out-of-cluster only)",
				Sources: cli.EnvVars("KUBE_NAMESPACE"),
			},
		},
		Action: runAdministrator,
	}
}

func runAdministrator(ctx context.Context, c *cli.Command) error {
	addr := c.String("addr")
	proxyImage := c.String("proxy-image")
	serviceAccount := c.String("service-account")
	adminNamespace := c.String("namespace")

	clientset, err := kube.NewClientset(kube.Config{
		Context:   c.String("kube-context"),
		Namespace: c.String("kube-namespace"),
	})
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Create gRPC server
	srv := grpc.NewServer()
	adminServer := &administratorServer{
		client:         clientset,
		proxyImage:     proxyImage,
		serviceAccount: serviceAccount,
		adminNamespace: adminNamespace,
	}
	bridgev1.RegisterAdministratorServiceServer(srv, adminServer)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Administrator server starting", "addr", addr, "namespace", adminNamespace)

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

// administratorServer implements the AdministratorService gRPC server.
type administratorServer struct {
	bridgev1.UnimplementedAdministratorServiceServer

	client         kubernetes.Interface
	proxyImage     string
	serviceAccount string
	adminNamespace string
}

func (s *administratorServer) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}

	nsName := identity.NamespaceForDevice(req.DeviceId)

	slog.Info("Creating bridge",
		"device_id", req.DeviceId,
		"namespace", nsName,
		"source_deployment", req.SourceDeployment,
		"source_namespace", req.SourceNamespace,
	)

	// If force is set and there's an existing bridge with the same name, tear it down
	if req.Force {
		deployName := req.SourceDeployment
		if _, err := s.client.AppsV1().Deployments(nsName).Get(ctx, deployName, metav1.GetOptions{}); err == nil {
			slog.Info("Tearing down existing bridge", "name", deployName, "namespace", nsName)
			_ = resources.DeleteBridgeResources(ctx, s.client, nsName, deployName)
		}
	}

	// Ensure namespace with labels and RBAC
	if err := namespace.EnsureNamespace(ctx, s.client, namespace.CreateConfig{
		Name:                    nsName,
		DeviceID:                req.DeviceId,
		SourceDeployment:        req.SourceDeployment,
		SourceNamespace:         req.SourceNamespace,
		ServiceAccountName:      s.serviceAccount,
		ServiceAccountNamespace: s.adminNamespace,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to ensure namespace: %v", err)
	}

	var result *resources.CopyResult

	if req.SourceDeployment != "" {
		if req.SourceNamespace == "" {
			return nil, status.Error(codes.InvalidArgument, "source_namespace is required when source_deployment is set")
		}

		// Copy and transform the source deployment
		var err error
		result, err = resources.CopyAndTransform(ctx, s.client, resources.CopyConfig{
			SourceNamespace:  req.SourceNamespace,
			SourceDeployment: req.SourceDeployment,
			TargetNamespace:  nsName,
			ProxyImage:       s.proxyImage,
		})
		if err != nil {
			if notFound, ok := err.(*resources.DeploymentNotFoundError); ok {
				return nil, status.Error(codes.NotFound, notFound.Error())
			}
			return nil, status.Errorf(codes.Internal, "failed to copy and transform: %v", err)
		}
	} else {
		// Create a simple bridge proxy deployment
		var err error
		result, err = resources.CreateSimpleDeployment(ctx, s.client, nsName, s.proxyImage)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create simple deployment: %v", err)
		}
	}

	// Wait for the pod to be ready
	podName, err := kube.WaitForPod(ctx, s.client, nsName, meta.DeploymentSelector(result.DeploymentName), 2*time.Minute)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed waiting for pod: %v", err)
	}

	// Call GetMetadata on the running proxy pod to retrieve the environment
	// variables that Kubernetes resolved from the source deployment's config.
	var envVars map[string]string
	pod, err := s.client.CoreV1().Pods(nsName).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		slog.Warn("Failed to get pod for GetMetadata", "pod", podName, "error", err)
	} else if pod.Status.PodIP != "" {
		proxyAddr := fmt.Sprintf("%s:%d", pod.Status.PodIP, result.PodPort)
		if md, err := fetchProxyMetadata(ctx, proxyAddr); err != nil {
			slog.Warn("GetMetadata call failed", "addr", proxyAddr, "error", err)
		} else {
			envVars = md
		}
	}

	return &bridgev1.CreateBridgeResponse{
		Namespace:      nsName,
		PodName:        podName,
		Port:           result.PodPort,
		DeploymentName: result.DeploymentName,
		EnvVars:        envVars,
	}, nil
}

func (s *administratorServer) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}

	nsName := identity.NamespaceForDevice(req.DeviceId)
	deploys, err := s.client.AppsV1().Deployments(nsName).List(ctx, metav1.ListOptions{
		LabelSelector: meta.ProxySelector,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list bridge deployments: %v", err)
	}

	var bridges []*bridgev1.BridgeInfo
	for _, d := range deploys.Items {
		bridges = append(bridges, &bridgev1.BridgeInfo{
			DeviceId:         req.DeviceId,
			SourceDeployment: d.Labels[meta.LabelWorkloadSource],
			SourceNamespace:  d.Labels[meta.LabelWorkloadSourceNamespace],
			Namespace:        nsName,
			DeploymentName:   d.Name,
			CreatedAt:        d.CreationTimestamp.Format(time.RFC3339),
		})
	}

	return &bridgev1.ListBridgesResponse{
		Bridges: bridges,
	}, nil
}

func (s *administratorServer) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	nsName := identity.NamespaceForDevice(req.DeviceId)

	slog.Info("Deleting bridge", "device_id", req.DeviceId, "namespace", nsName, "name", req.Name)

	if err := resources.DeleteBridgeResources(ctx, s.client, nsName, req.Name); err != nil {
		return nil, except.GRPCFromK8s(err, "failed to delete bridge")
	}

	return &bridgev1.DeleteBridgeResponse{}, nil
}

// fetchProxyMetadata dials the bridge proxy at the given address and calls
// GetMetadata to retrieve the pod's environment variables.
func fetchProxyMetadata(ctx context.Context, addr string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	defer conn.Close()

	client := bridgev1.NewBridgeProxyServiceClient(conn)
	resp, err := client.GetMetadata(ctx, &bridgev1.GetMetadataRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetMetadata: %w", err)
	}
	return resp.GetEnvVars(), nil
}

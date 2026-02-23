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
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
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
				Value:   "ghcr.io/vercel/bridge:edge",
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

	// If force is set and there's an existing bridge for the same deployment, tear it down
	if req.Force {
		existing, err := s.findExistingBridge(ctx, nsName, req.SourceDeployment)
		if err == nil && existing != "" {
			slog.Info("Tearing down existing bridge", "deployment", existing, "namespace", nsName)
			_ = s.client.AppsV1().Deployments(nsName).Delete(ctx, existing, metav1.DeleteOptions{})
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
	podName, err := kube.WaitForPod(ctx, s.client, nsName, meta.ProxySelector, 2*time.Minute)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed waiting for pod: %v", err)
	}

	return &bridgev1.CreateBridgeResponse{
		Namespace:      nsName,
		PodName:        podName,
		Port:           result.PodPort,
		DeploymentName: result.DeploymentName,
	}, nil
}

func (s *administratorServer) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}

	namespaces, err := namespace.ListBridgeNamespaces(ctx, s.client, req.DeviceId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list bridge namespaces: %v", err)
	}

	var bridges []*bridgev1.BridgeInfo
	for _, ns := range namespaces {
		info := &bridgev1.BridgeInfo{
			DeviceId:         ns.Labels[meta.LabelDeviceID],
			SourceDeployment: ns.Labels[meta.LabelWorkloadSource],
			SourceNamespace:  ns.Labels[meta.LabelWorkloadSourceNamespace],
			Namespace:        ns.Name,
			CreatedAt:        ns.CreationTimestamp.Format(time.RFC3339),
			Status:           string(ns.Status.Phase),
		}
		bridges = append(bridges, info)
	}

	return &bridgev1.ListBridgesResponse{
		Bridges: bridges,
	}, nil
}

func (s *administratorServer) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}

	nsName := identity.NamespaceForDevice(req.DeviceId)

	slog.Info("Deleting bridge", "device_id", req.DeviceId, "namespace", nsName, "source_deployment", req.SourceDeployment)

	if req.SourceDeployment != "" {
		// Delete just the deployment for this source
		err := s.client.AppsV1().Deployments(nsName).Delete(ctx, resources.BridgeDeployName(req.SourceDeployment), metav1.DeleteOptions{})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to delete deployment: %v", err)
		}
	} else {
		// Delete the entire namespace
		if err := namespace.DeleteNamespace(ctx, s.client, nsName); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to delete namespace: %v", err)
		}
	}

	return &bridgev1.DeleteBridgeResponse{}, nil
}

// findExistingBridge finds an existing bridge deployment in the namespace.
func (s *administratorServer) findExistingBridge(ctx context.Context, ns, deployName string) (string, error) {
	if deployName != "" {
		bridgeName := resources.BridgeDeployName(deployName)
		_, err := s.client.AppsV1().Deployments(ns).Get(ctx, bridgeName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return bridgeName, nil
	}

	deploys, err := s.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
		LabelSelector: meta.ProxySelector,
	})
	if err != nil {
		return "", err
	}
	if len(deploys.Items) > 0 {
		return deploys.Items[0].Name, nil
	}
	return "", fmt.Errorf("no existing bridge found")
}

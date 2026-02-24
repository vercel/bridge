package admin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/k8s/namespace"
	"github.com/vercel/bridge/pkg/k8s/portforward"
	"github.com/vercel/bridge/pkg/k8s/resources"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const defaultProxyImage = "ghcr.io/vercel/bridge:edge"

// LocalConfig configures the local admin implementation.
type LocalConfig struct {
	// ProxyImage is the container image for the bridge proxy pod.
	// Defaults to ghcr.io/vercel/bridge:edge.
	ProxyImage string
}

// localAdmin implements Admin by performing operations directly against the
// Kubernetes API using the user's local kubeconfig credentials.
type localAdmin struct {
	client     kubernetes.Interface
	restConfig *rest.Config
	config     LocalConfig
}

// NewLocal creates a local Admin that performs operations using the current
// kubeconfig context.
func NewLocal(cfg LocalConfig) (Admin, error) {
	restCfg, err := kube.RestConfig(kube.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	if cfg.ProxyImage == "" {
		cfg.ProxyImage = defaultProxyImage
	}
	return &localAdmin{
		client:     clientset,
		restConfig: restCfg,
		config:     cfg,
	}, nil
}

func (l *localAdmin) CreateBridge(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	if req.DeviceID == "" {
		return nil, fmt.Errorf("device_id is required")
	}

	nsName := identity.NamespaceForDevice(req.DeviceID)
	logger := slog.With("device_id", req.DeviceID, "namespace", nsName)
	logger.Info("Creating bridge (local)")

	// Tear down existing bridge if force is set.
	if req.Force {
		if existing := l.findExistingBridge(ctx, nsName, req.SourceDeployment); existing != "" {
			logger.Info("Tearing down existing bridge", "deployment", existing)
			_ = l.client.AppsV1().Deployments(nsName).Delete(ctx, existing, metav1.DeleteOptions{})
		}
	}

	// Ensure namespace with labels and RBAC.
	if err := namespace.EnsureNamespace(ctx, l.client, namespace.CreateConfig{
		Name:                    nsName,
		DeviceID:                req.DeviceID,
		SourceDeployment:        req.SourceDeployment,
		SourceNamespace:         req.SourceNamespace,
		ServiceAccountName:      "administrator",
		ServiceAccountNamespace: "bridge",
	}); err != nil {
		return nil, fmt.Errorf("failed to ensure namespace: %w", err)
	}

	var result *resources.CopyResult

	if req.SourceDeployment != "" {
		if req.SourceNamespace == "" {
			return nil, fmt.Errorf("source namespace is required when a deployment is specified")
		}
		var err error
		result, err = resources.CopyAndTransform(ctx, l.client, resources.CopyConfig{
			SourceNamespace:  req.SourceNamespace,
			SourceDeployment: req.SourceDeployment,
			TargetNamespace:  nsName,
			ProxyImage:       l.config.ProxyImage,
		})
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		result, err = resources.CreateSimpleDeployment(ctx, l.client, nsName, l.config.ProxyImage)
		if err != nil {
			return nil, err
		}
	}

	// Wait for the pod to be ready.
	podName, err := kube.WaitForPod(ctx, l.client, nsName, meta.DeploymentSelector(result.DeploymentName), 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("failed waiting for pod: %w", err)
	}

	// Fetch environment variables from the proxy pod via port-forward.
	var envVars map[string]string
	pod, err := l.client.CoreV1().Pods(nsName).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("Failed to get pod for metadata", "pod", podName, "error", err)
	} else if pod.Status.PodIP != "" {
		if md, err := l.fetchProxyMetadata(ctx, nsName, podName, int(result.PodPort)); err != nil {
			logger.Warn("GetMetadata call failed", "pod", podName, "error", err)
		} else {
			envVars = md
		}
	}

	return &CreateResponse{
		Namespace:      nsName,
		PodName:        podName,
		Port:           result.PodPort,
		DeploymentName: result.DeploymentName,
		EnvVars:        envVars,
	}, nil
}

func (l *localAdmin) ListBridges(ctx context.Context, deviceID string) ([]*BridgeInfo, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("device_id is required")
	}

	namespaces, err := namespace.ListBridgeNamespaces(ctx, l.client, deviceID)
	if err != nil {
		return nil, fmt.Errorf("failed to list bridge namespaces: %w", err)
	}

	var bridges []*BridgeInfo
	for _, ns := range namespaces {
		bridges = append(bridges, &BridgeInfo{
			DeviceID:         ns.Labels[meta.LabelDeviceID],
			SourceDeployment: ns.Labels[meta.LabelWorkloadSource],
			SourceNamespace:  ns.Labels[meta.LabelWorkloadSourceNamespace],
			Namespace:        ns.Name,
			CreatedAt:        ns.CreationTimestamp.Format(time.RFC3339),
			Status:           string(ns.Status.Phase),
		})
	}
	return bridges, nil
}

func (l *localAdmin) Close() error {
	return nil
}

func (l *localAdmin) findExistingBridge(ctx context.Context, ns, deployName string) string {
	if deployName != "" {
		bridgeName := resources.BridgeDeployName(deployName)
		_, err := l.client.AppsV1().Deployments(ns).Get(ctx, bridgeName, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		return bridgeName
	}
	deploys, err := l.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
		LabelSelector: meta.ProxySelector,
	})
	if err != nil || len(deploys.Items) == 0 {
		return ""
	}
	return deploys.Items[0].Name
}

func (l *localAdmin) fetchProxyMetadata(ctx context.Context, ns, podName string, port int) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dialer, err := portforward.NewDialer(l.restConfig, l.client, ns, podName, port)
	if err != nil {
		return nil, fmt.Errorf("create port-forward dialer: %w", err)
	}

	conn, err := grpc.NewClient("passthrough:///pod",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer.DialContext),
	)
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	defer conn.Close()

	client := pb.NewBridgeProxyServiceClient(conn)
	resp, err := client.GetMetadata(ctx, &pb.GetMetadataRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetMetadata: %w", err)
	}
	return resp.GetEnvVars(), nil
}

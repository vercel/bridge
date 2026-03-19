package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"buf.build/go/protovalidate"
	"github.com/vercel/bridge/pkg/plumbing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/k8s/portforward"
	"github.com/vercel/bridge/pkg/k8s/resources"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const defaultProxyImage = "ghcr.io/vercel/bridge-cli:latest"

// LocalConfig configures the local admin implementation.
type LocalConfig struct {
	// ServiceAccountName is the administrator's SA name for namespace RBAC.
	// Defaults to "administrator".
	ServiceAccountName string
	// ServiceAccountNamespace is the namespace of the administrator's SA.
	// Defaults to "bridge".
	ServiceAccountNamespace string
}

var _ Service = (*adminService)(nil)

// adminService implements Service by performing operations directly against the
// Kubernetes API using the user's local kubeconfig credentials.
type adminService struct {
	client     kubernetes.Interface
	dynClient  dynamic.Interface
	restConfig *rest.Config
	config     LocalConfig
}

// NewService creates a local Service that performs operations using the current
// kubeconfig context.
func NewService(cfg LocalConfig) (Service, error) {
	restCfg, err := kube.RestConfig(kube.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic kubernetes client: %w", err)
	}
	return NewLocalFromClient(clientset, dynClient, restCfg, cfg), nil
}

// NewLocalFromClient creates a local Service from an existing Kubernetes client.
// Used by the administrator server to share the same client.
func NewLocalFromClient(client kubernetes.Interface, dynClient dynamic.Interface, restCfg *rest.Config, cfg LocalConfig) Service {
	if cfg.ServiceAccountName == "" {
		cfg.ServiceAccountName = "administrator"
	}
	if cfg.ServiceAccountNamespace == "" {
		cfg.ServiceAccountNamespace = "bridge"
	}
	return &adminService{
		client:     client,
		dynClient:  dynClient,
		restConfig: restCfg,
		config:     cfg,
	}
}

func (l *adminService) CreateBridge(ctx context.Context, req *bridgev1.CreateBridgeRequest) (*bridgev1.CreateBridgeResponse, error) {
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if len(req.SourceManifests) == 0 && req.SourceDeployment != "" && req.SourceNamespace == "" {
		return nil, fmt.Errorf("source_namespace is required when source_deployment is set")
	}

	proxyImage := req.ProxyImage
	if proxyImage == "" {
		proxyImage = defaultProxyImage
	}

	targetNS := req.SourceNamespace
	if targetNS == "" {
		targetNS = "default"
	}

	logger := slog.With("device_id", req.DeviceId, "namespace", targetNS)

	suffix := "-" + identity.ShortDeviceID(req.DeviceId)

	var bundle *resources.Bundle
	var err error

	if len(req.SourceManifests) > 0 {
		logger.Info("Creating bridge from manifests")
		bundle, err = resources.SourceFromManifests(req.SourceManifests)
	} else if req.SourceDeployment != "" {
		logger.Info("Creating bridge")
		bundle, err = resources.SourceFromNamespace(ctx, l.client, targetNS, req.SourceDeployment)
	} else {
		logger.Info("Creating simple bridge")
		bundle, err = resources.SourceSimple(targetNS, proxyImage)
	}
	if err != nil {
		return nil, err
	}

	sourceName := resources.FindDeploymentName(bundle)
	if sourceName == "" {
		return nil, fmt.Errorf("no deployment found in bundle")
	}

	// Determine bridge name: explicit --name flag, or default to source workload name.
	bridgeName := req.Name
	if bridgeName == "" {
		bridgeName = sourceName
	}

	sourceNS := resources.FindNamespace(bundle)
	if sourceNS == "" {
		sourceNS = targetNS
	}

	// Transforms applied to all bridge deployments.
	transforms := []resources.Transformer{
		resources.SetNamespace(targetNS),
		resources.PruneAllMetadata(),
		resources.StripOrphanedVolumes(),
		resources.StripUnreferencedLabels(),
		resources.InjectProxyImage(proxyImage),
		resources.InjectServerFacades(req.GetServerFacades()),
		resources.InjectCA(targetNS),
		resources.ClearClusterIPs(),
		resources.Rename(func(name string) string {
			if name == sourceName {
				return "bdg-" + bridgeName + suffix
			}
			return "bdg-" + name + suffix
		}),
		resources.InjectLabels(),
		resources.TransformSelectors(),
		resources.RewriteRefs(),
		resources.AppendBridgeService(targetNS),
	}

	tc := &resources.TransformContext{
		Context:         ctx,
		DeviceID:        req.DeviceId,
		BridgeName:      bridgeName,
		SourceName:      sourceName,
		SourceNamespace: sourceNS,
	}

	if err := resources.Transform(tc, bundle, transforms); err != nil {
		return nil, err
	}

	deployName := resources.FindDeploymentName(bundle)
	logger.Info("Bridge deployment prepared", "deployment", deployName, "bridge_name", bridgeName, "source", sourceName)

	// Tear down any existing bridge for this source before creating a new one.
	if req.Force {
		if existing, err := l.findBridge(ctx, targetNS, bridgeName, req.DeviceId); err == nil {
			logger.Info("Tearing down existing bridge", "existing_deployment", existing.Name)
			_ = resources.DeleteBridgeResources(ctx, l.client, targetNS, existing.Name, req.DeviceId)
		}
	}

	if err := resources.Save(ctx, l.client, l.dynClient, bundle); err != nil {
		logger.Error("Failed to save bridge resources", "deployment", deployName, "error", err)
		_ = resources.DeleteBridgeResources(ctx, l.client, targetNS, deployName, req.DeviceId)
		return nil, err
	}

	grpcPort, err := resources.GetGRPCPort(bundle, deployName)
	if err != nil {
		return nil, err
	}
	appPorts, _ := resources.GetAppPorts(bundle, deployName)

	// Wait for the pod to be ready.
	pod, err := kube.WaitForPod(ctx, l.client, targetNS, meta.DeploymentSelector(deployName), 2*time.Minute)
	if err != nil {
		logger.Error("Pod failed to become ready", "deployment", deployName, "error", err)
		_ = resources.DeleteBridgeResources(ctx, l.client, targetNS, deployName, req.DeviceId)
		return nil, fmt.Errorf("failed waiting for pod: %w", err)
	}

	// Gather volume mount paths from the live pod so we capture projected
	// volumes injected by webhooks (e.g. IRSA tokens) at pod creation time.
	volumeMountPaths := podVolumeMountPaths(pod)

	// Fetch environment variables from the proxy pod.
	var envVars map[string]string
	if pod.Status.PodIP != "" {
		if md, err := l.fetchProxyMetadata(ctx, pod, int(grpcPort)); err != nil {
			logger.Warn("GetMetadata call failed", "pod", pod.Name, "error", err)
		} else {
			envVars = md
		}
	}

	logger.Info("Bridge created successfully", "deployment", deployName, "pod", pod.Name, "grpc_port", grpcPort)

	return &bridgev1.CreateBridgeResponse{
		Namespace:        targetNS,
		PodName:          pod.Name,
		Port:             grpcPort,
		DeploymentName:   deployName,
		EnvVars:          envVars,
		VolumeMountPaths: volumeMountPaths,
		AppPorts:         appPorts,
		Name:             bridgeName,
	}, nil
}

func (l *adminService) ListBridges(ctx context.Context, req *bridgev1.ListBridgesRequest) (*bridgev1.ListBridgesResponse, error) {
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	slog.Debug("ListBridges", "device_id", req.DeviceId)

	// List bridge deployments across all namespaces for this device.
	deploys, err := l.client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: meta.DeviceSelector(req.DeviceId),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list bridge deployments: %w", err)
	}

	var bridges []*bridgev1.BridgeInfo
	for _, d := range deploys.Items {
		status := "pending"
		if d.Status.ReadyReplicas > 0 {
			status = "running"
		}
		name := d.Labels[meta.LabelBridgeName]
		if name == "" {
			name = d.Name
		}
		bridges = append(bridges, &bridgev1.BridgeInfo{
			DeviceId:         req.DeviceId,
			SourceDeployment: d.Labels[meta.LabelWorkloadSource],
			SourceNamespace:  d.Labels[meta.LabelWorkloadSourceNamespace],
			Namespace:        d.Namespace,
			DeploymentName:   d.Name,
			CreatedAt:        d.CreationTimestamp.Format(time.RFC3339),
			Status:           status,
			Name:             name,
		})
	}

	slog.Debug("ListBridges result", "device_id", req.DeviceId, "count", len(bridges))

	return &bridgev1.ListBridgesResponse{Bridges: bridges}, nil
}

func (l *adminService) DeleteBridge(ctx context.Context, req *bridgev1.DeleteBridgeRequest) (*bridgev1.DeleteBridgeResponse, error) {
	if err := protovalidate.Validate(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	deploy, err := l.findBridge(ctx, "", req.Name, req.DeviceId)
	if err != nil {
		return nil, err
	}
	// Resolve namespace from the bridge if not provided.
	ns := req.Namespace
	if ns == "" {
		ns = deploy.Namespace
	}

	slog.Info("Deleting bridge", "device_id", req.DeviceId, "namespace", ns, "deployment", deploy.Name)

	if err := resources.DeleteBridgeResources(ctx, l.client, ns, deploy.Name, req.DeviceId); err != nil {
		slog.Error("Failed to delete bridge", "device_id", req.DeviceId, "namespace", ns, "name", req.Name, "error", err)
		return nil, err
	}

	return &bridgev1.DeleteBridgeResponse{}, nil
}

// findBridge locates a bridge deployment by label selector instead of direct Get.
// It tries the vercel.sh/bridge-name label first, then falls back to matching
// the deployment name for backwards compatibility with bridges created before
// the bridge-name label existed.
// If namespace is empty, searches across all namespaces.
func (l *adminService) findBridge(ctx context.Context, namespace, name, deviceID string) (*appsv1.Deployment, error) {
	// Try bridge-name label first.
	deploys, err := l.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: meta.BridgeNameSelector(name, deviceID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find bridge: %w", err)
	}
	if len(deploys.Items) > 0 {
		return &deploys.Items[0], nil
	}

	// Fall back to matching by deployment name for bridges without the
	// bridge-name label. List all bridge deployments for this device and
	// find one whose k8s name matches.
	deploys, err = l.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: meta.DeviceSelector(deviceID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find bridge: %w", err)
	}
	for i := range deploys.Items {
		if deploys.Items[i].Name == name {
			return &deploys.Items[i], nil
		}
	}

	return nil, fmt.Errorf("no bridge named %q found", name)
}

// Close releases resources. No-op for local admin.
func (l *adminService) Close() error {
	return nil
}

// newPodDialer returns a gRPC context dialer and target address for reaching
// a pod. In-cluster it dials the pod IP directly; out-of-cluster it uses a
// port-forward through the Kubernetes API server.
func (l *adminService) newPodDialer(pod *corev1.Pod, port int) (plumbing.GRPCContextDialer, string, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port))
		return plumbing.GRPCContextDialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}), addr, nil
	}
	dialer, err := portforward.NewDialer(l.restConfig, l.client, pod.Namespace, pod.Name, port)
	if err != nil {
		return nil, "", fmt.Errorf("create port-forward dialer: %w", err)
	}
	return dialer, "passthrough:///pod", nil
}

// podVolumeMountPaths returns the volume mount paths from the first container
// of a live pod. This captures mounts injected by webhooks (e.g. IRSA tokens)
// that aren't present in the deployment spec.
func podVolumeMountPaths(pod *corev1.Pod) []string {
	if len(pod.Spec.Containers) == 0 {
		return nil
	}
	var paths []string
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		paths = append(paths, vm.MountPath)
	}
	return paths
}

func (l *adminService) fetchProxyMetadata(ctx context.Context, pod *corev1.Pod, port int) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dialer, target, err := l.newPodDialer(pod, port)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer.DialContext),
	)
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

package resources

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SourceFromNamespace fetches a source deployment from the cluster and returns
// a Bundle containing just that deployment. When a live pod is available, its
// container spec is used instead of the deployment template to capture runtime
// mutations (e.g. injected sidecars).
func SourceFromNamespace(ctx context.Context, client kubernetes.Interface, namespace, deployment string) (*Bundle, error) {
	srcDeploy, err := client.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, &DeploymentNotFoundError{Name: deployment, Namespace: namespace}
		}
		return nil, fmt.Errorf("failed to get source deployment %s/%s: %w", namespace, deployment, err)
	}

	// Use a live pod's container spec when available — it reflects runtime
	// mutations (e.g. injected sidecars, admission webhooks) that the
	// deployment template doesn't capture. Fall back to the template spec.
	sourceContainers := srcDeploy.Spec.Template.Spec.Containers
	if podName, err := kube.GetFirstPodForDeployment(ctx, client, namespace, deployment); err == nil {
		if pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{}); err == nil && len(pod.Spec.Containers) > 0 {
			slog.Info("Using live pod for source configuration", "pod", podName)
			sourceContainers = pod.Spec.Containers
		}
	}

	if len(sourceContainers) > 0 {
		srcDeploy.Spec.Template.Spec.Containers = sourceContainers
	}

	return &Bundle{
		Resources: []Resource{
			{Object: srcDeploy, GVK: appsv1.SchemeGroupVersion.WithKind("Deployment")},
		},
	}, nil
}

// SourceSimple builds a minimal Deployment with just the bridge proxy container.
// TransformResult is pre-set (GRPCPort = defaultProxyPort) so callers only need
// to append a bridge Service.
func SourceSimple(namespace, proxyImage string) (*Bundle, error) {
	if proxyImage == "" {
		return nil, fmt.Errorf("proxy image is required")
	}

	name := randomBridgeName()
	replicas := int32(1)
	podLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: name,
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				meta.LabelBridgeType:       meta.BridgeTypeProxy,
				meta.LabelBridgeDeployment: name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "bridge-proxy",
							Image:   proxyImage,
							Command: []string{"bridge", "server", "--addr", fmt.Sprintf(":%d", defaultProxyPort)},
							Ports: []corev1.ContainerPort{
								{Name: "grpc", ContainerPort: defaultProxyPort, Protocol: corev1.ProtocolTCP},
							},
						},
					},
				},
			},
		},
	}

	return &Bundle{
		Resources: []Resource{
			{Object: deploy, GVK: appsv1.SchemeGroupVersion.WithKind("Deployment")},
		},
	}, nil
}

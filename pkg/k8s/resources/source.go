package resources

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// SourceFromNamespace fetches the named workload from the cluster, trying each
// of workloadKinds in order, and returns a Bundle holding it as a Deployment
// (see DeploymentFromWorkload). Only the workload's pod template is used — not a
// live pod — so that webhook-injected env vars and volume mounts (e.g. IRSA)
// are absent and get cleanly re-injected on the bridge pod.
func SourceFromNamespace(ctx context.Context, client dynamic.Interface, namespace, name string) (*Bundle, error) {
	for _, kind := range workloadKinds {
		obj, err := client.Resource(gvkToGVR(kind)).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get source %s %s/%s: %w", kind.Kind, namespace, name, err)
		}
		deploy, err := DeploymentFromWorkload(obj)
		if err != nil {
			return nil, err
		}
		if deploy == nil {
			return nil, fmt.Errorf("%s %s/%s has no pod template", kind.Kind, namespace, name)
		}
		return &Bundle{
			Resources: []Resource{
				{Object: deploy, GVK: appsv1.SchemeGroupVersion.WithKind("Deployment")},
			},
		}, nil
	}
	return nil, &WorkloadNotFoundError{Name: name, Namespace: namespace}
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
		"app": name,
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    podLabels,
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
							ReadinessProbe: grpcReadinessProbe(defaultProxyPort),
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

// Package resources provides utilities for copying and transforming Kubernetes
// resources from a source namespace to a bridge namespace. It handles extracting
// ConfigMap and Secret dependencies from Deployments and swapping the application
// container with the bridge proxy container.
package resources

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/vercel/bridge/pkg/k8s/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	// defaultProxyPort is used when no source deployment exists to infer a port from.
	defaultProxyPort int32 = 3000
)

// DeploymentNotFoundError is returned when the source deployment does not exist.
type DeploymentNotFoundError struct {
	Name      string
	Namespace string
}

func (e *DeploymentNotFoundError) Error() string {
	return fmt.Sprintf("no deployment found named '%s' in namespace '%s'", e.Name, e.Namespace)
}

// BridgeDeployName returns the bridge deployment name for a source deployment.
func BridgeDeployName(sourceDeployment string) string {
	return "bridge-" + sourceDeployment
}

var adjectives = []string{
	"bold", "calm", "cool", "dark", "fair", "fast", "keen", "kind",
	"live", "neat", "pure", "rare", "safe", "slim", "soft", "warm",
	"wise", "able", "blue", "deep",
}

var nouns = []string{
	"arch", "beam", "bell", "bolt", "cape", "cask", "dawn", "dove",
	"edge", "fern", "flint", "gate", "glen", "haze", "iris", "jade",
	"knot", "lake", "lark", "mesa",
}

func randomBridgeName() string {
	adj := adjectives[rand.IntN(len(adjectives))]
	noun := nouns[rand.IntN(len(nouns))]
	return adj + "-" + noun
}

// CopyConfig holds configuration for a resource copy+transform operation.
type CopyConfig struct {
	// SourceNamespace is where the original deployment lives.
	SourceNamespace string
	// SourceDeployment is the name of the Deployment to clone.
	SourceDeployment string
	// TargetNamespace is the bridge namespace to place resources into.
	TargetNamespace string
	// ProxyImage overrides the default bridge proxy image.
	ProxyImage string
}

// CopyResult contains the results of a copy+transform operation.
type CopyResult struct {
	// DeploymentName is the name of the created deployment in the target namespace.
	DeploymentName string
	// PodPort is the port the bridge proxy is listening on.
	PodPort int32
}

// CopyAndTransform reads a source Deployment, extracts its config dependencies
// (ConfigMaps, Secrets), copies them to the target namespace, and creates a
// transformed Deployment with the app container swapped for the bridge proxy.
func CopyAndTransform(ctx context.Context, client kubernetes.Interface, cfg CopyConfig) (*CopyResult, error) {
	if cfg.ProxyImage == "" {
		return nil, fmt.Errorf("proxy image is required")
	}

	// Get the source deployment
	srcDeploy, err := client.AppsV1().Deployments(cfg.SourceNamespace).Get(ctx, cfg.SourceDeployment, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, &DeploymentNotFoundError{Name: cfg.SourceDeployment, Namespace: cfg.SourceNamespace}
		}
		return nil, fmt.Errorf("failed to get source deployment %s/%s: %w", cfg.SourceNamespace, cfg.SourceDeployment, err)
	}

	// Extract and copy config dependencies with prefixed names.
	nameMap, err := copyConfigDependencies(ctx, client, srcDeploy, cfg.SourceNamespace, cfg.TargetNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to copy config dependencies: %w", err)
	}

	// Collect all ports from the source deployment's first container.
	var appPorts []int32
	if containers := srcDeploy.Spec.Template.Spec.Containers; len(containers) > 0 {
		for _, p := range containers[0].Ports {
			appPorts = append(appPorts, p.ContainerPort)
		}
	}

	// Choose a gRPC port that doesn't conflict with any app port so the
	// bridge server can bind both the gRPC addr and ingress listeners.
	grpcPort := chooseGRPCPort(appPorts)

	// Create the transformed deployment
	deployName, err := createBridgedDeployment(ctx, client, srcDeploy, cfg.TargetNamespace, cfg.ProxyImage, grpcPort, appPorts, nameMap)
	if err != nil {
		return nil, fmt.Errorf("failed to create bridged deployment: %w", err)
	}

	// Create a Service so the proxy is addressable by name within the cluster.
	// The Service targets the first app port (ingress listener) when available,
	// falling back to the gRPC port.
	svcTargetPort := grpcPort
	if len(appPorts) > 0 {
		svcTargetPort = appPorts[0]
	}
	if err := ensureService(ctx, client, cfg.TargetNamespace, deployName, svcTargetPort); err != nil {
		return nil, fmt.Errorf("failed to create service: %w", err)
	}

	return &CopyResult{
		DeploymentName: deployName,
		PodPort:        grpcPort,
	}, nil
}

// chooseGRPCPort picks a port for the gRPC server starting from 8080,
// skipping any ports that are already used as app listen ports.
func chooseGRPCPort(appPorts []int32) int32 {
	used := make(map[int32]bool, len(appPorts))
	for _, p := range appPorts {
		used[p] = true
	}
	for p := int32(8080); ; p++ {
		if !used[p] {
			return p
		}
	}
}

// CreateSimpleDeployment creates a minimal Deployment with just the bridge proxy
// container when no source deployment is specified.
func CreateSimpleDeployment(ctx context.Context, client kubernetes.Interface, namespace, proxyImage string) (*CopyResult, error) {
	if proxyImage == "" {
		return nil, fmt.Errorf("proxy image is required")
	}

	replicas := int32(1)
	name := randomBridgeName()
	podLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: name,
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "bridge-proxy",
							Image:   proxyImage,
							Command: []string{"bridge", "server", "--addr", fmt.Sprintf(":%d", defaultProxyPort)},
							Ports: []corev1.ContainerPort{
								{ContainerPort: defaultProxyPort, Protocol: corev1.ProtocolTCP},
							},
						},
					},
				},
			},
		},
	}

	existing, err := client.AppsV1().Deployments(namespace).Get(ctx, deploy.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		if _, err := client.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("failed to create simple deployment: %w", err)
		}
	} else if err != nil {
		return nil, err
	} else {
		existing.Spec = deploy.Spec
		if _, err := client.AppsV1().Deployments(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("failed to update simple deployment: %w", err)
		}
	}

	if err := ensureService(ctx, client, namespace, deploy.Name, defaultProxyPort); err != nil {
		return nil, fmt.Errorf("failed to create service: %w", err)
	}

	return &CopyResult{
		DeploymentName: deploy.Name,
		PodPort:        defaultProxyPort,
	}, nil
}

// configRef tracks a reference to a Secret or ConfigMap.
type configRef struct {
	name     string
	optional bool
}

// copyConfigDependencies extracts Secret and ConfigMap references from a
// Deployment's pod spec, copies each resource to the target namespace with a
// deployment-scoped prefix to avoid name collisions, and returns a name map
// (original → prefixed) so callers can rewrite references on the pod spec.
func copyConfigDependencies(ctx context.Context, client kubernetes.Interface, deploy *appsv1.Deployment, srcNS, targetNS string) (nameMap map[string]string, err error) {
	podSpec := &deploy.Spec.Template.Spec
	prefix := deploy.Name + "-"

	// Collect every unique Secret/ConfigMap name referenced by the pod.
	secretRefs := make(map[string]configRef)
	configMapRefs := make(map[string]configRef)

	addSecret := func(name string, optional bool) {
		if existing, ok := secretRefs[name]; ok {
			if !optional {
				existing.optional = false
				secretRefs[name] = existing
			}
		} else {
			secretRefs[name] = configRef{name: name, optional: optional}
		}
	}
	addConfigMap := func(name string, optional bool) {
		if existing, ok := configMapRefs[name]; ok {
			if !optional {
				existing.optional = false
				configMapRefs[name] = existing
			}
		} else {
			configMapRefs[name] = configRef{name: name, optional: optional}
		}
	}

	for _, container := range append(podSpec.Containers, podSpec.InitContainers...) {
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				addSecret(ref.Name, ref.Optional != nil && *ref.Optional)
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				addConfigMap(ref.Name, ref.Optional != nil && *ref.Optional)
			}
		}
		for _, ef := range container.EnvFrom {
			if ef.SecretRef != nil {
				addSecret(ef.SecretRef.Name, ef.SecretRef.Optional != nil && *ef.SecretRef.Optional)
			}
			if ef.ConfigMapRef != nil {
				addConfigMap(ef.ConfigMapRef.Name, ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional)
			}
		}
	}
	for _, vol := range podSpec.Volumes {
		if vol.Secret != nil {
			addSecret(vol.Secret.SecretName, vol.Secret.Optional != nil && *vol.Secret.Optional)
		}
		if vol.ConfigMap != nil {
			addConfigMap(vol.ConfigMap.Name, vol.ConfigMap.Optional != nil && *vol.ConfigMap.Optional)
		}
	}

	nameMap = make(map[string]string)

	// Copy each Secret to the target namespace with a prefixed name.
	for _, ref := range secretRefs {
		src, err := client.CoreV1().Secrets(srcNS).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if ref.optional {
				slog.Debug("Skipping optional secret", "name", ref.name, "namespace", srcNS)
				continue
			}
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", srcNS, ref.name, err)
		}
		dstName := prefix + ref.name
		nameMap[ref.name] = dstName
		dst := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dstName, Namespace: targetNS},
			Type:       src.Type,
			Data:       src.Data,
		}
		if err := createOrUpdate(ctx, client, targetNS, dst); err != nil {
			return nil, fmt.Errorf("failed to copy secret %s: %w", ref.name, err)
		}
	}

	// Copy each ConfigMap to the target namespace with a prefixed name.
	for _, ref := range configMapRefs {
		src, err := client.CoreV1().ConfigMaps(srcNS).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if ref.optional {
				slog.Debug("Skipping optional configmap", "name", ref.name, "namespace", srcNS)
				continue
			}
			return nil, fmt.Errorf("failed to get configmap %s/%s: %w", srcNS, ref.name, err)
		}
		dstName := prefix + ref.name
		nameMap[ref.name] = dstName
		dst := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: dstName, Namespace: targetNS},
			Data:       src.Data,
		}
		if err := createOrUpdateConfigMap(ctx, client, targetNS, dst); err != nil {
			return nil, fmt.Errorf("failed to copy configmap %s: %w", ref.name, err)
		}
	}

	return nameMap, nil
}

// createBridgedDeployment creates a new Deployment in the target namespace with the
// application container replaced by the bridge proxy.
func createBridgedDeployment(ctx context.Context, client kubernetes.Interface, src *appsv1.Deployment, targetNS, proxyImage string, grpcPort int32, listenPorts []int32, nameMap map[string]string) (string, error) {
	replicas := int32(1)

	// Clone containers from the source, modifying only the first (primary app)
	// container in-place: swap its image/command/ports for the bridge proxy
	// while keeping everything else (env, envFrom, volumeMounts, resources, etc.).
	containers := make([]corev1.Container, len(src.Spec.Template.Spec.Containers))
	copy(containers, src.Spec.Template.Spec.Containers)

	if len(containers) > 0 {
		c := &containers[0]

		args := []string{"bridge", "server", "--addr", fmt.Sprintf(":%d", grpcPort)}
		if len(listenPorts) > 0 {
			var specs []string
			for _, p := range listenPorts {
				specs = append(specs, fmt.Sprintf("%d/tcp", p))
			}
			args = append(args, "--listen-ports", strings.Join(specs, ","))
		}

		c.Image = proxyImage
		c.Command = args
		c.Args = nil
		c.Ports = []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: grpcPort, Protocol: corev1.ProtocolTCP},
		}
		for _, p := range listenPorts {
			c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: p, Protocol: corev1.ProtocolTCP})
		}
		// Clear probes — the bridge proxy doesn't implement the app's health checks.
		c.LivenessProbe = nil
		c.ReadinessProbe = nil
		c.StartupProbe = nil
	}

	deployName := BridgeDeployName(src.Name)
	podLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: deployName,
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: targetNS,
			Labels: map[string]string{
				meta.LabelBridgeType:              meta.BridgeTypeProxy,
				meta.LabelWorkloadSource:          src.Name,
				meta.LabelWorkloadSourceNamespace: src.Namespace,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					Containers:     containers,
					InitContainers: src.Spec.Template.Spec.InitContainers,
					Volumes:        src.Spec.Template.Spec.Volumes,
				},
			},
		},
	}

	// Rewrite all Secret/ConfigMap references to use the prefixed names.
	rewriteConfigRefs(&deploy.Spec.Template.Spec, nameMap)

	existing, err := client.AppsV1().Deployments(targetNS).Get(ctx, deploy.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		if _, err := client.AppsV1().Deployments(targetNS).Create(ctx, deploy, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("failed to create bridged deployment: %w", err)
		}
	} else if err != nil {
		return "", err
	} else {
		existing.Spec = deploy.Spec
		existing.Labels = deploy.Labels
		if _, err := client.AppsV1().Deployments(targetNS).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("failed to update bridged deployment: %w", err)
		}
	}

	return deploy.Name, nil
}

// rewriteConfigRefs rewrites all Secret/ConfigMap references in the pod spec
// (env, envFrom, volumes) using the provided name map (original → prefixed).
func rewriteConfigRefs(podSpec *corev1.PodSpec, nameMap map[string]string) {
	rewrite := func(name string) string {
		if mapped, ok := nameMap[name]; ok {
			return mapped
		}
		return name
	}

	for i := range podSpec.Containers {
		rewriteContainerRefs(&podSpec.Containers[i], rewrite)
	}
	for i := range podSpec.InitContainers {
		rewriteContainerRefs(&podSpec.InitContainers[i], rewrite)
	}
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Secret != nil {
			podSpec.Volumes[i].Secret.SecretName = rewrite(podSpec.Volumes[i].Secret.SecretName)
		}
		if podSpec.Volumes[i].ConfigMap != nil {
			podSpec.Volumes[i].ConfigMap.Name = rewrite(podSpec.Volumes[i].ConfigMap.Name)
		}
	}
}

func rewriteContainerRefs(c *corev1.Container, rewrite func(string) string) {
	for i := range c.Env {
		if c.Env[i].ValueFrom == nil {
			continue
		}
		if ref := c.Env[i].ValueFrom.SecretKeyRef; ref != nil {
			ref.Name = rewrite(ref.Name)
		}
		if ref := c.Env[i].ValueFrom.ConfigMapKeyRef; ref != nil {
			ref.Name = rewrite(ref.Name)
		}
	}
	for i := range c.EnvFrom {
		if c.EnvFrom[i].SecretRef != nil {
			c.EnvFrom[i].SecretRef.Name = rewrite(c.EnvFrom[i].SecretRef.Name)
		}
		if c.EnvFrom[i].ConfigMapRef != nil {
			c.EnvFrom[i].ConfigMapRef.Name = rewrite(c.EnvFrom[i].ConfigMapRef.Name)
		}
	}
}

// ensureService creates or updates a ClusterIP Service that maps port 80 to the
// given target port, selecting pods via the bridge proxy label.
func ensureService(ctx context.Context, client kubernetes.Interface, namespace, name string, targetPort int32) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt32(targetPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	existing, err := client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	existing.Spec.Ports = svc.Spec.Ports
	existing.Spec.Selector = svc.Spec.Selector
	_, err = client.CoreV1().Services(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func createOrUpdate(ctx context.Context, client kubernetes.Interface, ns string, secret *corev1.Secret) error {
	existing, err := client.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	existing.Data = secret.Data
	_, err = client.CoreV1().Secrets(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func createOrUpdateConfigMap(ctx context.Context, client kubernetes.Interface, ns string, cm *corev1.ConfigMap) error {
	existing, err := client.CoreV1().ConfigMaps(ns).Get(ctx, cm.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	existing.Data = cm.Data
	_, err = client.CoreV1().ConfigMaps(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

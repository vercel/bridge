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

	// copiedSecretName is the name of the consolidated Secret in the target namespace.
	copiedSecretName = "bridge-copied-config"
	// copiedConfigMapName is the name of the consolidated ConfigMap in the target namespace.
	copiedConfigMapName = "bridge-copied-config"
)

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
		return nil, fmt.Errorf("failed to get source deployment %s/%s: %w", cfg.SourceNamespace, cfg.SourceDeployment, err)
	}

	// Extract and copy config dependencies
	if err := copyConfigDependencies(ctx, client, srcDeploy, cfg.SourceNamespace, cfg.TargetNamespace); err != nil {
		return nil, fmt.Errorf("failed to copy config dependencies: %w", err)
	}

	// Use the first port from the source deployment's first container,
	// falling back to the default if none is defined.
	proxyPort := defaultProxyPort
	if containers := srcDeploy.Spec.Template.Spec.Containers; len(containers) > 0 {
		if ports := containers[0].Ports; len(ports) > 0 {
			proxyPort = ports[0].ContainerPort
		}
	}

	// Create the transformed deployment
	deployName, err := createBridgedDeployment(ctx, client, srcDeploy, cfg.TargetNamespace, cfg.ProxyImage, proxyPort)
	if err != nil {
		return nil, fmt.Errorf("failed to create bridged deployment: %w", err)
	}

	// Create a Service so the proxy is addressable by name within the cluster.
	if err := ensureService(ctx, client, cfg.TargetNamespace, deployName, proxyPort); err != nil {
		return nil, fmt.Errorf("failed to create service: %w", err)
	}

	return &CopyResult{
		DeploymentName: deployName,
		PodPort:        proxyPort,
	}, nil
}

// CreateSimpleDeployment creates a minimal Deployment with just the bridge proxy
// container when no source deployment is specified.
func CreateSimpleDeployment(ctx context.Context, client kubernetes.Interface, namespace, proxyImage string) (*CopyResult, error) {
	if proxyImage == "" {
		return nil, fmt.Errorf("proxy image is required")
	}

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      randomBridgeName(),
			Namespace: namespace,
			Labels:    map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
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

// copyConfigDependencies extracts ConfigMap and Secret references from a Deployment's
// pod spec and copies the referenced keys into consolidated resources in the target namespace.
// configRef tracks a reference to a Secret or ConfigMap, either for a specific
// key or for all keys (key == "").
type configRef struct {
	name     string
	key      string // empty means all keys
	optional bool
}

func copyConfigDependencies(ctx context.Context, client kubernetes.Interface, deploy *appsv1.Deployment, srcNS, targetNS string) error {
	podSpec := &deploy.Spec.Template.Spec

	// Phase 1: Walk the pod spec and collect every Secret/ConfigMap reference.
	var secretRefs, configMapRefs []configRef

	for _, container := range append(podSpec.Containers, podSpec.InitContainers...) {
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				secretRefs = append(secretRefs, configRef{
					name:     ref.Name,
					key:      ref.Key,
					optional: ref.Optional != nil && *ref.Optional,
				})
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				configMapRefs = append(configMapRefs, configRef{
					name:     ref.Name,
					key:      ref.Key,
					optional: ref.Optional != nil && *ref.Optional,
				})
			}
		}

		for _, envFrom := range container.EnvFrom {
			if envFrom.SecretRef != nil {
				secretRefs = append(secretRefs, configRef{
					name:     envFrom.SecretRef.Name,
					optional: envFrom.SecretRef.Optional != nil && *envFrom.SecretRef.Optional,
				})
			}
			if envFrom.ConfigMapRef != nil {
				configMapRefs = append(configMapRefs, configRef{
					name:     envFrom.ConfigMapRef.Name,
					optional: envFrom.ConfigMapRef.Optional != nil && *envFrom.ConfigMapRef.Optional,
				})
			}
		}
	}

	for _, vol := range podSpec.Volumes {
		if vol.Secret != nil {
			secretRefs = append(secretRefs, configRef{
				name:     vol.Secret.SecretName,
				optional: vol.Secret.Optional != nil && *vol.Secret.Optional,
			})
		}
		if vol.ConfigMap != nil {
			configMapRefs = append(configMapRefs, configRef{
				name:     vol.ConfigMap.Name,
				optional: vol.ConfigMap.Optional != nil && *vol.ConfigMap.Optional,
			})
		}
	}

	// Phase 2: Fetch each unique Secret/ConfigMap once.
	secrets, err := fetchSecrets(ctx, client, srcNS, secretRefs)
	if err != nil {
		return err
	}
	configMaps, err := fetchConfigMaps(ctx, client, srcNS, configMapRefs)
	if err != nil {
		return err
	}

	// Phase 3: Extract the needed keys into the consolidated data maps.
	secretData := make(map[string][]byte)
	for _, ref := range secretRefs {
		src, ok := secrets[ref.name]
		if !ok {
			continue // optional and missing — already handled in fetch
		}
		if ref.key != "" {
			if val, ok := src.Data[ref.key]; ok {
				secretData[fmt.Sprintf("%s_%s", ref.name, ref.key)] = val
			}
		} else {
			for k, v := range src.Data {
				secretData[k] = v
			}
		}
	}

	configData := make(map[string]string)
	for _, ref := range configMapRefs {
		src, ok := configMaps[ref.name]
		if !ok {
			continue
		}
		if ref.key != "" {
			if val, ok := src.Data[ref.key]; ok {
				configData[fmt.Sprintf("%s_%s", ref.name, ref.key)] = val
			}
		} else {
			for k, v := range src.Data {
				configData[k] = v
			}
		}
	}

	// Phase 4: Write the consolidated resources to the target namespace.
	if len(secretData) > 0 {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      copiedSecretName,
				Namespace: targetNS,
			},
			Data: secretData,
		}
		if err := createOrUpdate(ctx, client, targetNS, secret); err != nil {
			return fmt.Errorf("failed to create copied secret: %w", err)
		}
	}

	if len(configData) > 0 {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      copiedConfigMapName,
				Namespace: targetNS,
			},
			Data: configData,
		}
		if err := createOrUpdateConfigMap(ctx, client, targetNS, cm); err != nil {
			return fmt.Errorf("failed to create copied configmap: %w", err)
		}
	}

	return nil
}

// fetchSecrets fetches each unique Secret referenced by refs, returning a map
// keyed by name. Optional refs that are not found are silently skipped.
func fetchSecrets(ctx context.Context, client kubernetes.Interface, ns string, refs []configRef) (map[string]*corev1.Secret, error) {
	result := make(map[string]*corev1.Secret)
	optional := make(map[string]bool)

	for _, ref := range refs {
		if _, ok := result[ref.name]; ok {
			continue
		}
		if ref.optional {
			optional[ref.name] = true
		}

		secret, err := client.CoreV1().Secrets(ns).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if optional[ref.name] {
				slog.Debug("Skipping optional secret", "name", ref.name, "namespace", ns)
				continue
			}
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", ns, ref.name, err)
		}
		result[ref.name] = secret
	}
	return result, nil
}

// fetchConfigMaps fetches each unique ConfigMap referenced by refs, returning a
// map keyed by name. Optional refs that are not found are silently skipped.
func fetchConfigMaps(ctx context.Context, client kubernetes.Interface, ns string, refs []configRef) (map[string]*corev1.ConfigMap, error) {
	result := make(map[string]*corev1.ConfigMap)
	optional := make(map[string]bool)

	for _, ref := range refs {
		if _, ok := result[ref.name]; ok {
			continue
		}
		if ref.optional {
			optional[ref.name] = true
		}

		cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if optional[ref.name] {
				slog.Debug("Skipping optional configmap", "name", ref.name, "namespace", ns)
				continue
			}
			return nil, fmt.Errorf("failed to get configmap %s/%s: %w", ns, ref.name, err)
		}
		result[ref.name] = cm
	}
	return result, nil
}

// createBridgedDeployment creates a new Deployment in the target namespace with the
// application container replaced by the bridge proxy.
func createBridgedDeployment(ctx context.Context, client kubernetes.Interface, src *appsv1.Deployment, targetNS, proxyImage string, port int32) (string, error) {
	replicas := int32(1)

	// Build the transformed container list: replace the first container with bridge proxy,
	// keep sidecars intact.
	containers := make([]corev1.Container, len(src.Spec.Template.Spec.Containers))
	for i, c := range src.Spec.Template.Spec.Containers {
		if i == 0 {
			// Replace the primary app container with bridge proxy.
			// Pass --addr so the gRPC server listens on the same port
			// as the original app container.
			containers[i] = corev1.Container{
				Name:    c.Name,
				Image:   proxyImage,
				Command: []string{"bridge", "server", "--addr", fmt.Sprintf(":%d", port)},
				Ports: []corev1.ContainerPort{
					{ContainerPort: port, Protocol: corev1.ProtocolTCP},
				},
			}
		} else {
			// Keep sidecar containers as-is
			containers[i] = c
		}
	}

	// Rewrite env references to use our consolidated config
	for i := range containers {
		rewriteEnvRefs(&containers[i])
	}

	deployName := BridgeDeployName(src.Name)
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
				MatchLabels: map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{meta.LabelBridgeType: meta.BridgeTypeProxy},
				},
				Spec: corev1.PodSpec{
					Containers: containers,
					// Don't copy volumes — we've consolidated config into single resources
					// and the bridge proxy doesn't need the original volume mounts.
				},
			},
		},
	}

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

// rewriteEnvRefs rewrites Secret/ConfigMap env references to point to our consolidated resources.
func rewriteEnvRefs(container *corev1.Container) {
	for i := range container.Env {
		if container.Env[i].ValueFrom == nil {
			continue
		}
		if ref := container.Env[i].ValueFrom.SecretKeyRef; ref != nil {
			ref.Name = copiedSecretName
		}
		if ref := container.Env[i].ValueFrom.ConfigMapKeyRef; ref != nil {
			ref.Name = copiedConfigMapName
		}
	}
	for i := range container.EnvFrom {
		if container.EnvFrom[i].SecretRef != nil {
			container.EnvFrom[i].SecretRef.Name = copiedSecretName
		}
		if container.EnvFrom[i].ConfigMapRef != nil {
			container.EnvFrom[i].ConfigMapRef.Name = copiedConfigMapName
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

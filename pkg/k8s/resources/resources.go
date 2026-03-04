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
	"k8s.io/apimachinery/pkg/runtime/schema"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// VolumeMountPaths are the absolute mount paths from the source application container.
	VolumeMountPaths []string
	// AppPorts are the container ports from the source application container.
	AppPorts []int32
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

	deployName := srcDeploy.Name

	// Extract and copy config dependencies with prefixed names.
	names, err := copyConfigDependencies(ctx, client, srcDeploy, cfg.SourceNamespace, cfg.TargetNamespace, deployName)
	if err != nil {
		return nil, fmt.Errorf("failed to copy config dependencies: %w", err)
	}

	// Copy the source deployment's service account so init containers and
	// sidecars retain the same workload identity (e.g. IRSA role).
	if saName := srcDeploy.Spec.Template.Spec.ServiceAccountName; saName != "" {
		if err := copyServiceAccount(ctx, client, cfg.SourceNamespace, cfg.TargetNamespace, saName, deployName); err != nil {
			slog.Warn("Failed to copy service account", "name", saName, "error", err)
		}
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
	if err := createBridgedDeployment(ctx, client, srcDeploy, cfg.TargetNamespace, cfg.ProxyImage, deployName, grpcPort, appPorts, names); err != nil {
		return nil, fmt.Errorf("failed to create bridged deployment: %w", err)
	}

	// Create a Service so the proxy is addressable by name within the cluster.
	// The Service targets the first app port (ingress listener) when available,
	// falling back to the gRPC port.
	svcTargetPort := grpcPort
	if len(appPorts) > 0 {
		svcTargetPort = appPorts[0]
	}
	svc := NewBridgeService(cfg.TargetNamespace, deployName, svcTargetPort)
	if err := upsertService(ctx, client, svc); err != nil {
		return nil, fmt.Errorf("failed to create service: %w", err)
	}

	return &CopyResult{
		DeploymentName: deployName,
		PodPort:        grpcPort,
		AppPorts:       appPorts,
	}, nil
}

// findApplicationDeployment locates a Deployment by name in the bundle.
func findApplicationDeployment(b *Bundle, name string) (*appsv1.Deployment, error) {
	for _, r := range b.Resources {
		if deploy, ok := r.Object.(*appsv1.Deployment); ok && deploy.Name == name {
			return deploy, nil
		}
	}
	return nil, fmt.Errorf("deployment %q not found in bundle", name)
}

// injectProxyImage swaps the first container in the deployment for the bridge
// proxy. App ports are preserved as additional container ports after the named
// "grpc" port so they can be queried from the deployment state later.
func injectProxyImage(deploy *appsv1.Deployment, proxyImage string) {
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return
	}

	c := &containers[0]

	var srcPorts []int32
	for _, p := range c.Ports {
		srcPorts = append(srcPorts, p.ContainerPort)
	}

	grpcPort := chooseGRPCPort(srcPorts)

	args := []string{"bridge", "--log-paths", "stdout", "server", "--addr", fmt.Sprintf(":%d", grpcPort)}
	if len(srcPorts) > 0 {
		var specs []string
		for _, p := range srcPorts {
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
	for _, p := range srcPorts {
		c.Ports = append(c.Ports, corev1.ContainerPort{ContainerPort: p, Protocol: corev1.ProtocolTCP})
	}
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
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

// configRef tracks a reference to a Secret or ConfigMap.
type configRef struct {
	name     string
	optional bool
}

// copyConfigDependencies extracts Secret and ConfigMap references from a
// Deployment's pod spec, copies each resource to the target namespace with a
// deployment-scoped prefix to avoid name collisions, and returns a name map
// (original → prefixed) so callers can rewrite references on the pod spec.
func copyConfigDependencies(ctx context.Context, client kubernetes.Interface, deploy *appsv1.Deployment, srcNS, targetNS, bridgeDeployName string) (NameMap, error) {
	podSpec := &deploy.Spec.Template.Spec
	prefix := deploy.Name + "-"
	ownerLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: bridgeDeployName,
	}

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

	names := make(NameMap)

	// Copy each Secret to the target namespace with a prefixed name.
	for _, ref := range secretRefs {
		secret, err := client.CoreV1().Secrets(srcNS).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if ref.optional {
				slog.Debug("Skipping optional secret", "name", ref.name, "namespace", srcNS)
				continue
			}
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", srcNS, ref.name, err)
		}
		dstName := prefix + ref.name
		names[ResourceKey{GroupKind: schema.GroupKind{Kind: "Secret"}, Name: ref.name}] = dstName
		secret.Name = dstName
		secret.Namespace = targetNS
		secret.ResourceVersion = ""
		secret.UID = ""
		secret.CreationTimestamp = metav1.Time{}
		for k, v := range ownerLabels {
			if secret.Labels == nil {
				secret.Labels = make(map[string]string)
			}
			secret.Labels[k] = v
		}
		if err := upsertSecret(ctx, client, targetNS, secret); err != nil {
			return nil, fmt.Errorf("failed to copy secret %s: %w", ref.name, err)
		}
	}

	// Copy each ConfigMap to the target namespace with a prefixed name.
	for _, ref := range configMapRefs {
		cm, err := client.CoreV1().ConfigMaps(srcNS).Get(ctx, ref.name, metav1.GetOptions{})
		if err != nil {
			if ref.optional {
				slog.Debug("Skipping optional configmap", "name", ref.name, "namespace", srcNS)
				continue
			}
			return nil, fmt.Errorf("failed to get configmap %s/%s: %w", srcNS, ref.name, err)
		}
		dstName := prefix + ref.name
		names[ResourceKey{GroupKind: schema.GroupKind{Kind: "ConfigMap"}, Name: ref.name}] = dstName
		cm.Name = dstName
		cm.Namespace = targetNS
		cm.ResourceVersion = ""
		cm.UID = ""
		cm.CreationTimestamp = metav1.Time{}
		if cm.Labels == nil {
			cm.Labels = make(map[string]string)
		}
		for k, v := range ownerLabels {
			cm.Labels[k] = v
		}
		if err := upsertConfigMap(ctx, client, targetNS, cm); err != nil {
			return nil, fmt.Errorf("failed to copy configmap %s: %w", ref.name, err)
		}
	}

	return names, nil
}

// createBridgedDeployment creates a new Deployment in the target namespace with the
// application container replaced by the bridge proxy.
func createBridgedDeployment(ctx context.Context, client kubernetes.Interface, src *appsv1.Deployment, targetNS, proxyImage, deployName string, grpcPort int32, listenPorts []int32, names NameMap) error {
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

	podLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: deployName,
	}

	// Use the prefixed service account if the source specifies one.
	saName := ""
	if src.Spec.Template.Spec.ServiceAccountName != "" {
		saName = deployName + "-" + src.Spec.Template.Spec.ServiceAccountName
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: targetNS,
			Labels: map[string]string{
				meta.LabelBridgeType:              meta.BridgeTypeProxy,
				meta.LabelBridgeDeployment:        deployName,
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
					ServiceAccountName: saName,
					Containers:         containers,
					InitContainers:     src.Spec.Template.Spec.InitContainers,
					Volumes:            src.Spec.Template.Spec.Volumes,
				},
			},
		},
	}

	// Rewrite all Secret/ConfigMap references to use the prefixed names.
	rewriteConfigRefs(&deploy.Spec.Template.Spec, names)

	existing, err := client.AppsV1().Deployments(targetNS).Get(ctx, deploy.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		if _, err := client.AppsV1().Deployments(targetNS).Create(ctx, deploy, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create bridged deployment: %w", err)
		}
	} else if err != nil {
		return err
	} else {
		existing.Spec = deploy.Spec
		existing.Labels = deploy.Labels
		if _, err := client.AppsV1().Deployments(targetNS).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update bridged deployment: %w", err)
		}
	}

	return nil
}

// rewriteConfigRefs rewrites all Secret/ConfigMap references in the pod spec
// (env, envFrom, volumes) using the provided NameMap keyed by GroupKind+Name.
func rewriteConfigRefs(podSpec *corev1.PodSpec, names NameMap) {
	for i := range podSpec.Containers {
		rewriteContainerRefs(&podSpec.Containers[i], names)
	}
	for i := range podSpec.InitContainers {
		rewriteContainerRefs(&podSpec.InitContainers[i], names)
	}
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Secret != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "Secret"}, Name: podSpec.Volumes[i].Secret.SecretName}]; ok {
				podSpec.Volumes[i].Secret.SecretName = mapped
			}
		}
		if podSpec.Volumes[i].ConfigMap != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "ConfigMap"}, Name: podSpec.Volumes[i].ConfigMap.Name}]; ok {
				podSpec.Volumes[i].ConfigMap.Name = mapped
			}
		}
	}
}

func rewriteContainerRefs(c *corev1.Container, names NameMap) {
	for i := range c.Env {
		if c.Env[i].ValueFrom == nil {
			continue
		}
		if ref := c.Env[i].ValueFrom.SecretKeyRef; ref != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "Secret"}, Name: ref.Name}]; ok {
				ref.Name = mapped
			}
		}
		if ref := c.Env[i].ValueFrom.ConfigMapKeyRef; ref != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "ConfigMap"}, Name: ref.Name}]; ok {
				ref.Name = mapped
			}
		}
	}
	for i := range c.EnvFrom {
		if c.EnvFrom[i].SecretRef != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "Secret"}, Name: c.EnvFrom[i].SecretRef.Name}]; ok {
				c.EnvFrom[i].SecretRef.Name = mapped
			}
		}
		if c.EnvFrom[i].ConfigMapRef != nil {
			if mapped, ok := names[ResourceKey{GroupKind: schema.GroupKind{Kind: "ConfigMap"}, Name: c.EnvFrom[i].ConfigMapRef.Name}]; ok {
				c.EnvFrom[i].ConfigMapRef.Name = mapped
			}
		}
	}
}

// copyServiceAccount copies a ServiceAccount from the source namespace into
// the target namespace with a deployment-scoped prefix. It also copies any
// secrets referenced by imagePullSecrets.
func copyServiceAccount(ctx context.Context, client kubernetes.Interface, srcNS, targetNS, saName, deployName string) error {
	sa, err := client.CoreV1().ServiceAccounts(srcNS).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service account %s/%s: %w", srcNS, saName, err)
	}

	prefix := deployName + "-"
	ownerLabels := map[string]string{
		meta.LabelBridgeType:       meta.BridgeTypeProxy,
		meta.LabelBridgeDeployment: deployName,
	}

	// Copy imagePullSecrets and rewrite references.
	for i, ref := range sa.ImagePullSecrets {
		secret, err := client.CoreV1().Secrets(srcNS).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			slog.Warn("Failed to copy imagePullSecret", "name", ref.Name, "error", err)
			continue
		}
		dstName := prefix + ref.Name
		secret.Name = dstName
		secret.Namespace = targetNS
		secret.ResourceVersion = ""
		secret.UID = ""
		secret.CreationTimestamp = metav1.Time{}
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		for k, v := range ownerLabels {
			secret.Labels[k] = v
		}
		if err := upsertSecret(ctx, client, targetNS, secret); err != nil {
			slog.Warn("Failed to upsert imagePullSecret", "name", dstName, "error", err)
			continue
		}
		sa.ImagePullSecrets[i].Name = dstName
	}

	sa.Name = prefix + saName
	sa.Namespace = targetNS
	sa.ResourceVersion = ""
	sa.UID = ""
	sa.CreationTimestamp = metav1.Time{}
	if sa.Labels == nil {
		sa.Labels = make(map[string]string)
	}
	for k, v := range ownerLabels {
		sa.Labels[k] = v
	}

	existing, err := client.CoreV1().ServiceAccounts(targetNS).Get(ctx, sa.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(targetNS).Create(ctx, sa, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	sa.ResourceVersion = existing.ResourceVersion
	_, err = client.CoreV1().ServiceAccounts(targetNS).Update(ctx, sa, metav1.UpdateOptions{})
	return err
}

// ListBridgeResources returns a Bundle of all bridge resources in the given
// namespace matching the deployment name and device ID labels.
func ListBridgeResources(ctx context.Context, client kubernetes.Interface, namespace, deployName, deviceID string) (*Bundle, error) {
	sel := meta.LabelBridgeDeployment + "=" + deployName + "," + meta.LabelDeviceID + "=" + deviceID
	listOpts := metav1.ListOptions{LabelSelector: sel}

	var resources []Resource

	if deploys, err := client.AppsV1().Deployments(namespace).List(ctx, listOpts); err == nil {
		for i := range deploys.Items {
			resources = append(resources, Resource{
				Object: &deploys.Items[i],
				GVK:    appsv1.SchemeGroupVersion.WithKind("Deployment"),
			})
		}
	}
	if svcs, err := client.CoreV1().Services(namespace).List(ctx, listOpts); err == nil {
		for i := range svcs.Items {
			resources = append(resources, Resource{
				Object: &svcs.Items[i],
				GVK:    corev1.SchemeGroupVersion.WithKind("Service"),
			})
		}
	}
	if secrets, err := client.CoreV1().Secrets(namespace).List(ctx, listOpts); err == nil {
		for i := range secrets.Items {
			resources = append(resources, Resource{
				Object: &secrets.Items[i],
				GVK:    corev1.SchemeGroupVersion.WithKind("Secret"),
			})
		}
	}
	if cms, err := client.CoreV1().ConfigMaps(namespace).List(ctx, listOpts); err == nil {
		for i := range cms.Items {
			resources = append(resources, Resource{
				Object: &cms.Items[i],
				GVK:    corev1.SchemeGroupVersion.WithKind("ConfigMap"),
			})
		}
	}
	if sas, err := client.CoreV1().ServiceAccounts(namespace).List(ctx, listOpts); err == nil {
		for i := range sas.Items {
			resources = append(resources, Resource{
				Object: &sas.Items[i],
				GVK:    corev1.SchemeGroupVersion.WithKind("ServiceAccount"),
			})
		}
	}

	return &Bundle{Resources: resources}, nil
}

// DeleteBridgeResources deletes all resources associated with a bridge deployment
// in the given namespace, identified by the bridge deployment and device ID labels.
func DeleteBridgeResources(ctx context.Context, client kubernetes.Interface, namespace, deployName, deviceID string) error {
	sel := meta.LabelBridgeDeployment + "=" + deployName + "," + meta.LabelDeviceID + "=" + deviceID
	listOpts := metav1.ListOptions{LabelSelector: sel}
	delOpts := metav1.DeleteOptions{}

	// Delete deployment
	if err := client.AppsV1().Deployments(namespace).DeleteCollection(ctx, delOpts, listOpts); err != nil && !errors.IsNotFound(err) {
		slog.Warn("Failed to delete deployments", "bridge", deployName, "error", err)
	}
	// Delete services
	if svcs, err := client.CoreV1().Services(namespace).List(ctx, listOpts); err == nil {
		for _, svc := range svcs.Items {
			if err := client.CoreV1().Services(namespace).Delete(ctx, svc.Name, delOpts); err != nil && !errors.IsNotFound(err) {
				slog.Warn("Failed to delete service", "name", svc.Name, "error", err)
			}
		}
	}
	// Delete secrets
	if err := client.CoreV1().Secrets(namespace).DeleteCollection(ctx, delOpts, listOpts); err != nil && !errors.IsNotFound(err) {
		slog.Warn("Failed to delete secrets", "bridge", deployName, "error", err)
	}
	// Delete configmaps
	if err := client.CoreV1().ConfigMaps(namespace).DeleteCollection(ctx, delOpts, listOpts); err != nil && !errors.IsNotFound(err) {
		slog.Warn("Failed to delete configmaps", "bridge", deployName, "error", err)
	}
	// Delete service accounts
	if err := client.CoreV1().ServiceAccounts(namespace).DeleteCollection(ctx, delOpts, listOpts); err != nil && !errors.IsNotFound(err) {
		slog.Warn("Failed to delete service accounts", "bridge", deployName, "error", err)
	}

	return nil
}

package resources

import (
	"github.com/vercel/bridge/pkg/k8s/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SetNamespace returns a Transformer that sets .metadata.namespace on all resources.
func SetNamespace(ns string) Transformer {
	return TransformFunc(func(_ *TransformContext, b *Bundle) error {
		for _, r := range b.Resources {
			r.Object.(metav1.Object).SetNamespace(ns)
		}
		return nil
	})
}

// PruneAllMetadata returns a Transformer that clears server-set metadata fields
// on all resources.
func PruneAllMetadata() Transformer {
	return TransformFunc(func(_ *TransformContext, b *Bundle) error {
		for _, r := range b.Resources {
			PruneMetadata(r.Object.(metav1.Object))
		}
		return nil
	})
}

// StripOrphanedVolumes returns a Transformer that removes projected volumes
// from deployments and strips volume mounts that reference non-existent
// volumes. This handles both explicitly projected volumes and volume mounts
// injected by the kubelet (e.g. kube-api-access-*) that have no matching
// volume definition in the deployment template.
func StripOrphanedVolumes() Transformer {
	return TransformFunc(func(_ *TransformContext, b *Bundle) error {
		for _, r := range b.Resources {
			deploy, ok := r.Object.(*appsv1.Deployment)
			if !ok {
				continue
			}
			spec := &deploy.Spec.Template.Spec

			// Remove projected volumes.
			var kept []corev1.Volume
			for _, v := range spec.Volumes {
				if v.Projected == nil {
					kept = append(kept, v)
				}
			}
			spec.Volumes = kept

			// Build set of remaining volume names.
			valid := make(map[string]bool, len(kept))
			for _, v := range kept {
				valid[v.Name] = true
			}

			// Strip mounts referencing removed or missing volumes.
			for i := range spec.Containers {
				spec.Containers[i].VolumeMounts = filterMounts(spec.Containers[i].VolumeMounts, valid)
			}
			for i := range spec.InitContainers {
				spec.InitContainers[i].VolumeMounts = filterMounts(spec.InitContainers[i].VolumeMounts, valid)
			}
		}
		return nil
	})
}

func filterMounts(mounts []corev1.VolumeMount, validVolumes map[string]bool) []corev1.VolumeMount {
	var kept []corev1.VolumeMount
	for _, m := range mounts {
		if validVolumes[m.Name] {
			kept = append(kept, m)
		}
	}
	return kept
}

// InjectProxyImage returns a Transformer that finds the target Deployment and
// swaps its first container for the bridge proxy. It also sets replicas to 1.
// Runs before SuffixNames — finds the deployment by tc.SourceName.
func InjectProxyImage(proxyImage string) Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deploy, err := findApplicationDeployment(b, tc.SourceName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}
		replicas := int32(1)
		deploy.Spec.Replicas = &replicas
		injectProxyImage(deploy, proxyImage)
		return nil
	})
}

// ClearClusterIPs returns a Transformer that clears ClusterIP and ClusterIPs on
// all Service resources so the cluster assigns fresh IPs on creation.
func ClearClusterIPs() Transformer {
	return TransformFunc(func(_ *TransformContext, b *Bundle) error {
		for _, r := range b.Resources {
			if svc, ok := r.Object.(*corev1.Service); ok {
				svc.Spec.ClusterIP = ""
				svc.Spec.ClusterIPs = nil
			}
		}
		return nil
	})
}

// SuffixNames returns a Transformer that appends suffix to the name of every
// resource and populates tc.NameMap with the old→new mappings.
func SuffixNames(suffix string) Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		if tc.NameMap == nil {
			tc.NameMap = make(NameMap)
		}
		for _, r := range b.Resources {
			obj := r.Object.(metav1.Object)
			oldName := obj.GetName()
			newName := oldName + suffix
			obj.SetName(newName)
			tc.NameMap[ResourceKey{GroupKind: r.GVK.GroupKind(), Name: oldName}] = newName
		}
		return nil
	})
}

// InjectLabels returns a Transformer that merges bridge labels into every
// resource, including source workload labels from tc.SourceName and
// tc.SourceNamespace.
// Runs after SuffixNames — resolves the deploy name via Lookup.
func InjectLabels() Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deployName := Lookup(NewResourceKey(tc.SourceName, "apps", "Deployment"), b, tc.NameMap)
		for _, r := range b.Resources {
			obj := r.Object.(metav1.Object)
			InjectBridgeLabels(obj, tc.DeviceID, deployName)
			labels := obj.GetLabels()
			labels[meta.LabelWorkloadSource] = tc.SourceName
			labels[meta.LabelWorkloadSourceNamespace] = tc.SourceNamespace
			obj.SetLabels(labels)
		}
		return nil
	})
}

// TransformSelectors returns a Transformer that sets the deployment's pod
// template labels and label selector to match the deployment's bridge labels.
// Runs after SuffixNames — resolves the deploy name via Lookup.
func TransformSelectors() Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deployName := Lookup(NewResourceKey(tc.SourceName, "apps", "Deployment"), b, tc.NameMap)
		deploy, err := findApplicationDeployment(b, deployName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}
		selectorLabels := map[string]string{
			meta.LabelBridgeType:       meta.BridgeTypeProxy,
			meta.LabelBridgeDeployment: deployName,
		}
		if deviceID, ok := deploy.Labels[meta.LabelDeviceID]; ok {
			selectorLabels[meta.LabelDeviceID] = deviceID
		}
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels}
		// Merge bridge labels into existing pod template labels so original
		// labels (e.g. app, version) are preserved.
		existing := deploy.Spec.Template.ObjectMeta.Labels
		if existing == nil {
			existing = make(map[string]string)
		}
		for k, v := range selectorLabels {
			existing[k] = v
		}
		deploy.Spec.Template.ObjectMeta.Labels = existing
		return nil
	})
}

// RewriteRefs returns a Transformer that rewrites ConfigMap, Secret, and
// ServiceAccount references in the deployment's pod spec using tc.NameMap.
// Runs after SuffixNames — resolves the deploy name via Lookup.
func RewriteRefs() Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deployName := Lookup(NewResourceKey(tc.SourceName, "apps", "Deployment"), b, tc.NameMap)
		deploy, err := findApplicationDeployment(b, deployName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}
		if saName := deploy.Spec.Template.Spec.ServiceAccountName; saName != "" {
			if mapped, ok := tc.NameMap[NewResourceKey(saName, "", "ServiceAccount")]; ok {
				deploy.Spec.Template.Spec.ServiceAccountName = mapped
			}
		}
		rewriteConfigRefs(&deploy.Spec.Template.Spec, tc.NameMap)
		return nil
	})
}

// AppendBridgeService returns a Transformer that creates a bridge Service and
// appends it to the bundle. It reads the target port from the deployment's
// container ports. If tc.DeviceID is non-empty, bridge labels are injected on
// the service.
// Runs after SuffixNames — resolves the deploy name via Lookup.
func AppendBridgeService(ns string) Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deployName := Lookup(NewResourceKey(tc.SourceName, "apps", "Deployment"), b, tc.NameMap)
		deploy, err := findApplicationDeployment(b, deployName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}
		svc := NewBridgeService(ns, deployName, serviceTargetPort(deploy))
		if tc.DeviceID != "" {
			InjectBridgeLabels(svc, tc.DeviceID, deployName)
		}
		b.Resources = append(b.Resources, Resource{
			Object: svc,
			GVK:    corev1.SchemeGroupVersion.WithKind("Service"),
		})
		return nil
	})
}

// NewBridgeService creates a ClusterIP Service that maps port 80 to the given
// target port, selecting pods by LabelBridgeDeployment.
func NewBridgeService(namespace, deployName string, targetPort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				meta.LabelBridgeDeployment: deployName,
			},
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
}

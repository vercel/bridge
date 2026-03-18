package resources

import (
	"github.com/vercel/bridge/pkg/k8s/meta"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResourceKey uniquely identifies a resource by its GroupKind and name.
type ResourceKey struct {
	schema.GroupKind
	Name string
}

// NameMap tracks old-name → new-name mappings keyed by GroupKind so that
// resources of different kinds with the same name don't collide.
type NameMap map[ResourceKey]string

// NewResourceKey creates a ResourceKey from a name, API group, and kind.
func NewResourceKey(name, group, kind string) ResourceKey {
	return ResourceKey{GroupKind: schema.GroupKind{Group: group, Kind: kind}, Name: name}
}

// Lookup resolves the current name of a resource in the bundle. If the bundle
// contains a resource matching the key directly, its name is returned. Otherwise
// the NameMap is consulted to find the renamed (post-prefix) name.
func Lookup(key ResourceKey, b *Bundle, nm NameMap) string {
	for _, r := range b.Resources {
		if r.GVK.GroupKind() == key.GroupKind && r.Object.(metav1.Object).GetName() == key.Name {
			return key.Name
		}
	}
	if nm != nil {
		if mapped, ok := nm[key]; ok {
			return mapped
		}
	}
	return key.Name
}

// PruneMetadata clears server-set fields so the object can be created fresh.
func PruneMetadata(obj metav1.Object) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
}

// InjectBridgeLabels merges the standard bridge labels into the object's
// existing labels, initialising the map if necessary.
func InjectBridgeLabels(obj metav1.Object, deviceID, deploymentName, bridgeName string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[meta.LabelBridgeType] = meta.BridgeTypeProxy
	labels[meta.LabelBridgeDeployment] = deploymentName
	labels[meta.LabelDeviceID] = deviceID
	if bridgeName != "" {
		labels[meta.LabelBridgeName] = bridgeName
	}
	obj.SetLabels(labels)
}

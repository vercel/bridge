package resources

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Resource wraps a Kubernetes object with its GVK.
type Resource struct {
	Object runtime.Object
	GVK    schema.GroupVersionKind
}

// Bundle holds resources to transform and save.
type Bundle struct {
	Resources []Resource
}

// TransformContext carries shared state through the transform pipeline.
type TransformContext struct {
	context.Context
	NameMap         NameMap // built by SuffixNames, read by RewriteRefs
	DeviceID        string  // device that owns this bridge
	SourceName      string  // original deployment name (before prefix)
	SourceNamespace string  // namespace the source came from (target NS for manifests/simple)
}

// FindDeploymentName returns the name of the first Deployment in the bundle.
// Returns an empty string if no Deployment is found.
func FindDeploymentName(b *Bundle) string {
	for _, r := range b.Resources {
		if deploy, ok := r.Object.(*appsv1.Deployment); ok {
			return deploy.Name
		}
	}
	return ""
}

// FindNamespace returns the namespace of the first Deployment in the bundle.
// Returns an empty string if no Deployment is found or its namespace is unset.
func FindNamespace(b *Bundle) string {
	for _, r := range b.Resources {
		if deploy, ok := r.Object.(*appsv1.Deployment); ok {
			return deploy.Namespace
		}
	}
	return ""
}

// Transformer mutates a Bundle.
type Transformer interface {
	Apply(tc *TransformContext, b *Bundle) error
}

// TransformFunc adapts a function to the Transformer interface.
type TransformFunc func(tc *TransformContext, b *Bundle) error

func (f TransformFunc) Apply(tc *TransformContext, b *Bundle) error {
	return f(tc, b)
}

// Transform runs a chain of transforms on a bundle in order.
func Transform(tc *TransformContext, b *Bundle, transforms []Transformer) error {
	for _, t := range transforms {
		if err := t.Apply(tc, b); err != nil {
			return err
		}
	}
	return nil
}

// GetGRPCPort returns the gRPC port from the named deployment's first container.
// It looks for a port named "grpc", falling back to the first port.
func GetGRPCPort(b *Bundle, deployName string) (int32, error) {
	deploy, err := findApplicationDeployment(b, deployName)
	if err != nil {
		return 0, &DeploymentNotFoundError{Name: deployName}
	}
	return grpcPort(deploy), nil
}

// GetAppPorts returns the application listen ports from the named deployment's
// first container — all ports except the one named "grpc".
func GetAppPorts(b *Bundle, deployName string) ([]int32, error) {
	deploy, err := findApplicationDeployment(b, deployName)
	if err != nil {
		return nil, &DeploymentNotFoundError{Name: deployName}
	}
	return appPorts(deploy), nil
}

// GetVolumeMountPaths returns the volume mount paths from the named
// deployment's first container.
func GetVolumeMountPaths(b *Bundle, deployName string) ([]string, error) {
	deploy, err := findApplicationDeployment(b, deployName)
	if err != nil {
		return nil, &DeploymentNotFoundError{Name: deployName}
	}
	return volumeMountPaths(deploy), nil
}

func grpcPort(deploy *appsv1.Deployment) int32 {
	if containers := deploy.Spec.Template.Spec.Containers; len(containers) > 0 {
		for _, p := range containers[0].Ports {
			if p.Name == "grpc" {
				return p.ContainerPort
			}
		}
		if len(containers[0].Ports) > 0 {
			return containers[0].Ports[0].ContainerPort
		}
	}
	return 0
}

func appPorts(deploy *appsv1.Deployment) []int32 {
	if containers := deploy.Spec.Template.Spec.Containers; len(containers) > 0 {
		var ports []int32
		for _, p := range containers[0].Ports {
			if p.Name != "grpc" {
				ports = append(ports, p.ContainerPort)
			}
		}
		return ports
	}
	return nil
}

func volumeMountPaths(deploy *appsv1.Deployment) []string {
	if containers := deploy.Spec.Template.Spec.Containers; len(containers) > 0 {
		var paths []string
		for _, vm := range containers[0].VolumeMounts {
			paths = append(paths, vm.MountPath)
		}
		return paths
	}
	return nil
}

// serviceTargetPort returns the port a bridge Service should target: the first
// app port if any exist, otherwise the gRPC port.
func serviceTargetPort(deploy *appsv1.Deployment) int32 {
	if ports := appPorts(deploy); len(ports) > 0 {
		return ports[0]
	}
	return grpcPort(deploy)
}

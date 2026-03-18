package resources

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

const (
	facadeConfigMapName = "bridge-server-facades"
	facadeVolumeName    = "bridge-server-facades"
	facadeMountPath     = "/etc/bridge/server-facades"
)

// InjectServerFacades returns a Transformer that creates a ConfigMap containing
// server facade specs, mounts it into the proxy pod, and adds --server-facades flags
// pointing to each mounted file.
// Runs before SuffixNames — finds the deployment by tc.SourceName.
func InjectServerFacades(facades []*bridgev1.ServerFacade) Transformer {
	if len(facades) == 0 {
		return TransformFunc(func(_ *TransformContext, _ *Bundle) error { return nil })
	}
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deploy, err := findApplicationDeployment(b, tc.SourceName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}

		// Serialize each facade to JSON for the ConfigMap.
		data := make(map[string]string, len(facades))
		for i, f := range facades {
			j, err := protojson.Marshal(f)
			if err != nil {
				return fmt.Errorf("marshal server facade %d: %w", i, err)
			}
			data[fmt.Sprintf("facade-%d.json", i)] = string(j)
		}

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      facadeConfigMapName,
				Namespace: deploy.Namespace,
			},
			Data: data,
		}
		b.Resources = append(b.Resources, Resource{
			Object: cm,
			GVK:    corev1.SchemeGroupVersion.WithKind("ConfigMap"),
		})

		mountFacadeConfigMap(deploy, len(facades))

		return nil
	})
}

func mountFacadeConfigMap(deploy *appsv1.Deployment, count int) {
	spec := &deploy.Spec.Template.Spec

	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: facadeVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: facadeConfigMapName,
				},
			},
		},
	})

	if len(spec.Containers) > 0 {
		c := &spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      facadeVolumeName,
			MountPath: facadeMountPath,
			ReadOnly:  true,
		})
		for i := range count {
			c.Command = append(c.Command, "--server-facades", fmt.Sprintf("%s/facade-%d.json", facadeMountPath, i))
		}
	}
}

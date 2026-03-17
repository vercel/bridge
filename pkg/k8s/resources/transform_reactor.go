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
	reactorConfigMapName = "bridge-reactors"
	reactorVolumeName    = "bridge-reactors"
	reactorMountPath     = "/etc/bridge/reactors"
)

// InjectReactors returns a Transformer that creates a ConfigMap containing
// reactor specs, mounts it into the proxy pod, and adds --reactors flags
// pointing to each mounted file.
// Runs before SuffixNames — finds the deployment by tc.SourceName.
func InjectReactors(reactors []*bridgev1.Reactor) Transformer {
	if len(reactors) == 0 {
		return TransformFunc(func(_ *TransformContext, _ *Bundle) error { return nil })
	}
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		deploy, err := findApplicationDeployment(b, tc.SourceName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}

		// Serialize each reactor to JSON for the ConfigMap.
		data := make(map[string]string, len(reactors))
		for i, r := range reactors {
			j, err := protojson.Marshal(r)
			if err != nil {
				return fmt.Errorf("marshal reactor %d: %w", i, err)
			}
			data[fmt.Sprintf("reactor-%d.json", i)] = string(j)
		}

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      reactorConfigMapName,
				Namespace: deploy.Namespace,
			},
			Data: data,
		}
		b.Resources = append(b.Resources, Resource{
			Object: cm,
			GVK:    corev1.SchemeGroupVersion.WithKind("ConfigMap"),
		})

		mountReactorConfigMap(deploy, len(reactors))

		return nil
	})
}

func mountReactorConfigMap(deploy *appsv1.Deployment, count int) {
	spec := &deploy.Spec.Template.Spec

	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: reactorVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: reactorConfigMapName,
				},
			},
		},
	})

	if len(spec.Containers) > 0 {
		c := &spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      reactorVolumeName,
			MountPath: reactorMountPath,
			ReadOnly:  true,
		})
		for i := range count {
			c.Command = append(c.Command, "--reactors", fmt.Sprintf("%s/reactor-%d.json", reactorMountPath, i))
		}
	}
}

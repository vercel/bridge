package resources

import (
	"fmt"
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// WorkloadKind is a kind of workload a bridge can be created from.
type WorkloadKind interface {
	// GVK is the workload's group, version and kind.
	GVK() schema.GroupVersionKind
	// PodTemplatePath is the field path of the workload's pod template.
	PodTemplatePath() []string
}

type deploymentKind struct{}

func (deploymentKind) GVK() schema.GroupVersionKind {
	return appsv1.SchemeGroupVersion.WithKind("Deployment")
}
func (deploymentKind) PodTemplatePath() []string { return []string{"spec", "template"} }

type statefulSetKind struct{}

func (statefulSetKind) GVK() schema.GroupVersionKind {
	return appsv1.SchemeGroupVersion.WithKind("StatefulSet")
}
func (statefulSetKind) PodTemplatePath() []string { return []string{"spec", "template"} }

type daemonSetKind struct{}

func (daemonSetKind) GVK() schema.GroupVersionKind {
	return appsv1.SchemeGroupVersion.WithKind("DaemonSet")
}
func (daemonSetKind) PodTemplatePath() []string { return []string{"spec", "template"} }

type replicaSetKind struct{}

func (replicaSetKind) GVK() schema.GroupVersionKind {
	return appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
}
func (replicaSetKind) PodTemplatePath() []string { return []string{"spec", "template"} }

type jobKind struct{}

func (jobKind) GVK() schema.GroupVersionKind {
	return batchv1.SchemeGroupVersion.WithKind("Job")
}
func (jobKind) PodTemplatePath() []string { return []string{"spec", "template"} }

type cronJobKind struct{}

func (cronJobKind) GVK() schema.GroupVersionKind {
	return batchv1.SchemeGroupVersion.WithKind("CronJob")
}
func (cronJobKind) PodTemplatePath() []string {
	return []string{"spec", "jobTemplate", "spec", "template"}
}

// workloadKinds are the workload kinds a bridge can be created from, in the
// order a name is looked up and a bundle's workloads are ordered.
var workloadKinds = []WorkloadKind{
	deploymentKind{},
	statefulSetKind{},
	daemonSetKind{},
	replicaSetKind{},
	jobKind{},
	cronJobKind{},
}

// workloadKindIndex returns gvk's position in workloadKinds, or -1 if it isn't a
// workload kind.
func workloadKindIndex(gvk schema.GroupVersionKind) int {
	for i, kind := range workloadKinds {
		if k := kind.GVK(); k.Group == gvk.Group && k.Kind == gvk.Kind {
			return i
		}
	}
	return -1
}

// DeploymentFromWorkload returns a single-replica Deployment built from a
// workload's pod template and metadata, so the rest of the pipeline only ever
// deals with Deployments.
//
// A bridge built from a Job or CronJob runs its pod, not its schedule or
// completion: run the job in the devcontainer with `bridge exec`.
func DeploymentFromWorkload(kind WorkloadKind, obj *unstructured.Unstructured) (*appsv1.Deployment, error) {
	path := kind.PodTemplatePath()
	raw, found, err := unstructured.NestedMap(obj.Object, path...)
	if err != nil || !found {
		return nil, fmt.Errorf("%s %s has no pod template at %s", kind.GVK().Kind, obj.GetName(), strings.Join(path, "."))
	}
	var template corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &template); err != nil {
		return nil, fmt.Errorf("invalid pod template in %s %s: %w", kind.GVK().Kind, obj.GetName(), err)
	}
	// A Deployment's pods must restart Always and can't set a deadline.
	template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	template.Spec.ActiveDeadlineSeconds = nil
	if len(template.Labels) == 0 {
		template.Labels = map[string]string{"app": obj.GetName()}
	}

	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        obj.GetName(),
			Namespace:   obj.GetNamespace(),
			Labels:      obj.GetLabels(),
			Annotations: obj.GetAnnotations(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: maps.Clone(template.Labels)},
			Template: template,
		},
	}, nil
}

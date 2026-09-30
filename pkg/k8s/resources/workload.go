package resources

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// workloadKinds are the workload kinds a bridge can be created from, in the
// order a name is looked up and a bundle's workloads are ordered.
var workloadKinds = []schema.GroupVersionKind{
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	appsv1.SchemeGroupVersion.WithKind("DaemonSet"),
	appsv1.SchemeGroupVersion.WithKind("ReplicaSet"),
	batchv1.SchemeGroupVersion.WithKind("Job"),
	batchv1.SchemeGroupVersion.WithKind("CronJob"),
}

// podTemplatePaths are where a workload keeps its pod template: spec.template,
// or spec.jobTemplate.spec.template for a CronJob.
var podTemplatePaths = [][]string{
	{"spec", "template"},
	{"spec", "jobTemplate", "spec", "template"},
}

// DeploymentFromWorkload returns a single-replica Deployment built from any
// workload's pod template and metadata, so the rest of the pipeline only ever
// deals with Deployments. It returns nil when obj has no pod template.
//
// A bridge built from a Job or CronJob runs its pod, not its schedule or
// completion: run the job in the devcontainer with `bridge exec`.
func DeploymentFromWorkload(obj *unstructured.Unstructured) (*appsv1.Deployment, error) {
	template, err := podTemplate(obj)
	if err != nil || template == nil {
		return nil, err
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
			Template: *template,
		},
	}, nil
}

// podTemplate returns obj's pod template: the first of podTemplatePaths that
// holds one with containers, or nil.
func podTemplate(obj *unstructured.Unstructured) (*corev1.PodTemplateSpec, error) {
	for _, path := range podTemplatePaths {
		raw, found, err := unstructured.NestedMap(obj.Object, path...)
		if err != nil || !found {
			continue
		}
		if _, ok, _ := unstructured.NestedSlice(raw, "spec", "containers"); !ok {
			continue
		}
		var template corev1.PodTemplateSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &template); err != nil {
			return nil, fmt.Errorf("invalid pod template in %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		return &template, nil
	}
	return nil, nil
}

// workloadRank orders a bundle's workloads by workloadKinds, so a Deployment is
// its source whenever it has one.
func workloadRank(gvk schema.GroupVersionKind) int {
	for i, k := range workloadKinds {
		if k.Group == gvk.Group && k.Kind == gvk.Kind {
			return i
		}
	}
	return len(workloadKinds)
}

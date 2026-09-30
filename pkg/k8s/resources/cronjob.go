package resources

import (
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeploymentFromCronJob returns a single-replica Deployment that runs the
// CronJob's pod template, so a CronJob can be bridged like a Deployment. The
// bridge gets the job's service account, environment and volumes, but not its
// schedule: run the job in the devcontainer with `bridge exec`.
func DeploymentFromCronJob(cj *batchv1.CronJob) *appsv1.Deployment {
	template := *cj.Spec.JobTemplate.Spec.Template.DeepCopy()
	// A Deployment's pods must restart Always and can't set a deadline.
	template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	template.Spec.ActiveDeadlineSeconds = nil
	if len(template.Labels) == 0 {
		template.Labels = map[string]string{"app": cj.Name}
	}

	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cj.Name,
			Namespace:   cj.Namespace,
			Labels:      maps.Clone(cj.Labels),
			Annotations: maps.Clone(cj.Annotations),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: maps.Clone(template.Labels)},
			Template: template,
		},
	}
}

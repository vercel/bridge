package resources

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/kubernetes/fake"
)

const testCronJobManifest = `apiVersion: batch/v1
kind: CronJob
metadata:
  name: my-cron
  labels:
    team: platform
spec:
  schedule: "0 * * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            app: my-cron
        spec:
          serviceAccountName: my-cron-sa
          restartPolicy: Never
          activeDeadlineSeconds: 600
          containers:
          - name: job
            image: my-cron:latest
            env:
            - name: FOO
              value: bar
`

func TestDeploymentFromCronJob(t *testing.T) {
	deadline := int64(600)
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-cron",
			Namespace:   "jobs",
			Labels:      map[string]string{"team": "platform"},
			Annotations: map[string]string{"owner": "me"},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-cron"}},
				Spec: corev1.PodSpec{
					ServiceAccountName:    "my-cron-sa",
					RestartPolicy:         corev1.RestartPolicyOnFailure,
					ActiveDeadlineSeconds: &deadline,
					Containers:            []corev1.Container{{Name: "job", Image: "my-cron:latest"}},
				},
			}}},
		},
	}

	deploy := DeploymentFromCronJob(cj)

	assert.Equal(t, "my-cron", deploy.Name)
	assert.Equal(t, "jobs", deploy.Namespace)
	assert.Equal(t, map[string]string{"team": "platform"}, deploy.Labels)
	assert.Equal(t, map[string]string{"owner": "me"}, deploy.Annotations)
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
	assert.Equal(t, map[string]string{"app": "my-cron"}, deploy.Spec.Selector.MatchLabels)

	pod := deploy.Spec.Template.Spec
	assert.Equal(t, "my-cron-sa", pod.ServiceAccountName)
	assert.Equal(t, corev1.RestartPolicyAlways, pod.RestartPolicy)
	assert.Nil(t, pod.ActiveDeadlineSeconds)

	// The CronJob itself is left untouched.
	assert.Equal(t, corev1.RestartPolicyOnFailure, cj.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy)
	assert.NotNil(t, cj.Spec.JobTemplate.Spec.Template.Spec.ActiveDeadlineSeconds)
}

func TestDeploymentFromCronJob_UnlabeledTemplate(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "my-cron"}}

	deploy := DeploymentFromCronJob(cj)

	assert.Equal(t, map[string]string{"app": "my-cron"}, deploy.Spec.Template.Labels)
	assert.Equal(t, map[string]string{"app": "my-cron"}, deploy.Spec.Selector.MatchLabels)
}

func TestCreateFromManifests_CronJob(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{"cron.yaml": testCronJobManifest})

	client := k8stesting.NewSimpleClientset()
	deployName, bundle, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)
	assert.Equal(t, testDeployName("my-cron"), deployName)

	for _, r := range bundle.Resources {
		_, isCronJob := r.Object.(*batchv1.CronJob)
		assert.False(t, isCronJob, "the bridged CronJob must not also be saved as a CronJob")
	}

	deploy, err := client.AppsV1().Deployments("default").Get(context.Background(), deployName, metav1.GetOptions{})
	require.NoError(t, err)
	pod := deploy.Spec.Template.Spec
	assert.Equal(t, "my-cron-sa", pod.ServiceAccountName)
	assert.Equal(t, corev1.RestartPolicyAlways, pod.RestartPolicy)
	assert.Nil(t, pod.ActiveDeadlineSeconds)
	assert.Contains(t, pod.Containers[0].Env, corev1.EnvVar{Name: "FOO", Value: "bar"})
}

func TestSourceFromManifests_PrefersDeploymentToCronJob(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"cron.yaml": testCronJobManifest,
		"deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
      - name: app
        image: my-app:latest
`,
	})

	bundle, err := SourceFromManifests(manifests)
	require.NoError(t, err)
	assert.Equal(t, "my-app", FindDeploymentName(bundle))

	var deployments, cronJobs int
	for _, r := range bundle.Resources {
		switch r.Object.(type) {
		case *appsv1.Deployment:
			deployments++
		case *batchv1.CronJob:
			cronJobs++
		}
	}
	assert.Equal(t, 1, deployments)
	assert.Equal(t, 1, cronJobs, "a CronJob next to a Deployment is kept as it is")
}

func TestSourceFromNamespace_CronJob(t *testing.T) {
	client := k8stesting.NewSimpleClientset(&batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cron", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "my-cron-sa"}},
		}}},
	})

	bundle, err := SourceFromNamespace(context.Background(), client, "default", "my-cron")
	require.NoError(t, err)
	require.Len(t, bundle.Resources, 1)
	deploy, ok := bundle.Resources[0].Object.(*appsv1.Deployment)
	require.True(t, ok)
	assert.Equal(t, "my-cron", deploy.Name)
	assert.Equal(t, "my-cron-sa", deploy.Spec.Template.Spec.ServiceAccountName)
}

func TestSourceFromNamespace_NeitherFound(t *testing.T) {
	_, err := SourceFromNamespace(context.Background(), k8stesting.NewSimpleClientset(), "default", "missing")

	var notFound *DeploymentNotFoundError
	assert.ErrorAs(t, err, &notFound)
}

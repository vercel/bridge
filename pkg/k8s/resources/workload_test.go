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
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// podTemplateYAML is a pod template, indented to sit under a `template:` key
// at the given depth.
func podTemplateYAML(indent string) string {
	return indent + `metadata:
` + indent + `  labels:
` + indent + `    app: my-workload
` + indent + `spec:
` + indent + `  serviceAccountName: my-sa
` + indent + `  restartPolicy: Never
` + indent + `  activeDeadlineSeconds: 600
` + indent + `  containers:
` + indent + `  - name: main
` + indent + `    image: my-workload:latest
` + indent + `    env:
` + indent + `    - name: FOO
` + indent + `      value: bar
`
}

const workloadMetadata = `metadata:
  name: my-workload
  namespace: jobs
  labels:
    team: platform
  annotations:
    owner: me
`

func workloadYAML(apiVersion, kind string) string {
	head := "apiVersion: " + apiVersion + "\nkind: " + kind + "\n" + workloadMetadata
	if kind == "CronJob" {
		return head + "spec:\n  schedule: \"0 * * * *\"\n  jobTemplate:\n    spec:\n      template:\n" + podTemplateYAML("        ")
	}
	return head + "spec:\n  template:\n" + podTemplateYAML("    ")
}

func TestDeploymentFromWorkload_EveryKind(t *testing.T) {
	for _, kind := range workloadKinds {
		gvk := kind.GVK()
		t.Run(gvk.Kind, func(t *testing.T) {
			obj, err := decodeUnstructured([]byte(workloadYAML(gvk.GroupVersion().String(), gvk.Kind)))
			require.NoError(t, err)

			deploy, err := DeploymentFromWorkload(kind, obj)
			require.NoError(t, err)
			require.NotNil(t, deploy)

			assert.Equal(t, "my-workload", deploy.Name)
			assert.Equal(t, "jobs", deploy.Namespace)
			assert.Equal(t, map[string]string{"team": "platform"}, deploy.Labels)
			assert.Equal(t, map[string]string{"owner": "me"}, deploy.Annotations)
			require.NotNil(t, deploy.Spec.Replicas)
			assert.Equal(t, int32(1), *deploy.Spec.Replicas)
			assert.Equal(t, map[string]string{"app": "my-workload"}, deploy.Spec.Selector.MatchLabels)

			pod := deploy.Spec.Template.Spec
			assert.Equal(t, "my-sa", pod.ServiceAccountName)
			assert.Equal(t, corev1.RestartPolicyAlways, pod.RestartPolicy)
			assert.Nil(t, pod.ActiveDeadlineSeconds)
			require.Len(t, pod.Containers, 1)
			assert.Contains(t, pod.Containers[0].Env, corev1.EnvVar{Name: "FOO", Value: "bar"})
		})
	}
}

func TestDeploymentFromWorkload_NoPodTemplate(t *testing.T) {
	obj, err := decodeUnstructured([]byte("apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: my-cron\nspec:\n  schedule: \"0 * * * *\"\n"))
	require.NoError(t, err)

	_, err = DeploymentFromWorkload(&cronJobKind{}, obj)
	assert.ErrorContains(t, err, "no pod template at spec.jobTemplate.spec.template")
}

func TestDeploymentFromWorkload_UnlabeledTemplate(t *testing.T) {
	obj, err := decodeUnstructured([]byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: my-job
spec:
  template:
    spec:
      containers:
      - name: main
        image: my-job:latest
`))
	require.NoError(t, err)

	deploy, err := DeploymentFromWorkload(&jobKind{}, obj)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"app": "my-job"}, deploy.Spec.Template.Labels)
	assert.Equal(t, map[string]string{"app": "my-job"}, deploy.Spec.Selector.MatchLabels)
}

func TestCreateFromManifests_CronJob(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"cron.yaml": workloadYAML("batch/v1", "CronJob"),
	})

	client := k8stesting.NewSimpleClientset()
	deployName, bundle, err := testCreateFromManifests(t, client, manifests, "default")
	require.NoError(t, err)
	assert.Equal(t, testDeployName("my-workload"), deployName)

	for _, r := range bundle.Resources {
		_, isCronJob := r.Object.(*batchv1.CronJob)
		assert.False(t, isCronJob, "the CronJob must be bridged, not saved as a CronJob")
	}

	deploy, err := client.AppsV1().Deployments("default").Get(context.Background(), deployName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "my-sa", deploy.Spec.Template.Spec.ServiceAccountName)
	assert.Equal(t, corev1.RestartPolicyAlways, deploy.Spec.Template.Spec.RestartPolicy)
}

func TestSourceFromManifests_DeploymentIsTheSource(t *testing.T) {
	manifests := packTestManifests(t, map[string]string{
		"cron.yaml":   workloadYAML("batch/v1", "CronJob"),
		"config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config\ndata:\n  a: b\n",
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

	var deployments []string
	var configMaps int
	for _, r := range bundle.Resources {
		switch o := r.Object.(type) {
		case *appsv1.Deployment:
			deployments = append(deployments, o.Name)
		case *corev1.ConfigMap:
			configMaps++
		}
	}
	assert.Equal(t, []string{"my-app", "my-workload"}, deployments)
	assert.Equal(t, 1, configMaps, "a ConfigMap is kept as a ConfigMap")
}

func TestSourceFromNamespace(t *testing.T) {
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-workload"}},
		Spec: corev1.PodSpec{
			ServiceAccountName: "cron-sa",
			Containers:         []corev1.Container{{Name: "main", Image: "my-workload:latest"}},
		},
	}
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "my-workload", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
			Spec: batchv1.JobSpec{Template: template},
		}},
	}

	t.Run("finds a CronJob", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, cronJob)

		bundle, err := SourceFromNamespace(context.Background(), client, "default", "my-workload")
		require.NoError(t, err)
		require.Len(t, bundle.Resources, 1)
		deploy, ok := bundle.Resources[0].Object.(*appsv1.Deployment)
		require.True(t, ok)
		assert.Equal(t, "my-workload", deploy.Name)
		assert.Equal(t, "cron-sa", deploy.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("prefers a Deployment of the same name", func(t *testing.T) {
		deployTemplate := *template.DeepCopy()
		deployTemplate.Spec.ServiceAccountName = "deploy-sa"
		client := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, cronJob, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "my-workload", Namespace: "default"},
			Spec:       appsv1.DeploymentSpec{Template: deployTemplate},
		})

		bundle, err := SourceFromNamespace(context.Background(), client, "default", "my-workload")
		require.NoError(t, err)
		deploy := bundle.Resources[0].Object.(*appsv1.Deployment)
		assert.Equal(t, "deploy-sa", deploy.Spec.Template.Spec.ServiceAccountName)
	})

	t.Run("is not found when no workload has the name", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)

		_, err := SourceFromNamespace(context.Background(), client, "default", "missing")
		var notFound *WorkloadNotFoundError
		assert.ErrorAs(t, err, &notFound)
	})
}

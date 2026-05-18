package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStripOrphanedVolumes_PreservesExplicitProjectedVolumes verifies that
// explicitly declared projected volumes (e.g. AKS workload identity tokens,
// IRSA tokens with stable names) survive the transform. The previous
// implementation stripped every projected volume unconditionally, breaking
// Cosmos auth for services using workload identity.
func TestStripOrphanedVolumes_PreservesExplicitProjectedVolumes(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-devbox"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "api-devbox",
						VolumeMounts: []corev1.VolumeMount{
							{Name: "azure-identity-token", MountPath: "/var/run/secrets/azure/tokens"},
							{Name: "aws-iam-token", MountPath: "/var/run/secrets/eks.amazonaws.com/serviceaccount"},
							{Name: "kube-api-access-j2fc9", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"},
							{Name: "decrypted-env", MountPath: "/usr/share"},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "azure-identity-token",
							VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{
									ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
										Audience: "api://AzureADTokenExchange",
										Path:     "token",
									},
								}},
							}},
						},
						{
							Name: "aws-iam-token",
							VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{
									ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
										Audience: "sts.amazonaws.com",
										Path:     "token",
									},
								}},
							}},
						},
						{
							Name: "kube-api-access-j2fc9",
							VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{
									{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
								},
							}},
						},
						{
							Name:         "decrypted-env",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}

	bundle := &Bundle{Resources: []*Resource{{Object: deploy}}}
	require.NoError(t, StripOrphanedVolumes().Transform(&TransformContext{}, bundle))

	gotVolumeNames := make([]string, 0, len(deploy.Spec.Template.Spec.Volumes))
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		gotVolumeNames = append(gotVolumeNames, v.Name)
	}
	assert.ElementsMatch(t,
		[]string{"azure-identity-token", "aws-iam-token", "decrypted-env"},
		gotVolumeNames,
		"explicit projected volumes must survive; only kube-api-access-* should be stripped",
	)

	gotMountNames := make([]string, 0)
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		gotMountNames = append(gotMountNames, m.Name)
	}
	assert.ElementsMatch(t,
		[]string{"azure-identity-token", "aws-iam-token", "decrypted-env"},
		gotMountNames,
		"mounts referencing kept volumes preserved; mount for stripped kube-api-access-* removed",
	)
}

// TestStripOrphanedVolumes_StripsKubeAPIAccess verifies the targeted strip
// still happens — the kubelet-injected SA token mount is correctly removed.
func TestStripOrphanedVolumes_StripsKubeAPIAccess(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         "x",
						VolumeMounts: []corev1.VolumeMount{{Name: "kube-api-access-abcde", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "kube-api-access-abcde",
						VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
							},
						}},
					}},
				},
			},
		},
	}

	bundle := &Bundle{Resources: []*Resource{{Object: deploy}}}
	require.NoError(t, StripOrphanedVolumes().Transform(&TransformContext{}, bundle))

	assert.Empty(t, deploy.Spec.Template.Spec.Volumes, "kube-api-access-* volume should be stripped")
	assert.Empty(t, deploy.Spec.Template.Spec.Containers[0].VolumeMounts, "mount referencing stripped volume should be removed")
}

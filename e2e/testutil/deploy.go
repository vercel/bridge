package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/vercel/bridge/pkg/k8s/manifests"
)

const (
	// AdministratorNamespace is where the administrator runs.
	AdministratorNamespace = "bridge"

	// UserserviceNamespace is where the userservice workload runs.
	UserserviceNamespace = "userservice"

	// UserserviceName is the deployment name for the userservice.
	UserserviceName = "userservice"

	// UserserviceServiceName is the Kubernetes Service name for the userservice.
	UserserviceServiceName = "svc"

	// UserservicePort is the Kubernetes Service port for the userservice.
	UserservicePort = 80
)

// DeployAdministrator applies the administrator manifests and waits for the
// pod to be ready. The image must already be pushed to the test registry.
func DeployAdministrator(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, imageRef, proxyImageRef string) (*corev1.Pod, error) {
	projectRoot, err := FindProjectRoot()
	if err != nil {
		return nil, err
	}

	manifestPath := filepath.Join(projectRoot, "deploy", "k8s", "administrator.yaml")
	if err := manifests.Apply(ctx, cfg, manifestPath, map[string]string{
		"{{ADMINISTRATOR_IMAGE}}": imageRef,
		"{{PROXY_IMAGE}}":         proxyImageRef,
	}); err != nil {
		return nil, fmt.Errorf("apply administrator manifests: %w", err)
	}

	slog.Info("Administrator deployed", "namespace", AdministratorNamespace, "image", imageRef)

	if err := WaitForDeploymentReady(ctx, clientset, AdministratorNamespace, "administrator", 1*time.Minute); err != nil {
		return nil, fmt.Errorf("administrator not ready: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(AdministratorNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=administrator",
	})
	if err != nil {
		return nil, fmt.Errorf("list administrator pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no administrator pods found")
	}

	return &pods.Items[0], nil
}

// DeployUserservice applies the userservice manifests and waits for the pod
// to be ready. The image must already be pushed to the test registry.
func DeployUserservice(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, imageRef string) error {
	projectRoot, err := FindProjectRoot()
	if err != nil {
		return err
	}

	manifestPath := filepath.Join(projectRoot, "deploy", "k8s", "testserver.yaml")
	if err := manifests.Apply(ctx, cfg, manifestPath, map[string]string{
		"{{USERSERVICE_IMAGE}}": imageRef,
	}); err != nil {
		return fmt.Errorf("apply userservice manifests: %w", err)
	}

	slog.Info("Userservice deployed", "namespace", UserserviceNamespace, "image", imageRef)

	if err := WaitForDeploymentReady(ctx, clientset, UserserviceNamespace, UserserviceName, 1*time.Minute); err != nil {
		return fmt.Errorf("userservice not ready: %w", err)
	}

	return nil
}

// WaitForDeploymentReady polls until the named Deployment has at least one
// ready replica, or the timeout elapses.
func WaitForDeploymentReady(ctx context.Context, clientset kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	slog.Info("Waiting for deployment to be ready", "namespace", namespace, "name", name)

	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // retry on transient errors
		}
		return deploy.Status.ReadyReplicas > 0, nil
	})
}

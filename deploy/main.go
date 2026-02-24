// deploy seeds a local k3d cluster with the bridge administrator and test server.
//
// Usage:
//
//	go run deploy/main.go [-registry addr]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/manifests"
)

const (
	// pushRegistry is the host-accessible registry address used for docker push.
	pushRegistry = "k3d-bridge-registry.localhost:5111"
	// pullRegistry is the in-cluster registry address used in pod image references.
	// k3d maps this to the internal registry endpoint via registries.yaml.
	pullRegistry = "bridge-registry:5111"
)

func main() {
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	root, err := findProjectRoot()
	if err != nil {
		log.Fatalf("Failed to find project root: %v", err)
	}

	// Push images use the host-accessible registry; pull images use the in-cluster name.
	adminPushImage := pushRegistry + "/administrator:latest"
	adminPullImage := pullRegistry + "/administrator:latest"
	userservicePushImage := pushRegistry + "/userservice:latest"
	userservicePullImage := pullRegistry + "/userservice:latest"

	// 1. Build and push images.
	log.Println("Building administrator image...")
	dockerBuild(root, filepath.Join("services", "administrator", "Dockerfile"), adminPushImage)

	log.Println("Building userservice image...")
	dockerBuild(filepath.Join(root, "e2e", "testserver"), "Dockerfile", userservicePushImage)

	log.Println("Pushing images to registry...")
	run("docker", "push", adminPushImage)
	run("docker", "push", userservicePushImage)

	// 2. Connect to the cluster and apply manifests.
	restCfg, err := kube.RestConfig(kube.Config{})
	if err != nil {
		log.Fatalf("Failed to get kubeconfig: %v", err)
	}

	log.Println("Applying administrator manifests...")
	if err := manifests.Apply(ctx, restCfg, filepath.Join(root, "deploy", "k8s", "administrator.yaml"), map[string]string{
		"{{ADMINISTRATOR_IMAGE}}": adminPullImage,
		"{{PROXY_IMAGE}}":         adminPullImage,
	}); err != nil {
		log.Fatalf("Failed to apply administrator manifests: %v", err)
	}

	log.Println("Applying userservice manifests...")
	if err := manifests.Apply(ctx, restCfg, filepath.Join(root, "deploy", "k8s", "testserver.yaml"), map[string]string{
		"{{USERSERVICE_IMAGE}}": userservicePullImage,
	}); err != nil {
		log.Fatalf("Failed to apply test server manifests: %v", err)
	}

	// 3. Restart deployments to pick up new images.
	log.Println("Restarting deployments...")
	run("kubectl", "rollout", "restart", "deployment/administrator", "-n", "bridge")
	run("kubectl", "rollout", "restart", "deployment/userservice", "-n", "userservice")

	// 4. Wait for rollouts.
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	log.Println("Waiting for administrator deployment...")
	if err := waitForDeployment(ctx, clientset, "bridge", "administrator"); err != nil {
		log.Fatalf("Administrator not ready: %v", err)
	}

	log.Println("Waiting for userservice deployment...")
	if err := waitForDeployment(ctx, clientset, "userservice", "userservice"); err != nil {
		log.Fatalf("Userservice not ready: %v", err)
	}

	log.Println("Done. Cluster is seeded.")
}

func dockerBuild(contextDir, dockerfile, tag string) {
	run("docker", "build", "-t", tag, "-f", filepath.Join(contextDir, dockerfile), contextDir)
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s %v failed: %v", name, args, err)
	}
}

func waitForDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return deploy.Status.ReadyReplicas > 0, nil
	})
}

func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find project root (go.mod)")
		}
		dir = parent
	}
}

package testutil

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/docker/docker/api/types/image"
	registrytypes "github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	"github.com/testcontainers/testcontainers-go/modules/registry"
	"github.com/testcontainers/testcontainers-go/network"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// k3sImage is the k3s image pinned to Kubernetes 1.29.
	k3sImage = "rancher/k3s:v1.29.10-k3s1"

	// registryImage is the Docker registry image.
	registryImage = "registry:2"

	// registryAlias is the hostname the k3s cluster uses to reach the registry
	// inside the shared Docker network.
	registryAlias = "registry.test"

	// registryPort is the port the registry listens on inside the container.
	registryPort = "5000"
)

// Cluster holds a k3s cluster and a local registry for e2e tests.
type Cluster struct {
	K3s      *k3s.K3sContainer
	Registry *registry.RegistryContainer
	Network  *testcontainers.DockerNetwork

	// RestConfig is the REST config for the cluster.
	RestConfig *rest.Config

	// Clientset is a ready-to-use Kubernetes client for the cluster.
	Clientset kubernetes.Interface

	// RegistryAddress is the host:port address of the registry reachable from
	// the test host (e.g. "localhost:xxxxx").
	RegistryAddress string

	// RegistryClusterAddress is the address of the registry reachable from
	// inside the k3s cluster (e.g. "registry.test:5000").
	RegistryClusterAddress string

	// KubeConfig is the raw kubeconfig YAML for the cluster.
	KubeConfig []byte

	// KubeConfigPath is the path to a temp file containing the kubeconfig
	// (host-accessible, e.g. server is localhost:port).
	KubeConfigPath string

	// InternalKubeConfigPath is the path to a temp file containing the
	// kubeconfig rewritten for Docker-network access (server is k3s-server:6443).
	InternalKubeConfigPath string
}

// SetupCluster creates a Docker network, starts a local registry and a k3s
// cluster configured to pull from that registry. The caller must call
// Cluster.TearDown when done.
func SetupCluster(ctx context.Context) (*Cluster, error) {
	c := &Cluster{}

	// 1. Create a shared Docker network so k3s can reach the registry by alias.
	net, err := network.New(ctx, network.WithDriver("bridge"))
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}
	c.Network = net

	// 2. Start the local registry on the shared network.
	reg, err := registry.Run(ctx, registryImage,
		network.WithNetwork([]string{registryAlias}, net),
	)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("start registry: %w", err)
	}
	c.Registry = reg
	c.RegistryClusterAddress = fmt.Sprintf("%s:%s", registryAlias, registryPort)

	hostAddr, err := reg.HostAddress(ctx)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("registry host address: %w", err)
	}
	c.RegistryAddress = hostAddr

	slog.Info("Registry started", "host_address", c.RegistryAddress, "cluster_address", c.RegistryClusterAddress)

	// 3. Start k3s on the same network, configured to mirror the registry.
	registriesYAML := fmt.Sprintf(`mirrors:
  "%s":
    endpoint:
      - "http://%s:%s"
`, c.RegistryClusterAddress, registryAlias, registryPort)

	k3sContainer, err := k3s.Run(ctx, k3sImage,
		network.WithNetwork([]string{"k3s-server"}, net),
		testcontainers.WithEnv(map[string]string{
			"K3S_ARGS": "--disable=traefik --disable=metrics-server",
		}),
		testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Files: []testcontainers.ContainerFile{
					{
						Reader:            strings.NewReader(registriesYAML),
						ContainerFilePath: "/etc/rancher/k3s/registries.yaml",
						FileMode:          0644,
					},
				},
			},
		}),
	)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("start k3s: %w", err)
	}
	c.K3s = k3sContainer

	// 4. Build a Kubernetes clientset from the k3s kubeconfig.
	kubeConfigYAML, err := k3sContainer.GetKubeConfig(ctx)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}
	c.KubeConfig = kubeConfigYAML

	// Write kubeconfig to a temp file so callers can set KUBECONFIG.
	kubeconfigFile, err := os.CreateTemp("", "bridge-e2e-kubeconfig-*")
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("create kubeconfig temp file: %w", err)
	}
	if _, err := kubeconfigFile.Write(kubeConfigYAML); err != nil {
		kubeconfigFile.Close()
		c.TearDown(ctx)
		return nil, fmt.Errorf("write kubeconfig temp file: %w", err)
	}
	kubeconfigFile.Close()
	c.KubeConfigPath = kubeconfigFile.Name()

	// Get the k3s container IP on the test network so the kubeconfig can use an
	// IP address rather than a hostname. This avoids DNS circular dependencies
	// when the bridge DNS interceptor replaces /etc/resolv.conf.
	k3sIP, err := k3sContainer.ContainerIP(ctx)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("get k3s container IP: %w", err)
	}
	slog.Info("k3s container IP", "ip", k3sIP)

	// Write a Docker-network-rewritten kubeconfig for use inside containers.
	internalKubeconfig, err := RewriteKubeconfigServer(kubeConfigYAML, fmt.Sprintf("https://%s:6443", k3sIP))
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("rewrite kubeconfig for internal use: %w", err)
	}
	internalFile, err := os.CreateTemp("", "bridge-e2e-kubeconfig-internal-*")
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("create internal kubeconfig temp file: %w", err)
	}
	if _, err := internalFile.Write(internalKubeconfig); err != nil {
		internalFile.Close()
		c.TearDown(ctx)
		return nil, fmt.Errorf("write internal kubeconfig temp file: %w", err)
	}
	internalFile.Close()
	c.InternalKubeConfigPath = internalFile.Name()

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigYAML)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	c.RestConfig = restCfg

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		c.TearDown(ctx)
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	c.Clientset = clientset

	slog.Info("Cluster started", "k3s_image", k3sImage)

	return c, nil
}

// TearDown stops all containers, removes the network, and cleans up temp files.
func (c *Cluster) TearDown(ctx context.Context) {
	if c.KubeConfigPath != "" {
		os.Remove(c.KubeConfigPath)
	}
	if c.InternalKubeConfigPath != "" {
		os.Remove(c.InternalKubeConfigPath)
	}
	if c.K3s != nil {
		if err := testcontainers.TerminateContainer(c.K3s); err != nil {
			slog.Warn("Failed to terminate k3s", "error", err)
		}
	}
	if c.Registry != nil {
		if err := testcontainers.TerminateContainer(c.Registry); err != nil {
			slog.Warn("Failed to terminate registry", "error", err)
		}
	}
	if c.Network != nil {
		if err := c.Network.Remove(ctx); err != nil {
			slog.Warn("Failed to remove network", "error", err)
		}
	}
}

// PushImage tags a local image for the test registry and pushes it.
// localRef should be a locally available image (e.g. "bridge:test").
// remoteName is the name:tag used in the registry (e.g. "bridge:test").
// Returns the full cluster-internal reference (e.g. "registry.test:5000/bridge:test").
func (c *Cluster) PushImage(ctx context.Context, localRef, remoteName string) (string, error) {
	clusterRef := fmt.Sprintf("%s/%s", c.RegistryClusterAddress, remoteName)
	hostRef := fmt.Sprintf("%s/%s", c.RegistryAddress, remoteName)

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return "", fmt.Errorf("create docker client: %w", err)
	}
	defer cli.Close()

	// Tag the local image for the registry.
	if err := cli.ImageTag(ctx, localRef, hostRef); err != nil {
		return "", fmt.Errorf("tag image %s -> %s: %w", localRef, hostRef, err)
	}

	// Push with empty auth (local registry has no auth).
	authBytes, _ := json.Marshal(registrytypes.AuthConfig{})
	pushOpts := image.PushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(authBytes),
	}
	reader, err := cli.ImagePush(ctx, hostRef, pushOpts)
	if err != nil {
		return "", fmt.Errorf("push image %s: %w", hostRef, err)
	}
	defer reader.Close()
	// Drain the push output to ensure completion.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return "", fmt.Errorf("push image %s: %w", hostRef, err)
	}

	slog.Info("Pushed image to registry", "local", localRef, "cluster_ref", clusterRef)
	return clusterRef, nil
}

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// DispatcherPort is the default port the dispatcher listens on
	DispatcherPort = "8080/tcp"
)

// DispatcherContainer represents a running dispatcher container
type DispatcherContainer struct {
	Container testcontainers.Container
	Host      string
	Port      string
}

// DispatcherConfig configures the dispatcher container
type DispatcherConfig struct {
	// Env is additional environment variables (e.g., BRIDGE_SERVER_ADDR)
	Env map[string]string
	// Network is the Docker network to join
	Network string
	// NetworkAliases are DNS aliases for this container on the network
	NetworkAliases []string
	// ExtraHosts are additional /etc/hosts entries (format: "hostname:ip")
	ExtraHosts []string
}

// NewDispatcher creates and starts a new dispatcher container.
// The caller is responsible for calling Terminate() when done.
func NewDispatcher(ctx context.Context, cfg DispatcherConfig) (*DispatcherContainer, error) {
	buildCtx, err := createDispatcherBuildContext()
	if err != nil {
		return nil, fmt.Errorf("failed to create dispatcher build context: %w", err)
	}

	env := map[string]string{
		"LOG_LEVEL": "debug",
	}
	for k, v := range cfg.Env {
		env[k] = v
	}

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    buildCtx,
			Dockerfile: "Dockerfile",
		},
		ExposedPorts: []string{DispatcherPort},
		Env:          env,
		WaitingFor:   wait.ForHTTP("/__bridge/health").WithPort(DispatcherPort).WithStartupTimeout(120 * time.Second),
	}

	if len(cfg.ExtraHosts) > 0 {
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.ExtraHosts = append(hc.ExtraHosts, cfg.ExtraHosts...)
		}
	}

	if cfg.Network != "" {
		req.Networks = []string{cfg.Network}
		if len(cfg.NetworkAliases) > 0 {
			req.NetworkAliases = map[string][]string{
				cfg.Network: cfg.NetworkAliases,
			}
		}
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		os.RemoveAll(buildCtx)
		return nil, fmt.Errorf("failed to start dispatcher container: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		os.RemoveAll(buildCtx)
		return nil, fmt.Errorf("failed to get container host: %w", err)
	}

	mappedPort, err := container.MappedPort(ctx, DispatcherPort)
	if err != nil {
		container.Terminate(ctx)
		os.RemoveAll(buildCtx)
		return nil, fmt.Errorf("failed to get mapped port: %w", err)
	}

	return &DispatcherContainer{
		Container: container,
		Host:      host,
		Port:      mappedPort.Port(),
	}, nil
}

// URL returns the HTTP URL to reach the dispatcher
func (d *DispatcherContainer) URL() string {
	return fmt.Sprintf("http://%s:%s", d.Host, d.Port)
}

// Terminate stops and removes the container
func (d *DispatcherContainer) Terminate(ctx context.Context) error {
	if d.Container != nil {
		return d.Container.Terminate(ctx)
	}
	return nil
}

// Logs returns the container logs
func (d *DispatcherContainer) Logs(ctx context.Context) (string, error) {
	reader, err := d.Container.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ContainerIP returns the container's IP address on the given network
func (d *DispatcherContainer) ContainerIP(ctx context.Context, network string) (string, error) {
	inspect, err := d.Container.Inspect(ctx)
	if err != nil {
		return "", err
	}

	if network == "" {
		for _, net := range inspect.NetworkSettings.Networks {
			return net.IPAddress, nil
		}
		return "", fmt.Errorf("no network found")
	}

	net, ok := inspect.NetworkSettings.Networks[network]
	if !ok {
		return "", fmt.Errorf("network %s not found", network)
	}
	return net.IPAddress, nil
}

// createDispatcherBuildContext creates a temporary directory with the dispatcher source,
// its local @vercel/bridge-api dependency, and the Dockerfile.
func createDispatcherBuildContext() (string, error) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp("", "dispatcher-docker-ctx-*")
	if err != nil {
		return "", err
	}

	// Copy the Dockerfile from the dispatcher service
	srcDockerfile := filepath.Join(projectRoot, "services", "dispatcher", "Dockerfile")
	if err := copyFile(srcDockerfile, filepath.Join(tmpDir, "Dockerfile")); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to copy Dockerfile: %w", err)
	}

	// Copy api/ts (the local @vercel/bridge-api dependency)
	apiTsSrc := filepath.Join(projectRoot, "api", "ts")
	apiTsDst := filepath.Join(tmpDir, "api", "ts")
	if err := copyDir(apiTsSrc, apiTsDst); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to copy api/ts: %w", err)
	}

	// Copy services/dispatcher
	dispatcherSrc := filepath.Join(projectRoot, "services", "dispatcher")
	dispatcherDst := filepath.Join(tmpDir, "services", "dispatcher")
	if err := copyDir(dispatcherSrc, dispatcherDst); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to copy services/dispatcher: %w", err)
	}

	return tmpDir, nil
}

// copyDir recursively copies a directory tree, skipping node_modules and dist directories.
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Skip node_modules and dist to keep the build context small
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "dist") {
			continue
		}

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	buildMu    sync.Mutex
	binaryPath string
)

// BuildBridge compiles the bridge CLI for Linux.
// The binary is cached and reused across tests. If the cached binary has been
// deleted (e.g. by CleanupBuild in another suite's teardown), it will be rebuilt.
func BuildBridge() (string, error) {
	buildMu.Lock()
	defer buildMu.Unlock()

	// Return cached path if the binary still exists.
	if binaryPath != "" {
		if _, err := os.Stat(binaryPath); err == nil {
			return binaryPath, nil
		}
		// Binary was deleted; rebuild.
		binaryPath = ""
	}

	projectRoot, err := FindProjectRoot()
	if err != nil {
		return "", fmt.Errorf("failed to find project root: %w", err)
	}

	// Create a temp directory for the binary
	tmpDir, err := os.MkdirTemp("", "bridge-e2e-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	binaryPath = filepath.Join(tmpDir, "bridge")

	// Cross-compile for Linux (same architecture as host)
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/bridge")
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		binaryPath = ""
		return "", fmt.Errorf("failed to build bridge: %w", err)
	}

	return binaryPath, nil
}

// CleanupBuild removes the built binary.
// It is safe to call from any suite's teardown — BuildBridge will detect
// the missing file and rebuild on the next call.
func CleanupBuild() {
	buildMu.Lock()
	defer buildMu.Unlock()
	if binaryPath != "" {
		os.RemoveAll(filepath.Dir(binaryPath))
		binaryPath = ""
	}
}

// BuildBridgeImage builds the bridge Docker image from the project's root
// Dockerfile. The build context is the project root so that go.mod, cmd/,
// pkg/, etc. are all available.
func BuildBridgeImage(ctx context.Context, tag string) error {
	projectRoot, err := FindProjectRoot()
	if err != nil {
		return fmt.Errorf("find project root: %w", err)
	}

	dockerfile := "Dockerfile"
	slog.Info("Building bridge image", "tag", tag, "dockerfile", dockerfile)

	cmd := exec.CommandContext(ctx, "docker", "build",
		"-t", tag,
		"-f", dockerfile,
		".",
	)
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

// BuildUserserviceImage builds the userservice Docker image from e2e/testserver/Dockerfile.
// The build context is the e2e/testserver directory.
func BuildUserserviceImage(ctx context.Context, tag string) error {
	projectRoot, err := FindProjectRoot()
	if err != nil {
		return fmt.Errorf("find project root: %w", err)
	}

	buildCtx := filepath.Join(projectRoot, "e2e", "testserver")
	slog.Info("Building userservice image", "tag", tag, "context", buildCtx)

	cmd := exec.CommandContext(ctx, "docker", "build",
		"-t", tag,
		".",
	)
	cmd.Dir = buildCtx
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

// BuildInterceptImage builds a Docker image from e2e/Dockerfile.devcontainer
// with the pre-built bridge binary copied into the build context. This is needed
// because the Dockerfile expects a `bridge` binary in the build context, while
// BuildBridge() outputs it to a temp path.
func BuildInterceptImage(ctx context.Context, bridgeBinPath, tag string) error {
	projectRoot, err := FindProjectRoot()
	if err != nil {
		return fmt.Errorf("find project root: %w", err)
	}

	// Create a temp build context with the bridge binary and Dockerfile.
	tmpDir, err := os.MkdirTemp("", "bridge-intercept-image-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy bridge binary into the build context.
	bridgeSrc, err := os.ReadFile(bridgeBinPath)
	if err != nil {
		return fmt.Errorf("read bridge binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "bridge"), bridgeSrc, 0755); err != nil {
		return fmt.Errorf("write bridge binary to build context: %w", err)
	}

	// Copy Dockerfile.devcontainer as Dockerfile.
	dockerfileSrc, err := os.ReadFile(filepath.Join(projectRoot, "e2e", "Dockerfile.devcontainer"))
	if err != nil {
		return fmt.Errorf("read Dockerfile.devcontainer: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), dockerfileSrc, 0644); err != nil {
		return fmt.Errorf("write Dockerfile to build context: %w", err)
	}

	slog.Info("Building intercept image", "tag", tag, "context", tmpDir)

	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, ".")
	cmd.Dir = tmpDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

// FindProjectRoot finds the project root by looking for go.mod.
func FindProjectRoot() (string, error) {
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

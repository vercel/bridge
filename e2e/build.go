package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	buildOnce  sync.Once
	buildErr   error
	binaryPath string
)

// BuildBridge compiles the bridge CLI for Linux.
// The binary is cached and reused across tests.
func BuildBridge() (string, error) {
	buildOnce.Do(func() {
		projectRoot, err := findProjectRoot()
		if err != nil {
			buildErr = fmt.Errorf("failed to find project root: %w", err)
			return
		}

		// Create a temp directory for the binary
		tmpDir, err := os.MkdirTemp("", "bridge-e2e-*")
		if err != nil {
			buildErr = fmt.Errorf("failed to create temp dir: %w", err)
			return
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
			buildErr = fmt.Errorf("failed to build bridge: %w", err)
			return
		}
	})

	return binaryPath, buildErr
}

// CleanupBuild removes the built binary.
// Call this after all tests are done.
func CleanupBuild() {
	if binaryPath != "" {
		os.RemoveAll(filepath.Dir(binaryPath))
	}
}

// BuildBridgeImage builds the bridge Docker image from the project's root
// Dockerfile. The build context is the project root so that go.mod, cmd/,
// pkg/, etc. are all available.
func BuildBridgeImage(ctx context.Context, tag string) error {
	projectRoot, err := findProjectRoot()
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

// findProjectRoot walks up from the current directory until it finds go.mod.
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

// BuildUserserviceImage builds the userservice Docker image from e2e/testserver/Dockerfile.
// The build context is the e2e/testserver directory.
func BuildUserserviceImage(ctx context.Context, tag string) error {
	projectRoot, err := findProjectRoot()
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

// copyDir recursively copies a directory tree.
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

// copyFile copies a single file preserving permissions.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode())
}

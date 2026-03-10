package commands

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/intercept"
)

const execUsageText = `bridge exec <deployment> <command...>

Examples:
  bridge exec my-api -- curl http://redis:6379
  bridge exec my-api -- npm test
  bridge exec my-api -- wget -O - http://svc.ns.svc.cluster.local/`

// Exec returns the CLI command for running a command as a deployment.
func Exec() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "Run a command in a deployment's environment",
		UsageText: execUsageText,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "devcontainer-config",
				Aliases: []string{"f"},
				Usage:   "Path to a devcontainer.json to use (overrides the auto-derived config)",
			},
		},
		Before: preflightCreate,
		Action: runExec,
	}
}

func runExec(ctx context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	if len(args) < 2 {
		return fmt.Errorf("usage: bridge exec <deployment> <command...>")
	}
	deploymentName := args[0]
	cmdArgs := args[1:]

	w := c.Root().Writer

	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}

	bridgeName := identity.BridgeResourceName(deviceID, deploymentName)
	ct := container.NewDockerClient()
	labels := map[string]string{labelBridgeDeployment: bridgeName}

	// Resolve the devcontainer config path. When -f is provided, use it
	// directly; otherwise derive it from the base config and bridge name.
	var dcConfigPath string
	if explicit := c.String("devcontainer-config"); explicit != "" {
		dcConfigPath = explicit
	} else {
		baseConfig, err := devcontainer.ResolveConfigPath("")
		if err != nil {
			return err
		}
		dcConfigPath = bridgeConfigPath(baseConfig, bridgeName)
	}
	workspaceFolder, _ := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(dcConfigPath))))

	// Check if a container is already running for this bridge.
	if containerID, findErr := ct.FindID(ctx, container.FindOpts{Labels: labels}); findErr == nil {
		slog.Debug("Container already running, checking health", "bridge", bridgeName)
		if err := intercept.WaitForReady(ctx, ct, containerID); err != nil {
			return fmt.Errorf("interceptor is not healthy: %w", err)
		}
		return execInDevcontainer(ctx, workspaceFolder, dcConfigPath, nil, cmdArgs)
	}

	// No running container — check if a bridge config exists.
	if _, err := os.Stat(dcConfigPath); err != nil {
		return fmt.Errorf("no bridge found for %q — run: bridge create %s", deploymentName, deploymentName)
	}

	// Config exists but container isn't running — start it.
	sp := interact.NewSpinner(w, "Starting devcontainer...")
	ctx = interact.WithSpinner(ctx, sp)
	sp.Start(ctx)

	if err := startDevcontainer(ctx, w, ct, dcConfigPath, bridgeName, ""); err != nil {
		return err
	}

	return execInDevcontainer(ctx, workspaceFolder, dcConfigPath, nil, cmdArgs)
}

// execInDevcontainer runs a command via `devcontainer exec` with stdio
// attached to the parent process.
func execInDevcontainer(ctx context.Context, workspaceFolder, configPath string, stdin io.Reader, cmdArgs []string) error {
	dcClient := &devcontainer.Client{
		WorkspaceFolder: workspaceFolder,
		ConfigPath:      configPath,
		Stdin:           stdin,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
	}
	return dcClient.ExecAttached(ctx, cmdArgs)
}

// bridgeConfigPath returns the expected devcontainer config path for a bridge,
// given the base config path and bridge deployment name.
func bridgeConfigPath(baseConfigPath, bridgeName string) string {
	baseParent := filepath.Dir(baseConfigPath)
	if filepath.Base(baseParent) == ".devcontainer" {
		return filepath.Join(baseParent, fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	}
	return filepath.Join(baseParent, ".devcontainer", fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
}

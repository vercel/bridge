package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/intercept"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/session"
)

const execUsageText = `bridge exec <bridge name> <command...>

Examples:
  bridge exec my-api -- curl http://redis:6379
  bridge exec my-api -- npm test
  bridge exec my-api -- wget -O - http://svc.ns.svc.cluster.local/`

// Exec returns the CLI command for running a command as a deployment.
func Exec() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "Run a command in a bridge created Devcontainer",
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
		return fmt.Errorf("usage: bridge exec <bridge name> <command...>")
	}
	bridgeName := args[0]
	cmdArgs := args[1:]

	root := c.Root()
	w := root.Writer

	ct := container.NewDockerClient()
	labels := map[string]string{labelBridgeDeployment: bridgeName}

	newDC := func(configPath string) devcontainer.Client {
		workspaceFolder, _ := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(configPath))))
		return &devcontainer.CLIClient{
			WorkspaceFolder: workspaceFolder,
			ConfigPath:      configPath,
			Stdin:           root.Reader,
			Stdout:          root.Writer,
			Stderr:          root.ErrWriter,
		}
	}

	// Check if a container is already running for this bridge. When it is,
	// read the config path from the container's devcontainer.config_file
	// label so exec works regardless of the CWD or -f flag used at create time.
	if containerID, findErr := ct.FindID(ctx, container.FindOpts{Labels: labels}); findErr == nil {
		slog.Debug("Container already running, checking health", "bridge", bridgeName)
		if err := intercept.WaitForReady(ctx, ct, containerID); err != nil {
			return fmt.Errorf("interceptor is not healthy: %w", err)
		}

		// Always read the config from the container — this is the config it
		// was actually created with, regardless of what -f points to now.
		dcConfigPath, err := ct.InspectLabel(ctx, containerID, meta.LabelDevcontainerConfigFile)
		if err != nil || dcConfigPath == "" {
			return fmt.Errorf("could not determine config path for running bridge %q", bridgeName)
		}
		return newDC(dcConfigPath).Exec(ctx, cmdArgs)
	}

	// No running container — resolve the devcontainer config to use.
	// If -f is explicitly set, use it directly; otherwise look up the session.
	dcConfigPath := c.String("devcontainer-config")
	if dcConfigPath == "" {
		sess, sessErr := session.Load(bridgeName)
		if sessErr != nil {
			return fmt.Errorf("no bridge found for %q — run: bridge create %s", bridgeName, bridgeName)
		}
		dcConfigPath = sess.DevcontainerConfigPath
	}
	if _, err := os.Stat(dcConfigPath); err != nil {
		return fmt.Errorf("devcontainer config not found at %s — run: bridge create %s", dcConfigPath, bridgeName)
	}

	// Config exists but container isn't running — start it.
	sp := interact.NewSpinner(w, "Starting devcontainer...")
	ctx = interact.WithSpinner(ctx, sp)
	sp.Start(ctx)

	dc := newDC(dcConfigPath)

	if err := startDevcontainer(ctx, w, ct, dc, bridgeName); err != nil {
		return err
	}

	return dc.Exec(ctx, cmdArgs)
}

package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/interact"
)

const (
	installEdgeURL = "https://raw.githubusercontent.com/vercel/bridge/main/install-edge.sh"
	installURL     = "https://raw.githubusercontent.com/vercel/bridge/main/install.sh"
)

// Update returns the CLI command for updating the bridge binary.
func Update() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Update bridge to the latest version",
		Action: func(ctx context.Context, c *cli.Command) error {
			p := interact.NewPrinter(c.Root().Writer)

			scriptURL := installURL
			channel := "stable"
			if strings.HasPrefix(Version, "edge-") || Version == "dev" {
				scriptURL = installEdgeURL
				channel = "edge"
			}

			p.Info(fmt.Sprintf("Updating bridge (%s channel)...", channel))

			cmd := exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("curl -fsSL %s | sh", scriptURL))
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				return fmt.Errorf("update failed: %w", err)
			}

			p.Success("Bridge updated successfully.")
			return nil
		},
	}
}

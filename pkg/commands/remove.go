package commands

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/urfave/cli/v3"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/session"
)

// Remove returns the CLI command for removing a bridge.
func Remove() *cli.Command {
	return &cli.Command{
		Name:    "remove",
		Aliases: []string{"rm", "delete"},
		Usage:   "Tear down a running bridge",
		Description: `With --output=json, emits a CommandResult envelope (see "bridge --help").
Run "bridge schema remove-response" for the response payload schema.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "admin-addr",
				Usage:   "Address of the bridge administrator",
				Value:   defaultAdminAddr,
				Sources: cli.EnvVars("BRIDGE_ADMIN_ADDR"),
			},
			&cli.BoolFlag{
				Name:    "yes",
				Aliases: []string{"y"},
				Usage:   "Auto-accept all confirmation prompts",
			},
		},
		Action: runRemove,
	}
}

func runRemove(ctx context.Context, c *cli.Command) error {
	names := c.Args().Slice()
	if len(names) == 0 {
		return fmt.Errorf("at least one bridge name is required")
	}
	adminAddr := c.String("admin-addr")

	w := c.Root().Writer
	p := interact.NewPrinter(w)

	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}

	sp := interact.NewSpinner(w, "Connecting to bridge administrator...")
	ctx = interact.WithSpinner(ctx, sp)
	sp.Start(ctx)

	adm, err := connectAdmin(ctx, adminAddr)
	if err != nil {
		sp.Stop()
		return err
	}
	defer adm.Close()

	var errs []error
	var removed []string
	ct := container.NewDockerClient()
	for _, name := range names {
		sp.SetTitle(fmt.Sprintf("Removing bridge %q...", name))

		_, err = adm.DeleteBridge(ctx, &bridgev1.DeleteBridgeRequest{
			DeviceId:   deviceID,
			Name:       name,
			DeviceInfo: deviceInfo(),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to remove bridge %q: %w", name, err))
			continue
		}

		ct.StopAll(ctx, container.StopAllOpts{
			Labels: map[string]string{labelBridgeDeployment: name},
		})

		if err := session.Delete(name); err != nil {
			slog.Warn("Failed to delete session", "name", name, "error", err)
		}

		removed = append(removed, name)
		p.Newline()
		p.Success(fmt.Sprintf("Bridge %q removed", name))
	}
	sp.Stop()

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		errMsg := strings.Join(msgs, "\n")
		if interact.IsJSON() {
			resp := &bridgev1.RemoveCommandResponse{Removed: removed}
			return writeResult(w, resp, errMsg)
		}
		return fmt.Errorf("%s", errMsg)
	}

	if interact.IsJSON() {
		resp := &bridgev1.RemoveCommandResponse{Removed: removed}
		return writeResult(w, resp, "")
	}
	return nil
}

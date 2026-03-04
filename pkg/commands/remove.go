package commands

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
)

// Remove returns the CLI command for removing a bridge.
func Remove() *cli.Command {
	return &cli.Command{
		Name:    "remove",
		Aliases: []string{"rm"},
		Usage:   "Remove a bridge",
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
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "name",
				UsageText: "Name of the bridge to remove",
				Config: cli.StringConfig{
					TrimSpace: true,
				},
			},
		},
		Action: runRemove,
	}
}

func runRemove(ctx context.Context, c *cli.Command) error {
	name := c.StringArg("name")
	if name == "" {
		return fmt.Errorf("bridge name is required")
	}
	adminAddr := c.String("admin-addr")
	yes := c.Bool("yes")

	r := c.Root().Reader
	p := interact.NewPrinter(c.Root().Writer)

	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}

	adm, isLocal, err := connectAdmin(ctx, adminAddr, deviceID)
	if err != nil {
		return err
	}
	defer adm.Close()

	if isLocal && !yes {
		if !confirmLocalFallback(p, r) {
			p.Println("Aborted.")
			return nil
		}
	}

	listResp, err := adm.ListBridges(ctx, &bridgev1.ListBridgesRequest{DeviceId: deviceID})
	if err != nil {
		return fmt.Errorf("failed to list bridges: %w", err)
	}

	// Find the bridge by deployment name.
	var found *bridgev1.BridgeInfo
	for _, b := range listResp.Bridges {
		if b.DeploymentName == name {
			found = b
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no bridge named %q found", name)
	}

	sp := interact.NewSpinner("Removing bridge...")
	go sp.Start(ctx)

	_, err = adm.DeleteBridge(ctx, &bridgev1.DeleteBridgeRequest{
		DeviceId:  deviceID,
		Name:      name,
		Namespace: found.Namespace,
	})
	sp.Stop()
	if err != nil {
		return fmt.Errorf("failed to remove bridge: %w", err)
	}

	// Clean up local Docker containers for this bridge.
	stopBridgeContainers(ctx, name)

	p.Newline()
	p.Success(fmt.Sprintf("Bridge %q removed", name))
	return nil
}

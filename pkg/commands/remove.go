package commands

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/container"
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
	yes := c.Bool("yes") || interact.IsAgent()

	r := c.Root().Reader
	w := c.Root().Writer
	p := interact.NewPrinter(w)

	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}

	sp := interact.NewSpinner(w, "Connecting to bridge administrator...")
	ctx = interact.WithSpinner(ctx, sp)
	go sp.Start(ctx)

	adm, isLocal, err := connectAdmin(ctx, adminAddr, deviceID)
	if err != nil {
		sp.Stop()
		return err
	}
	defer adm.Close()

	if isLocal && !yes {
		sp.Stop()
		if !confirmLocalFallback(p, r) {
			p.Println("Aborted.")
			return nil
		}
		sp = interact.NewSpinner(w, "")
		ctx = interact.WithSpinner(ctx, sp)
		go sp.Start(ctx)
	}

	listResp, err := adm.ListBridges(ctx, &bridgev1.ListBridgesRequest{DeviceId: deviceID})
	if err != nil {
		sp.Stop()
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
		sp.Stop()
		return fmt.Errorf("no bridge named %q found", name)
	}

	sp.SetTitle("Removing bridge...")

	_, err = adm.DeleteBridge(ctx, &bridgev1.DeleteBridgeRequest{
		DeviceId:  deviceID,
		Name:      name,
		Namespace: found.Namespace,
	})
	sp.Stop()
	if err != nil {
		return fmt.Errorf("failed to remove bridge: %w", err)
	}

	// Clean up local containers for this bridge.
	container.NewDockerClient().StopAll(ctx, container.StopAllOpts{
		Labels: map[string]string{labelBridgeDeployment: name},
	})

	p.Newline()
	p.Success(fmt.Sprintf("Bridge %q removed", name))
	return nil
}

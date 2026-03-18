package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
)

// Get returns the CLI command for listing or inspecting bridges.
func Get() *cli.Command {
	return &cli.Command{
		Name:  "get",
		Usage: "List or inspect running bridges",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "admin-addr",
				Usage:   "Address of the bridge administrator",
				Value:   defaultAdminAddr,
				Sources: cli.EnvVars("BRIDGE_ADMIN_ADDR"),
			},
		},
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "name",
				UsageText: "Name of the bridge to inspect (omit to list all)",
				Config: cli.StringConfig{
					TrimSpace: true,
				},
			},
		},
		Action: runGet,
	}
}

func runGet(ctx context.Context, c *cli.Command) error {
	name := c.StringArg("name")
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
	sp.Stop()
	if err != nil {
		return err
	}
	defer adm.Close()

	listResp, err := adm.ListBridges(ctx, &bridgev1.ListBridgesRequest{DeviceId: deviceID, DeviceInfo: deviceInfo()})
	if err != nil {
		return fmt.Errorf("failed to list bridges: %w", err)
	}

	if name == "" {
		return listBridges(p, listResp.Bridges)
	}
	return showBridge(p, listResp.Bridges, name)
}

func listBridges(p interact.Printer, bridges []*bridgev1.BridgeInfo) error {
	if len(bridges) == 0 {
		p.Muted("No active bridges")
		return nil
	}

	p.Printlnf("%-30s %-30s %-10s %s", "NAME", "SOURCE", "STATUS", "AGE")
	for _, b := range bridges {
		age := humanAge(b.CreatedAt)
		source := b.SourceDeployment
		if b.SourceNamespace != "" {
			source += "/" + b.SourceNamespace
		}
		p.Printlnf("%-30s %-30s %-10s %s", b.Name, source, b.Status, age)
	}
	return nil
}

func showBridge(p interact.Printer, bridges []*bridgev1.BridgeInfo, name string) error {
	for _, b := range bridges {
		if b.Name == name {
			p.Newline()
			p.KeyValue("Name", b.Name)
			p.KeyValue("Deployment", b.DeploymentName)
			p.KeyValue("Status", b.Status)
			p.KeyValue("Age", humanAge(b.CreatedAt))
			p.KeyValue("Namespace", b.Namespace)
			if b.SourceDeployment != "" {
				source := b.SourceDeployment
				if b.SourceNamespace != "" {
					source += " (" + b.SourceNamespace + ")"
				}
				p.KeyValue("Source", source)
			}
			p.Newline()
			return nil
		}
	}
	return fmt.Errorf("no bridge named %q found", name)
}

// humanAge parses an RFC 3339 timestamp and returns a human-readable duration
// string using the shortest unit: "30s", "5m", "2h", "3d".
func humanAge(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

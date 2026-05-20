package commands

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urfave/cli/v3"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/intercept"
)

// appStatusTimeout caps how long bridge get waits for any single
// interceptor to respond. Bridges with stuck containers can otherwise hang
// the whole listing.
const appStatusTimeout = 1500 * time.Millisecond

// Get returns the CLI command for listing or inspecting bridges.
func Get() *cli.Command {
	return &cli.Command{
		Name:  "get",
		Usage: "List or inspect running bridges",
		UsageText: `With --output=json, emits a CommandResult envelope (see "bridge --help").
Run "bridge schema get-response" for the response payload schema.`,
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

	appStatuses := queryAppStatuses(ctx, listResp.Bridges)

	if interact.IsJSON() {
		resp := &bridgev1.GetCommandResponse{}
		for _, b := range listResp.Bridges {
			resp.Bridges = append(resp.Bridges, &bridgev1.GetCommandResponseBridge{
				Name:              b.Name,
				SourceDeployment:  b.SourceDeployment,
				SourceNamespace:   b.SourceNamespace,
				Namespace:         b.Namespace,
				CreatedAt:         b.CreatedAt,
				Status:            b.Status,
				DeploymentName:    b.DeploymentName,
				ApplicationStatus: appStatuses[b.Name],
			})
		}
		return writeResult(w, resp, "")
	}

	if name == "" {
		return listBridges(p, listResp.Bridges, appStatuses)
	}
	return showBridge(p, listResp.Bridges, name, appStatuses)
}

// queryAppStatuses asks each bridge's interceptor for its current health
// and aggregates the result into a per-bridge string. We run the queries
// concurrently with a small per-bridge timeout so a stuck devcontainer
// doesn't make `bridge get` hang.
func queryAppStatuses(ctx context.Context, bridges []*bridgev1.BridgeInfo) map[string]string {
	ct := container.NewDockerClient()
	out := make(map[string]string, len(bridges))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range bridges {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s := appStatusForBridge(ctx, ct, name)
			mu.Lock()
			out[name] = s
			mu.Unlock()
		}(b.Name)
	}
	wg.Wait()
	return out
}

// appStatusForBridge returns the developer-facing application status for a
// single bridge. See the field comment on
// GetCommandResponseBridge.application_status for the value set.
func appStatusForBridge(parent context.Context, ct container.Client, bridgeName string) string {
	ctx, cancel := context.WithTimeout(parent, appStatusTimeout)
	defer cancel()

	containerID, err := ct.FindID(ctx, container.FindOpts{Labels: map[string]string{labelBridgeDeployment: bridgeName}})
	if err != nil {
		return "stopped"
	}

	conn, err := intercept.Connect(ctx, ct, containerID)
	if err != nil {
		return "unreachable"
	}
	defer conn.Close()

	cli := bridgev1.NewInterceptorServiceClient(conn)
	resp, err := cli.GetStatus(ctx, &bridgev1.GetStatusRequest{})
	if err != nil {
		return "unreachable"
	}
	if !resp.GetReady() {
		return "starting"
	}

	var configured, healthy, unhealthy int
	for _, p := range []*bridgev1.ProbeStatus{resp.GetLiveness(), resp.GetReadiness(), resp.GetStartup()} {
		if p == nil {
			continue
		}
		configured++
		switch p.GetHealth() {
		case bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY:
			healthy++
		case bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY:
			unhealthy++
		}
	}
	switch {
	case configured == 0:
		// No probes declared on the source — match the bridge dev default
		// of treating "nothing to check" as healthy rather than surfacing
		// a "no probes" signal that most users find confusing.
		return "healthy"
	case unhealthy > 0:
		return "unhealthy"
	case healthy == configured:
		return "healthy"
	default:
		return "starting"
	}
}

func listBridges(p interact.Printer, bridges []*bridgev1.BridgeInfo, appStatuses map[string]string) error {
	if len(bridges) == 0 {
		p.Muted("No active bridges")
		return nil
	}

	p.Printlnf("%-30s %-30s %-10s %-12s %s", "NAME", "SOURCE", "STATUS", "APPLICATION", "AGE")
	for _, b := range bridges {
		age := humanAge(b.CreatedAt)
		source := b.SourceDeployment
		if b.SourceNamespace != "" {
			source += "/" + b.SourceNamespace
		}
		p.Printlnf("%-30s %-30s %-10s %-12s %s", b.Name, source, b.Status, appStatuses[b.Name], age)
	}
	return nil
}

func showBridge(p interact.Printer, bridges []*bridgev1.BridgeInfo, name string, appStatuses map[string]string) error {
	for _, b := range bridges {
		if b.Name == name {
			p.Newline()
			p.KeyValue("Name", b.Name)
			p.KeyValue("Deployment", b.DeploymentName)
			p.KeyValue("Status", b.Status)
			p.KeyValue("Application", appStatuses[b.Name])
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
